package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
)

func nativeAPI(t *testing.T, transport mockRoundTripper) *recoveryAPI {
	t.Helper()
	a, err := newRecoveryAPI("https://auth.example.invalid", "mock-api-credential")
	if err != nil {
		t.Fatal(err)
	}
	a.client.Transport = transport
	return a
}
func nativeHeaders(t *testing.T, r *http.Request) {
	t.Helper()
	if r.URL.Scheme != "https" || r.URL.Host != "auth.example.invalid" || r.URL.EscapedPath() != "/api/v3/flows/executor/approved-recovery/" || r.URL.RawQuery != "query=" {
		t.Fatal("native origin/path/query escaped configuration")
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("Accept") != "application/json" {
		t.Fatal("native authentication/header isolation failed")
	}
}
func redirectMock(code int, where string) *http.Response {
	r := mockResponse(code, nil)
	r.Header.Set("Location", where)
	return r
}
func TestNativeCookiePersistenceIsolationAndExactBody(t *testing.T) {
	calls, posts, guards := 0, 0, 0
	a := nativeAPI(t, func(r *http.Request) (*http.Response, error) {
		nativeHeaders(t, r)
		calls++
		if r.Method == "GET" {
			if len(r.Cookies()) != 0 {
				t.Fatal("anonymous jar reused from earlier action")
			}
			resp := mockResponse(200, identificationMock())
			resp.Header.Add("Set-Cookie", "sessionid=mock-session; Path=/; Secure; HttpOnly")
			return resp, nil
		}
		posts++
		cookie, err := r.Cookie("sessionid")
		if err != nil || cookie.Value != "mock-session" {
			t.Fatal("flow session cookie not retained")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil || string(body) != "{\"component\":\"ak-stage-identification\",\"uid_field\":\"alice\"}\n" {
			t.Fatal("native body must contain only component and exact identifier")
		}
		return mockResponse(200, map[string]string{"component": "ak-stage-email"}), nil
	})
	for j := 0; j < 2; j++ {
		if err := a.nativeRecovery("approved-recovery", "alice", func() error { guards++; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 4 || posts != 2 || guards != 2 {
		t.Fatal("flow advanced past email or reused submission")
	}
}
func TestNativeIdentificationSafety(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"password", func(m map[string]any) { m["password_fields"] = true }},
		{"missing password metadata", func(m map[string]any) { delete(m, "password_fields") }},
		{"captcha", func(m map[string]any) { m["captcha_stage"] = map[string]any{"interactive": true} }},
		{"empty captcha", func(m map[string]any) { m["captcha_stage"] = map[string]any{} }},
		{"passkey", func(m map[string]any) { m["passkey_challenge"] = map[string]any{"required": true} }},
		{"unknown component", func(m map[string]any) { m["component"] = "ak-stage-user-login" }},
		{"password stage", func(m map[string]any) { m["component"] = "ak-stage-password" }},
		{"MFA stage", func(m map[string]any) { m["component"] = "ak-stage-authenticator-validate" }},
		{"email before identifier", func(m map[string]any) { m["component"] = "ak-stage-email" }},
		{"error body", func(m map[string]any) {
			m["response_errors"] = map[string]any{"uid_field": []any{map[string]string{"string": "mock-private-response"}}}
		}},
		{"upn unsupported", func(m map[string]any) { m["user_fields"] = []string{"username", "upn"} }},
		{"no username", func(m map[string]any) { m["user_fields"] = []string{"email"} }},
		{"missing flow designation", func(m map[string]any) { delete(m, "flow_designation") }},
		{"login designation", func(m map[string]any) { m["flow_designation"] = "authentication" }},
		{"pending session", func(m map[string]any) { m["pending_user_identifier"] = "other" }},
		{"shell", func(m map[string]any) { m["type"] = "shell" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			a := nativeAPI(t, func(r *http.Request) (*http.Response, error) {
				nativeHeaders(t, r)
				calls++
				if r.Method != "GET" {
					t.Fatal("unsafe identification submitted")
				}
				m := identificationMock()
				tc.mutate(m)
				return mockResponse(200, m), nil
			})
			err := a.nativeRecovery("approved-recovery", "alice", func() error { t.Fatal("write callback reached"); return nil })
			if err == nil || calls != 1 || strings.Contains(err.Error(), "mock-private") {
				t.Fatal("unsafe challenge accepted or error leaked")
			}
		})
	}
}
func TestNativeRestrictedRedirects(t *testing.T) {
	paths := []struct {
		name, location string
		allowed        bool
	}{
		{"exact absolute", "https://auth.example.invalid/api/v3/flows/executor/approved-recovery/?query=", true},
		{"same path", "/api/v3/flows/executor/approved-recovery/", true},
		{"cross host", "https://other.invalid/api/v3/flows/executor/approved-recovery/", false},
		{"downgrade", "http://auth.example.invalid/api/v3/flows/executor/approved-recovery/", false},
		{"other flow", "/api/v3/flows/executor/other/", false},
		{"browser URL", "/if/flow/approved-recovery/", false},
		{"next query", "?next=/", false},
		{"flow query", "?query=flow_token%3Dmock-never-use", false},
		{"top-level token", "?flow_token=mock-never-use", false},
		{"duplicate query", "?query=&query=", false},
		{"encoded path", "/api/v3/flows/executor/%61pproved-recovery/", false},
		{"fragment", "#unexpected", false},
		{"credentials", "https://mock-user:mock-password@auth.example.invalid/api/v3/flows/executor/approved-recovery/", false},
	}
	for _, style := range []string{"http", "challenge"} {
		for _, tc := range paths {
			t.Run(style+"/"+tc.name, func(t *testing.T) {
				calls, posts := 0, 0
				a := nativeAPI(t, func(r *http.Request) (*http.Response, error) {
					nativeHeaders(t, r)
					calls++
					if r.Method == "POST" {
						posts++
						if style == "http" {
							return redirectMock(302, tc.location), nil
						}
						return mockResponse(200, map[string]any{"component": "xak-flow-redirect", "to": tc.location, "final_redirect": false}), nil
					}
					if posts > 0 {
						return mockResponse(200, map[string]string{"component": "ak-stage-email"}), nil
					}
					return mockResponse(200, identificationMock()), nil
				})
				err := a.nativeRecovery("approved-recovery", "alice", func() error { return nil })
				if (err == nil) != tc.allowed || posts != 1 || calls > 3 {
					t.Fatal("redirect restriction or no-replay rule failed")
				}
			})
		}
	}
}
func TestNativeNoReplayAndBoundedHops(t *testing.T) {
	for _, kind := range []string{"307 after POST", "308 after POST", "redirect loop", "final redirect", "repeated identification", "password after submit", "MFA after submit", "unknown after submit", "uncertain POST", "unavailable flow", "invalid JSON"} {
		t.Run(kind, func(t *testing.T) {
			calls, posts := 0, 0
			a := nativeAPI(t, func(r *http.Request) (*http.Response, error) {
				nativeHeaders(t, r)
				calls++
				if r.Method == "POST" {
					posts++
					switch kind {
					case "307 after POST":
						return redirectMock(307, r.URL.String()), nil
					case "308 after POST":
						return redirectMock(308, r.URL.String()), nil
					case "uncertain POST":
						return nil, errors.New("mock-private-transport")
					case "final redirect":
						return mockResponse(200, map[string]any{"component": "xak-flow-redirect", "to": r.URL.String(), "final_redirect": true}), nil
					case "password after submit":
						return mockResponse(200, map[string]any{"component": "ak-stage-password", "password": "mock-never-read"}), nil
					case "MFA after submit":
						return mockResponse(200, map[string]any{"component": "ak-stage-authenticator-totp", "config_url": "mock-never-read"}), nil
					case "unknown after submit":
						return mockResponse(200, map[string]string{"component": "unexpected"}), nil
					}
					return mockResponse(200, identificationMock()), nil
				}
				if kind == "unavailable flow" {
					return mockResponse(404, map[string]string{"error": "mock-private-response"}), nil
				}
				if kind == "invalid JSON" {
					resp := mockResponse(200, nil)
					resp.Body = io.NopCloser(strings.NewReader("mock-private-malformed"))
					return resp, nil
				}
				if kind == "redirect loop" {
					return redirectMock(302, r.URL.String()), nil
				}
				return mockResponse(200, identificationMock()), nil
			})
			err := a.nativeRecovery("approved-recovery", "alice", func() error { return nil })
			if err == nil || posts > 1 || calls > 6 || strings.Contains(err.Error(), "mock-private") {
				t.Fatal("flow retried/advanced or leaked response")
			}
		})
	}
}
func TestNativePrewriteGuardAndExpiry(t *testing.T) {
	f := newRecoveryFixture(t)
	id := f.preview(emailAction)
	f.hook = func(r *http.Request) *http.Response {
		if r.Method == "GET" && strings.Contains(r.URL.Path, "/flows/executor/") {
			f.user.Super = flag(true)
		}
		return nil
	}
	if _, err := f.r.confirm(f.ctx, id, false); err == nil || len(f.writes) != 0 || f.nativeGets != 1 {
		t.Fatal("promotion before native POST was not blocked")
	}
	f = newRecoveryFixture(t)
	id = f.preview(emailAction)
	f.hook = func(r *http.Request) *http.Response {
		if r.URL.Path == "/api/v3/core/users/" {
			f.now = f.now.Add(recoveryTTL)
		}
		return nil
	}
	if _, err := f.r.confirm(f.ctx, id, false); err == nil || len(f.writes) != 0 {
		t.Fatal("expired during eligibility read")
	}
}
func TestNativeErrorRedactionAndAuditOutcome(t *testing.T) {
	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)
	for _, code := range []int{400, 401, 403, 404, 429, 500} {
		f := newRecoveryFixture(t)
		id := f.preview(emailAction)
		f.hook = func(r *http.Request) *http.Response {
			if r.Method == "POST" {
				return mockResponse(code, map[string]string{"private": "mock-private-response"})
			}
			return nil
		}
		message, err := f.r.confirm(f.ctx, id, false)
		if err == nil || len(f.writes) != 1 || strings.Contains(message+err.Error()+logs.String(), "mock-private") {
			t.Fatal("native failure leaked or retried")
		}
		if _, err = f.r.confirm(f.ctx, id, false); err == nil || len(f.writes) != 1 {
			t.Fatal("uncertain confirmation reused")
		}
	}
}
func TestDynamicNativeIdentifierAmbiguity(t *testing.T) {
	for _, which := range []string{"case username", "cross-field email", "case email", "admin collision", "inactive collision", "missing email metadata", "missing UUID", "duplicate row", "no match", "same target username and email"} {
		t.Run(which, func(t *testing.T) {
			f := newRecoveryFixture(t)
			other := ordinaryMock()
			other.PK = 99
			other.UUID = "99999999-2222-3333-4444-555555555555"
			other.Username = "other"
			switch which {
			case "case username":
				other.Username = "ALIce"
			case "cross-field email":
				other.Email = stringPtr("alice")
			case "case email":
				other.Email = stringPtr("ALICE")
			case "admin collision":
				other.Email = stringPtr("Alice")
				other.Super = flag(true)
			case "inactive collision":
				other.Username = "ALICE"
				other.Active = flag(false)
			case "missing email metadata":
				other.Email = nil
			case "missing UUID":
				other.UUID = ""
			case "duplicate row":
				other = f.user
			case "no match":
				f.user.Username = "other"
			case "same target username and email":
				f.user.Email = stringPtr("ALICE")
			}
			if which != "same target username and email" {
				f.otherUsers = []recoveryUser{other}
			}
			target := ordinaryMock()
			if which == "same target username and email" {
				target = f.user
			}
			err := f.r.api.uniqueNativeIdentifier(target)
			if (err == nil) != (which == "same target username and email") {
				t.Fatal("identifier ambiguity check failed")
			}
			if len(f.writes) != 0 {
				t.Fatal("prevalidation submitted flow")
			}
		})
	}
}
func TestNativeOnlyPinnedFlowAndNeverPrivilegedPasswordRoutes(t *testing.T) {
	f := newRecoveryFixture(t)
	id := f.preview(emailAction)
	if _, err := f.r.confirm(f.ctx, id, false); err != nil {
		t.Fatal(err)
	}
	for _, req := range f.requests {
		if strings.Contains(req, "/brands/") || strings.Contains(req, "/stages/email/") || strings.Contains(req, "/recovery_email/") || strings.Contains(req, "/recovery/") || strings.Contains(req, "/set_password") {
			t.Fatal("obsolete privileged email path reached")
		}
	}
	for _, slug := range []string{"", "../other", "flow?next=x", "flow/other"} {
		calls := 0
		a := nativeAPI(t, func(*http.Request) (*http.Response, error) { calls++; return nil, fmt.Errorf("unexpected") })
		if a.nativeRecovery(slug, "alice", func() error { return nil }) == nil || calls != 0 {
			t.Fatal("unpinned flow allowed")
		}
	}
}

func TestNativeUnicodeIdentifierCollisions(t *testing.T) {
	for _, pair := range [][2]string{{"strasse", "straße"}, {"office", "oﬃce"}, {"Σ", "ς"}, {"I", "ı"}, {"i\u0307", "İ"}} {
		t.Run(pair[0], func(t *testing.T) {
			f := newRecoveryFixture(t)
			f.user.Username = pair[0]
			other := ordinaryMock()
			other.Username = "other"
			other.Email = stringPtr(pair[1])
			other.PK = 99
			other.UUID = "99999999-2222-3333-4444-555555555555"
			f.otherUsers = []recoveryUser{other}
			if _, _, err := f.r.preview(f.ctx, emailAction, f.user.Username); err == nil || len(f.writes) != 0 {
				t.Fatal("Unicode username/email case collision allowed")
			}
		})
	}
}
func TestRecoveryNeverUsesLegacyHTTPHelpers(t *testing.T) {
	previous := apiClient
	defer func() { apiClient = previous }()
	apiClient = &http.Client{Transport: mockRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Fatal("legacy credential/error helper reached")
		return nil, errors.New("unreachable")
	})}
	for _, action := range []string{emailAction, mfaAction} {
		f := newRecoveryFixture(t)
		id := f.preview(action)
		if _, err := f.r.confirm(f.ctx, id, false); err != nil {
			t.Fatal(err)
		}
	}
}
func TestNativeRedirectKeepsSessionAndCannotRepeatPOST(t *testing.T) {
	gets, posts := 0, 0
	a := nativeAPI(t, func(r *http.Request) (*http.Response, error) {
		nativeHeaders(t, r)
		if r.Method == "POST" {
			posts++
			if _, err := r.Cookie("sessionid"); err != nil {
				t.Fatal("POST lost isolated session")
			}
			return redirectMock(302, "/api/v3/flows/executor/approved-recovery/"), nil
		}
		gets++
		if gets == 1 {
			resp := mockResponse(200, identificationMock())
			resp.Header.Add("Set-Cookie", "sessionid=mock-session; Secure; Path=/")
			return resp, nil
		}
		if _, err := r.Cookie("sessionid"); err != nil {
			t.Fatal("redirect lost isolated session")
		}
		return mockResponse(200, map[string]string{"component": "ak-stage-email"}), nil
	})
	if err := a.nativeRecovery("approved-recovery", "alice", func() error { return nil }); err != nil || gets != 2 || posts != 1 {
		t.Fatal("redirect did not retain session or repeated POST")
	}
}
