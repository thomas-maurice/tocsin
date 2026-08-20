---
name: tocsin-dev
description: How to work on the tocsin repo — layout, dev stack lifecycle (up/seed/down/nuke), building, testing with cmdclient, talking to the bot, and troubleshooting. Use for ANY development, debugging, or ops task in this repository.
---

# Working on tocsin

Renamed from `matrix-notifier` on 2026-07-19 (repo, module path, binary,
image, `TOCSIN_*` env prefix, `tcsn_` token prefix). Legacy `mn_...` tokens
still authenticate (stored as SHA-256 of the full string). Prometheus
metric names deliberately kept as `matrix_notifier_*` (dashboards/alerts
reference them) and the Connect package stays `notifier.v1`.

Go Matrix notification gateway: HTTP ingest endpoints (Gotify, Alertmanager,
Gitea/Forgejo, Slack-compatible) → end-to-end-encrypted Matrix rooms. Routing is **channels**
(name → room ID) with **ingest tokens** belonging to channels; both live in
the DB and are managed via a Connect RPC admin API + embedded Vue UI, never
in the config file.

**Delivery is asynchronous through a durable outbox** (`outbox_entries`
table, `internal/outbox` dispatcher): ingest handlers only enqueue — a 200
means *accepted*, not delivered. The dispatcher retries with exponential
backoff (10s→10m cap) for 24h, then marks the entry failed; terminal rows
are the History shown in the UI, pruned after `outbox_retention_hours`
(default 168). Don't assert "curl 200 ⇒ message in room" — check the room,
the UI History tab, `ListDeliveries`, or `matrix_notifier_outbox_pending`.
Alertmanager charts are rendered by the dispatcher at send time.
**Grouped alerts keep one live Matrix message** (alertmanager + grafana,
matched by alert `fingerprint` via the `alert_messages` table): partial
and full resolutions EDIT the group message in place; a payload with any
unannounced firing fingerprint, or whose fingerprints map to more than one
message, POSTS instead (new alerts must ping). Repeats re-edit
idempotently (mappings are looked up, never consumed). Fallbacks → normal
message: unknown fingerprints, failed edit, chart/image originals (images
never record fingerprints). Mappings re-point on re-fire and are pruned
with the outbox retention.

## Maintenance contract (non-negotiable)

When you add or change **anything** — a feature, a procedure, a Make target,
a config key, a troubleshooting trick — you MUST update in the same session:

1. **`README.md`** — the user-facing documentation.
2. **This skill** — the agent-facing documentation.
3. **`ui/src/components/DocsPanel.vue`** — the in-UI "Docs" page. MANDATORY
   whenever an ingest/webhook endpoint is added, removed, or its behavior,
   auth, priorities, or sender-side configuration changes. It documents,
   per endpoint: how it works, how to configure the sender, and priority
   mapping. Remember `make ui && make build` afterwards or the embedded
   dist ships stale.

Also search cortex (`cortex_memory_search`, namespace is auto-derived from
the git remote) at task start: this repo has a rich decision history
(architecture, E2EE gotchas, prod deployment). Save new decisions/gotchas
back to cortex when done.

**Never commit or push** — Thomas reviews diffs and commits himself.

## Repo structure

```
cmd/tocsin/    main, `send` subcommand (CLI client), `token hash`
internal/
  matrix/     bot: login, E2EE (mautrix cryptohelper), cross-signing, SAS
              verification, key backup, !notify commands, room helpers
  api/        Connect RPC AdminService (channels/tokens/status/rooms) +
              auth.go: JWT session auth (Login/Logout/ChangeAdminPassword)
  server/     gin HTTP server: ingest routes (enqueue-only), /health,
              /metrics, per-token rate limiting, serves the embedded UI
  outbox/     delivery dispatcher: drains the outbox with backoff, renders
              alertmanager charts at send time, prunes history
  store/      GORM store (channels + ingest tokens + admin credential +
              outbox entries), shares the DB with the mautrix crypto store;
              ingest tokens stored as SHA-256, admin password as argon2id
  ingest/     gotify/, alertmanager/, gitea/, slack/ payload parsing + formatting
  chart/      Prometheus range-query → PNG chart rendering (go-charts v2)
  config/     viper config, TOCSIN_* env overrides
  logging/    slog + charmbracelet handler, logger-in-context
  metrics/    Prometheus collectors
  notify/     Notification struct + Sender interface
proto/notifier/v1/      admin.proto — the API source of truth
gen/                    generated stubs — NEVER edit, `make proto` regenerates
ui/                     Vue 3 + TypeScript + Vite + Tailwind 4 (shadcn-style
                        dark-zinc theme in src/style.css, hand-rolled ui/
                        components, @lucide/vue icons);
                        ui/src/gen = buf-generated TS client (never edit);
                        embedded via go:embed all:dist — StatusPanel/
                        ChannelsPanel/TokensPanel/SettingsPanel/DocsPanel
dev/                    dev stack: docker-compose, bootstrap.sh, cmdclient
test/integration/       testcontainers Synapse E2E (build tag `integration`)
```

## Building (gotchas first)

- **Always `-tags goolm`** on every go build/test/run — libolm (cgo) is
  mautrix's default; the Makefile handles it, raw `go test ./...` breaks.
- **UI changes are NOT picked up by `make build`** if `ui/dist` already
  exists (`ui-ensure` only builds when missing). After touching `ui/src`,
  run `make ui && make build`.
- **Proto changes**: edit `proto/notifier/v1/admin.proto`, run `make proto`
  (buf via `go run`, no global installs). Generates Go stubs into `gen/`
  AND the TypeScript client into `ui/src/gen/` (remote plugin
  buf.build/bufbuild/es, needs network) — both committed, NEVER hand-edit.
- **UI is TypeScript** (Vue 3 SFCs with `lang="ts"`): `npm run build` runs
  `vue-tsc --noEmit` first, so type errors fail the build (and CI) without
  a separate step; `npm run typecheck` runs it alone. The API client is the
  buf-generated Connect client (`ui/src/api.ts` wraps createClient; errMsg/
  isUnauthenticated helpers unwrap ConnectError for toasts). Proto int64 →
  bigint, Timestamp → message (use `timestampDate()` from
  @bufbuild/protobuf/wkt), bytes → Uint8Array.
- **typescript is pinned ~5.9**: TypeScript 7 (native compiler) has no
  `lib/tsc` and breaks vue-tsc — do not bump past 5.x until vue-tsc
  supports it.

```sh
make build            # ui-ensure + go build → bin/tocsin
make test             # go test -tags goolm ./...
make test-integration # real Synapse via testcontainers (needs Docker)
make fuzz             # FuzzParse on all 5 ingest parsers, FUZZTIME=10s each
cd ui && npm test     # vitest
golangci-lint run --build-tags goolm   # CI runs this; keep it at 0 issues
```

Full CI gate = UI build + vitest, go build/vet, golangci-lint, buf lint,
`go test -race`, `make fuzz` (10s/parser smoke).

## Dev stack lifecycle

Requires Docker, jq, Go, Node. Everything on localhost:

| Service       | URL                     | Credentials              |
|---------------|-------------------------|--------------------------|
| Synapse       | http://localhost:8008   | server_name `localhost`  |
| Element Web   | http://localhost:8009   | `admin` / `admin`        |
| synapse-admin | http://localhost:8010   | `admin` / `admin`        |
| Grafana       | http://localhost:8011   | `admin` / `admin`        |
| Prometheus    | http://localhost:9090   | —                        |
| Postgres      | localhost:5432          | `synapse`                |
| Bot UI/API    | http://localhost:8686   | password `dev-admin-token` |

```sh
make dev-up     # bootstrap.sh: containers, accounts, encrypted room with
                # canonical alias #notifications:localhost, config.dev.yaml
make run-dev    # build + run bot in FOREGROUND (blocks — see below)
make dev-seed   # channel "notifications" + ingest token (bot must be running;
                # prints the tcsn_... plaintext token)
make dev-down   # stop containers, keep state
make dev-nuke   # scorched earth: containers+volumes, synapse keys, room id,
                # config.dev.yaml, bot data/ — dev-up rebuilds from zero
```

Dev accounts: bot `@notifier:localhost` / `notifier-dev-password`, room ID
cached in `dev/.room_id`.

**Running the bot as an agent** (run-dev blocks): build, then

```sh
nohup ./bin/tocsin --config config.dev.yaml > <scratchpad>/bot.log 2>&1 &
# wait for readiness:
curl -sf http://localhost:8686/health   # {"health":"green"}
```

Before starting, check nothing stale holds the port:
`lsof -i :8686 -sTCP:LISTEN` — a bot from a previous session can survive a
`dev-nuke` and keep 8686 busy with a dead config. Kill it.

## Exercising the bot

**Ingest** (token from `make dev-seed`):

```sh
curl -X POST 'http://localhost:8686/message?token=tcsn_...' \
  -F title='Hello' -F message='**It works!**' -F priority=5
# also: POST /alertmanager?token=..., POST /gitea?token=... (X-Gitea-Event header)
```

**GOTCHA — adding a new ingest endpoint takes TWO registrations:** the gin
route in `internal/server/server.go` AND the path whitelist in
`cmd/tocsin/main.go` (the outer mux that splits admin API / ingest /
embedded UI). Miss the second and the route silently falls through to the
SPA handler: 200 + index.html, no delivery. Server-level tests exercise
server.New directly and CANNOT catch this — verify new endpoints against
the running dev-stack binary.

`POST /slack?token=...` is Slack-incoming-webhook compatible (token kind
`slack`): JSON body or legacy `payload=` form field; `text` + `blocks`
(header/section) + `attachments`; `username` → title; attachment color
danger/warning → priority 5/4; mrkdwn links/entities converted to markdown;
responds with Slack's literal `ok`. Built for TrueNAS SCALE alert services
and friends, which can't set headers — hence token-in-URL.

`POST /grafana?token=...` receives Grafana unified-alerting **webhook
contact points** (token kind `grafana`). Formatting mirrors alertmanager
(severity → priority: critical 8, warning 5), plus a 📈 panel/dashboard
link when the rule is bound to one; with no group labels the title falls
back to the first alert's alertname (Grafana's receiver name is just the
contact-point name — never title on it). Dev Grafana (port 8011) has the
dev Prometheus provisioned as datasource `dev-prometheus`
(`dev/grafana/provisioning/`); alerting config is NOT provisioned (it needs
a runtime-minted token) — recreate after a nuke via the API:

```sh
# contact point → bot on the host; policy → route + fast grouping; rule: vector(1)>0
curl -su admin:admin -X POST localhost:8011/api/v1/provisioning/contact-points \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"tocsin\",\"type\":\"webhook\",\"settings\":{\"url\":\"http://host.docker.internal:8686/grafana?token=$GTOKEN\"}}"
curl -su admin:admin -X PUT localhost:8011/api/v1/provisioning/policies \
  -H 'Content-Type: application/json' \
  -d '{"receiver":"tocsin","group_by":["grafana_folder","alertname"],"group_wait":"5s","group_interval":"10s","repeat_interval":"4h"}'
# then create a folder (POST /api/folders) and an alert-rule
# (POST /api/v1/provisioning/alert-rules, datasourceUid dev-prometheus,
# expr vector(1), threshold C > 0) — fires within ~1 min.
```

Gitea/Forgejo ingest auth: token via `?token=`, `X-Gotify-Key`, or
`Authorization: Bearer` (Forgejo's webhook "Authorization Header" field);
the Forgejo webhook Secret/HMAC signature is ignored by design. The parser
also handles the Forgejo-only (>= v12) `action_run_failure`/`_recover`/
`_success` CI events — repo/link live INSIDE the payload's `run` object
(not top-level `repository`); failure formats at priority 5, recover and
success at 3. Failure-only delivery is configured Forgejo-side ("Custom
Events" → Action Run Failure), no receiver-side filtering.

**Admin API** (plain Connect JSON, camelCase). JWT-only: `Login` is the sole
RPC that accepts the password; everything else needs the JWT (Bearer header,
or the httpOnly cookie browsers get). Login is rate-limited (burst 5, then
1 per 2s) — space out scripted logins:

```sh
JWT=$(curl -s -X POST http://localhost:8686/notifier.v1.AdminService/Login \
  -H 'Content-Type: application/json' -d '{"password": "dev-admin-token"}' | jq -r .token)
curl -s -X POST http://localhost:8686/notifier.v1.AdminService/ListChannels \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' -d '{}' | jq .
# Other RPCs: GetStatus, ListRooms, CreateChannel {name, roomId, chart},
# UpdateChannel, DeleteChannel, LeaveRoom, ListTokens, CreateToken
# {name, kind, channel, prefix, expiresAt?} (expired tokens 401 like wrong
# ones), UpdateToken {name, prefix, channel?, expiresAt?, clearExpiry?}
# (expiry editable in place; clearExpiry revives an expired token),
# DeleteToken,
# ListDeliveries {channel?, limit?} (outbox history, newest first),
# RetryDelivery {id} (failed entries only — re-queues immediately),
# SendTestNotification {channel}, TestToken {name}, Logout,
# GetProfile, SetProfile {displayName, avatar (base64 bytes)},
# ChangeAdminPassword {currentPassword, newPassword} (rotates the JWT
# secret: kills ALL other sessions, returns a fresh token for the caller)
```

Auth model: the admin password lives in the DB (`admin_credentials`, single
row, argon2id hash + JWT signing secret). Config `admin_token_hash` only
SEEDS it on first boot — after that the DB wins and the config value is
inert. The UI stores nothing: the JWT rides in an httpOnly SameSite=Strict
cookie set by the server.

**cmdclient** — standalone mautrix E2EE client (logs in as `@admin:localhost`,
sends one encrypted message, prints decrypted replies). This is how you test
`!notify` commands through real encryption from the CLI:

```sh
go run -tags goolm ./dev/cmdclient -room "$(cat dev/.room_id)" -message '!notify status'
go run -tags goolm ./dev/cmdclient -room "$(cat dev/.room_id)" -verify   # SAS: asserts mutual cross-signing
```

Flags: `-homeserver -user -password -room -message -db -wait -verify
-target -reset-cross-signing`. Its crypto store is `dev/cmdclient.db`
(recovery key beside it as `dev/cmdclient.db.recovery.key`).

**Room commands** (any joined room, encrypted messages only, no backfill):
`!notify ping | status | test | help`.

**Verification in the browser**: log into Element (localhost:8009) as
admin, open the room, "Verify user" on the bot — it auto-accepts SAS.

## Troubleshooting

- **`olm account is marked as shared, keys seem to have disappeared`** —
  something logged out the bot's device. Known cause: Synapse admin
  `PUT /_synapse/admin/v2/users/<id>` with a password logs out all devices
  unless `"logout_devices": false` (bootstrap.sh passes it). Fix: point the
  bot at an empty crypto store; with `data_dir/recovery.key` intact it
  re-verifies automatically as a new device.
- **Bot process exits on fatal sync error** (e.g. M_UNKNOWN_TOKEN) — by
  design; just restart it, it self-heals.
- **Every message shows "Unable to decrypt message" in clients** — the
  megolm session was shared with zero devices. mautrix only learns other
  users' devices from `device_lists.changed` in `/sync`, and
  `ShareGroupSession` refetches keys only for users the crypto store has
  *never* seen (`GetDevices` returns nil); a user that is tracked but whose
  `crypto_device` rows are gone is logged `user has no devices, skipping`,
  the share reports success with `destination_map={}`, and the session is
  then persisted as shared and reused. `internal/matrix/devicekeys.go`
  guards this: `Client.Crypto` is wrapped so every encrypt first queries
  keys for members with no known devices and refuses the send when the
  session would reach nobody (`matrix_notifier_key_share_empty_total`).
  Diagnose with `tocsin_log_level: debug`: `destination_map` must be
  non-empty and `device_count` must match the members' device count.
  **Never "fix" this by deleting `crypto_device` / `crypto_tracked_user`
  rows** — an emptied-but-tracked user is exactly the unrecoverable state.
  Messages sent while broken stay unreadable forever; those session keys
  never left the bot.
- **Bot logs a healthy send but the recipient sees "Unable to decrypt"** —
  distinct from the empty-share bug above: here the share genuinely reached
  devices (`crypto_megolm_outbound_session_shared` has rows for them). Either
  the peer failed to import the megolm session, or the olm session is wedged
  so the room key never arrived. Neither self-heals — the stored session is
  reused until expiry. Fix: `tocsin unwedge -c cfg --room '!id' [--user '@who']`
  (`cmd/tocsin/unwedge.go`), which discards the outbound megolm sessions and
  optionally the olm sessions so the next send re-shares from scratch. Safe
  on a live bot. **Diagnose before deploying anything**: query the crypto
  store first — `crypto_megolm_outbound_session`, its `_shared` table, and
  `crypto_device` — to confirm whether the share actually reached the peer's
  devices. On prod, `URI=$(sudo grep -oP '(?<=uri: ")[^"]+' /data/tocsin/config/config.yaml); psql "$URI" ...`.
  **Note synapse's own DB is the `postgres-matrix_maurice_fr` CONTAINER
  (`synapse_matrix_maurice_fr`), not the host postgres** — the host has a
  stale leftover `synapse` DB that will silently give you months-old answers.
- **"room is not encrypted" on a room that is** — stale state store; the
  bot refetches `/state` automatically at send time. If creating a channel
  by `#alias`, the alias is resolved once and the ID is stored (all internal
  lookups are ID-keyed). Raw room IDs are shape-validated
  (`matrix.ValidateRoomID`: `!opaque:server`, or `!opaque` for room v12+) —
  garbage gets invalid_argument; existence is deliberately not checked
  (mapping a not-yet-joined room is a supported flow).
- **Element never shows the green shield** — your Element session holds an
  old cross-signing identity. Verify the Element session with the current
  Security Key (`dev/cmdclient.db.recovery.key` for the dev admin) or reset
  the cryptographic identity in Element, THEN verify the bot.
- **Port 8686 already in use / UI shows stale data** — stale bot process
  from before a nuke: `lsof -i :8686 -sTCP:LISTEN`, kill it. Remember it
  also means the running binary predates your code changes.
- **UI change not visible** — you forgot `make ui` before `make build`
  (embedded dist was stale), or you're hitting the stale process above.
- **Chart layout iteration** —
  `CHART_SAMPLE_OUT=/tmp/x.png go test -tags goolm -run TestRenderLayoutSample ./internal/chart/`.
- **Admin password lost / 401 on everything** — the DB credential is
  authoritative, changing `admin_token_hash` in config does nothing after
  first boot. Recovery: delete the `admin_credentials` row (`DELETE FROM
  admin_credentials;`) and restart — it re-seeds from the config hash. A
  password change also rotates the JWT secret, so 401s right after one are
  just dead sessions: log in again.
- **Bot logs** — stderr (slog + charm handler); mautrix internals logged at
  DBG. When running detached, tail the nohup log file.
- **Health** — `GET /health` is real: 503 with a reason when sync age > 90s
  or not logged in. `GET /metrics` has `matrix_notifier_sync_age_seconds`,
  delivered/failed counters per channel/kind.

## Bot profile

Display name + avatar are managed at RUNTIME via the admin API
(`GetProfile`/`SetProfile` RPCs, UI Settings tab), NOT the config file.
`Bot.Profile`/`Bot.SetProfile` in bot.go wrap the Matrix profile +
media-upload calls; GetProfile inlines the avatar bytes (base64 in Connect
JSON) so the browser previews it without authenticated media. SetProfile
caps avatars at 1 MiB and rejects non-image bytes. The UI Docs tab also
documents room retention (`m.room.retention` + Synapse `retention.enabled`)
for purging old notifications.

## Identity model (don't break it)

`data_dir/recovery.key` is the bot's identity anchor (SSSS recovery key);
`pickle.key` encrypts the crypto store. DB loss is recoverable with the
recovery key (automatic); recovery-key loss requires
`--reset-identity` (destructive: logs out devices, new cross-signing keys,
everyone re-verifies). Never put `--reset-identity` in a restart loop.
**Backup = recovery.key ONLY** — never snapshot/restore the crypto store or
pickle.key (megolm ratchet replay + olm account desync).
`tocsin verify-identity -c config.yaml` proves the on-disk key
still matches the server (temp device, removed after; exit 0/1, cron-able;
`internal/matrix/verify.go`). Covered by the integration test.

## Production (context, not for agents to touch casually)

Prod runs at https://notifier.lil.maurice.fr (`@notifier:matrix.maurice.fr`),
deployed from `~/git/ansible-basics` (role `matrix_notifier`, host
synapse.lil.maurice.fr) with secrets in Vault `prod/kv/tocsin`.
Image: `ghcr.io/thomas-maurice/tocsin:latest` (GHA on master push,
linux/amd64 + linux/arm64, arm64 built under qemu). Weekly dependabot PRs
(gomod grouped minor/patch, npm in ui/ with TS major pinned out, actions,
docker). Ask before doing anything prod-facing.
