package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

func TestDynamicCurrentAndFutureHumanTargets(t *testing.T) {
	for _, kind := range []string{"internal", "external"} {
		for _, action := range []string{emailAction, mfaAction} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				f := newRecoveryFixture(t)
				f.user.Username = "new-human-created-after-bot-start"
				f.user.PK = 1234
				f.user.Type = kind
				id, _, err := f.r.preview(f.ctx, action, f.user.Username)
				if err != nil {
					t.Fatal(err)
				}
				msg, err := f.r.confirm(f.ctx, id, false)
				if err != nil || msg == "" {
					t.Fatal("ordinary future target failed")
				}
				expected := 1
				if action == mfaAction {
					expected = 2
				}
				if len(f.writes) != expected {
					t.Fatal("unexpected mutation count")
				}
			})
		}
	}
	for _, name := range []string{"AnonymousUser", "anonymous", "system"} {
		f := newRecoveryFixture(t)
		f.user.Username = name
		if _, _, err := f.r.preview(f.ctx, mfaAction, name); err == nil || len(f.writes) != 0 {
			t.Fatal("system/anonymous target accepted")
		}
	}
}
func TestDynamicAdministrativePermissionProtection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*recoveryFixture)
		allow  bool
	}{
		{"empty roles", func(f *recoveryFixture) { f.roles = []recoveryRole{} }, true},
		{"empty role grants", func(f *recoveryFixture) { f.globals = map[string][]recoveryPermission{} }, false},
		{"application view", func(*recoveryFixture) {}, true},
		{"LDAP search", func(f *recoveryFixture) {
			f.globals[mockRoleID] = []recoveryPermission{{ID: 2, App: "authentik_providers_ldap", Model: "ldapprovider", Code: "search_full_directory"}}
		}, true},
		{"object application view", func(f *recoveryFixture) {
			p := ordinaryPermissionMock()
			p.Object = stringPtr("approved-app")
			f.objects[mockRoleID] = []recoveryPermission{p}
		}, true},
		{"global admin access", func(f *recoveryFixture) { f.globals[mockRoleID][0].Code = "access_admin_interface" }, false},
		{"inherited group administration", func(f *recoveryFixture) {
			f.globals[mockRoleID][0].Model = "group"
			f.globals[mockRoleID][0].Code = "change_group"
		}, false},
		{"global user manager", func(f *recoveryFixture) {
			f.globals[mockRoleID][0].Code = "change_user"
			f.globals[mockRoleID][0].Model = "user"
		}, false},
		{"global unknown", func(f *recoveryFixture) { f.globals[mockRoleID][0].Code = "custom_future_permission" }, false},
		{"unknown application", func(f *recoveryFixture) { f.globals[mockRoleID][0].App = "custom_application" }, false},
		{"wrong model for ordinary permission", func(f *recoveryFixture) { f.globals[mockRoleID][0].Model = "user" }, false},
		{"missing model", func(f *recoveryFixture) { f.globals[mockRoleID][0].Model = "" }, false},
		{"missing app", func(f *recoveryFixture) { f.globals[mockRoleID][0].App = "" }, false},
		{"missing codename", func(f *recoveryFixture) { f.globals[mockRoleID][0].Code = "" }, false},
		{"missing permission ID", func(f *recoveryFixture) { f.globals[mockRoleID][0].ID = 0 }, false},
		{"duplicate global", func(f *recoveryFixture) {
			f.globals[mockRoleID] = append(f.globals[mockRoleID], f.globals[mockRoleID][0])
		}, false},
		{"duplicate role", func(f *recoveryFixture) { f.roles = append(f.roles, f.roles[0]) }, false},
		{"invalid role UUID", func(f *recoveryFixture) { f.roles[0].PK = "missing" }, false},
		{"object token manager", func(f *recoveryFixture) {
			f.objects[mockRoleID] = []recoveryPermission{{ID: 8, App: "authentik_core", Model: "token", Code: "change_token", Object: stringPtr("token-object")}}
		}, false},
		{"object missing scope", func(f *recoveryFixture) { f.objects[mockRoleID] = []recoveryPermission{ordinaryPermissionMock()} }, false},
		{"global with unexpected scope", func(f *recoveryFixture) { f.globals[mockRoleID][0].Object = stringPtr("app") }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			tc.mutate(f)
			_, _, err := f.r.preview(f.ctx, mfaAction, "alice")
			if (err == nil) != tc.allow {
				t.Fatalf("ordinary decision incorrect: %v", err)
			}
			if len(f.writes) != 0 {
				t.Fatal("eligibility preview mutated")
			}
		})
	}
}
func TestDynamicRoleAndGrantPagination(t *testing.T) {
	f := newRecoveryFixture(t)
	r2 := "22222222-2222-3333-4444-555555555555"
	f.hook = func(r *http.Request) *http.Response {
		page := r.URL.Query().Get("page")
		switch r.URL.Path {
		case "/api/v3/rbac/permissions/assigned_by_roles/":
			p := ordinaryPermissionMock()
			p2 := p
			p2.ID = 2
			op := p
			op.Object = stringPtr("app-1")
			op2 := p2
			op2.Object = stringPtr("app-2")
			items := []recoveryRoleAssignments{{Role: mockRoleID, Global: []recoveryPermission{p, p2}, Objects: []recoveryPermission{op, op2}}, {Role: r2, Global: []recoveryPermission{p, p2}, Objects: []recoveryPermission{op, op2}}}
			// Repeated identical permission meanings in global fixture are invalid; use
			// the second permitted ordinary model for page two.
			for i := range items {
				items[i].Global[1] = recoveryPermission{ID: 2, App: "authentik_providers_ldap", Model: "ldapprovider", Code: "search_full_directory"}
			}
			return mockResponse(200, pageMock(items, 1, 0, 1, len(items)))
		case "/api/v3/rbac/roles/":
			if page == "1" {
				return mockResponse(200, pageMock([]recoveryRole{{PK: mockRoleID}}, 1, 2, 2, 2))
			}
			return mockResponse(200, pageMock([]recoveryRole{{PK: r2}}, 2, 0, 2, 2))
		case "/api/v3/rbac/permissions/":
			p := ordinaryPermissionMock()
			if page == "1" {
				return mockResponse(200, pageMock([]recoveryPermission{p}, 1, 2, 2, 2))
			}
			p = recoveryPermission{ID: 2, App: "authentik_providers_ldap", Model: "ldapprovider", Code: "search_full_directory"}
			return mockResponse(200, pageMock([]recoveryPermission{p}, 2, 0, 2, 2))
		case "/api/v3/rbac/permissions/roles/":
			if r.URL.Query().Get("page_size") != "10" {
				t.Fatal("object grants page size")
			}
			p := ordinaryPermissionMock()
			p.Object = stringPtr("app-1")
			if page == "1" {
				return mockResponse(200, pageMock([]recoveryPermission{p}, 1, 2, 2, 2))
			}
			p.ID = 2
			p.Object = stringPtr("app-2")
			return mockResponse(200, pageMock([]recoveryPermission{p}, 2, 0, 2, 2))
		}
		return nil
	}
	if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err != nil {
		t.Fatal(err)
	}
}
func TestDynamicUnreadableOrInconsistentMetadata(t *testing.T) {
	for _, path := range []string{"/api/v3/rbac/roles/", "/api/v3/rbac/permissions/", "/api/v3/rbac/permissions/roles/", "/api/v3/rbac/permissions/assigned_by_roles/"} {
		for _, which := range []string{"denied", "missing results", "missing pagination field", "inconsistent count", "null results"} {
			t.Run(path+which, func(t *testing.T) {
				f := newRecoveryFixture(t)
				f.hook = func(r *http.Request) *http.Response {
					if r.URL.Path != path {
						return nil
					}
					switch which {
					case "denied":
						return mockResponse(403, map[string]string{"private": "mock-private-metadata"})
					case "missing results":
						return mockResponse(200, map[string]any{"pagination": map[string]int{"next": 0, "current": 1, "count": 0, "total_pages": 1}})
					case "missing pagination field":
						return mockResponse(200, map[string]any{"results": []any{}, "pagination": map[string]int{"current": 1, "count": 0, "total_pages": 1}})
					case "inconsistent count":
						return mockResponse(200, pageMock([]recoveryRole{}, 1, 0, 1, 1))
					default:
						return mockResponse(200, pageMock[recoveryRole](nil, 1, 0, 1, 0))
					}
				}
				_, _, err := f.r.preview(f.ctx, mfaAction, "alice")
				if err == nil || len(f.writes) != 0 || strings.Contains(err.Error(), "mock-private") {
					t.Fatal("incomplete metadata accepted/leaked")
				}
			})
		}
	}
	for _, which := range []string{"role changes during scan", "grant changes between scans"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			reads := 0
			f.hook = func(r *http.Request) *http.Response {
				if which == "role changes during scan" && r.URL.Path == "/api/v3/rbac/roles/" {
					reads++
					if reads%2 == 0 {
						return mockResponse(200, pageMock([]recoveryRole{}, 1, 0, 1, 0))
					}
				}
				if which == "grant changes between scans" && r.URL.Path == "/api/v3/rbac/permissions/" {
					reads++
					p := ordinaryPermissionMock()
					p.ID = int64(reads)
					return mockResponse(200, pageMock([]recoveryPermission{p}, 1, 0, 1, 1))
				}
				return nil
			}
			if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err == nil {
				t.Fatal("inconsistent snapshots accepted")
			}
		})
	}
}
func TestDynamicPromotionAndOrdinaryFingerprintChanges(t *testing.T) {
	for _, action := range []string{emailAction, mfaAction} {
		for _, which := range []string{"superuser ancestor", "global promotion", "object promotion", "new effective role", "ordinary object scope changed", "ordinary global assignment changed"} {
			t.Run(action+which, func(t *testing.T) {
				f := newRecoveryFixture(t)
				if which == "ordinary object scope changed" {
					p := ordinaryPermissionMock()
					p.Object = stringPtr("before")
					f.objects[mockRoleID] = []recoveryPermission{p}
				}
				id := f.preview(action)
				switch which {
				case "superuser ancestor":
					f.user.Super = flag(true)
				case "global promotion":
					f.globals[mockRoleID][0].Code = "access_admin_interface"
				case "object promotion":
					f.objects[mockRoleID] = []recoveryPermission{{ID: 9, App: "authentik_core", Model: "user", Code: "change_user", Object: stringPtr("any-user")}}
				case "new effective role":
					f.roles = append(f.roles, recoveryRole{PK: "22222222-2222-3333-4444-555555555555"})
				case "ordinary object scope changed":
					f.objects[mockRoleID][0].Object = stringPtr("after")
				case "ordinary global assignment changed":
					f.globals[mockRoleID][0].ID = 5
				}
				if _, err := f.r.confirm(f.ctx, id, false); err == nil || len(f.writes) != 0 {
					t.Fatal("promotion/fingerprint change not blocked")
				}
			})
		}
	}
	for _, kind := range []string{"global", "object"} {
		t.Run("between TOTP deletes "+kind, func(t *testing.T) {
			f := newRecoveryFixture(t)
			id := f.preview(mfaAction)
			changed := false
			f.hook = func(r *http.Request) *http.Response {
				if !changed && len(f.devices) == 1 {
					changed = true
					if kind == "global" {
						f.globals[mockRoleID][0].Code = "access_admin_interface"
					} else {
						f.objects[mockRoleID] = []recoveryPermission{{ID: 9, App: "authentik_core", Model: "user", Code: "change_user", Object: stringPtr("42")}}
					}
				}
				return nil
			}
			message, err := f.r.confirm(f.ctx, id, false)
			if err == nil || len(f.writes) != 1 || !strings.Contains(message, "1 confirmed") || !strings.Contains(strings.Join(f.audit.statuses(), ","), "partial") {
				t.Fatal("continued after promotion between deletes")
			}
		})
	}
}
func TestAuthenticatedRecoveryClientCannotWriteOtherRoutes(t *testing.T) {
	f := newRecoveryFixture(t)
	for _, route := range []string{"/api/v3/core/users/42/recovery_email/", "/api/v3/core/users/42/recovery/", "/api/v3/core/users/42/set_password/", "/api/v3/authenticators/admin/webauthn/1/", "/api/v3/authenticators/admin/static/1/"} {
		for _, method := range []string{"POST", "DELETE", "PATCH"} {
			if err := f.r.api.request(method, route, nil, nil, nil); err == nil {
				t.Fatal("recovery API client allows unrelated mutation")
			}
		}
	}
	if len(f.requests) != 0 {
		t.Fatal("forbidden authenticated route reached transport")
	}
	for _, scope := range []string{"totp,webauthn", "static", "email", "sms", "duo", "webauthn"} {
		_, err := readRecoveryConfig(Config{}, func(k string) string {
			if k == "RECOVERY_MFA_FACTOR_TYPES" {
				return scope
			}
			return ""
		})
		if err == nil {
			t.Fatalf("accepted unsupported scope %s", scope)
		}
	}
}
func TestDynamicRateLimitUsesSameTargetAcrossActions(t *testing.T) {
	f := newRecoveryFixture(t)
	id := f.preview(mfaAction)
	other := f.ctx
	other.Caller = "101"
	if _, _, err := f.r.preview(other, emailAction, "alice"); err == nil {
		t.Fatal("second caller bypassed target lock")
	}
	if _, err := f.r.confirm(f.ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.r.preview(other, emailAction, "alice"); err == nil {
		t.Fatal("cancel bypassed cooldown")
	}
	// Names are exact and bound to PK/UUID; no caller may redirect a handle.
	if len(f.writes) != 0 {
		t.Fatal(fmt.Sprint("unexpected writes"))
	}
}

func TestDynamicSupplementDetectsHiddenGlobalGrants(t *testing.T) {
	for _, which := range []string{"guardian hidden", "custom hidden", "missing assigned role", "missing nested grants", "scope mismatch", "inconsistent assignment ID", "object-only ordinary anchor"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			if which == "object-only ordinary anchor" {
				f.globals = map[string][]recoveryPermission{}
				p := ordinaryPermissionMock()
				p.Object = stringPtr("app")
				f.objects[mockRoleID] = []recoveryPermission{p}
			}
			reads := 0
			f.hook = func(r *http.Request) *http.Response {
				if r.URL.Path != "/api/v3/rbac/permissions/assigned_by_roles/" {
					return nil
				}
				reads++
				p := ordinaryPermissionMock()
				assigned := recoveryRoleAssignments{Role: mockRoleID, Global: []recoveryPermission{p}, Objects: []recoveryPermission{}}
				switch which {
				case "guardian hidden":
					assigned.Global = append(assigned.Global, recoveryPermission{ID: 99, App: "guardian", Model: "roleobjectpermission", Code: "view_roleobjectpermission"})
				case "custom hidden":
					assigned.Global = append(assigned.Global, recoveryPermission{ID: 99, App: "third_party", Model: "customthing", Code: "manage_everything"})
				case "missing assigned role":
					return mockResponse(200, pageMock([]recoveryRoleAssignments{}, 1, 0, 1, 0))
				case "missing nested grants":
					assigned.Global = nil
				case "scope mismatch":
					assigned.Objects = []recoveryPermission{{ID: 2, App: "authentik_core", Model: "application", Code: "view_application", Object: stringPtr("unexpected")}}
				case "inconsistent assignment ID":
					assigned.Global[0].ID = int64(reads)
				case "object-only ordinary anchor":
					if r.URL.Query().Get("model") != "authentik_core.application" || r.URL.Query().Get("object_pk") != "app" {
						t.Fatal("missing role-selection anchor")
					}
					assigned.Global = []recoveryPermission{}
					p.Object = stringPtr("app")
					assigned.Objects = []recoveryPermission{p}
				}
				return mockResponse(200, pageMock([]recoveryRoleAssignments{assigned}, 1, 0, 1, 1))
			}
			_, _, err := f.r.preview(f.ctx, mfaAction, "alice")
			if (err == nil) != (which == "object-only ordinary anchor") {
				t.Fatal("supplementary completeness check failed")
			}
			if len(f.writes) != 0 {
				t.Fatal("metadata check mutated")
			}
		})
	}
}
func TestDynamicPermissionGrantCatalogDoesNotReadObsoleteConfig(t *testing.T) {
	_, err := readRecoveryConfig(Config{}, func(key string) string {
		if key == "RECOVERY_TARGET_USERNAMES" || key == "RECOVERY_EMAIL_STAGE_UUID" {
			t.Fatal("obsolete target/stage setting read")
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDynamicEligibilityChangesDuringGrantScan(t *testing.T) {
	for _, which := range []string{"target super", "target UUID", "identity super", "identity changed"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			changed := false
			f.hook = func(r *http.Request) *http.Response {
				if !changed && r.URL.Path == "/api/v3/rbac/permissions/" {
					changed = true
					switch which {
					case "target super":
						f.user.Super = flag(true)
					case "target UUID":
						f.user.UUID = "99999999-2222-3333-4444-555555555555"
					case "identity super":
						f.service.Super = flag(true)
					case "identity changed":
						f.service.PK = 88
					}
				}
				return nil
			}
			if _, _, err := f.r.preview(f.ctx, mfaAction, "alice"); err == nil || len(f.writes) != 0 {
				t.Fatal("eligibility change during grant scan not detected")
			}
		})
	}
}
