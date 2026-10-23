package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"time"
)

var errNativeRecovery = errors.New("Native recovery stopped safely; outcome may be uncertain after submission. No automatic retry; operator review required")

// Track only presence, never decode/store CAPTCHA, passkey or error material.
type nativePresence bool

func (p *nativePresence) UnmarshalJSON(b []byte) error {
	*p = nativePresence(!bytes.Equal(bytes.TrimSpace(b), []byte("null")))
	return nil
}

type nativeErrors bool

func (p *nativeErrors) UnmarshalJSON(b []byte) error {
	s := bytes.TrimSpace(b)
	*p = nativeErrors(!bytes.Equal(s, []byte("null")) && !bytes.Equal(s, []byte("{}")))
	return nil
}

type nativeChallenge struct {
	Component   string         `json:"component"`
	Type        string         `json:"type"`
	Password    *bool          `json:"password_fields"`
	UserFields  []string       `json:"user_fields"`
	Captcha     nativePresence `json:"captcha_stage"`
	Passkey     nativePresence `json:"passkey_challenge"`
	Errors      nativeErrors   `json:"response_errors"`
	Pending     string         `json:"pending_user_identifier"`
	Designation string         `json:"flow_designation"`
	To          string         `json:"to"`
	Final       bool           `json:"final_redirect"`
}

func (c nativeChallenge) identification() bool {
	if c.Component != "ak-stage-identification" || c.Password == nil || *c.Password || bool(c.Captcha) || bool(c.Passkey) || bool(c.Errors) || c.Pending != "" || c.To != "" || c.Final || (c.Type != "" && c.Type != "native") || c.Designation != "recovery" {
		return false
	}
	username := false
	for _, f := range c.UserFields {
		if f == "username" {
			username = true
		} else if f != "email" {
			return false
		}
	}
	return username
}
func safeNativeRedirect(endpoint *url.URL, location string) bool {
	to, err := url.Parse(location)
	if err != nil || location == "" {
		return false
	}
	to = endpoint.ResolveReference(to)
	if to.Scheme != "https" || to.Host != endpoint.Host || to.User != nil || to.Path != endpoint.Path || to.EscapedPath() != endpoint.EscapedPath() || to.Fragment != "" {
		return false
	}
	q, err := url.ParseQuery(to.RawQuery)
	if err != nil || len(q) > 1 {
		return false
	}
	if len(q) == 1 {
		values, ok := q["query"]
		if !ok || len(values) != 1 || values[0] != "" {
			return false
		}
	}
	return true
}

// This client is deliberately independent of the authenticated API client.
// No headers, cookies, or session state are shared. Real transports use fresh
// connections to prevent implicit retries of GETs that can enter the EmailStage.
// Each action gets a fresh in-memory jar and makes at most one identification POST.
func (a *recoveryAPI) nativeRecovery(slug, name string, beforeWrite func() error) error {
	if !recoveryFlowSlug.MatchString(slug) || beforeWrite == nil {
		return errNativeRecovery
	}
	endpoint := *a.base
	endpoint.Path = "/api/v3/flows/executor/" + slug + "/"
	endpoint.RawQuery = "query="
	jar, err := cookiejar.New(nil)
	if err != nil {
		return errNativeRecovery
	}
	transport := a.client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if real, ok := transport.(*http.Transport); ok {
		cloned := real.Clone()
		cloned.DisableKeepAlives = true
		cloned.Protocols = new(http.Protocols)
		cloned.Protocols.SetHTTP1(true)
		cloned.ForceAttemptHTTP2 = false
		transport = cloned
	}
	client := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	redirects := 0
	exchange := func(method string, body any, advancing bool) (nativeChallenge, error) {
		for {
			if advancing {
				if e := beforeWrite(); e != nil {
					return nativeChallenge{}, e
				}
			}
			var b bytes.Buffer
			if body != nil {
				if json.NewEncoder(&b).Encode(body) != nil {
					return nativeChallenge{}, errNativeRecovery
				}
			}
			req, e := http.NewRequest(method, endpoint.String(), &b)
			if e != nil {
				return nativeChallenge{}, errNativeRecovery
			}
			req.Header.Set("Accept", "application/json")
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("User-Agent", "Knapp-AuthentikBot-Recovery/1.0")
			resp, e := client.Do(req)
			if e != nil {
				return nativeChallenge{}, errNativeRecovery
			}
			code := resp.StatusCode
			if code >= 300 && code <= 399 {
				location := resp.Header.Get("Location")
				resp.Body.Close()
				if (code != 301 && code != 302 && code != 303 && code != 307 && code != 308) || (method == "POST" && (code == 307 || code == 308)) || redirects >= 5 || !safeNativeRedirect(&endpoint, location) {
					return nativeChallenge{}, errNativeRecovery
				}
				redirects++
				method = "GET"
				body = nil
				continue
			}
			if code != http.StatusOK {
				resp.Body.Close()
				return nativeChallenge{}, errNativeRecovery
			}
			var c nativeChallenge
			dec := json.NewDecoder(io.LimitReader(resp.Body, 256<<10))
			e = dec.Decode(&c)
			if e == nil {
				var extra any
				if dec.Decode(&extra) != io.EOF {
					e = errNativeRecovery
				}
			}
			resp.Body.Close()
			if e != nil || bool(c.Errors) {
				return nativeChallenge{}, errNativeRecovery
			}
			if c.Component == "xak-flow-redirect" {
				if c.Final || redirects >= 5 || !safeNativeRedirect(&endpoint, c.To) {
					return nativeChallenge{}, errNativeRecovery
				}
				redirects++
				method = "GET"
				body = nil
				continue
			}
			return c, nil
		}
	}
	challenge, err := exchange("GET", nil, false)
	if err != nil {
		return err
	}
	if !challenge.identification() {
		return errNativeRecovery
	}
	challenge, err = exchange("POST", struct {
		Component string `json:"component"`
		UID       string `json:"uid_field"`
	}{"ak-stage-identification", name}, true)
	if err != nil {
		return err
	}
	if challenge.Component != "ak-stage-email" || challenge.To != "" || challenge.Final || bool(challenge.Captcha) || bool(challenge.Passkey) || (challenge.Type != "" && challenge.Type != "native") {
		return errNativeRecovery
	}
	return nil // Never POST to email, follow a link, or advance any further stage.
}
