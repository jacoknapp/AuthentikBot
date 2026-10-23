package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type scanRoleFixture struct {
	f     *recoveryFixture
	mu    sync.Mutex
	calls map[string]int
}

func newScanRoleFixture(t *testing.T) *scanRoleFixture {
	s := &scanRoleFixture{f: newRecoveryFixture(t), calls: map[string]int{}}
	s.f.r.api.client.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls[req.URL.Path]++
		return s.f.roundTrip(req)
	})
	return s
}
func (s *scanRoleFixture) count(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[path]
}
func (s *scanRoleFixture) scanAPI() *recoveryAPI {
	a := *s.f.r.api
	a.ctx = context.Background()
	a.scanRoles = newRecoveryScanRoles()
	return &a
}

func TestScanRoleMemoSingleFlightAndImmutableKeys(t *testing.T) {
	s := newScanRoleFixture(t)
	a := s.scanAPI()
	var wg sync.WaitGroup
	for n := 0; n < 32; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entries, err := a.rolePermissionEntries(mockRoleID, false)
			if err != nil || len(entries) == 0 {
				t.Error("shared role lookup failed", err)
			}
		}()
	}
	wg.Wait()
	for _, path := range []string{"/api/v3/rbac/permissions/", "/api/v3/rbac/permissions/roles/", "/api/v3/rbac/permissions/assigned_by_roles/"} {
		if s.count(path) != 1 {
			t.Fatalf("role metadata was not coalesced: %s count=%d", path, s.count(path))
		}
	}
	entries, _ := a.rolePermissionEntries(mockRoleID, false)
	entries[0] = "caller-mutated"
	again, _ := a.rolePermissionEntries(mockRoleID, false)
	if again[0] == "caller-mutated" {
		t.Fatal("caller modified shared memo")
	}
	if _, err := a.rolePermissionEntries(mockRoleID, true); err != nil {
		t.Fatal(err)
	}
	if s.count("/api/v3/rbac/permissions/") != 2 {
		t.Fatal("visibility mode omitted from key")
	}
	if _, err := a.rolePermissionEntries("mutable-role-name", false); !errors.Is(err, errRecoveryProtected) || s.count("/api/v3/rbac/permissions/") != 2 {
		t.Fatal("invalid role identifier queried")
	}
}
func TestScanRoleMemoFailureSingleFlight(t *testing.T) {
	for _, status := range []int{403, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			s := newScanRoleFixture(t)
			s.f.hook = func(req *http.Request) *http.Response {
				if req.URL.Path == "/api/v3/rbac/permissions/" {
					return mockResponse(status, map[string]string{"private": "SYNTHETIC_PRIVATE_ERROR"})
				}
				return nil
			}
			a := s.scanAPI()
			var wg sync.WaitGroup
			for n := 0; n < 16; n++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					entries, err := a.rolePermissionEntries(mockRoleID, false)
					if err == nil || len(entries) != 0 {
						t.Error("failed metadata was allowed")
					}
					if status == 403 && !errors.Is(err, errRecoveryAccessDenied) {
						t.Error("denial type lost")
					}
					if err != nil && strings.Contains(err.Error(), "PRIVATE") {
						t.Error("error body exposed")
					}
				}()
			}
			wg.Wait()
			if s.count("/api/v3/rbac/permissions/") != 1 {
				t.Fatal("failed/denied read retried within scan")
			}
		})
	}
}
func TestScanRoleMemoWaiterCancellation(t *testing.T) {
	s := newScanRoleFixture(t)
	a := s.scanAPI()
	started, release := make(chan struct{}), make(chan struct{})
	inner := a.client.Transport
	var once sync.Once
	a.client.Transport = mockRoundTripper(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path == "/api/v3/rbac/permissions/" {
			once.Do(func() { close(started) })
			select {
			case <-release:
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return inner.RoundTrip(req)
	})
	done := make(chan error, 1)
	go func() { _, err := a.rolePermissionEntries(mockRoleID, false); done <- err }()
	<-started
	waiter := *a
	ctx, cancel := context.WithCancel(context.Background())
	waiter.ctx = ctx
	waiting := make(chan error, 1)
	go func() { _, err := waiter.rolePermissionEntries(mockRoleID, false); waiting <- err }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	select {
	case err := <-waiting:
		if err == nil {
			t.Fatal("cancelled waiter allowed metadata")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("waiter did not stop while shared owner blocked")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatal("waiter cancelled shared owner", err)
	}
	if s.count("/api/v3/rbac/permissions/") != 1 {
		t.Fatal("waiter duplicated owner request")
	}
}
func TestScanRoleMemoInvalidationAndResetIsolation(t *testing.T) {
	s := newScanRoleFixture(t)
	a := s.scanAPI()
	if _, err := a.rolePermissionEntries(mockRoleID, false); err != nil {
		t.Fatal(err)
	}
	// Grant mutation is synthetic. A scan may retain this one bounded proof, but a
	// subsequent scan and every actual reset validation must observe the promotion.
	s.f.globals[mockRoleID] = []recoveryPermission{{ID: 9, App: "authentik_core", Model: "user", Code: "change_user"}}
	if _, err := a.rolePermissionEntries(mockRoleID, false); err != nil {
		t.Fatal("within-scan proof unexpectedly changed")
	}
	fresh := s.scanAPI()
	if _, err := fresh.rolePermissionEntries(mockRoleID, false); !errors.Is(err, errRecoveryProtected) {
		t.Fatal("new scan reused old role proof")
	}
	if s.f.r.api.scanRoles != nil {
		t.Fatal("scan memo escaped onto reset API")
	}
	if _, _, _, err := s.f.r.validate(s.f.user.Username); err == nil {
		t.Fatal("fresh reset validation reused scan proof")
	}
	if _, _, err := s.f.r.preview(s.f.ctx, emailAction, s.f.user.Username); err == nil || len(s.f.writes) != 0 {
		t.Fatal("promotion did not block recovery")
	}
}
func TestSuggestionScanMemoNeverAuthorizesPreviewOrConfirmation(t *testing.T) {
	s := newScanRoleFixture(t)
	if err := s.f.r.refreshAutocomplete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.f.r.api.scanRoles != nil {
		t.Fatal("background clone mutated original client")
	}
	before := s.count("/api/v3/rbac/permissions/")
	if _, _, _, err := s.f.r.validate(s.f.user.Username); err != nil {
		t.Fatal(err)
	}
	if s.count("/api/v3/rbac/permissions/")-before != 2 {
		t.Fatal("reset validation did not repeat both fresh permission snapshots")
	}
	id, _, err := s.f.r.preview(s.f.ctx, emailAction, s.f.user.Username)
	if err != nil {
		t.Fatal(err)
	}
	s.f.globals[mockRoleID] = []recoveryPermission{{ID: 9, App: "authentik_core", Model: "user", Code: "change_user"}}
	if _, err = s.f.r.confirm(s.f.ctx, id, false); err == nil || len(s.f.writes) != 0 {
		t.Fatal("cached role proof authorized promoted target at confirmation")
	}
	if err = s.f.r.refreshAutocomplete(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(s.f.r.autocompleteChoices(context.Background(), s.f.ctx, emailAction, "")) != 0 {
		t.Fatal("refresh retained promoted account")
	}
}

func TestScanRoleMemoDoesNotShareSuperuserEmptyRoleProof(t *testing.T) {
	s := newScanRoleFixture(t)
	s.f.globals = map[string][]recoveryPermission{}
	s.f.hook = func(req *http.Request) *http.Response {
		if strings.HasSuffix(req.URL.Path, "/used_by/") {
			return mockResponse(200, []map[string]any{})
		}
		return nil
	}
	a := s.scanAPI()
	if _, err := a.rolePermissionEntries(mockRoleID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.rolePermissionEntries(mockRoleID, false); !errors.Is(err, errRecoveryProtected) {
		t.Fatal("lower-visibility caller reused superuser empty-role proof")
	}
}
func TestScanRoleMemoFailureInvalidatedOnlyByNewScan(t *testing.T) {
	s := newScanRoleFixture(t)
	s.f.hook = func(req *http.Request) *http.Response {
		if req.URL.Path == "/api/v3/rbac/permissions/" {
			return mockResponse(503, nil)
		}
		return nil
	}
	a := s.scanAPI()
	if _, err := a.rolePermissionEntries(mockRoleID, false); err == nil {
		t.Fatal("failure allowed")
	}
	s.f.hook = nil
	if _, err := a.rolePermissionEntries(mockRoleID, false); err == nil || s.count("/api/v3/rbac/permissions/") != 1 {
		t.Fatal("failed read retried in same scan")
	}
	if _, err := s.scanAPI().rolePermissionEntries(mockRoleID, false); err != nil || s.count("/api/v3/rbac/permissions/") != 2 {
		t.Fatal("new scan retained failed result")
	}
}
