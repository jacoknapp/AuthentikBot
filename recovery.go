package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const recoveryTTL = 2 * time.Minute
const recoveryCooldown = 5 * time.Minute
const recoveryCapacity = 128
const emailAction = "password-reset-email"
const mfaAction = "mfa-reset"

var recovery *recoveryService
var discordID = regexp.MustCompile(`^[0-9]{1,20}$`)
var supportedFactors = map[string]bool{"totp": true, "webauthn": true, "static": true, "email": true, "sms": true, "duo": true}

type recoveryConfig struct {
	EmailEnabled, MFAEnabled                                            bool
	Token, ServiceUsername, Guild, Channel, EmailStage, Flow, AuditPath string
	Callers, Targets, Immutable                                         map[string]bool
	Factors                                                             []string
}

func recoveryCSV(s string) map[string]bool {
	m := map[string]bool{}
	for _, v := range strings.Split(s, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			m[v] = true
		}
	}
	return m
}
func readRecoveryConfig(legacy Config, env func(string) string) (recoveryConfig, error) {
	c := recoveryConfig{Channel: legacy.CommandChannelID, EmailStage: env("RECOVERY_EMAIL_STAGE_UUID"), Factors: []string{"totp"}}
	for key, dst := range map[string]*bool{"RECOVERY_EMAIL_ENABLED": &c.EmailEnabled, "RECOVERY_MFA_ENABLED": &c.MFAEnabled} {
		switch env(key) {
		case "", "false":
		case "true":
			*dst = true
		default:
			return c, errors.New("Recovery switches must be true or false")
		}
	}
	if c.EmailStage == "" {
		c.EmailStage = "5780da44-a43d-4791-b14e-e880cc8aa895"
	}
	c.Token = env("RECOVERY_AUTHENTIK_TOKEN")
	c.ServiceUsername = env("RECOVERY_SERVICE_USERNAME")
	c.Guild = env("RECOVERY_GUILD_ID")
	c.Flow = env("RECOVERY_FLOW_SLUG")
	c.AuditPath = env("RECOVERY_AUDIT_PATH")
	c.Callers = recoveryCSV(env("RECOVERY_DISCORD_USER_IDS"))
	c.Targets = recoveryCSV(env("RECOVERY_TARGET_USERNAMES"))
	c.Immutable = map[string]bool{}
	for _, v := range legacy.ImmutableUsernames {
		c.Immutable[strings.ToLower(strings.TrimSpace(v))] = true
	}
	if raw := env("RECOVERY_MFA_FACTOR_TYPES"); raw != "" {
		m := recoveryCSV(raw)
		c.Factors = nil
		for k := range m {
			if !supportedFactors[k] {
				return c, errors.New("Unknown recovery factor type")
			}
			c.Factors = append(c.Factors, k)
		}
		sort.Strings(c.Factors)
	}
	if !c.EmailEnabled && !c.MFAEnabled {
		return c, nil
	}
	if c.Token == "" || strings.TrimSpace(c.Token) == strings.TrimSpace(legacy.AuthentikToken) || strings.TrimSpace(c.ServiceUsername) == "" || !discordID.MatchString(c.Guild) || !discordID.MatchString(c.Channel) || len(c.Callers) == 0 || len(c.Callers) > 128 || len(c.Targets) == 0 || len(c.Targets) > 128 || !filepath.IsAbs(c.AuditPath) || len(c.Factors) == 0 {
		return c, errors.New("Required recovery authorization configuration missing")
	}
	for id := range c.Callers {
		if !discordID.MatchString(id) {
			return c, errors.New("Invalid recovery caller ID")
		}
	}
	for name := range c.Targets {
		if !validRecoveryName(name) || c.Immutable[strings.ToLower(name)] || name == c.ServiceUsername {
			return c, errors.New("Invalid or protected recovery target")
		}
	}
	if c.EmailEnabled {
		if _, err := uuid.Parse(c.EmailStage); err != nil || strings.TrimSpace(c.Flow) == "" {
			return c, errors.New("Recovery email stage UUID and flow slug required")
		}
	}
	return c, nil
}
func validRecoveryName(name string) bool {
	return name != "" && len(name) <= 150 && name == strings.TrimSpace(name) && !strings.ContainsAny(name, "*?\r\n\x00")
}

type recoveryContext struct{ Caller, Channel, Guild string }
type recoveryAuditEvent struct {
	Timestamp string `json:"timestamp"`
	Action    string `json:"action"`
	Caller    string `json:"caller_id"`
	Target    int64  `json:"target_id"`
	Status    string `json:"status"`
	Deleted   int    `json:"deleted_count,omitempty"`
}
type recoveryAudit interface {
	Write(recoveryAuditEvent) error
	Close() error
}
type fileRecoveryAudit struct {
	mu     sync.Mutex
	f      *os.File
	failed bool
}

func openRecoveryAudit(path string) (*fileRecoveryAudit, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("Audit path must be absolute")
	}
	// Explicit file, private mode, no symlinks. Sync every event before proceeding.
	fd, err := syscall.Open(path, syscall.O_APPEND|syscall.O_CREAT|syscall.O_WRONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, errors.New("Audit file unavailable")
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("Audit file must be private and regular")
	}
	a := &fileRecoveryAudit{f: f}
	if err = a.Write(recoveryAuditEvent{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Action: "recovery", Status: "startup"}); err != nil {
		f.Close()
		return nil, err
	}
	return a, nil
}
func (a *fileRecoveryAudit) Write(e recoveryAuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failed {
		return errors.New("Recovery audit unavailable")
	}
	b, err := json.Marshal(e)
	if err == nil {
		_, err = a.f.Write(append(b, '\n'))
	}
	if err == nil {
		err = a.f.Sync()
	}
	if err != nil {
		a.failed = true
		return errors.New("Recovery audit unavailable")
	}
	return nil
}
func (a *fileRecoveryAudit) Close() error { return a.f.Close() }

type recoveryPending struct {
	Snapshot  []recoveryDevice
	Context   recoveryContext
	Action    string
	User      recoveryUser
	Devices   []recoveryDevice
	Expires   time.Time
	ServiceID int64
}
type recoveryGate struct {
	Until time.Time
	Busy  bool
}
type recoveryService struct {
	cfg     recoveryConfig
	api     *recoveryAPI
	audit   recoveryAudit
	now     func() time.Time
	mu      sync.Mutex
	pending map[string]recoveryPending
	gates   map[string]recoveryGate
	callers map[string]time.Time
}

func newRecoveryFromEnv(legacy Config) (*recoveryService, error) {
	cfg, err := readRecoveryConfig(legacy, os.Getenv)
	if err != nil {
		return nil, err
	}
	if !cfg.EmailEnabled && !cfg.MFAEnabled {
		return nil, nil
	}
	api, err := newRecoveryAPI(legacy.AuthentikURL, cfg.Token)
	if err != nil {
		return nil, err
	}
	audit, err := openRecoveryAudit(cfg.AuditPath)
	if err != nil {
		return nil, err
	}
	return newRecoveryService(cfg, api, audit), nil
}
func newRecoveryService(c recoveryConfig, a *recoveryAPI, audit recoveryAudit) *recoveryService {
	return &recoveryService{cfg: c, api: a, audit: audit, now: time.Now, pending: map[string]recoveryPending{}, gates: map[string]recoveryGate{}, callers: map[string]time.Time{}}
}
func (r *recoveryService) allowed(c recoveryContext, action string) bool {
	return r != nil && r.audit != nil && ((action == emailAction && r.cfg.EmailEnabled) || (action == mfaAction && r.cfg.MFAEnabled)) && c.Caller != "" && r.cfg.Callers[c.Caller] && r.cfg.Guild != "" && c.Guild == r.cfg.Guild && r.cfg.Channel != "" && c.Channel == r.cfg.Channel
}
func (r *recoveryService) record(c recoveryContext, action string, target int64, status string, deleted int) error {
	if r.audit == nil {
		return errors.New("Recovery audit unavailable")
	}
	return r.audit.Write(recoveryAuditEvent{Timestamp: r.now().UTC().Format(time.RFC3339Nano), Action: action, Caller: c.Caller, Target: target, Status: status, Deleted: deleted})
}
func (r *recoveryService) eligible(u recoveryUser, serviceID int64) bool {
	return u.ordinary() && r.cfg.Targets[u.Username] && !r.cfg.Immutable[strings.ToLower(u.Username)] && u.PK != serviceID && u.Username != r.cfg.ServiceUsername
}
func (r *recoveryService) validate(name string) (recoveryUser, int64, error) {
	id, err := r.api.identity(r.cfg.ServiceUsername)
	if err != nil {
		return recoveryUser{}, 0, err
	}
	u, err := r.api.exactUser(name)
	if err != nil {
		return u, 0, err
	}
	if !r.eligible(u, id) {
		return u, 0, errors.New("Recovery target is protected or ineligible")
	}
	return u, id, nil
}
func (r *recoveryService) reserve(c recoveryContext, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	for id, p := range r.pending {
		if !now.Before(p.Expires) {
			delete(r.pending, id)
			g := r.gates[p.User.Username]
			g.Busy = false
			r.gates[p.User.Username] = g
		}
	}
	for name, g := range r.gates {
		if !g.Busy && !now.Before(g.Until) {
			delete(r.gates, name)
		}
	}
	if now.Before(r.callers[c.Caller]) {
		return errors.New("Recovery caller rate limit; wait before trying again")
	}
	r.callers[c.Caller] = now.Add(5 * time.Second)
	if len(r.pending) >= recoveryCapacity || len(r.gates) >= recoveryCapacity {
		return errors.New("Recovery pending capacity reached")
	}
	if g, ok := r.gates[name]; ok && (g.Busy || now.Before(g.Until)) {
		return errors.New("Recovery target busy or on five-minute cooldown")
	}
	r.gates[name] = recoveryGate{Until: now.Add(recoveryCooldown), Busy: true}
	return nil
}
func (r *recoveryService) release(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gates[name]
	g.Busy = false
	r.gates[name] = g
}
func (r *recoveryService) preview(c recoveryContext, action, name string) (string, recoveryPending, error) {
	p := recoveryPending{Context: c, Action: action}
	if err := r.record(c, action, 0, "attempt", 0); err != nil {
		return "", p, err
	}
	deny := func(err error) (string, recoveryPending, error) {
		if e := r.record(c, action, p.User.PK, "denied", 0); e != nil {
			err = e
		}
		return "", p, err
	}
	if !r.allowed(c, action) || !validRecoveryName(name) || !r.cfg.Targets[name] {
		return deny(errors.New("Recovery disabled or unauthorized"))
	}
	if err := r.reserve(c, name); err != nil {
		return deny(err)
	}
	retained := false
	defer func() {
		if !retained {
			r.release(name)
		}
	}()
	u, id, err := r.validate(name)
	p.User = u
	if err != nil {
		return deny(err)
	}
	p.User = u
	p.ServiceID = id
	if action == emailAction {
		err = r.api.emailReady(r.cfg.Flow, r.cfg.EmailStage, u)
	} else {
		p.Snapshot, err = r.api.devices(u.PK)
		for _, d := range p.Snapshot {
			for _, kind := range r.cfg.Factors {
				if d.Kind == kind {
					p.Devices = append(p.Devices, d)
				}
			}
		}
		if err == nil && len(p.Devices) == 0 {
			err = errors.New("No enrollments in configured factor scope")
		}
	}
	if err != nil {
		return deny(err)
	}
	var raw [24]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return deny(errors.New("Confirmation unavailable"))
	}
	idString := base64.RawURLEncoding.EncodeToString(raw[:])
	p.Expires = r.now().Add(recoveryTTL)
	if err = r.record(c, action, u.PK, "preview", 0); err != nil {
		return "", p, err
	}
	r.mu.Lock()
	r.pending[idString] = p
	r.mu.Unlock()
	retained = true
	return idString, p, nil
}
func (r *recoveryService) claim(c recoveryContext, id string, cancel bool) (recoveryPending, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, ok := r.pending[id]
	if !ok {
		return p, errors.New("Confirmation expired, consumed or unavailable")
	}
	if p.Context != c || !r.allowed(c, p.Action) {
		return recoveryPending{}, errors.New("Recovery confirmation unauthorized")
	}
	delete(r.pending, id) // Atomic, single use, including expiry and cancel.
	if cancel || !r.now().Before(p.Expires) {
		g := r.gates[p.User.Username]
		g.Busy = false
		r.gates[p.User.Username] = g
	}
	if !r.now().Before(p.Expires) {
		return p, errors.New("Confirmation expired")
	}
	return p, nil
}
func (r *recoveryService) confirm(c recoveryContext, id string, cancel bool) (string, error) {
	p, err := r.claim(c, id, cancel)
	if err != nil {
		if p.Action == "" {
			p.Action = "recovery-confirmation"
		}
		if e := r.record(c, p.Action, p.User.PK, "denied", 0); e != nil {
			err = e
		}
		return "", err
	}
	defer r.release(p.User.Username)
	if cancel {
		return "Recovery cancelled; no changes requested.", r.record(c, p.Action, p.User.PK, "cancelled", 0)
	}
	done := 0
	fail := func(e error) (string, error) {
		status := "denied"
		if done > 0 {
			status = "partial"
		}
		if auditErr := r.record(c, p.Action, p.User.PK, status, done); auditErr != nil {
			e = auditErr
		}
		if p.Action == emailAction {
			return "Email request stopped. No automatic retry; an interrupted request may have queued email. Operator review is required.", e
		}
		return fmt.Sprintf("Stopped: %d confirmed enrollment deletions of %d. No automatic retry. Review any interrupted request before retrying.", done, len(p.Devices)), e
	}
	if err = r.record(c, p.Action, p.User.PK, "confirm", 0); err != nil {
		return fail(err)
	}
	check := func() error {
		if !r.now().Before(p.Expires) {
			return errors.New("Confirmation expired during revalidation")
		}
		if !r.allowed(c, p.Action) {
			return errors.New("Recovery disabled or unauthorized")
		}
		u, id, e := r.validate(p.User.Username)
		if e != nil {
			return e
		}
		if !r.now().Before(p.Expires) {
			return errors.New("Confirmation expired during revalidation")
		}
		if id != p.ServiceID || !p.User.same(u) {
			return errors.New("Target or credential identity changed; request a fresh preview")
		}
		return nil
	}
	if err = check(); err != nil {
		return fail(err)
	}
	if p.Action == emailAction {
		if err = r.api.emailReady(r.cfg.Flow, r.cfg.EmailStage, p.User); err != nil {
			return fail(err)
		}
		if err = check(); err != nil {
			return fail(err)
		}
		if err = r.record(c, p.Action, p.User.PK, "write_attempt", 0); err != nil {
			return fail(err)
		}
		err = r.api.request("POST", fmt.Sprintf("/api/v3/core/users/%d/recovery_email/", p.User.PK), nil, map[string]string{"email_stage": r.cfg.EmailStage}, nil)
		if err != nil {
			return fail(err)
		}
		msg := "Password recovery email accepted as queued; delivery is not guaranteed."
		if err = r.record(c, p.Action, p.User.PK, "success", 0); err != nil {
			return msg, err
		}
		return msg, nil
	}
	remaining := append([]recoveryDevice(nil), p.Devices...)
	snapshot := append([]recoveryDevice(nil), p.Snapshot...)
	for len(remaining) > 0 {
		if err = check(); err != nil {
			return fail(err)
		}
		current, e := r.api.devices(p.User.PK)
		if e != nil {
			return fail(e)
		}
		if !sameDevices(current, snapshot) {
			return fail(errors.New("Enrollment snapshot changed; request a fresh preview"))
		}
		if err = check(); err != nil {
			return fail(err)
		}
		if err = r.record(c, p.Action, p.User.PK, "write_attempt", done); err != nil {
			return fail(err)
		}
		d := remaining[0]
		if err = r.api.request("DELETE", fmt.Sprintf("%s%s/", factorPath(d.Kind), d.PK), nil, nil, nil); err != nil {
			return fail(err)
		}
		done++
		for j, device := range snapshot {
			if device.Kind == d.Kind && device.PK == d.PK {
				snapshot = append(snapshot[:j], snapshot[j+1:]...)
				break
			}
		}
		remaining = remaining[1:]
	}
	msg := fmt.Sprintf("Deleted %d Authentik enrollments in scope %s. Mandatory MFA policies still apply; Duo upstream is unchanged.", done, strings.Join(r.cfg.Factors, ", "))
	if err = r.record(c, p.Action, p.User.PK, "success", done); err != nil {
		return msg, err
	}
	return msg, nil
}
