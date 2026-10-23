package main

import (
	"net/http"
	"strings"
	"testing"
)

func existingRecoveryFixture(t *testing.T) *recoveryFixture {
	f := newRecoveryFixture(t)
	f.r.cfg.UseExistingToken = true
	f.r.cfg.ExpectedUserID = f.service.PK
	f.service.Type = "internal"
	f.service.Super = flag(true)
	return f
}
func TestExistingCredentialRequiresExplicitOptInAndPinnedIdentity(t *testing.T) {
	for _, which := range []string{"valid", "no opt in", "bad switch", "no ID", "bad ID", "separate also supplied", "missing legacy"} {
		t.Run(which, func(t *testing.T) {
			legacy := Config{AuthentikToken: "mock-existing-secret", CommandChannelID: "200"}
			env := map[string]string{"RECOVERY_EMAIL_ENABLED": "true", "RECOVERY_MFA_ENABLED": "true", "RECOVERY_USE_EXISTING_AUTHENTIK_TOKEN": "true", "RECOVERY_EXPECTED_USER_ID": "7", "RECOVERY_SERVICE_USERNAME": "recovery-svc", "RECOVERY_GUILD_ID": "300", "RECOVERY_DISCORD_USER_IDS": "100", "RECOVERY_FLOW_SLUG": "approved-recovery", "RECOVERY_AUDIT_PATH": "/tmp/mock-audit.jsonl"}
			switch which {
			case "no opt in":
				delete(env, "RECOVERY_USE_EXISTING_AUTHENTIK_TOKEN")
			case "bad switch":
				env["RECOVERY_USE_EXISTING_AUTHENTIK_TOKEN"] = "yes"
			case "no ID":
				delete(env, "RECOVERY_EXPECTED_USER_ID")
			case "bad ID":
				env["RECOVERY_EXPECTED_USER_ID"] = "07"
			case "separate also supplied":
				env["RECOVERY_AUTHENTIK_TOKEN"] = "mock-separate-secret"
			case "missing legacy":
				legacy.AuthentikToken = ""
			}
			cfg, err := readRecoveryConfig(legacy, func(k string) string { return env[k] })
			if (err == nil) != (which == "valid") {
				t.Fatalf("wrong configuration decision: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "mock-") {
				t.Fatal("credential leaked")
			}
			if which == "valid" && (!cfg.UseExistingToken || cfg.Token != legacy.AuthentikToken || cfg.ExpectedUserID != 7) {
				t.Fatal("existing credential not explicitly bound")
			}
		})
	}
}
func TestExistingCredentialIdentityAndCommands(t *testing.T) {
	for _, which := range []string{"email", "TOTP", "wrong ID", "wrong username", "not superuser", "service type", "inactive", "impersonated", "privilege removed during scan"} {
		t.Run(which, func(t *testing.T) {
			f := existingRecoveryFixture(t)
			action := mfaAction
			switch which {
			case "email":
				action = emailAction
			case "wrong ID":
				f.r.cfg.ExpectedUserID++
			case "wrong username":
				f.service.Username = "other"
			case "not superuser":
				f.service.Super = flag(false)
			case "service type":
				f.service.Type = "service_account"
			case "inactive":
				f.service.Active = flag(false)
			case "impersonated":
				f.hook = func(r *http.Request) *http.Response {
					if strings.HasSuffix(r.URL.Path, "/me/") {
						return mockResponse(200, map[string]any{"user": f.service, "original": map[string]int{"pk": 99}})
					}
					return nil
				}
			case "privilege removed during scan":
				reads := 0
				f.hook = func(r *http.Request) *http.Response {
					if strings.HasSuffix(r.URL.Path, "/me/") {
						reads++
						if reads == 2 {
							f.service.Super = flag(false)
						}
					}
					return nil
				}
			}
			id, _, err := f.r.preview(f.ctx, action, f.user.Username)
			allow := which == "email" || which == "TOTP"
			if (err == nil) != allow {
				t.Fatal("wrong existing credential identity decision")
			}
			if allow {
				if _, err = f.r.confirm(f.ctx, id, false); err != nil {
					t.Fatal(err)
				}
			} else if len(f.writes) > 0 {
				t.Fatal("rejected identity mutated")
			}
		})
	}
}
func TestExistingSuperuserProvesEmptyRoleAndRejectsHiddenGrants(t *testing.T) {
	for _, which := range []string{"empty", "membership only", "hidden global", "hidden object", "unknown guardian", "missing metadata", "denied", "null", "duplicate"} {
		t.Run(which, func(t *testing.T) {
			f := existingRecoveryFixture(t)
			f.globals = map[string][]recoveryPermission{}
			f.hook = func(r *http.Request) *http.Response {
				if !strings.HasSuffix(r.URL.Path, "/used_by/") {
					return nil
				}
				refs := []map[string]string{}
				switch which {
				case "membership only":
					refs = append(refs, map[string]string{"app": "authentik_core", "model_name": "group", "pk": "group-id"})
				case "hidden global", "hidden object", "unknown guardian":
					model := "rolemodelpermission"
					if which == "hidden object" {
						model = "roleobjectpermission"
					}
					if which == "unknown guardian" {
						model = "futurepermission"
					}
					refs = append(refs, map[string]string{"app": "guardian", "model_name": model, "pk": "1"})
				case "missing metadata":
					refs = append(refs, map[string]string{"app": "guardian"})
				case "denied":
					return mockResponse(403, map[string]string{"private": "mock-private"})
				case "null":
					return mockResponse(200, nil)
				case "duplicate":
					x := map[string]string{"app": "authentik_core", "model_name": "group", "pk": "group-id"}
					refs = append(refs, x, x)
				}
				return mockResponse(200, refs)
			}
			_, _, err := f.r.preview(f.ctx, mfaAction, f.user.Username)
			if (err == nil) != (which == "empty" || which == "membership only") {
				t.Fatal("incomplete empty-role proof accepted or empty role rejected")
			}
			if len(f.writes) > 0 {
				t.Fatal("privilege inspection mutated")
			}
		})
	}
}
