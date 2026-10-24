package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

type autocompleteFixture struct {
	f            *recoveryFixture
	mu           sync.Mutex
	users        []recoveryUser
	admin        map[int64]bool
	fail         string
	delay        time.Duration
	freshChanged bool
	calls        atomic.Int64
	writes       atomic.Int64
	active       atomic.Int64
	maxActive    atomic.Int64
}

func newAutocompleteFixture(t *testing.T, n int) *autocompleteFixture {
	a := &autocompleteFixture{f: newRecoveryFixture(t), admin: map[int64]bool{}}
	for i := 0; i < n; i++ {
		u := ordinaryMock()
		u.PK = int64(42 + i)
		u.Username = fmt.Sprintf("human-%03d", i)
		if i%2 == 1 {
			u.Type = "external"
		}
		a.users = append(a.users, u)
	}
	a.f.r.api.client.Transport = mockRoundTripper(a.roundTrip)
	return a
}
func (a *autocompleteFixture) roundTrip(req *http.Request) (*http.Response, error) {
	a.calls.Add(1)
	active := a.active.Add(1)
	defer a.active.Add(-1)
	for old := a.maxActive.Load(); active > old; old = a.maxActive.Load() {
		if a.maxActive.CompareAndSwap(old, active) {
			break
		}
	}
	if req.Method != "GET" {
		a.writes.Add(1)
		a.f.t.Error("autocomplete attempted a mutation")
	}
	a.mu.Lock()
	users := append([]recoveryUser{}, a.users...)
	delay, fail, changed := a.delay, a.fail, a.freshChanged
	admin := map[int64]bool{}
	for k, v := range a.admin {
		admin[k] = v
	}
	a.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
	}
	if req.Context().Err() != nil {
		return nil, req.Context().Err()
	}
	if req.URL.Path == fail {
		return mockResponse(503, map[string]string{"secret": "MOCK_PRIVATE_UPSTREAM"}), nil
	}
	switch req.URL.Path {
	case "/api/v3/core/users/me/":
		return mockResponse(200, map[string]any{"user": a.f.service}), nil
	case "/api/v3/core/users/":
		if name := req.URL.Query().Get("username"); name != "" {
			filtered := []recoveryUser{}
			for _, u := range users {
				if u.Username == name {
					if changed {
						u.Updated = "later"
					}
					filtered = append(filtered, u)
				}
			}
			users = filtered
		}
		count := len(users)
		page, _ := strconv.Atoi(req.URL.Query().Get("page"))
		if page < 1 {
			page = 1
		}
		total := (count + 99) / 100
		if total == 0 {
			total = 1
		}
		next := 0
		if page < total {
			next = page + 1
		}
		start := (page - 1) * 100
		end := start + 100
		if end > count {
			end = count
		}
		if start > count {
			start = count
		}
		users = users[start:end]
		return mockResponse(200, pageMock(users, page, next, total, count)), nil
	case "/api/v3/rbac/roles/":
		roles := []recoveryRole{}
		pk, _ := strconv.ParseInt(req.URL.Query().Get("users"), 10, 64)
		if admin[pk] {
			roles = append(roles, recoveryRole{PK: mockRoleID})
		}
		return mockResponse(200, pageMock(roles, 1, 0, 1, len(roles))), nil
	case "/api/v3/rbac/permissions/":
		perms := []recoveryPermission{{ID: 9, App: "authentik_core", Model: "user", Code: "change_user"}}
		return mockResponse(200, pageMock(perms, 1, 0, 1, 1)), nil
	}
	if strings.HasPrefix(req.URL.Path, "/api/v3/core/users/") {
		raw := strings.TrimSuffix(strings.TrimPrefix(req.URL.Path, "/api/v3/core/users/"), "/")
		pk, _ := strconv.ParseInt(raw, 10, 64)
		for _, u := range users {
			if u.PK == pk {
				return mockResponse(200, u), nil
			}
		}
		return mockResponse(404, nil), nil
	}
	a.f.t.Errorf("unexpected autocomplete request %s", req.URL.Path)
	return mockResponse(404, nil), nil
}
func autocompleteData(name, query string) discordgo.ApplicationCommandInteractionData {
	d := recoveryCommandData(name, query)
	d.Options[1].Focused = true
	return d
}
func (a *autocompleteFixture) warm() {
	a.f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.f.r.refreshAutocomplete(ctx); err != nil {
		a.f.t.Fatal("snapshot refresh failed", err)
	}
}
func (a *autocompleteFixture) choices(query string) []*discordgo.ApplicationCommandOptionChoice {
	a.f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return a.f.r.autocompleteChoices(ctx, a.f.ctx, emailAction, query)
}
func TestRecoveryAutocompleteCallerDenyBeforeHTTP(t *testing.T) {
	for _, which := range []string{"caller", "guild", "channel", "dm", "disabled", "audit"} {
		t.Run(which, func(t *testing.T) {
			a := newAutocompleteFixture(t, 2)
			c := a.f.ctx
			switch which {
			case "caller":
				c.Caller = "999"
			case "guild":
				c.Guild = "999"
			case "channel":
				c.Channel = "999"
			case "dm":
				c.Guild = ""
			case "disabled":
				a.f.r.cfg.EmailEnabled = false
			case "audit":
				a.f.r.audit = nil
			}
			got := a.f.r.autocompleteChoices(context.Background(), c, emailAction, "")
			if len(got) != 0 || a.calls.Load() != 0 {
				t.Fatal("unauthorized autocomplete queried Authentik")
			}
		})
	}
}
func TestRecoveryAutocompleteEligibilitySearchAndFutureUsers(t *testing.T) {
	a := newAutocompleteFixture(t, 11)
	a.users[2].Super = flag(true)
	a.users[3].Type = "service_account"
	a.users[4].Path = stringPtr("goauthentik.io/automatic")
	a.users[5].Username = "jacob"
	a.users[6].Active = flag(false)
	a.admin[a.users[7].PK] = true
	a.users[8].Super = nil
	a.users[9].Username = "AnonymousUser"
	a.users[10].Username = a.f.service.Username
	a.warm()
	got := a.choices("")
	if len(got) != 2 || got[0].Name != "human-000" || got[1].Name != "human-001" {
		t.Fatal("protected targets shown or external human omitted")
	}
	got = a.choices("HUMAN-001")
	if len(got) != 1 || got[0].Value != "ak-user:43" {
		t.Fatal("case-insensitive exact search failed")
	}
	if len(a.choices("not-present")) != 0 {
		t.Fatal("unmatched search returned names")
	}
	a.mu.Lock()
	u := ordinaryMock()
	u.PK = 1000
	u.Username = "future-account"
	u.Type = "external"
	a.users = append(a.users, u)
	a.mu.Unlock()
	a.warm()
	got = a.choices("future")
	if len(got) != 1 || got[0].Name != "future-account" {
		t.Fatal("new human not discoverable")
	}
	if a.writes.Load() != 0 || a.maxActive.Load() > 4 {
		t.Fatal("mutating or unbounded autocomplete")
	}
}
func TestRecoveryAutocompleteLimitsAndLongNames(t *testing.T) {
	a := newAutocompleteFixture(t, 270)
	a.warm()
	got := a.choices("")
	if len(got) != 25 {
		t.Fatalf("expected 25 choices, got %d", len(got))
	}
	got = a.choices("human-269")
	if len(got) != 1 || got[0].Name != "human-269" {
		t.Fatal("search limited to first page of suggestions")
	}
	a.mu.Lock()
	a.users[0].Username = strings.Repeat("界", 40)
	a.mu.Unlock()
	a.warm()
	got = a.choices("界")
	if len(got) != 1 || utf8.RuneCountInString(got[0].Name) > 100 || len(got[0].Value.(string)) > 100 {
		t.Fatal("Unicode username mishandled")
	}
	a.mu.Lock()
	a.users[0].Username = strings.Repeat("a", 140)
	a.mu.Unlock()
	a.warm()
	got = a.choices(strings.Repeat("a", 140))
	if len(got) != 1 || utf8.RuneCountInString(got[0].Name) > 100 || got[0].Value != "ak-user:42" {
		t.Fatal("long account cannot be selected safely")
	}
	id, p, err := a.f.r.preview(a.f.ctx, emailAction, got[0].Value.(string))
	if err != nil || id == "" || p.User.PK != 42 || p.User.Username != strings.Repeat("a", 140) {
		t.Fatal("long selection did not resolve to canonical account", err)
	}
}
func TestRecoveryAutocompleteFailuresDeadlinesAndBusy(t *testing.T) {
	for _, path := range []string{"/api/v3/core/users/me/", "/api/v3/core/users/", "/api/v3/rbac/roles/"} {
		t.Run(path, func(t *testing.T) {
			a := newAutocompleteFixture(t, 4)
			a.warm()
			a.fail = path
			if err := a.f.r.refreshAutocomplete(context.Background()); err == nil || len(a.choices("")) != 0 {
				t.Fatal("failed refresh retained suggestions")
			}
		})
	}
	a := newAutocompleteFixture(t, 4)
	a.delay = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := a.f.r.refreshAutocomplete(ctx); err == nil || time.Since(start) > 300*time.Millisecond || len(a.choices("")) != 0 {
		t.Fatal("refresh deadline not respected")
	}
	a = newAutocompleteFixture(t, 4)
	a.f.r.suggestRefreshMu.Lock()
	start = time.Now()
	if err := a.f.r.refreshAutocomplete(context.Background()); err != errSuggestionRefreshBusy || a.calls.Load() != 0 || time.Since(start) > 100*time.Millisecond {
		t.Fatal("busy refresh queued work")
	}
	a.f.r.suggestRefreshMu.Unlock()
	for _, q := range []string{"\x00", strings.Repeat("a", 151), "\xff"} {
		a = newAutocompleteFixture(t, 1)
		if len(a.choices(q)) != 0 || a.calls.Load() != 0 {
			t.Fatal("invalid query reached API")
		}
	}
	a = newAutocompleteFixture(t, 2)
	a.users[1].PK = a.users[0].PK
	if err := a.f.r.refreshAutocomplete(context.Background()); err == nil || len(a.choices("")) != 0 {
		t.Fatal("duplicate identity accepted")
	}
}
func TestRecoveryAutocompleteSelectionStillRevalidates(t *testing.T) {
	for _, which := range []string{"admin", "role promotion", "deleted", "changed", "unauthorized", "invalid ID"} {
		t.Run(which, func(t *testing.T) {
			a := newAutocompleteFixture(t, 1)
			a.warm()
			got := a.choices("")
			if len(got) != 1 {
				t.Fatal("no selection")
			}
			value := got[0].Value.(string)
			c := a.f.ctx
			switch which {
			case "admin":
				a.users[0].Super = flag(true)
			case "role promotion":
				a.admin[42] = true
			case "deleted":
				a.users = nil
			case "changed":
				a.freshChanged = true
			case "unauthorized":
				c.Caller = "999"
			case "invalid ID":
				value = "ak-user:042"
			}
			if len(a.choices("")) != 1 {
				t.Fatal("expected still-bounded cached label")
			}
			before := a.calls.Load()
			if _, _, err := a.f.r.preview(c, emailAction, value); err == nil || len(a.f.r.pending) != 0 {
				t.Fatal("stale/unauthorized selection allowed")
			}
			if (which == "unauthorized" || which == "invalid ID") && a.calls.Load() != before {
				t.Fatal("denied selection reached HTTP")
			}
			if a.writes.Load() != 0 {
				t.Fatal("selection caused mutation")
			}
		})
	}
}
func TestRecoveryAutocompletePartialParser(t *testing.T) {
	for _, name := range []string{passwordCommand, mfaCommand} {
		d := autocompleteData(name, "")
		action, query, ok := parseRecoveryAutocomplete(d)
		if !ok || query != "" || action == "" {
			t.Fatal("valid focus rejected")
		}
		d.Options = d.Options[1:]
		if _, _, ok = parseRecoveryAutocomplete(d); !ok {
			t.Fatal("partial action rejected")
		}
	}
	for _, which := range []string{"authctl wrapper", "old email", "old MFA", "nested reset", "action focused", "user unfocused", "duplicate", "unknown field", "wrong action", "wrong type", "nested", "nil"} {
		t.Run(which, func(t *testing.T) {
			d := autocompleteData(passwordCommand, "")
			options := d.Options
			switch which {
			case "authctl wrapper":
				d.Name = authctlCommand
				d.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: passwordCommand, Type: discordgo.ApplicationCommandOptionSubCommand, Options: options}}
			case "old email":
				d.Name = emailAction
			case "old MFA":
				d.Name = mfaAction
			case "nested reset":
				d.Options = []*discordgo.ApplicationCommandInteractionDataOption{{Name: "reset", Type: discordgo.ApplicationCommandOptionSubCommand, Options: options}}
			case "action focused":
				options[0].Focused = true
			case "user unfocused":
				options[1].Focused = false
			case "duplicate":
				d.Options = append(options, options[1])
			case "unknown field":
				options[0].Name = "unknown"
			case "wrong action":
				options[0].Value = "remove"
			case "wrong type":
				options[1].Type = discordgo.ApplicationCommandOptionUser
			case "nested":
				options[1].Options = []*discordgo.ApplicationCommandInteractionDataOption{options[0]}
			case "nil":
				options[0] = nil
			}
			if _, _, ok := parseRecoveryAutocomplete(d); ok {
				t.Fatal("malformed autocomplete accepted")
			}
		})
	}
}
func TestRecoveryAutocompleteDispatcherAndRedaction(t *testing.T) {
	old := recovery
	defer func() { recovery = old }()
	for _, allowed := range []bool{true, false} {
		a := newAutocompleteFixture(t, 2)
		a.warm()
		before := a.calls.Load()
		recovery = a.f.r
		caller := "100"
		if !allowed {
			caller = "999"
		}
		var response struct {
			Type int `json:"type"`
			Data struct {
				Choices []*discordgo.ApplicationCommandOptionChoice `json:"choices"`
			} `json:"data"`
		}
		s, _ := discordgo.New("Bot fake-autocomplete-token")
		s.Client = &http.Client{Transport: mockRoundTripper(func(req *http.Request) (*http.Response, error) {
			b, _ := io.ReadAll(req.Body)
			if json.Unmarshal(b, &response) != nil {
				t.Fatal("bad response")
			}
			return mockResponse(200, nil), nil
		})}
		i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "mock-id", AppID: "mock-app", Token: "mock-token", Type: discordgo.InteractionApplicationCommandAutocomplete, ChannelID: "200", GuildID: "300", Member: &discordgo.Member{User: &discordgo.User{ID: caller}}, Data: autocompleteData(passwordCommand, "")}}
		interactionCreate(s, i)
		if response.Type != 8 {
			t.Fatal("autocomplete not answered with type8")
		}
		if allowed && len(response.Data.Choices) != 2 {
			t.Fatal("authorized suggestions absent")
		}
		if !allowed && (len(response.Data.Choices) != 0 || a.calls.Load() != before) {
			t.Fatal("unauthorized autocomplete leaked")
		}
	}
	a := newAutocompleteFixture(t, 1)
	recovery = a.f.r
	writer := log.Writer()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(writer)
	s, _ := discordgo.New("Bot fake-autocomplete-token")
	s.Client = &http.Client{Transport: mockRoundTripper(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("MOCK_PRIVATE_DISCORD_TOKEN") })}
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "mock", Token: "mock", Type: discordgo.InteractionApplicationCommandAutocomplete, Data: autocompleteData(passwordCommand, "private-query")}}
	interactionCreate(s, i)
	if strings.Contains(logs.String(), "MOCK_PRIVATE") || strings.Contains(logs.String(), "private-query") {
		t.Fatal("sensitive Discord error logged")
	}
}

func TestRecoveryAutocompleteCacheExpiryAndResponseLatency(t *testing.T) {
	a := newAutocompleteFixture(t, 2)
	if len(a.choices("")) != 0 || a.calls.Load() != 0 {
		t.Fatal("cold cache queried API")
	}
	a.warm()
	before := a.calls.Load()
	a.delay = time.Second
	start := time.Now()
	if len(a.choices("")) != 2 || time.Since(start) > 50*time.Millisecond || a.calls.Load() != before {
		t.Fatal("cached response blocked on HTTP")
	}
	a.f.now = a.f.now.Add(recoverySuggestionMaxAge - time.Nanosecond)
	if len(a.choices("")) != 2 {
		t.Fatal("unexpired snapshot absent")
	}
	a.f.now = a.f.now.Add(time.Nanosecond)
	if len(a.choices("")) != 0 || a.calls.Load() != before {
		t.Fatal("expired snapshot served or request triggered refresh")
	}
	a.f.now = a.f.now.Add(-2 * recoverySuggestionMaxAge)
	if len(a.choices("")) != 0 {
		t.Fatal("clock rollback extended snapshot")
	}
}
func TestRecoveryAutocompleteRefreshRemovesPromotedAndDeleted(t *testing.T) {
	a := newAutocompleteFixture(t, 3)
	a.warm()
	a.admin[42] = true
	a.users = a.users[:2]
	if len(a.choices("")) != 3 {
		t.Fatal("snapshot unexpectedly mutated")
	}
	a.warm()
	got := a.choices("")
	if len(got) != 1 || got[0].Value != "ak-user:43" {
		t.Fatal("refresh retained promoted/deleted account")
	}
	a.f.now = a.f.now.Add(recoverySuggestionMaxAge)
	a.fail = "/api/v3/core/users/me/"
	if err := a.f.r.refreshAutocomplete(context.Background()); err == nil || len(a.choices("")) != 0 {
		t.Fatal("failed refresh extended expired cache")
	}
}
func TestRecoveryAutocompleteCachedDataStillRequiresCaller(t *testing.T) {
	a := newAutocompleteFixture(t, 2)
	a.warm()
	before := a.calls.Load()
	c := a.f.ctx
	c.Caller = "999"
	if len(a.f.r.autocompleteChoices(context.Background(), c, emailAction, "")) != 0 || a.calls.Load() != before {
		t.Fatal("unauthorized caller read cached targets")
	}
}
func TestRecoveryAutocompleteBackgroundRefreshAndConcurrency(t *testing.T) {
	a := newAutocompleteFixture(t, 10)
	a.f.r.now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.f.r.startAutocompleteCache(ctx)
	a.f.r.startAutocompleteCache(ctx)
	until := time.Now().Add(2 * time.Second)
	for len(a.choices("")) == 0 && time.Now().Before(until) {
		time.Sleep(time.Millisecond)
	}
	if len(a.choices("")) != 10 {
		t.Fatal("background cache did not warm")
	}
	before := a.calls.Load()
	var wg sync.WaitGroup
	for n := 0; n < 100; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if len(a.choices("human")) != 10 {
				t.Error("concurrent cache read lost data")
			}
		}()
	}
	wg.Wait()
	if a.calls.Load() != before || a.maxActive.Load() > 4 {
		t.Fatal("request-triggered refresh storm or unbounded worker pool")
	}
	cancel()
	for n := 0; n < 4; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = a.f.r.refreshAutocomplete(context.Background()) }()
	}
	wg.Wait()
}

func TestRecoveryAutocompleteCachedSelectionRecheckedAtConfirmation(t *testing.T) {
	for _, which := range []string{"role promotion", "deleted"} {
		t.Run(which, func(t *testing.T) {
			a := newAutocompleteFixture(t, 1)
			a.warm()
			value := a.choices("")[0].Value.(string)
			id, _, err := a.f.r.preview(a.f.ctx, emailAction, value)
			if err != nil {
				t.Fatal(err)
			}
			if which == "role promotion" {
				a.admin[42] = true
			} else {
				a.users = nil
			}
			if _, err = a.f.r.confirm(a.f.ctx, id, false); err == nil || a.writes.Load() != 0 {
				t.Fatal("cached selection bypassed fresh confirmation check")
			}
		})
	}
}
func TestRecoveryAutocompleteMaximumAgeStartsBeforeScan(t *testing.T) {
	a := newAutocompleteFixture(t, 1)
	start := a.f.now
	var calls atomic.Int64
	var elapsed atomic.Int64
	elapsed.Store(int64(30 * time.Second))
	a.f.r.now = func() time.Time {
		if calls.Add(1) == 1 {
			return start
		}
		return start.Add(time.Duration(elapsed.Load()))
	}
	a.warm()
	if len(a.choices("")) != 1 {
		t.Fatal("completed snapshot absent")
	}
	elapsed.Store(int64(recoverySuggestionMaxAge))
	if len(a.choices("")) != 0 {
		t.Fatal("refresh completion improperly renewed maximum age")
	}
}
func TestRecoveryAutocompleteAccessDenialStopsBackgroundRefresh(t *testing.T) {
	a := newAutocompleteFixture(t, 1)
	a.f.r.now = time.Now
	a.f.r.api.client.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		a.calls.Add(1)
		return mockResponse(403, map[string]string{"private": "MOCK_PRIVATE_DENIAL"}), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), recoverySuggestionRefreshPause+time.Second)
	defer cancel()
	a.f.r.startAutocompleteCache(ctx)
	time.Sleep(recoverySuggestionRefreshPause + 100*time.Millisecond)
	if a.calls.Load() != 1 || len(a.choices("")) != 0 {
		t.Fatal("denied background read was retried or cached data served")
	}
}
