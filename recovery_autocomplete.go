package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

const recoveryChoicePrefix = "ak-user:"
const recoveryAutocompleteLimit = 25
const recoveryAutocompleteBudget = 100 * time.Millisecond
const recoverySuggestionMaxAge = 90 * time.Second
const recoverySuggestionRefreshTimeout = 40 * time.Second
const recoverySuggestionRefreshPause = 5 * time.Second

type recoverySuggestion struct {
	PK       int64
	Username string
}
type recoverySuggestionSnapshot struct {
	Generation uint64
	Users      []recoverySuggestion
	Started    time.Time
	Ready      bool
}

var errSuggestionRefreshBusy = errors.New("Suggestion refresh already running")

// Autocomplete receives partial command data: action may not be filled yet, but
// any supplied action must remain reset and only the user field may be focused.
func parseRecoveryAutocomplete(data discordgo.ApplicationCommandInteractionData) (action, query string, valid bool) {
	if data.Name != authctlCommand || len(data.Options) != 1 || data.Options[0] == nil {
		return
	}
	sub := data.Options[0]
	if sub.Type != discordgo.ApplicationCommandOptionSubCommand || len(sub.Options) < 1 || len(sub.Options) > 2 {
		return
	}
	switch sub.Name {
	case passwordCommand:
		action = emailAction
	case mfaCommand:
		action = mfaAction
	default:
		return
	}
	seen := map[string]bool{}
	for _, o := range sub.Options {
		if o == nil || o.Type != discordgo.ApplicationCommandOptionString || len(o.Options) != 0 || seen[o.Name] {
			return "", "", false
		}
		seen[o.Name] = true
		value, ok := o.Value.(string)
		if !ok {
			return "", "", false
		}
		switch o.Name {
		case "action":
			if o.Focused || value != "reset" {
				return "", "", false
			}
		case "user":
			if !o.Focused {
				return "", "", false
			}
			query = value
		default:
			return "", "", false
		}
	}
	return action, query, seen["user"]
}

func recoveryChoice(u recoverySuggestion) *discordgo.ApplicationCommandOptionChoice {
	name := u.Username
	if len([]rune(name)) > 100 {
		suffix := fmt.Sprintf("… #%d", u.PK)
		name = string([]rune(name)[:100-len([]rune(suffix))]) + suffix
	}
	return &discordgo.ApplicationCommandOptionChoice{Name: name, Value: recoveryChoicePrefix + strconv.FormatInt(u.PK, 10)}
}

// Only names and stable account IDs are cached, never passwords, factor data,
// emails, credentials or authorization decisions for an actual recovery action.
func (r *recoveryService) buildAutocompleteSnapshot(ctx context.Context) ([]recoverySuggestion, error) {
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	api := *r.api
	api.ctx = work
	api.scanRoles = newRecoveryScanRoles()
	reader := &recoveryService{cfg: r.cfg, api: &api, audit: r.audit}
	serviceID, err := api.identity(r.cfg.ServiceUsername, r.cfg.UseExistingToken, r.cfg.ExpectedUserID)
	if err != nil {
		return nil, err
	}
	users, err := recoveryList[recoveryUser](&api, "/api/v3/core/users/", url.Values{"include_groups": {"false"}, "include_roles": {"false"}, "ordering": {"pk"}})
	if err != nil {
		return nil, err
	}
	seenID := map[int64]bool{}
	seenName := map[string]bool{}
	candidates := []recoveryUser{}
	for _, u := range users {
		if seenID[u.PK] || seenName[u.Username] {
			return nil, errRecoveryProtected
		}
		seenID[u.PK] = true
		seenName[u.Username] = true
		if reader.eligible(u, serviceID) && validRecoveryName(u.Username) && utf8.ValidString(u.Username) {
			candidates = append(candidates, u)
		}
	}
	jobs := make(chan recoveryUser, len(candidates))
	for _, u := range candidates {
		jobs <- u
	}
	close(jobs)
	results := make(chan recoverySuggestion, len(candidates))
	var wg sync.WaitGroup
	failures := make(chan error, 1)
	fail := func(err error) {
		select {
		case failures <- err:
		default:
		}
		cancel()
	}
	failure := func() error {
		select {
		case err := <-failures:
			return err
		default:
			return errRecoveryAPI
		}
	}
	workers := 4
	if len(candidates) < workers {
		workers = len(candidates)
	}
	for n := 0; n < workers; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for u := range jobs {
				if work.Err() != nil {
					return
				}
				fresh, _, _, e := reader.validate(u.Username)
				if e != nil {
					if !errors.Is(e, errRecoveryProtected) && work.Err() == nil {
						fail(e)
						return
					}
					continue
				}
				if !u.same(fresh) {
					fail(errRecoveryProtected)
					return
				}
				select {
				case results <- recoverySuggestion{PK: fresh.PK, Username: fresh.Username}:
				case <-work.Done():
					return
				}
			}
		}()
	}
	go func() { wg.Wait(); close(results) }()
	verified := []recoverySuggestion{}
	for {
		select {
		case u, ok := <-results:
			if !ok {
				if work.Err() != nil {
					return nil, failure()
				}
				return verified, nil
			}
			verified = append(verified, u)
		case <-work.Done():
			return nil, failure()
		}
	}
}

// Expiry is measured from the START of the full scan, so a slow refresh cannot
// extend the oldest check beyond the disclosed 90-second age bound. Never publish
// partial refreshes. Any failed refresh clears the old snapshot, even if unexpired.
func (r *recoveryService) refreshAutocomplete(ctx context.Context) error {
	if !r.suggestRefreshMu.TryLock() {
		return errSuggestionRefreshBusy
	}
	defer r.suggestRefreshMu.Unlock()
	started := r.now()
	work, cancel := context.WithTimeout(ctx, recoverySuggestionRefreshTimeout)
	defer cancel()
	users, err := r.buildAutocompleteSnapshot(work)
	now := r.now()
	if err == nil && (work.Err() != nil || now.Before(started) || now.Sub(started) >= recoverySuggestionMaxAge) {
		err = errRecoveryAPI
	}
	r.suggestMu.Lock()
	defer r.suggestMu.Unlock()
	if err != nil {
		r.suggestions = recoverySuggestionSnapshot{Generation: r.suggestions.Generation + 1}
		return err
	}
	r.suggestions = recoverySuggestionSnapshot{Users: users, Started: started, Ready: true, Generation: r.suggestions.Generation + 1}
	return nil
}

func (r *recoveryService) startAutocompleteCache(ctx context.Context) {
	r.suggestLoop.Do(func() {
		go func() {
			for {
				if ctx.Err() != nil {
					return
				}
				started := time.Now()
				err := r.refreshAutocomplete(ctx)
				if ctx.Err() != nil {
					return
				}
				if errors.Is(err, errRecoveryAccessDenied) {
					log.Print("Recovery suggestions refresh stopped: API access denied")
					return
				}
				if err != nil {
					log.Print("Recovery suggestions refresh failed; suggestions unavailable")
				} else {
					r.suggestMu.Lock()
					count := len(r.suggestions.Users)
					r.suggestMu.Unlock()
					log.Printf("Recovery suggestions refreshed: accounts=%d elapsed_ms=%d max_age_seconds=90", count, time.Since(started).Milliseconds())
				}
				timer := time.NewTimer(recoverySuggestionRefreshPause)
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
			}
		}()
	})
}

// Every request checks caller/guild/channel/feature authorization before touching
// the snapshot. Request volume never starts refreshes or makes Authentik requests.
func (r *recoveryService) autocompleteChoices(ctx context.Context, c recoveryContext, action, query string) []*discordgo.ApplicationCommandOptionChoice {
	empty := []*discordgo.ApplicationCommandOptionChoice{}
	if !r.allowed(c, action) || !utf8.ValidString(query) || utf8.RuneCountInString(query) > 150 || strings.ContainsAny(query, "\x00\r\n") || ctx.Err() != nil {
		return empty
	}
	r.suggestMu.Lock()
	snapshot := r.suggestions
	r.suggestMu.Unlock()
	now := r.now()
	if !snapshot.Ready || now.Before(snapshot.Started) || now.Sub(snapshot.Started) >= recoverySuggestionMaxAge {
		return empty
	}
	query = recoveryIdentifierFold(strings.TrimSpace(query))
	matches := []recoverySuggestion{}
	for _, u := range snapshot.Users {
		if strings.Contains(recoveryIdentifierFold(u.Username), query) {
			matches = append(matches, u)
		}
	}
	rank := func(name string) int {
		fold := recoveryIdentifierFold(name)
		if fold == query {
			return 0
		}
		if strings.HasPrefix(fold, query) {
			return 1
		}
		return 2
	}
	sort.Slice(matches, func(i, j int) bool {
		a, b := rank(matches[i].Username), rank(matches[j].Username)
		if a != b {
			return a < b
		}
		return matches[i].Username < matches[j].Username
	})
	if len(matches) > recoveryAutocompleteLimit {
		matches = matches[:recoveryAutocompleteLimit]
	}
	r.suggestMu.Lock()
	current := r.suggestions
	r.suggestMu.Unlock()
	now = r.now()
	if !current.Ready || current.Generation != snapshot.Generation || ctx.Err() != nil || !r.allowed(c, action) || now.Before(snapshot.Started) || now.Sub(snapshot.Started) >= recoverySuggestionMaxAge {
		return empty
	}
	choices := make([]*discordgo.ApplicationCommandOptionChoice, 0, len(matches))
	for _, u := range matches {
		choices = append(choices, recoveryChoice(u))
	}
	return choices
}

func handleRecoveryAutocomplete(s *discordgo.Session, i *discordgo.InteractionCreate) {
	// Autocomplete cannot be deferred. Serve only the bounded in-memory snapshot;
	// never wait for Authentik or call the legacy autocomplete handler.
	ctx, cancel := context.WithTimeout(context.Background(), recoveryAutocompleteBudget)
	defer cancel()
	choices := []*discordgo.ApplicationCommandOptionChoice{}
	if action, query, ok := parseRecoveryAutocomplete(i.ApplicationCommandData()); ok && recovery != nil {
		choices = recovery.autocompleteChoices(ctx, recoveryCaller(i), action, query)
	}
	responseCtx, done := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer done()
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionApplicationCommandAutocompleteResult, Data: &discordgo.InteractionResponseData{Choices: choices}}, discordgo.WithContext(responseCtx), discordgo.WithRestRetries(0), discordgo.WithRetryOnRatelimit(false)); err != nil {
		// Discord errors may contain an interaction token. Never log the error itself.
		// A generic failure is useful without exposing the query or any account names.
		log.Print("Recovery autocomplete response failed")
	}
}

// Choice values use account IDs so names longer than Discord's 100-character
// value limit can still be selected. Called only after preview's audit/auth gate.
func (r *recoveryService) selectedUser(value string) (*recoveryUser, error) {
	if !strings.HasPrefix(value, recoveryChoicePrefix) {
		return nil, nil
	}
	raw := strings.TrimPrefix(value, recoveryChoicePrefix)
	pk, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || pk <= 0 || strconv.FormatInt(pk, 10) != raw {
		return nil, errRecoveryProtected
	}
	if _, err = r.api.identity(r.cfg.ServiceUsername, r.cfg.UseExistingToken, r.cfg.ExpectedUserID); err != nil {
		return nil, err
	}
	var u recoveryUser
	if err = r.api.request("GET", "/api/v3/core/users/"+raw+"/", nil, nil, &u); err != nil {
		return nil, err
	}
	if u.PK != pk || !validRecoveryName(u.Username) {
		return nil, errRecoveryProtected
	}
	return &u, nil
}
