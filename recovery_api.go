package main

// Recovery has its own HTTP client and never uses the legacy API helpers.
import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

var errRecoveryAPI = errors.New("Recovery API request failed; no automatic retry. An interrupted write may have taken effect; operator review is required.")

type recoveryUser struct {
	PK       int64  `json:"pk"`
	Username string `json:"username"`
	Email    string `json:"email"`
	Active   *bool  `json:"is_active"`
	Super    *bool  `json:"is_superuser"`
	Type     string `json:"type"`
	UUID     string `json:"uuid"`
	Updated  string `json:"last_updated"`
}

// bool pointers ensure omitted security fields fail closed.
func (u recoveryUser) ordinary() bool {
	return u.PK > 0 && u.Active != nil && *u.Active && u.Super != nil && !*u.Super && u.Type == "internal" && u.UUID != "" && u.Updated != ""
}
func (u recoveryUser) same(v recoveryUser) bool {
	return u.PK == v.PK && u.Username == v.Username && u.UUID == v.UUID && u.Email == v.Email && u.Updated == v.Updated && u.ordinary() && v.ordinary()
}

type recoveryAPI struct {
	base   *url.URL
	token  string
	client *http.Client
}

func newRecoveryAPI(base, token string) (*recoveryAPI, error) {
	u, err := url.Parse(base)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(token) == "" {
		return nil, errors.New("Recovery requires an HTTPS origin and a separate credential")
	}
	u.Path = ""
	return &recoveryAPI{base: u, token: token, client: &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

// Paths are constructed internally; pagination never follows server-supplied URLs.
func (a *recoveryAPI) request(method, path string, q url.Values, body, out any) error {
	if !strings.HasPrefix(path, "/api/v3/") || strings.ContainsAny(path, "?#%\\") || strings.Contains(path, "..") {
		return errRecoveryAPI
	}
	u := *a.base
	u.Path = path
	u.RawQuery = q.Encode()
	var b bytes.Buffer
	if body != nil {
		if json.NewEncoder(&b).Encode(body) != nil {
			return errRecoveryAPI
		}
	}
	req, err := http.NewRequest(method, u.String(), &b)
	if err != nil {
		return errRecoveryAPI
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "Knapp-AuthentikBot-Recovery/1.0")
	resp, err := a.client.Do(req)
	if err != nil {
		return errRecoveryAPI
	}
	defer resp.Body.Close()
	want := http.StatusOK
	if method != http.MethodGet {
		want = http.StatusNoContent
	}
	if resp.StatusCode != want {
		return errRecoveryAPI
	} // Never read or log error bodies.
	if out != nil {
		dec := json.NewDecoder(io.LimitReader(resp.Body, 2<<20))
		if dec.Decode(out) != nil {
			return errRecoveryAPI
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return errRecoveryAPI
		}
	}
	return nil
}

type recoveryPage[T any] struct {
	Results    []T `json:"results"`
	Pagination *struct {
		Next    int `json:"next"`
		Current int `json:"current"`
		Total   int `json:"total_pages"`
		Count   int `json:"count"`
	} `json:"pagination"`
}

func recoveryList[T any](a *recoveryAPI, path string, q url.Values) ([]T, error) {
	if q == nil {
		q = url.Values{}
	}
	all := []T{}
	for page := 1; page <= 100; page++ {
		q.Set("page", strconv.Itoa(page))
		q.Set("page_size", "100")
		var data recoveryPage[T]
		if err := a.request("GET", path, q, nil, &data); err != nil {
			return nil, err
		}
		p := data.Pagination
		if data.Results == nil || p == nil || p.Current != page || p.Total < page || p.Total > 100 || p.Count < 0 {
			return nil, errRecoveryAPI
		}
		all = append(all, data.Results...)
		if p.Next == 0 {
			if p.Total != page || p.Count != len(all) {
				return nil, errRecoveryAPI
			}
			return all, nil
		}
		if p.Next != page+1 || p.Next > p.Total {
			return nil, errRecoveryAPI
		}
	}
	return nil, errRecoveryAPI
}
func (a *recoveryAPI) identity(expected string) (int64, error) {
	var data struct {
		User     recoveryUser    `json:"user"`
		Original json.RawMessage `json:"original"`
	}
	if err := a.request("GET", "/api/v3/core/users/me/", nil, nil, &data); err != nil {
		return 0, err
	}
	u := data.User
	if u.PK <= 0 || u.Username != expected || u.Active == nil || !*u.Active || u.Super == nil || *u.Super || u.Type != "service_account" || (len(data.Original) > 0 && string(data.Original) != "null") {
		return 0, errors.New("Recovery credential identity rejected")
	}
	return u.PK, nil
}
func (a *recoveryAPI) exactUser(name string) (recoveryUser, error) {
	users, err := recoveryList[recoveryUser](a, "/api/v3/core/users/", url.Values{"username": {name}, "include_groups": {"false"}, "include_roles": {"false"}})
	if err != nil {
		return recoveryUser{}, err
	}
	var found recoveryUser
	count := 0
	for _, u := range users {
		if u.Username == name {
			found = u
			count++
		}
	}
	if count != 1 {
		return recoveryUser{}, errors.New("Target must resolve to exactly one exact username")
	}
	return found, nil
}
func (a *recoveryAPI) emailReady(flow, stage string, u recoveryUser) error {
	if strings.TrimSpace(u.Email) == "" {
		return errors.New("Target has no email address; nothing queued")
	}
	var brand struct {
		Recovery string `json:"flow_recovery"`
	}
	if err := a.request("GET", "/api/v3/core/brands/current/", nil, nil, &brand); err != nil {
		return err
	}
	if brand.Recovery == "" || brand.Recovery != flow {
		return errors.New("Current brand recovery flow missing or differs from configured flow; nothing queued")
	}
	var emailStage struct {
		PK string `json:"pk"`
	}
	if err := a.request("GET", "/api/v3/stages/email/"+stage+"/", nil, nil, &emailStage); err != nil {
		return errors.New("Configured email stage unavailable or not viewable; nothing queued")
	}
	if emailStage.PK != stage {
		return errors.New("Configured email stage missing; nothing queued")
	}
	return nil
}

// The generic admin/all endpoint is intentionally used for enumeration. In
// 2026.8.5 the static list/retrieve serializers expose token_set; never call them.
// admin/all is unpaginated, supports an exact validated user PK, and returns
// metadata only (including type, confirmed and last_updated).
type recoveryDevice struct {
	PK        string `json:"pk"`
	Name      string `json:"name"`
	Type      string `json:"type"`
	Confirmed *bool  `json:"confirmed"`
	Created   string `json:"created"`
	Updated   string `json:"last_updated"`
	LastUsed  string `json:"last_used"`
	Kind      string `json:"-"`
}

var recoveryDeviceModels = map[string]string{
	"authentik_stages_authenticator_totp.TOTPDevice":         "totp",
	"authentik_stages_authenticator_webauthn.WebAuthnDevice": "webauthn",
	"authentik_stages_authenticator_static.StaticDevice":     "static",
	"authentik_stages_authenticator_email.EmailDevice":       "email",
	"authentik_stages_authenticator_sms.SMSDevice":           "sms",
	"authentik_stages_authenticator_duo.DuoDevice":           "duo",
}

func factorPath(kind string) string { return "/api/v3/authenticators/admin/" + kind + "/" }
func (a *recoveryAPI) devices(pk int64) ([]recoveryDevice, error) {
	var all []recoveryDevice
	if err := a.request("GET", "/api/v3/authenticators/admin/all/", url.Values{"user": {strconv.FormatInt(pk, 10)}}, nil, &all); err != nil {
		return nil, err
	}
	if all == nil || len(all) > 10000 {
		return nil, errRecoveryAPI
	}
	seen := map[string]bool{}
	for i := range all {
		d := &all[i]
		d.Kind = recoveryDeviceModels[d.Type]
		id, err := strconv.ParseInt(d.PK, 10, 64)
		if d.Kind == "" || err != nil || id <= 0 || strconv.FormatInt(id, 10) != d.PK || d.Confirmed == nil || d.Created == "" || d.Updated == "" {
			return nil, errors.New("Unknown or invalid enrollment metadata; no reset permitted")
		}
		key := d.Kind + ":" + d.PK
		if seen[key] {
			return nil, errors.New("Duplicate enrollment metadata")
		}
		seen[key] = true
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Kind != all[j].Kind {
			return all[i].Kind < all[j].Kind
		}
		return all[i].PK < all[j].PK
	})
	return all, nil
}
func sameDevices(a, b []recoveryDevice) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		x, y := a[i], b[i]
		if x.PK != y.PK || x.Name != y.Name || x.Type != y.Type || x.Kind != y.Kind || x.Created != y.Created || x.Updated != y.Updated || x.LastUsed != y.LastUsed || x.Confirmed == nil || y.Confirmed == nil || *x.Confirmed != *y.Confirmed {
			return false
		}
	}
	return true
}
