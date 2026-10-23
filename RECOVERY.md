# Native recovery email and TOTP reset — source review

This implementation follows the revised October 23 scope. The parent owns
credentials, Authentik settings, publication, build, activation and deployment.
the operator subsequently explicitly approved reusing the existing bot superuser credential.
The deployed choice requires explicit opt-in and a pinned account ID; no new
credential or Vault change is needed. Admin exclusion remains bot-enforced.

This source task makes no live recovery requests, sends no email, changes no
accounts or login/recovery flows, and deletes no real enrollment. Mock HTTP and
Discord transports run entirely in memory. Both features remain off by default.

## Commands and controls

- `/authctl password action:reset user:<Authentik account>`: prevalidate a human account,
  obtain an ephemeral confirmation, then initiate the configured native anonymous
  recovery flow with that username and stop at `ak-stage-email`.
- `/authctl mfa action:reset user:<Authentik account>`: preview the exact TOTP enrollment count,
  confirm, and delete only those TOTP IDs. Other enrollment types are preserved.

`action` is a required choice field; `reset` is its only permitted value.

The `user` field provides searchable Authentik username suggestions for eligible
internal and external human accounts. Only authorized recovery callers in the
configured guild/channel can receive them. This is a STRING autocomplete field,
not a Discord member picker; `action` remains the single static `reset` choice.
A background scan reuses the full account and effective-permission checks and
caches only usernames and stable IDs in memory. Its maximum age is **90 seconds,
measured from the start of the scan**. One refresh runs at a time with at most
four concurrent checks, a 40-second timeout and a five-second pause after each
attempt. Failed refreshes clear the snapshot; startup and expired caches return
no suggestions. API authentication/permission denials stop background refresh
until restart; they are not retried automatically. Request traffic never triggers
refresh storms or waits on HTTP.
An account promoted or deleted after a scan can briefly remain visible, but the
cache never authorizes recovery: preview, confirmation and execution still check
caller/target eligibility afresh. This suggestion-only staleness is intentional.
Discord permits at most 25 suggestions: type part of a username to search the
entire snapshot, including accounts beyond the initial results. Inventory reads
retain the existing fail-closed pagination bounds (100 pages/10,000 records).
No email addresses, credentials or factor data are cached or shown. Long names
use a shortened label plus account ID; selections resolve the canonical identity
again before preview. Plain exact username entry remains supported. Operation
prerequisites (for example, a TOTP enrollment) are still checked during preview;
suggestions perform no reset.


Only configured explicit Discord callers in the exact configured guild and the
existing exact `COMMAND_CHANNEL_ID` may preview, confirm, or cancel. DMs and missing
configuration deny access. Recovery does not inherit the old channel-member
fallback or optional legacy `REQUIRED_ROLE_ID` behavior. Command registration
limits context to guilds and defaults member permissions to zero; Discord's own
visibility settings never bypass the bot's caller check.

Confirmation is ephemeral, opaque, single-use, bound to caller/channel/guild,
action, target PK/UUID, user metadata and permission/enrollment snapshots, and
expires after two minutes. Claims are atomic. Preview creates no flow session or
email. Target/caller authorization is revalidated at confirmation and immediately
before each mutation-capable request. There is a five-second per-caller preview
rate limit, five-minute target cooldown including failed/cancelled previews, one
pending/executing request per target username across both commands, and a
128-target pending/cooldown capacity. Expired entries are pruned. Mentions are
disabled in every recovery response.

Run one bot instance. Pending state/cooldowns are in memory, reset on restart,
and are not coordinated across replicas. An old button remains unusable after
consumption, expiry, restart, or feature disablement.

## Environment and defaults

| Variable | Default | Enabled-feature requirement |
| --- | --- | --- |
| `RECOVERY_EMAIL_ENABLED` | `false` | Literal `true` registers/enables native email initiation |
| `RECOVERY_MFA_ENABLED` | `false` | Literal `true` registers/enables TOTP deletion |
| `RECOVERY_USE_EXISTING_AUTHENTIK_TOKEN` | `false` | Explicit `true` reuses the existing `AUTHENTIK_TOKEN` in memory |
| `RECOVERY_EXPECTED_USER_ID` | unset | Positive pinned Authentik user ID; mandatory in existing-credential mode |
| `RECOVERY_AUTHENTIK_TOKEN` | unset | Separate credential in default mode; must be absent when reusing existing credentials |
| `RECOVERY_SERVICE_USERNAME` | unset | Exact expected identity: active internal superuser for explicit reuse, non-superuser `service_account` otherwise |
| `RECOVERY_GUILD_ID` | unset | Exact guild ID |
| `RECOVERY_DISCORD_USER_IDS` | unset | Explicit comma-separated Discord user IDs; maximum 128 |
| `RECOVERY_FLOW_SLUG` | unset | Explicit pinned recovery slug; required for native email; letters/digits/underscore/hyphen only |
| `RECOVERY_MFA_FACTOR_TYPES` | `totp` | Compatibility setting; only `totp` is accepted; other values rejected |
| `RECOVERY_AUDIT_PATH` | unset | Absolute path to private regular durable JSONL audit file |
| `COMMAND_CHANNEL_ID` | existing required value | Same exact existing channel lock, independently enforced |
| `AUTHENTIK_URL` | existing required value | HTTPS origin only; no path, query, fragment or embedded credentials |
| `IMMUTABLE_USERNAMES` | existing value | Case-insensitive exclusions also apply to recovery |

`RECOVERY_TARGET_USERNAMES` and `RECOVERY_EMAIL_STAGE_UUID` are obsolete and no
longer read. There is no target allowlist and no caller-selected factor, stage,
flow, destination email, query, URL, token, next parameter or context override.
Missing/invalid enabled-feature configuration or unavailable audit setup stops
startup before connecting to Discord. Legacy production startup requirements
are preserved; tests no longer depend on package `init` reading credentials.

Caller/application identities and guild/channel identifiers must be resolved and configured privately for each installation.

The repository's sample Compose explicitly passes legacy variables only. New
values in a local `.env` do not automatically reach that sample service. The
verified production service uses `<private-deployment-path>` through `env_file`.
No Compose or credential files are changed here.

## Dynamic account eligibility

Exact, uniquely resolved usernames are required. Active `internal` and `external`
human accounts can qualify now or after creation without bot configuration or
per-user grants. Exclusions include superusers, the service identity, service and
internal-service types, immutable names, reserved anonymous/system usernames,
unknown types, and users in Authentik's reserved `goauthentik.io` path hierarchy.
Missing active/superuser/email/path/UUID/update metadata fails closed. PK, UUID,
username, account type, email, path and last-updated metadata remain bound to the
preview. `is_superuser` already includes ancestor-group superuser membership.

Administrative capability is determined from effective grants, never role/group
names. The bot reads every paginated effective role with
`GET /api/v3/rbac/roles/?users=<pk>&inherited=true`, including direct, group and
ancestor-group roles. For each role UUID it reads:

- `GET /api/v3/rbac/permissions/?role=<uuid>` for global/model permissions;
- `GET /api/v3/rbac/permissions/roles/?uuid=<uuid>&page_size=10` for object grants.

The conservative ordinary set is exactly `authentik_core.view_application` on
model `application` and `authentik_providers_ldap.search_full_directory` on model
`ldapprovider`. Every other returned explicit grant protects the target,
including admin interface access, user/group/role/token/MFA/flow/provider/policy
management and unknown/custom grants. Accounts with no effective roles can be ordinary. Missing,
malformed, duplicate, unreadable or inconsistent metadata denies access.

Snapshots include role UUIDs and every returned permission assignment ID,
app/model/codename and global/object scope. Two consecutive full scans must
agree, with role IDs rechecked after collecting grants. Fingerprints must still
match preview at confirmation and before each native submission or TOTP delete;
even an ordinary role/scope change requires a fresh preview. Pagination never
follows a response URL, validates all required envelope fields and stable
count/page totals, and has 100-page/10,000-record bounds. Exceeding a bound denies.

**Global grant completeness cross-check:** official 2026.8.5
`RBACPermissionViewSet.get_queryset()` filters app labels to those starting with
`authentik`. The catalog alone could hide a global `guardian.*` or third-party
grant. For each role, the bot additionally queries the read-only
`GET /api/v3/rbac/permissions/assigned_by_roles/?model=<ordinary app.model>`
(and `object_pk` when anchored on an object grant). The model/object filter
selects roles; its serializer returns **all** nested global/model and object
assignments for each selected role. The target role must be present with both
complete arrays. Unknown hidden grants, scope disagreements, duplicates or
changed assignment IDs deny access. No role/permission mutation endpoint is used.
This endpoint requires the already proposed global role visibility, not additional
write privileges. The code and mocks verify the supplemental serializer contract;
parent still owns live read-only RBAC qualification with the new credential.

**Empty roles with verified full visibility:** in explicit existing-credential mode,
each validation requires the pinned identity to remain an active internal
superuser before and after the target scan. Only that proof allows an apparently
empty role to be checked through `GET /rbac/roles/<uuid>/used_by/`. The deployed
source filters references by read permissions; a superuser sees all guardian
assignment references. Any guardian reference, malformed/duplicate metadata or
read failure denies the target. A truly empty role can qualify. Non-superuser
separate-credential mode retains conservative denial for unanchored roles;
accounts with no effective roles qualify in either mode. No roles are altered.

**External promotion race:** these APIs do not offer a transaction/precondition
spanning reads, native identification and deletion. The bot detects observed
promotion/role changes and serializes its own requests; another administrator
can still change account/grants or identifier collisions after the last read. No atomic server guarantee
is claimed. A stolen TOTP-delete token can bypass application-level exclusion.

## Native email protocol and ambiguity protection

The authenticated recovery API client is used only for read-only eligibility and
TOTP deletion. It rejects other mutation routes, including password-change and
privileged recovery routes. Recovery never calls the legacy invite/group/user
helpers or their raw-body error paths.

For each confirmed email action a separate anonymous HTTP client and fresh
in-memory cookie jar are created. Cookies persist only inside that action,
never on disk or across users. No Authorization/Bearer header is sent. Native
requests retain `Accept: application/json` and the exact stable `query=` value.
The configured HTTPS origin and exact executor path are pinned:

1. `GET /api/v3/flows/executor/<RECOVERY_FLOW_SLUG>/?query=`.
2. Require `ak-stage-identification`, explicit `password_fields=false`, username
   support and recovery designation, no CAPTCHA/passkey requirement, no validation
   error or prior pending
   identity. No unsupported challenge is answered.
3. Renew credential/target/grant checks and identifier uniqueness, audit the
   write attempt, then make exactly one POST with only
   `{"component":"ak-stage-identification","uid_field":"<exact username>"}`.
4. Follow at most five same-origin, same-exact-executor-path redirects with cookies,
   Accept and `query=` preserved. Only an omitted query or a single empty `query`
   is permitted on redirect; all other query parameters, paths, hosts, fragments,
   userinfo or HTTPS downgrades stop the request. HTTP 307/308 after POST are
   rejected to prevent identification replay. Same-path nonterminal redirect
   challenges can be followed; terminal flow redirects are refused.
5. Stop at `ak-stage-email`. Never POST to it, retry a challenge, respond to a
   password/MFA/setup stage, follow/consume an email reset link, or complete the
   flow. Real native transports disable connection reuse to avoid implicit replay
   of a stateful GET. Uncertain requests are never automatically retried.

Native identification matches username OR email case-insensitively and takes
the first result. Before submission, all paginated visible user metadata is
scanned without excluding inactive or administrative accounts. Full Unicode case-folded
username/email collisions, case variants, mismatched PK/UUID or incomplete
metadata deny initiation; both fields matching the same exact target is allowed.
Identifier folding uses Go simple-fold tables plus pinned Unicode full-fold
expansion data in `recovery_casefold.go` (generated from installed Python Unicode
14.0.0 data), with conservative upper/lower equivalents; it requires no new
module or runtime data download. The selected target must have an existing
email. No destination override is sent.
This matters because unknown identifiers can intentionally receive an email-sent
challenge without a real email being sent.

Success says **native recovery request submitted; inbox delivery is not guaranteed**.
Errors/redirects/unexpected stages stop safely without response bodies, cookies,
links or transport error strings in Discord/logs. A request may have sent email
before a response is lost; operator review is required before trying again.

No `reset_user_password` privilege, email-stage-view privilege or brand recovery
assignment is needed for native initiation. `flow_recovery=null` on the current
brand does not block this pinned public executor path. Source inspection and
mock protocol verification do not establish inbox delivery or live flow success.

## TOTP reset and verified re-enrollment limitation

Only metadata from `GET /api/v3/authenticators/admin/all/?user=<exact pk>` is
read: PK, model type, name, confirmed, created/last-updated/last-used. No per-factor
list/retrieve is used; the static-device serializer can expose recovery codes.
All visible metadata is snapshotted; unknown/duplicate/malformed records deny.
Only previewed TOTP IDs can be deleted using
`DELETE /api/v3/authenticators/admin/totp/<id>/`, after renewed identity, eligibility,
permission fingerprint and enrollment snapshot checks before each deletion.
A 204 increments the acknowledged deletion count. Failure stops further work,
reports partial/uncertain results truthfully, and never retries or restores devices.

The proposed token has only TOTP device view/delete permissions. Other factors
may be invisible to `admin/all`; complete cross-factor visibility is not claimed.
TOTP snapshot freshness is the guaranteed scope. Passkeys, static codes, email,
SMS, Duo, user attributes/state and existing sessions are never reset or revoked.

**Verified re-enrollment gap is intentionally preserved:** the normal login flow
has identification 10, password 20, MFA validation 30, login 100. Its validation
accepts static/TOTP/WebAuthn/Duo/SMS and configures the existing TOTP setup stage
only if no compatible factor remains (`not_configured_action=configure`, fresh
validation threshold zero). Deleting the only TOTP factor can therefore force
fresh TOTP setup, but a retained passkey can satisfy validation without TOTP setup.
The passwordless flow performs WebAuthn validation at 10 and login at 20, with no
TOTP guard. Existing sessions are not explicitly revoked. the operator has been told
this limitation; the source update preserves that behavior. No reset marker,
`change_user` right, mandatory global TOTP requirement or unrelated login change
is introduced. Selective re-enrollment design would need further exact approval.

## Existing-credential deployment

Existing-credential reuse requires explicit operator approval, the configured service username and pinned positive user ID. Supply these installation-specific values privately; keep a separate credential unset in reuse mode. Reusing an active superuser retains its broad authority, and bot target checks cannot constrain a stolen credential. Native recovery never handles passwords.

## Optional separate credential and parent-owned Authentik settings

Alternative default mode (not the approved deployment choice): active non-superuser `service_account`
`authentik-discord-recovery`, with a separate API-intent token expiring in 90 days.
Expected username is explicit configuration. The bot validates `/core/users/me/`
and rejects unexpected name/type, inactive/superuser or impersonated identities
before recovery activity. No silent legacy token fallback in this mode; legacy the operator superuser
operations remain unchanged by instruction.

Intended **global** permissions, without per-user/per-device grant maintenance:

- `authentik_core.view_user`
- `authentik_rbac.view_role`
- `guardian.view_roleobjectpermission`
- `authentik_stages_authenticator_totp.view_totpdevice`
- `authentik_stages_authenticator_totp.delete_totpdevice`

The global permission catalog endpoint is IsAuthenticated. Neither native email
nor this bot requires reset-password, email-stage-view, user-write, group/role
mutation, flow-edit, token-create or impersonation privileges. Global TOTP delete
can affect administrators if the token or bot is compromised; application checks
and their unavoidable external race do not narrow the server-side permission.
No accounts, credentials, Vault values or grants are created/changed by this task.

Configure and qualify native recovery-flow ordering, disabled-account behavior and email delivery for each installation. Source/mocks do not establish inbox delivery or safe concurrent account-state writes. Installation-specific flow and SMTP settings belong in private operational records.

## Audit, verification and parent handoff

Enabled recovery requires a private regular durable file, private parent directory
and writable persistent mount. Symlinks, nonregular/public files or unavailable
storage fail closed. Each structured JSONL event is appended and fsynced before
proceeding; failures latch audit closed until restart. UTC timestamp, action,
Discord caller ID, numeric target ID (0 if unresolved), status and acknowledged
TOTP deletion count are recorded. Statuses cover startup/attempt/preview/confirm,
write_attempt/success/denied/uncertain/partial/cancelled. No API body, target name/email,
credential, cookie, MFA material, confirmation handle or recovery link is logged.
Discord errors also omit raw errors to avoid webhook credential exposure.
If audit fails after acknowledged success, the response preserves that outcome
and reports audit failure; the preceding write_attempt requires reconciliation.
Audit retention/rotation/disk monitoring remain operator responsibilities.

Use cached modules only:

```sh
gofmt -w recovery*.go
GOCACHE=/tmp/authbot-go-cache GOPROXY=off GOSUMDB=off go test ./...
GOCACHE=/tmp/authbot-go-cache GOPROXY=off GOSUMDB=off go test -race ./...
GOCACHE=/tmp/authbot-go-cache GOPROXY=off GOSUMDB=off go vet ./...
git diff --check
```

Tests mock all Authentik/Discord requests in memory, including role/grant pages,
native cookie/redirect state, future internal/external targets, collisions,
promotion, expiry/replay, stale enrollments, partial failures and redaction.
No test requires live credentials, network, recovery/login execution or real MFA.

Parent reviews/commits the patch, qualifies complete grant visibility (including
the supplemental read), provisions credentials/settings and handles publication/build/deployment. Preserve all legacy
config and commands. Use an approved immutable image digest, prevent Watchtower
or other floating-tag autoactivation, and recreate only `authentik-bot` with
`--no-deps --pull never`; do not run broad stack workflows. Keep recovery switches
false until parent supplies the approved caller/guild/channel, dedicated token,
pinned flow and audit mount. Parent owns all live qualification and any separately
authorized test-account email/reset operation. None is authorized in this task.

Rollback disables both switches, invalidates pending handles by a bot-only
recreation, and if needed pins the recorded prior immutable bot digest with updater
exclusion retained. This cannot undo a sent email or deleted enrollment. No
password, permission, flow, token, enrollment or upstream-system rollback is
attempted automatically. Reconciled source baseline is
`05bd292403775047cd03744a9cd265b7afa99c5a` on
`feature/recovery-commands-review`; parent reports production still on the original
healthy image and handles any ordinary local Git approval. No Git escalation,
remote publishing or deployment is attempted here.

## Official source references

- [Executor protocol](https://api.goauthentik.io/flow-executor/)
- [2026.8.5 executor](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/flows/views/executor.py),
  [identification matching](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/identification/stage.py),
  [native email stage](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/email/stage.py)
- [Effective inherited roles](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/rbac/api/roles.py),
  [global catalog and its app filter](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/rbac/api/rbac.py),
  [object grants](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/rbac/api/rbac_roles.py),
  [complete nested assignment serializer and model/object role filter](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/rbac/api/rbac_assigned_by_roles.py)
- [Metadata-only enrollment enumeration](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/core/api/devices.py),
  [TOTP admin delete](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/authenticator_totp/api.py),
  [pagination](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/api/pagination.py)
