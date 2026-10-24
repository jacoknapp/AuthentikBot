package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// All HTTP, including Discord, uses in-memory transports. No environment
// credentials, sockets, DNS, or live Authentik/Discord services are used.
type mockRoundTripper func(*http.Request) (*http.Response, error)

func (f mockRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func mockResponse(code int, data any) *http.Response {
	b, _ := json.Marshal(data)
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(b))}
}
func flag(b bool) *bool { return &b }
func ordinaryMock() recoveryUser {
	return recoveryUser{PK: 42, Username: "alice", Email: stringPtr("alice@example.invalid"), Path: stringPtr("users"), Active: flag(true), Super: flag(false), Type: "internal", UUID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", Updated: "2026-10-22T00:00:00Z"}
}
func deviceMock(id, kind string) recoveryDevice {
	model := ""
	for k, v := range recoveryDeviceModels {
		if v == kind {
			model = k
		}
	}
	return recoveryDevice{PK: id, Name: "device", Type: model, Kind: kind, Confirmed: flag(true), Created: "2026-01-01T00:00:00Z", Updated: "2026-01-01T00:00:00Z"}
}

type memoryAudit struct {
	mu     sync.Mutex
	events []recoveryAuditEvent
	fail   bool
}

func (a *memoryAudit) Write(e recoveryAuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return errors.New("Recovery audit unavailable")
	}
	a.events = append(a.events, e)
	return nil
}
func (*memoryAudit) Close() error { return nil }
func (a *memoryAudit) statuses() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var s []string
	for _, e := range a.events {
		s = append(s, e.Status)
	}
	return s
}

type recoveryFixture struct {
	t                       *testing.T
	r                       *recoveryService
	audit                   *memoryAudit
	ctx                     recoveryContext
	user                    recoveryUser
	service                 recoveryUser
	devices                 []recoveryDevice
	writes                  []string
	requests                []string
	hook                    func(*http.Request) *http.Response
	now                     time.Time
	roles                   []recoveryRole
	globals                 map[string][]recoveryPermission
	objects                 map[string][]recoveryPermission
	otherUsers              []recoveryUser
	nativeGets, nativePosts int
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	f := &recoveryFixture{t: t, ctx: recoveryContext{Caller: "100", Channel: "200", Guild: "300"}, user: ordinaryMock(), service: recoveryUser{PK: 7, Username: "recovery-svc", Type: "service_account", Active: flag(true), Super: flag(false)}, devices: []recoveryDevice{deviceMock("1", "totp"), deviceMock("2", "totp")}, audit: &memoryAudit{}, now: time.Unix(10000, 0)}
	f.roles = []recoveryRole{{PK: mockRoleID}}
	f.globals = map[string][]recoveryPermission{mockRoleID: {ordinaryPermissionMock()}}
	f.objects = map[string][]recoveryPermission{}
	api, err := newRecoveryAPI("https://auth.example.invalid", "mock-recovery-credential")
	if err != nil {
		t.Fatal(err)
	}
	api.client.Transport = mockRoundTripper(f.roundTrip)
	cfg := recoveryConfig{EmailEnabled: true, MFAEnabled: true, Token: "mock-recovery-credential", ServiceUsername: "recovery-svc", Guild: "300", Channel: "200", Flow: "approved-recovery", Callers: map[string]bool{"100": true, "101": true}, Immutable: map[string]bool{"jacob": true}, Factors: []string{"totp"}}
	f.r = newRecoveryService(cfg, api, f.audit)
	f.r.now = func() time.Time { return f.now }
	return f
}
func pageMock[T any](items []T, page, next, total, count int) map[string]any {
	return map[string]any{"results": items, "pagination": map[string]int{"current": page, "next": next, "total_pages": total, "count": count}}
}
func (f *recoveryFixture) roundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "auth.example.invalid" {
		f.t.Error("unexpected origin")
	}
	if req.Header.Get("User-Agent") != "Knapp-AuthentikBot-Recovery/1.0" {
		f.t.Error("recovery User-Agent missing")
	}
	if !strings.Contains(req.URL.Path, "/flows/executor/") && req.Header.Get("Authorization") != "Bearer mock-recovery-credential" {
		f.t.Error("recovery credential was not isolated")
	}
	f.requests = append(f.requests, req.Method+" "+req.URL.Path)
	if req.Method != "GET" {
		f.writes = append(f.writes, req.Method+" "+req.URL.Path)
	}
	if f.hook != nil {
		if resp := f.hook(req); resp != nil {
			return resp, nil
		}
	}
	switch req.URL.Path {
	case "/api/v3/core/users/me/":
		return mockResponse(200, map[string]any{"user": f.service}), nil
	case "/api/v3/core/users/":
		items := []recoveryUser{f.user}
		if name := req.URL.Query().Get("username"); name != "" {
			if name != f.user.Username {
				f.t.Error("exact username filter missing")
			}
		} else {
			items = append(items, f.otherUsers...)
		}
		return mockResponse(200, pageMock(items, 1, 0, 1, len(items))), nil
	case "/api/v3/rbac/roles/":
		if req.URL.Query().Get("users") != fmt.Sprint(f.user.PK) || req.URL.Query().Get("inherited") != "true" {
			f.t.Error("effective inherited role filter missing")
		}
		return mockResponse(200, pageMock(f.roles, 1, 0, 1, len(f.roles))), nil
	case "/api/v3/rbac/permissions/":
		items := f.globals[req.URL.Query().Get("role")]
		if items == nil {
			items = []recoveryPermission{}
		}
		return mockResponse(200, pageMock(items, 1, 0, 1, len(items))), nil
	case "/api/v3/rbac/permissions/assigned_by_roles/":
		results := []recoveryRoleAssignments{}
		for _, role := range f.roles {
			global := f.globals[role.PK]
			objects := f.objects[role.PK]
			if global == nil {
				global = []recoveryPermission{}
			}
			if objects == nil {
				objects = []recoveryPermission{}
			}
			matches := false
			for _, p := range global {
				if p.App+"."+p.Model == req.URL.Query().Get("model") {
					matches = true
				}
			}
			for _, p := range objects {
				if p.App+"."+p.Model == req.URL.Query().Get("model") && p.Object != nil && *p.Object == req.URL.Query().Get("object_pk") {
					matches = true
				}
			}
			if matches {
				results = append(results, recoveryRoleAssignments{Role: role.PK, Global: global, Objects: objects})
			}
		}
		return mockResponse(200, pageMock(results, 1, 0, 1, len(results))), nil
	case "/api/v3/rbac/permissions/roles/":
		if req.URL.Query().Get("page_size") != "10" {
			f.t.Error("object grant pagination must be small")
		}
		items := f.objects[req.URL.Query().Get("uuid")]
		if items == nil {
			items = []recoveryPermission{}
		}
		return mockResponse(200, pageMock(items, 1, 0, 1, len(items))), nil
	case "/api/v3/flows/executor/approved-recovery/":
		if req.Header.Get("Authorization") != "" || req.URL.RawQuery != "query=" || req.Header.Get("Accept") != "application/json" {
			f.t.Error("unsafe native headers/query")
		}
		if req.Method == "GET" {
			f.nativeGets++
			return mockResponse(200, identificationMock()), nil
		}
		if req.Method != "POST" {
			f.t.Error("unexpected native method")
		}
		f.nativePosts++
		var body map[string]string
		if json.NewDecoder(req.Body).Decode(&body) != nil || !reflect.DeepEqual(body, map[string]string{"component": "ak-stage-identification", "uid_field": f.user.Username}) {
			f.t.Error("unsafe native identification body")
		}
		return mockResponse(200, map[string]string{"component": "ak-stage-email"}), nil
	case "/api/v3/authenticators/admin/all/":
		if req.URL.Query().Get("user") != fmt.Sprint(f.user.PK) {
			f.t.Error("exact target device filter missing")
		}
		return mockResponse(200, f.devices), nil

	}
	if req.Method == "DELETE" {
		for i, d := range f.devices {
			if req.URL.Path == factorPath(d.Kind)+d.PK+"/" {
				f.devices = append(f.devices[:i], f.devices[i+1:]...)
				return mockResponse(204, nil), nil
			}
		}
	}
	f.t.Errorf("unexpected mock request %s %s", req.Method, req.URL.Path)
	return mockResponse(404, nil), nil
}
func (f *recoveryFixture) preview(action string) string {
	f.t.Helper()
	id, _, err := f.r.preview(f.ctx, action, f.user.Username)
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func TestRecoveryConfigFailClosed(t *testing.T) {
	base := map[string]string{"RECOVERY_EMAIL_ENABLED": "true", "RECOVERY_MFA_ENABLED": "true", "RECOVERY_AUTHENTIK_TOKEN": "mock-separate", "RECOVERY_SERVICE_USERNAME": "recovery-svc", "RECOVERY_GUILD_ID": "300", "RECOVERY_DISCORD_USER_IDS": "100", "RECOVERY_FLOW_SLUG": "approved-recovery", "RECOVERY_AUDIT_PATH": "/tmp/proposed-audit"}
	legacy := Config{AuthentikToken: "mock-legacy", CommandChannelID: "200", ImmutableUsernames: []string{"jacob"}}
	cases := []struct {
		name    string
		mutate  func(map[string]string, *Config)
		wantErr bool
	}{
		{"valid", func(map[string]string, *Config) {}, false},
		{"disabled defaults", func(m map[string]string, c *Config) {
			for k := range m {
				delete(m, k)
			}
		}, false},
		{"token missing", func(m map[string]string, c *Config) { delete(m, "RECOVERY_AUTHENTIK_TOKEN") }, true},
		{"legacy fallback forbidden", func(m map[string]string, c *Config) { m["RECOVERY_AUTHENTIK_TOKEN"] = c.AuthentikToken }, true},
		{"no caller", func(m map[string]string, c *Config) { delete(m, "RECOVERY_DISCORD_USER_IDS") }, true},
		{"no channel", func(m map[string]string, c *Config) { c.CommandChannelID = "" }, true},
		{"no guild", func(m map[string]string, c *Config) { delete(m, "RECOVERY_GUILD_ID") }, true},
		{"no identity", func(m map[string]string, c *Config) { delete(m, "RECOVERY_SERVICE_USERNAME") }, true},
		{"no flow", func(m map[string]string, c *Config) { delete(m, "RECOVERY_FLOW_SLUG") }, true},
		{"no audit", func(m map[string]string, c *Config) { delete(m, "RECOVERY_AUDIT_PATH") }, true},
		{"relative audit", func(m map[string]string, c *Config) { m["RECOVERY_AUDIT_PATH"] = "audit.jsonl" }, true},
		{"bad switch", func(m map[string]string, c *Config) { m["RECOVERY_MFA_ENABLED"] = "yes" }, true},
		{"non-TOTP factor", func(m map[string]string, c *Config) { m["RECOVERY_MFA_FACTOR_TYPES"] = "webauthn" }, true},
		{"bad slug", func(m map[string]string, c *Config) { m["RECOVERY_FLOW_SLUG"] = "../other?next=x" }, true},
		{"unknown factor", func(m map[string]string, c *Config) { m["RECOVERY_MFA_FACTOR_TYPES"] = "totp,any" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range base {
				m[k] = v
			}
			c := legacy
			tc.mutate(m, &c)
			got, err := readRecoveryConfig(c, func(k string) string { return m[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("error=%v", err)
			}
			if err == nil && !reflect.DeepEqual(got.Factors, []string{"totp"}) {
				t.Error("default scope must be TOTP only")
			}
		})
	}
}
func TestRecoveryCallerDenyBeforeHTTP(t *testing.T) {
	for _, which := range []string{"disabled", "caller", "guild", "channel", "dm", "missing channel", "missing guild", "missing allowlist"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			c := f.ctx
			switch which {
			case "disabled":
				f.r.cfg.EmailEnabled = false
			case "caller":
				c.Caller = "999"
			case "guild":
				c.Guild = "999"
			case "channel":
				c.Channel = "999"
			case "dm":
				c.Guild = ""
			case "missing channel":
				f.r.cfg.Channel = ""
			case "missing guild":
				f.r.cfg.Guild = ""
			case "missing allowlist":
				f.r.cfg.Callers = nil
			}
			if _, _, err := f.r.preview(c, emailAction, "alice"); err == nil {
				t.Fatal("allowed")
			}
			if len(f.requests) != 0 {
				t.Fatal("unauthorized HTTP")
			}
			if !reflect.DeepEqual(f.audit.statuses(), []string{"attempt", "denied"}) {
				t.Fatal("missing audit")
			}
		})
	}
}
func TestRecoveryTargetsAndIdentity(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*recoveryFixture)
	}{
		{"inactive", func(f *recoveryFixture) { f.user.Active = flag(false) }},
		{"missing active", func(f *recoveryFixture) { f.user.Active = nil }},
		{"superuser", func(f *recoveryFixture) { f.user.Super = flag(true) }},
		{"missing superuser", func(f *recoveryFixture) { f.user.Super = nil }},
		{"service target", func(f *recoveryFixture) { f.user.Type = "service_account" }},
		{"internal service target", func(f *recoveryFixture) { f.user.Type = "internal_service_account" }},
		{"immutable", func(f *recoveryFixture) { f.r.cfg.Immutable["alice"] = true }},
		{"self pk", func(f *recoveryFixture) { f.user.PK = 7 }},
		{"missing UUID", func(f *recoveryFixture) { f.user.UUID = "" }},
		{"super service", func(f *recoveryFixture) { f.service.Super = flag(true) }},
		{"inactive service", func(f *recoveryFixture) { f.service.Active = flag(false) }},
		{"wrong identity", func(f *recoveryFixture) { f.service.Username = "other" }},
		{"internal identity", func(f *recoveryFixture) { f.service.Type = "internal" }},
		{"missing identity super", func(f *recoveryFixture) { f.service.Super = nil }},
		{"system path", func(f *recoveryFixture) { f.user.Path = stringPtr("goauthentik.io/system") }},
		{"missing path", func(f *recoveryFixture) { f.user.Path = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			tc.mutate(f)
			if _, _, err := f.r.preview(f.ctx, emailAction, "alice"); err == nil {
				t.Fatal("protected target/identity accepted")
			}
			if len(f.writes) != 0 {
				t.Fatal("unexpected write")
			}
		})
	}
}
func TestRecoveryConfirmationBindingsAndExpiry(t *testing.T) {
	for _, which := range []string{"caller", "guild", "channel", "dm", "disabled", "removed caller", "removed channel config", "removed guild config", "expired", "cancel"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			id := f.preview(emailAction)
			c := f.ctx
			cancel := false
			switch which {
			case "caller":
				c.Caller = "101"
			case "guild":
				c.Guild = "999"
			case "channel":
				c.Channel = "999"
			case "dm":
				c.Guild = ""
			case "disabled":
				f.r.cfg.EmailEnabled = false
			case "removed caller":
				f.r.cfg.Callers = nil
			case "removed channel config":
				f.r.cfg.Channel = ""
			case "removed guild config":
				f.r.cfg.Guild = ""
			case "expired":
				f.now = f.now.Add(recoveryTTL)
			case "cancel":
				cancel = true
			}
			_, err := f.r.confirm(c, id, cancel)
			if err == nil && !cancel {
				t.Fatal("confirmation accepted")
			}
			if len(f.writes) != 0 {
				t.Fatal("unexpected write")
			}
			if cancel || which == "expired" {
				if _, err = f.r.confirm(f.ctx, id, false); err == nil {
					t.Fatal("replay accepted")
				}
			}
		})
	}
}
func TestRecoveryEmailAndReplay(t *testing.T) {
	f := newRecoveryFixture(t)
	id := f.preview(emailAction)
	if len(f.writes) != 0 || f.nativeGets != 0 {
		t.Fatal("preview mutated or initiated native flow")
	}
	message, err := f.r.confirm(f.ctx, id, false)
	if err != nil || !strings.Contains(message, "submitted") {
		t.Fatalf("%s %v", message, err)
	}
	if !reflect.DeepEqual(f.writes, []string{"POST /api/v3/flows/executor/approved-recovery/"}) {
		t.Fatal(f.writes)
	}
	if _, err = f.r.confirm(f.ctx, id, false); err == nil || len(f.writes) != 1 {
		t.Fatal("replay")
	}
}
func TestRecoveryRevalidation(t *testing.T) {
	for _, which := range []string{"target super", "target changed", "email changed", "service changed", "service super", "target now service"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			id := f.preview(emailAction)
			switch which {
			case "target super":
				f.user.Super = flag(true)
			case "target changed":
				f.user.UUID = "different"
			case "email changed":
				f.user.Email = stringPtr("other@example.invalid")
			case "service changed":
				f.service.PK = 8
			case "service super":
				f.service.Super = flag(true)
			case "target now service":
				f.user.Type = "service_account"
			}
			if _, err := f.r.confirm(f.ctx, id, false); err == nil {
				t.Fatal("change accepted")
			}
			if len(f.writes) != 0 {
				t.Fatal("mutated")
			}
		})
	}
}
func TestRecoveryPaginationAndExactMatching(t *testing.T) {
	for _, which := range []string{"exact later page", "duplicate", "only similar", "bad next", "url next", "missing pagination", "count mismatch"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			f.hook = func(r *http.Request) *http.Response {
				if r.URL.Path != "/api/v3/core/users/" {
					return nil
				}
				p := r.URL.Query().Get("page")
				similar := f.user
				similar.Username = "alice-other"
				if which == "missing pagination" {
					return mockResponse(200, map[string]any{"results": []recoveryUser{f.user}})
				}
				if which == "url next" {
					return mockResponse(200, map[string]any{"results": []recoveryUser{f.user}, "pagination": map[string]any{"next": "https://evil.invalid", "current": 1, "total_pages": 2, "count": 2}})
				}
				if which == "bad next" {
					return mockResponse(200, pageMock([]recoveryUser{f.user}, 1, 1, 2, 2))
				}
				if which == "count mismatch" {
					return mockResponse(200, pageMock([]recoveryUser{f.user}, 1, 0, 1, 2))
				}
				if which == "only similar" {
					return mockResponse(200, pageMock([]recoveryUser{similar}, 1, 0, 1, 1))
				}
				if p == "1" {
					if which == "duplicate" {
						similar = f.user
					}
					return mockResponse(200, pageMock([]recoveryUser{similar}, 1, 2, 2, 2))
				}
				return mockResponse(200, pageMock([]recoveryUser{f.user}, 2, 0, 2, 2))
			}
			_, err := f.r.api.exactUser("alice")
			if (err == nil) != (which == "exact later page") {
				t.Fatal(err)
			}
		})
	}
}
func TestRecoveryTransportAndRedaction(t *testing.T) {
	for _, base := range []string{"http://auth.invalid", "https://user:pass@auth.invalid", "https://auth.invalid/path", "https://auth.invalid/?x=y", "https://auth.invalid/#x", "//auth.invalid"} {
		if _, err := newRecoveryAPI(base, "mock"); err == nil {
			t.Fatal("unsafe origin accepted")
		}
	}
	for _, code := range []int{301, 302, 303, 307, 308, 400, 401, 403, 404, 429, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newRecoveryFixture(t)
			count := 0
			f.r.api.client.Transport = mockRoundTripper(func(r *http.Request) (*http.Response, error) {
				count++
				resp := mockResponse(code, map[string]string{"error": "MOCK_SENSITIVE_BODY"})
				resp.Header.Set("Location", "https://other.invalid/sensitive")
				resp.Request = r
				return resp, nil
			})
			_, _, err := f.r.preview(f.ctx, emailAction, "alice")
			if err == nil || strings.Contains(err.Error(), "MOCK_SENSITIVE") || count != 1 {
				t.Fatal("unredacted or redirected")
			}
		})
	}
	f := newRecoveryFixture(t)
	f.r.api.client.Transport = mockRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("MOCK_SENSITIVE_TRANSPORT") })
	_, _, err := f.r.preview(f.ctx, emailAction, "alice")
	if err == nil || strings.Contains(err.Error(), "MOCK_SENSITIVE") {
		t.Fatal("transport error leak")
	}
}
func TestRecoveryMFASnapshotAndPartialFailure(t *testing.T) {
	for _, which := range []string{"success", "new", "removed", "renamed", "confirmed changed", "updated", "owner target changed", "unscoped new", "unknown", "partial", "empty"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			if which == "empty" {
				f.devices = []recoveryDevice{}
				if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err == nil {
					t.Fatal("empty accepted")
				}
				return
			}
			id := f.preview(mfaAction)
			switch which {
			case "new":
				f.devices = append(f.devices, deviceMock("3", "totp"))
			case "removed":
				f.devices = f.devices[1:]
			case "renamed":
				f.devices[0].Name = "changed"
			case "confirmed changed":
				f.devices[0].Confirmed = flag(false)
			case "updated":
				f.devices[0].Updated = "changed"
			case "owner target changed":
				f.user.UUID = "changed"
			case "unscoped new":
				f.devices = append(f.devices, deviceMock("3", "email"))
			case "unknown":
				f.devices[0].Type = "unknown"
			case "partial":
				f.hook = func(r *http.Request) *http.Response {
					if r.Method == "DELETE" && strings.HasSuffix(r.URL.Path, "/2/") {
						return mockResponse(403, map[string]string{"secret": "MOCK_SENSITIVE"})
					}
					return nil
				}
			}
			msg, err := f.r.confirm(f.ctx, id, false)
			switch which {
			case "success":
				if err != nil || len(f.writes) != 2 || !strings.Contains(msg, "Deleted 2") {
					t.Fatalf("%s %v %v", msg, err, f.writes)
				}
			case "partial":
				if err == nil || len(f.writes) != 2 || !strings.Contains(msg, "1 confirmed") || !strings.Contains(strings.Join(f.audit.statuses(), ","), "partial") {
					t.Fatalf("%s %v", msg, err)
				}
			default:
				if err == nil || len(f.writes) != 0 {
					t.Fatalf("stale snapshot mutated: %v", err)
				}
			}
			if _, err = f.r.confirm(f.ctx, id, false); err == nil {
				t.Fatal("replay")
			}
		})
	}
}
func TestRecoveryMFAScopesAndMetadataOnly(t *testing.T) {
	for kind := range supportedFactors {
		t.Run(kind, func(t *testing.T) {
			f := newRecoveryFixture(t)
			f.r.cfg.Factors = []string{kind}
			f.devices = []recoveryDevice{deviceMock("3", kind), deviceMock("4", "email")}
			if kind == "email" {
				f.devices = f.devices[:1]
			}
			id := f.preview(mfaAction)
			if _, err := f.r.confirm(f.ctx, id, false); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(f.writes, []string{"DELETE " + factorPath(kind) + "3/"}) {
				t.Fatal("wrong deletion scope")
			}
			for _, req := range f.requests {
				if strings.HasPrefix(req, "GET /api/v3/authenticators/admin/") && req != "GET /api/v3/authenticators/admin/all/" {
					t.Fatal("enumerated factor endpoint that may expose secrets")
				}
			}
		})
	}
}
func TestRecoveryConcurrencyCooldownAndCapacity(t *testing.T) {
	f := newRecoveryFixture(t)
	id := f.preview(emailAction)
	var winners atomic.Int32
	var wg sync.WaitGroup
	// Test atomic claim separately: real API mocks are deliberately stateful and
	// not shared by competing winners. There must be exactly one winner.
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := f.r.claim(f.ctx, id, false); err == nil {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatal("multiple confirmation winners")
	}
	if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err == nil {
		t.Fatal("concurrent target accepted")
	}
	f.r.release("alice")
	f.now = f.now.Add(6 * time.Second)
	if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err == nil {
		t.Fatal("cooldown bypass")
	}
	f.now = f.now.Add(recoveryCooldown)
	f.preview(mfaAction)
	for j := 0; j < recoveryCapacity; j++ {
		f.r.pending[fmt.Sprint(j)] = recoveryPending{Expires: f.now.Add(time.Minute)}
	}
	f.now = f.now.Add(6 * time.Second)
	if err := f.r.reserve(f.ctx, "bob"); err == nil {
		t.Fatal("capacity bypass")
	}
}
func TestRecoveryAuditFailure(t *testing.T) {
	f := newRecoveryFixture(t)
	f.audit.fail = true
	if _, _, err := f.r.preview(f.ctx, emailAction, "alice"); err == nil || len(f.requests) != 0 {
		t.Fatal("audit failed open")
	}
	f = newRecoveryFixture(t)
	id := f.preview(mfaAction)
	f.audit.fail = true
	if _, err := f.r.confirm(f.ctx, id, false); err == nil || len(f.writes) != 0 {
		t.Fatal("confirm audit failed open")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "audit.jsonl")
	a, err := openRecoveryAudit(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Write(recoveryAuditEvent{Action: mfaAction, Status: "partial", Caller: "100", Target: 42, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Deleted: 1}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), `"target_id":42`) {
		t.Fatal("missing structured audit")
	}
	if err = os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if a, err = openRecoveryAudit(path); err == nil {
		a.Close()
		t.Fatal("public audit accepted")
	}
	if err = os.Symlink(path, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if a, err = openRecoveryAudit(filepath.Join(dir, "link")); err == nil {
		a.Close()
		t.Fatal("symlink audit accepted")
	}
}
func TestRecoveryDiscordUI(t *testing.T) {
	old := recovery
	defer func() { recovery = old }()
	f := newRecoveryFixture(t)
	recovery = f.r
	session, _ := discordgo.New("Bot mock-discord")
	var responses []map[string]any
	session.Client = &http.Client{Transport: mockRoundTripper(func(r *http.Request) (*http.Response, error) {
		var b map[string]any
		if json.NewDecoder(r.Body).Decode(&b) != nil {
			t.Error("bad Discord body")
		}
		responses = append(responses, b)
		if r.Method == "PATCH" {
			return mockResponse(200, map[string]any{"id": "message"}), nil
		}
		return mockResponse(204, nil), nil
	})}
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "id", AppID: "app", Token: "mock-interaction", Type: discordgo.InteractionApplicationCommand, ChannelID: "200", GuildID: "300", Member: &discordgo.Member{User: &discordgo.User{ID: "100"}}, Data: recoveryCommandData(mfaCommand, "alice")}}
	interactionCreate(session, i)
	if len(responses) != 2 {
		t.Fatal("missing acknowledgement/preview")
	}
	data := responses[0]["data"].(map[string]any)
	if data["flags"] != float64(discordgo.MessageFlagsEphemeral) {
		t.Fatal("not ephemeral")
	}
	if !strings.Contains(responses[1]["content"].(string), "totp=2") {
		t.Fatal("missing exact scope/count")
	}
	if len(responses[1]["allowed_mentions"].(map[string]any)["parse"].([]any)) != 0 {
		t.Fatal("mentions enabled")
	}
	row := responses[1]["components"].([]any)[0].(map[string]any)
	buttons := row["components"].([]any)
	if len(buttons) != 2 {
		t.Fatal("missing confirm/cancel")
	}
	custom := buttons[0].(map[string]any)["custom_id"].(string)
	if !strings.HasPrefix(custom, "recovery:confirm:") || strings.Contains(custom, "alice") || len(strings.TrimPrefix(custom, "recovery:confirm:")) != 32 {
		t.Fatal("unsafe confirmation ID")
	}
	i.Type = discordgo.InteractionMessageComponent
	i.Data = discordgo.MessageComponentInteractionData{CustomID: custom}
	i.Member.User.ID = "101"
	interactionCreate(session, i)
	if len(f.writes) != 0 {
		t.Fatal("wrong caller confirmed")
	}
	i.Member.User.ID = "100"
	interactionCreate(session, i)
	if len(f.writes) != 2 {
		t.Fatal("valid confirmation failed")
	}
	recovery = nil
	if len(recoveryCommands()) != 0 {
		t.Fatal("disabled commands registered")
	}
	interactionCreate(session, i)
	if len(f.writes) != 2 {
		t.Fatal("disabled component mutated")
	}
}
func TestStartupStillFailsClosed(t *testing.T) {
	if os.Getenv("AUTHBOT_STARTUP_TEST") == "true" {
		loadConfig()
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStartupStillFailsClosed$")
	cmd.Env = []string{"AUTHBOT_STARTUP_TEST=true"}
	output, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "Missing required environment variable: DISCORD_TOKEN") {
		t.Fatal("production config stopped failing closed")
	}
}

func TestRecoveryConcurrentConfirmAndPreview(t *testing.T) {
	for _, action := range []string{emailAction, mfaAction} {
		t.Run(action, func(t *testing.T) {
			f := newRecoveryFixture(t)
			id := f.preview(action)
			var wg sync.WaitGroup
			var successes atomic.Int32
			for j := 0; j < 24; j++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if _, err := f.r.confirm(f.ctx, id, false); err == nil {
						successes.Add(1)
					}
				}()
			}
			wg.Wait()
			expected := 1
			if action == mfaAction {
				expected = 2
			}
			if successes.Load() != 1 || len(f.writes) != expected {
				t.Fatal("concurrent confirmation replay")
			}
		})
	}
	f := newRecoveryFixture(t)
	var wg sync.WaitGroup
	var previews atomic.Int32
	for j := 0; j < 24; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err == nil {
				previews.Add(1)
			}
		}()
	}
	wg.Wait()
	if previews.Load() != 1 || len(f.r.pending) != 1 || len(f.writes) != 0 {
		t.Fatal("concurrent target preview")
	}
}
func TestRecoveryStopsAfterChangesBetweenDeletes(t *testing.T) {
	for _, which := range []string{"new enrollment", "target protected", "identity superuser", "audit unavailable", "expired"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			id := f.preview(mfaAction)
			changed := false
			f.hook = func(r *http.Request) *http.Response {
				if !changed && len(f.devices) == 1 {
					changed = true
					switch which {
					case "new enrollment":
						f.devices = append(f.devices, deviceMock("3", "totp"))
					case "target protected":
						f.user.Super = flag(true)
					case "identity superuser":
						f.service.Super = flag(true)
					case "audit unavailable":
						f.audit.mu.Lock()
						f.audit.fail = true
						f.audit.mu.Unlock()
					case "expired":
						f.now = f.now.Add(recoveryTTL)
					}
				}
				return nil
			}
			msg, err := f.r.confirm(f.ctx, id, false)
			if err == nil || len(f.writes) != 1 || !strings.Contains(msg, "1 confirmed") {
				t.Fatalf("continued after state change: %s %v", msg, err)
			}
		})
	}
}
func TestRecoveryFailureOutcomeIsTruthful(t *testing.T) {
	f := newRecoveryFixture(t)
	id := f.preview(mfaAction)
	f.hook = func(r *http.Request) *http.Response {
		if r.Method == "DELETE" {
			return mockResponse(500, map[string]string{"secret": "MOCK_SENSITIVE"})
		}
		return nil
	}
	msg, err := f.r.confirm(f.ctx, id, false)
	if err == nil || len(f.writes) != 1 || !strings.Contains(msg, "0 confirmed") || strings.Contains(msg+err.Error(), "MOCK_SENSITIVE") {
		t.Fatal("failed delete retried or outcome leaked")
	}
	f = newRecoveryFixture(t)
	id = f.preview(emailAction)
	f.hook = func(r *http.Request) *http.Response {
		if r.Method == "POST" {
			f.audit.mu.Lock()
			f.audit.fail = true
			f.audit.mu.Unlock()
		}
		return nil
	}
	msg, err = f.r.confirm(f.ctx, id, false)
	if err == nil || len(f.writes) != 1 || !strings.Contains(msg, "submitted") {
		t.Fatal("acknowledged outcome lost on audit failure")
	}
}
func TestRecoveryRejectsMalformedMetadata(t *testing.T) {
	for _, which := range []string{"duplicate", "unsafe id", "missing confirmed", "unknown type", "null response", "missing freshness metadata"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			switch which {
			case "duplicate":
				f.devices = append(f.devices, f.devices[0])
			case "unsafe id":
				f.devices[0].PK = "../other"
			case "missing confirmed":
				f.devices[0].Confirmed = nil
			case "unknown type":
				f.devices[0].Type = "new-device-class"
			case "null response":
				f.devices = nil
			case "missing freshness metadata":
				f.devices[0].Updated = ""
			}
			if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err == nil || len(f.writes) != 0 {
				t.Fatal("malformed metadata accepted")
			}
		})
	}
}
func TestRecoveryAuditLatchesAndRejectsNonregular(t *testing.T) {
	dir := t.TempDir()
	a, err := openRecoveryAudit(filepath.Join(dir, "audit"))
	if err != nil {
		t.Fatal(err)
	}
	a.f.Close()
	if a.Write(recoveryAuditEvent{}) == nil || !a.failed || a.Write(recoveryAuditEvent{}) == nil {
		t.Fatal("audit did not latch")
	}
	if a, err = openRecoveryAudit(dir); err == nil {
		a.Close()
		t.Fatal("directory accepted")
	}
	if _, err = openRecoveryAudit("relative.jsonl"); err == nil {
		t.Fatal("relative path accepted")
	}
}

const mockRoleID = "11111111-2222-3333-4444-555555555555"

func ordinaryPermissionMock() recoveryPermission {
	return recoveryPermission{ID: 1, App: "authentik_core", Model: "application", Code: "view_application"}
}
func identificationMock() map[string]any {
	return map[string]any{"component": "ak-stage-identification", "password_fields": false, "user_fields": []string{"username", "email"}, "captcha_stage": nil, "passkey_challenge": nil, "flow_designation": "recovery"}
}

func factorPath(kind string) string { return "/api/v3/authenticators/admin/" + kind + "/" }
