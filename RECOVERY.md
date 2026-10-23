# Recovery implementation review — activation is separate

This change implements `/password-reset-email username:<exact username>` and
`/mfa-reset username:<exact username>`. Both are **off by default** and absent
from command registration until their switch is enabled. Stale Discord commands
and confirmation buttons still fail closed. No deployed config is changed by
this source change. No credential, actual caller ID, guild ID, or target allowlist
is supplied. Caller confirmation authorizes only a previously activated operation;
it is not approval to activate, publish, deploy, or change Authentik configuration.

## Proposed environment

| Variable | Default | Requirement when enabled |
| --- | --- | --- |
| `RECOVERY_EMAIL_ENABLED` | `false` | Literal `true` enables password email command |
| `RECOVERY_MFA_ENABLED` | `false` | Literal `true` enables enrollment deletion command |
| `RECOVERY_AUTHENTIK_TOKEN` | unset | Separate token; equal-to-legacy token rejected; no fallback |
| `RECOVERY_SERVICE_USERNAME` | unset | Exact expected active `service_account` username, never a superuser |
| `RECOVERY_GUILD_ID` | unset | Exact Discord guild ID; DMs always denied |
| `RECOVERY_DISCORD_USER_IDS` | unset | Comma-separated explicit Discord user IDs; initially the operator only, exact ID to be approved |
| `RECOVERY_TARGET_USERNAMES` | unset | Comma-separated exact ordinary internal usernames; explicit list to be approved |
| `RECOVERY_EMAIL_STAGE_UUID` | `5780da44-a43d-4791-b14e-e880cc8aa895` | Operator-selected email stage; no caller choice |
| `RECOVERY_FLOW_SLUG` | unset | Exact recovery-flow slug expected on the current brand; required for email |
| `RECOVERY_MFA_FACTOR_TYPES` | `totp` | Explicit comma-separated subset of `totp,webauthn,static,email,sms,duo`; unknown/empty lists rejected |
| `RECOVERY_AUDIT_PATH` | unset | Absolute path to a durable private regular file in a private writable directory; no symlink |
| `COMMAND_CHANNEL_ID` | existing required value | Existing exact channel lock is independently enforced for recovery |
| `AUTHENTIK_URL` | existing required value | Recovery requires an HTTPS origin with no path, userinfo, query, or fragment |
| `IMMUTABLE_USERNAMES` | existing value | Existing protections also apply case-insensitively to recovery |

Keep all existing config unchanged. `AUTHENTIK_TOKEN`, existing roles, and
`REQUIRED_ROLE_ID` are not reused for recovery authorization. Enabled recovery
with missing/invalid config or unusable audit storage stops startup, before the
Discord connection. Disabled recovery never reads the legacy token for API use.
Switches accept only `true`, `false`, or unset. Allowlists are capped at 128 entries.
Configuration is fixed for a process; changing it requires a bot-only recreation,
which also invalidates outstanding confirmations.

The repository's sample Compose file explicitly passes legacy variables only.
Do not infer that new recovery variables in a local `.env` reach that container.
The verified production service instead uses `<private-deployment-path>` via `env_file`.
No Compose changes are included in this implementation.

## Deployment configuration

Supply installation-specific caller, guild, channel, service-identity and storage settings privately. No live installation values are included here.

## Interaction and target controls

1. Each recovery interaction requires an allowlisted Discord member in the
   configured guild and exact command channel. Existing permissive channel-member
   access and optional role checks cannot authorize recovery. Command registration
   is guild-context only with zero default member permissions; Discord command
   visibility overrides may be needed for an approved non-administrator caller.
   These visibility settings cannot bypass application authorization.
2. The username must be a single exact allowlisted username. No autocomplete,
   wildcard, bulk input, caller-selected API endpoint, stage, flow, or factor scope.
   User pagination uses Authentik's numeric pagination envelope, preserves the
   exact username filter, and refuses ambiguous matches or inconsistent pages.
3. Targets must be active `internal` users with explicit false `is_superuser`.
   Service/external/internal-service accounts, immutable names and the recovery
   service identity are denied. Missing security fields deny access. Username,
   PK, UUID, email, last-updated metadata, and eligibility are checked again before
   mutation. The service identity is independently checked via `/core/users/me/`
   before preview and immediately before each write; an impersonated identity,
   inactive account, unexpected name/type, or superuser fails closed.
A non-superuser may still have powerful RBAC roles or group-inherited permissions.
The operator must review effective privileges for each exact target before
approving its allowlist entry and object grants. The bot does not automatically
classify all non-superuser administrative roles.

4. Preview is ephemeral and names the target and exact configured MFA scope/count,
   including zero counts. Confirm/Cancel use random opaque 192-bit handles, not
   API tokens or target data. The stored action/target/snapshot is bound to the
   caller/channel/guild; confirmation rechecks those restrictions. Handles expire
   after two minutes and are atomically consumed. There is at most one pending or
   executing operation per target across both actions, at most 128 pending targets,
   a five-minute target cooldown from preview attempt (including failure/cancel),
   and a five-second per-allowed-caller preview rate limit. Expired state is pruned.
5. Mentions are disabled in every recovery response, including echoed usernames.
   Discord errors are not printed because they may contain webhook tokens.

## Password EMAIL behavior and prerequisites

The only password mutation route used is
`POST /api/v3/core/users/{pk}/recovery_email/` with exactly
`{"email_stage":"<operator-configured UUID>"}`. It never calls `recovery/`,
`set_password/`, or password-hash endpoints and never reads a generated link.
The target must have an email; the current brand must expose exactly the configured
recovery-flow slug; the configured email stage must be viewable and match its UUID.
These checks run during preview and confirmation before any generation request.
The server still decides whether the flow is applicable. An API failure is redacted
and stops the action; the bot does not attempt to plan a flow or generate a link
as a preflight test. Only HTTP 204 is treated as accepted/queued, never as guaranteed
email delivery. No automatic retries after an interrupted or failed write.

The supplied deployment evidence says Authentik 2026.8.5's sole/default active
brand `authentik-default` has `flow_recovery=null`. `default-recovery-flow` exists
but is unassigned. Email is therefore blocked until an independently approved
brand assignment. Assigning a recovery flow to the active brand can expose
brand-level/self-service recovery beyond the bot caller allowlist; approve this
effect explicitly. The existing flow includes MFA validation: a password email
does not promise bypass of MFA or recovery for someone who has lost all factors.
The existing `default-recovery-email` stage uses global `<configured-SMTP-relay>` and
`email/password_reset.html`; delivery has not been tested. This task changes
none of those settings and sends no email.

## MFA behavior and API evidence

Snapshot enumeration uses only
`GET /api/v3/authenticators/admin/all/?user={exact target pk}`. In 2026.8.5 this is
an **unpaginated metadata array** with string `pk`, model `type`, name, confirmed,
created, last_updated and last_used. The bot decodes those fields only. It never
calls per-factor list/retrieve endpoints: the static serializer exposes
`token_set`, which would read recovery codes. Unknown model types or malformed or
duplicate IDs fail closed.

All visible enrollment metadata, including factors outside the deletion scope,
is compared before each deletion. New, removed, renamed, updated, used, or
confirmation-state-changed enrollments abort the operation. Deletion uses only
IDs in the confirmed preview, only in the operator-configured factor scope:
`DELETE /api/v3/authenticators/admin/{totp|webauthn|static|email|sms|duo}/{id}/`.
After each successful 204, only that ID is removed from the expected snapshot.
Any failure stops further deletions and reports the confirmed deletion count;
partial deletion is audited as `partial`. No rollback or automatic retry occurs.
The preview count includes unconfirmed enrollment records if returned by the
admin endpoint; “confirmed deletion count” in results means HTTP-acknowledged
operations, not the enrollment's `confirmed` flag.

Removing an Authentik Duo enrollment does not reset Duo upstream. Removing an
Authentik enrollment does not disable mandatory MFA policy. Re-enrollment or
access recovery may still require a separately approved administrator procedure.

Official version-pinned source used for implementation:

- [User recovery email and prerequisites](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/core/api/users.py).
- [Metadata-only admin enumeration and permission filtering](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/core/api/devices.py).
- [Numeric pagination](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/api/pagination.py).
- [Current-brand recovery flow](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/brands/api.py).
- [Email stage serializer](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/email/api.py).
- [TOTP](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/authenticator_totp/api.py),
  [WebAuthn](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/authenticator_webauthn/api/devices.py),
  [static](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/authenticator_static/api.py),
  [email](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/authenticator_email/api.py),
  [SMS](https://github.com/goauthentik/authentik/blob/version/2026.8.5/authentik/stages/authenticator_sms/api.py).

## Proposed least privilege prerequisites — do not grant as part of this change

After exact activation approval, an administrator would need to provision a
**new non-superuser service account and separate API token** with an agreed expiry,
rotation owner, exact username, and no Authentik Admins membership or inherited
superuser privilege. Never reuse the operator's existing legacy credential. No group,
role, permission, flow, credential, or account changes have been made here.

Start with object-scoped permissions and verify read access before considering
activation. Do not broaden permissions to make an API error disappear:

| Purpose | Proposed Authentik permissions |
| --- | --- |
| Identify token | Read its own identity via `GET /api/v3/core/users/me/`; must return active `service_account`, exact name, false superuser |
| Resolve/recheck allowed users | `authentik_core.view_user` scoped to the explicitly approved target objects where supported |
| Password email | `authentik_core.reset_user_password` scoped to each approved target object; `authentik_stages_email.view_emailstage` scoped to the chosen stage |
| Current recovery-flow check | Current-brand endpoint is read-only/public; no brand/flow change permission needed |
| Enrollment metadata/deletion | Per factor below: view permissions for metadata, delete only for approved deletion scope; object scope where feasible |

| Factor | Model permission prefix (append `view_…` / `delete_…`) |
| --- | --- |
| TOTP | `authentik_stages_authenticator_totp.view_totpdevice` / `delete_totpdevice` |
| WebAuthn/passkeys | `authentik_stages_authenticator_webauthn.view_webauthndevice` / `delete_webauthndevice` |
| Static/recovery codes | `authentik_stages_authenticator_static.view_staticdevice` / `delete_staticdevice` |
| Email | `authentik_stages_authenticator_email.view_emaildevice` / `delete_emaildevice` |
| SMS | `authentik_stages_authenticator_sms.view_smsdevice` / `delete_smsdevice` |
| Duo | `authentik_stages_authenticator_duo.view_duodevice` / `delete_duodevice` |

For example `delete_totpdevice` above means the full permission
`authentik_stages_authenticator_totp.delete_totpdevice`. No user/group/role mutation,
impersonation, token creation, flow editing or service-account creation permissions
are required. The password-reset permission also authorizes other sensitive server
routes; the bot's strict route selection is an additional control, not a narrower
server permission. Keep this token private and scoped to intended targets.

**Permission visibility limitation:** `admin/all` returns only devices the service
can view and silently omits others. A complete cross-factor freshness check requires
view coverage of all target devices (including unscoped supported types). The bot
cannot prove absent devices are not merely hidden. Object-scoped grants for newly
enrolled devices may require administrator maintenance and deployment-version RBAC
verification; do not assume a user's reset permission grants device permissions.
Use view coverage sufficient for the agreed snapshot guarantee, without granting
unneeded delete permissions. No live service-account/RBAC qualification was done.

**Concurrency limitation:** Authentik's delete API has no snapshot conditional or
transaction spanning the metadata read and delete. The bot rechecks immediately
before each mutation and serializes its own target operations. Another administrator
can still change Authentik after that final read. If strict server-atomic prevention
of all concurrent external changes is required, activation must remain blocked
pending server-side conditional support or an approved maintenance procedure.

## Audit and failure handling

Enabled recovery requires an explicitly configured private regular file, mode
0600, with a private parent directory and persistent bot-writable mount. Opening a
symlink, nonregular file, or group/world-readable file fails. Each JSON-line event
is synchronously appended and fsynced before proceeding. Any write/fsync failure
latches the file audit closed until process restart. Setup failure stops startup;
a runtime failure stops further recovery writes. Events contain UTC timestamp,
action, Discord caller ID, numeric target ID (0 if unresolved), status, and optional
confirmed deletion count. Statuses include startup, attempt, preview, confirm,
write_attempt, success, denied, partial and cancelled. No names, email addresses,
API bodies, API tokens, MFA material, device names, confirmation handles or links
are logged. Disabled requests get a minimal structured denial in ordinary bot logs.

If audit fails after an API success, the response preserves the acknowledged
outcome and reports audit failure. The durable preceding `write_attempt` indicates
an outcome requiring operator reconciliation. A failed/lost API response can have
taken effect; never retry blindly. Audit file rotation/retention and disk monitoring
are operator responsibilities; use controlled bot restart after rotation, since
the process holds its file descriptor. Run one bot instance: pending state and
cooldowns are in memory and reset on restart; replicas are not coordinated.

## Mock verification

No `.env` is loaded in tests. Config loading moved from package `init` into
`main`; production still fails before connecting when required legacy variables
are missing. All new Authentik and Discord requests use in-memory transports in
tests, without real credentials, DNS, network, email, or enrollments. Source
inspection used official public files only. Checks:

```sh
GOCACHE=/tmp/authbot-go-cache GOPROXY=off GOSUMDB=off go test ./...
GOCACHE=/tmp/authbot-go-cache GOPROXY=off GOSUMDB=off go test -race ./...
GOCACHE=/tmp/authbot-go-cache GOPROXY=off GOSUMDB=off go vet ./...
```

The isolated writable cache is needed in this workspace; its default Go cache is
read-only. Dependencies are already cached; `GOPROXY=off` forbids network downloads.

## Parent review and exact activation decisions

Before any publication, image pull, recreation, enablement or live test, obtain
approval for the exact immutable image digest, deployment time, the operator's Discord
user ID, guild ID, preserved channel ID, target username list, service-account
name and permission scope, token creation/expiry, audit mount/retention, factor
scope (proposed TOTP only), and each feature switch. Password email additionally
requires an exact brand recovery-flow assignment, stage UUID, acknowledgement of
the flow's MFA-validation behavior, and a separately approved email delivery test.
MFA deletion tests require separate approval naming target and factor scope/count.
Resolve the permission-visibility and external-concurrency limitations above.

### Proposed rollout after approval (not executed)

1. Review the patch/local commit and run the mock checks. Publish/build only after
   separate approval. Existing push/PR workflows run CI, and master/main/weekly
   workflows can publish `latest`; do not use them as an implicit deployment path.
   Record the resulting reviewed immutable GHCR digest and the current deployed
   image digest for rollback; source baseline is
   `d12cbf2626ab0d3dfc73100ff1d4d729f2065edc`.
2. On `<deployment-host>`, prepare a bot-only Compose override with image
   `ghcr.io/jacoknapp/authentikbot@sha256:<approved-digest>` and label
   `com.centurylinklabs.watchtower.enable: "false"`. Preserve all existing env and
   network settings. Persist that pin/label in both the authoritative source
   `<private-deployment-path>` and live `<private-deployment-path>`
   as separately reviewed changes, so normal stack maintenance cannot undo it.
   Verify the actual updater respects that label and no other updater overrides
   the pin. Prevent autoactivation before making an image available.
3. Keep both recovery switches false for initial code rollout. With exact approval,
   pull **only** the digest-pinned bot and recreate **only** `authentik-bot`, avoiding
   broad docker deployment workflows. Commands below are templates, not executed:

   ```sh
   docker compose -f <private-deployment-path> -f <private-deployment-path> pull authentik-bot
   docker compose -f <private-deployment-path> -f <private-deployment-path> up -d --no-deps --pull never --force-recreate authentik-bot
   ```

   The override must already contain the approved immutable digest and updater
   exclusion. Record prior digest first; do not use floating `latest` or a broad
   `compose pull/up`. Verify image digest, health, preserved existing commands and
   absence of recovery registration. Do not dump container environment or render
   Compose config containing secrets into logs.
4. Separately approved service-account/role/token and optional brand-flow changes
   are performed by an administrator. Review read-only identity/RBAC/prerequisite
   evidence with redacted output. No permission broadening on denial. Place the
   approved secret through the existing Vault-managed secret-handling process;
   never in source, shell command lines, logs or a PR. `<private-deployment-path>`
   is rendered by Vault Agent: `<private-deployment-path>` references
   `templates/authbot.ctmpl`, which reads `<private-secret-storage-path>` and
   currently emits only `DISCORD_TOKEN` and `AUTHENTIK_TOKEN`. Do not hand-edit
   the rendered file. The approval must cover a new `RECOVERY_AUTHENTIK_TOKEN`
   key in that existing KV record and conditional rendering of that key in the
   template (or a separately reviewed new template/env_file). Preserve existing
   values and do not print the secret. Non-secret recovery configuration belongs
   in the reviewed bot service environment. No Vault, template, or production
   Compose changes are included in this implementation.
5. Prepare the agreed private audit mount/file, write only the exact approved IDs,
   allowlist, factors and switches into the service config, and perform another
   explicitly approved **bot-only** recreation with the same pinned digest.
   First verify unauthorized guild/channel/caller and stale-button denials without
   approving an action. Preview/Cancel can read enrollment metadata; live Confirm
   tests are separate, specifically authorized actions. Confirm is never an
   activation approval.

### Proposed rollback after approval (not executed)

Set both recovery switches false and recreate only the bot using the same scoped
command above; this consumes no Authentik recovery action and invalidates pending
confirmations. For binary rollback, replace the override's immutable image with
its recorded pre-change digest, keep updater exclusion and recovery switches false,
then pull only that exact bot digest if absent and recreate only the bot with
`--no-deps --pull never`. Do not restore a floating tag. Verify old commands/health
and keep audit records. Rolling back the bot cannot undo sent email, an already
queued recovery token, or deleted MFA enrollments. Do not automatically restore
roles, alter flows, generate links, resend email or reset upstream Duo.
