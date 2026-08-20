# tocsin

> **tocsin** /tɔk.sɛ̃/ (the French way — not /ˈtɒk.sɪn/): the alarm bell rung
> to warn the town that something is on fire.

HTTP notification gateway that delivers to **end-to-end encrypted** Matrix
rooms. It impersonates servers your tooling already knows how to talk to, so
nothing needs a Matrix client:

- **Gotify**: `POST /message` — accepts JSON, urlencoded and multipart forms,
  tokens via `X-Gotify-Key`, `?token=`, or `Authorization: Bearer`. Message
  bodies are rendered as markdown.
- **Prometheus Alertmanager**: `POST /alertmanager` — webhook receiver
  (payload v4), formatted with firing/resolved counts, severity, summaries
  and generator links. The links are anchored to the alert's firing window
  (graph tab, window ending shortly after the onset) instead of Prometheus's
  default "ending now", so they still show the trigger when clicked late.
  **Grouped alerts keep one live message**: resolutions — partial or full —
  edit the group's message in place (matched by alert fingerprint), so
  individual alerts flip 🔥→✅ inside it instead of spawning new messages.
  A payload introducing a NEW firing alert always posts fresh (an edit
  pings nobody), and unknown fingerprints fall back to a normal message.
  Applies to the Grafana receiver too; chart (image) messages are excluded
  and always resolve as a new message.
- **Gitea / Forgejo**: `POST /gitea` (alias `POST /forgejo`) — webhook
  receiver for push, pull-request, issue, release and branch/tag events, plus
  the Forgejo-only (>= v12) `action_run_failure` / `action_run_recover` /
  `action_run_success` CI events. The event type is read from the
  `X-Gitea-Event` / `X-Forgejo-Event` header; pass the token as `?token=` or
  set the webhook's Authorization Header field to `Bearer tcsn_...`. For
  CI-failure notifications, pick "Custom Events" in the Forgejo webhook
  config and tick only Action Run Failure (and Recover for the all-clear);
  the webhook's Secret field is unused — auth is the ingest token.
- **Slack incoming webhook**: `POST /slack?token=tcsn_...` — for tools that
  only speak Slack webhooks (TrueNAS SCALE alert services, Uptime Kuma, ...).
  Accepts a JSON body or the legacy `payload=` form field; reads `text`,
  `blocks` (header/section) and `attachments` (title/text/fallback).
  mrkdwn labeled links (`<url|label>`) and Slack's escaped entities are
  converted to markdown; `username` becomes the notification title; an
  attachment color of `danger`/`warning` raises the priority to 5/4.
  Responds with Slack's literal `ok`. The token goes in the URL because
  Slack-webhook senders can't set headers.
- **Grafana alerting**: `POST /grafana` — receiver for Grafana's
  unified-alerting **webhook contact point**. Formatted like the
  Alertmanager receiver (one line per alert, firing/resolved counts,
  severity mapped to priority: `critical` → 8, `warning` → 5), with the
  alert name linked to the Grafana rule and a 📈 link to the panel/dashboard
  when the rule is bound to one. Configure: Alerting → Contact points →
  Webhook, URL `https://notifier.example.com/grafana?token=tcsn_...`.
  **Author rules for good notifications**: the message text comes from the
  alert's annotations (`summary`, falling back to `description`, then
  `message`) and the priority from its `severity` label — a rule with
  neither still delivers, but as a bare alert name at priority 3.

Each ingest token can carry a **prefix** (e.g. an emoji) prepended to its
notifications, so `sonarr 📺` / `radarr 🎬` / `gitea 🐙` are distinguishable
even in a shared room. A token's **prefix and target channel/room can be
changed in place** (UI or `UpdateToken`) without re-minting the credential a
producer already holds. Tokens are optionally restricted to one endpoint
kind, and can carry an **expiry** — settable at creation and editable in
place afterwards (extend, shorten, or clear back to never-expires); an
expired token gets the same 401 as a wrong one. The UI shows created /
last-used / expires per token. Per-token **rate
limiting** (token bucket, generous default) returns 429 to a runaway
producer instead of flooding a room.

## Observability

- `GET /metrics` — Prometheus metrics: notifications delivered/failed
  (by channel and kind), ingest rejections (by reason), send retries and
  latency, chart render outcomes and duration, **sync age**, queued
  deliveries (`matrix_notifier_outbox_pending`), and device verification
  status. Scrape it and alert on `matrix_notifier_sync_age_seconds`.
  `matrix_notifier_key_share_empty_total` counts sends refused because no
  recipient device was known — alert on any increase (see below).
- `GET /health` — reflects real Matrix state: 200 when logged in and syncing
  recently, 503 (with a reason) when the sync has stalled, so traefik and
  docker detect a zombie the fatal-exit path wouldn't catch.

### Durable delivery

Accepted notifications are written to a **persistent outbox** in the
database before the ingest endpoint answers `200` — so a `200` means
*accepted*, not yet delivered, and webhook senders get their response
immediately instead of waiting on Matrix. A dispatcher drains the queue and
retries failures with exponential backoff (10s doubling, capped at 10m) for
up to 24 hours before marking the delivery **failed**; a homeserver outage
or a bot restart therefore loses nothing. Alertmanager charts are rendered
at send time, so a delayed delivery still charts the alert's window.

The **History** tab in the UI (or `ListDeliveries`) shows every delivery —
pending, delivered, failed — with attempt counts and the last error; failed
entries can be re-queued from there (`RetryDelivery`). Terminal entries are
pruned after `outbox_retention_hours` (default 168 = 7 days). Refusal to
send to an unencrypted room is still refused permanently, not retried.

Notifications are routed by **channels**: a channel maps a name to a Matrix
room (ID or `#alias`, resolved at creation), and every ingest token belongs
to a channel (optionally restricted to one endpoint kind). Channels and
tokens live in the database and are managed at runtime through the **web
UI** (served at `/`) or the **Connect RPC admin API**
(`/notifier.v1.AdminService/...`) — never in the config file.

Tokens are minted as `tcsn_<hex>` and stored as a SHA-256 hash of the full
string, so tokens from before the rename (`mn_...`) keep authenticating —
the prefix is cosmetic, no rotation needed.

## Prometheus charts

Charts are double opt-in: the **channel** must have the chart flag, and the
**alert rule** must carry a `chart: true` annotation — so a chart-capable
channel doesn't graph every alert that passes through. When both match, the
bot extracts the firing expression from the alert's `generatorURL`,
range-queries `prometheus_url` (last 30 min, widened to the alert's start),
renders a Grafana-style PNG, and delivers the notification as a **single
message**: the chart with the alert text as its caption (MSC2530). The blob
is an **encrypted attachment** (AES-CTR, key inside the megolm event — the
server never sees the plot). Delivery is asynchronous and best-effort: if
Prometheus is down or the query fails, the notification degrades to plain
text; the webhook never waits and never fails because of a chart.

```yaml
# in your Prometheus alerting rule:
annotations:
  summary: "CPU above 90% for 10m"
  chart: "true"
```

For expiring rooms note that Synapse's event retention does not delete media
blobs — pair it with `media_retention` (the dev stack sets
`local_media_lifetime: 30d`). Purged events take the attachment decryption
keys with them, so expired charts are unreadable ciphertext either way.

The bot logs in with password auth (stable device ID across restarts),
bootstraps **cross-signing** on first run (recovery key persisted in
`data_dir`), signs its own device — so it shows up verified — and refuses to
send to a room that is not encrypted. If the sync loop dies fatally (revoked
token), the process exits loudly; restarting it self-heals.

### Recipient key bootstrap

Before every encrypted send the bot checks that the crypto store actually
knows a device for each room member, and queries `/keys/query` for the ones
it does not. This is not redundant with mautrix's own handling: mautrix
learns about other users' devices only from `device_lists.changed` in
`/sync` (and that handler only queries users it already tracks), while the
group-session share refetches keys only for users it has *never* heard of.
A member that is tracked but whose device list ended up empty is skipped
silently — the megolm session is shared with zero devices, marked shared,
persisted, and reused, so every message in that session arrives as *Unable
to decrypt message* while the send reports success.

If, after the key query, the session would still reach no device at all,
the send is **refused** rather than delivered as unreadable ciphertext:
the error surfaces on the outbox entry and
`matrix_notifier_key_share_empty_total` increments. If only *some* members
have no devices, the send proceeds and those users are named in a warning
log — they will not be able to read it.

### When a recipient still cannot decrypt

Correct key sharing is not sufficient on its own. Two states leave a peer
stuck and neither recovers by itself: a megolm session the peer failed to
import (reused until it expires, so every message in it is unreadable for
them), and a wedged olm session (the bot thinks it has a working channel to
a device, but that device cannot decrypt what the bot encrypts, so the room
key never arrives). In both cases the bot's own logs and metrics look
perfectly healthy.

`tocsin unwedge` throws that state away so the next send rebuilds it:

```sh
# force a fresh megolm session for one room
tocsin unwedge -c config.yaml --room '!room:example.org'
# also claim fresh one-time keys for a specific user's devices
tocsin unwedge -c config.yaml --room '!room:example.org' --user '@peer:example.org'
```

It is safe against a running bot — it only removes regenerable state.
Messages already sent with the discarded session stay unreadable: their keys
were never successfully delivered, and nothing can reissue them.

## Interactive verification (green shield)

The bot answers **SAS (emoji) verification**: hit "Verify user" in Element
from any room member, the bot auto-accepts and confirms, and both sides
cross-sign each other's master keys. The emojis are logged bot-side if you
want to compare them. After that the bot shows the green shield for you (and
you for it) permanently.

## Admin API and UI

Everything operational is driven over a Connect RPC service. Authentication
is session-based: the **`Login`** RPC exchanges the admin password for a
**JWT valid 7 days** — every other RPC requires it. In the browser the JWT
lives in an **httpOnly, SameSite=Strict cookie**: the UI never sees or
stores the token (nothing in localStorage, ever). API clients present it as
a bearer header. Login attempts are rate-limited (argon2 verification is
expensive by design).

The admin password is stored in the database as an argon2id hash. The config
key `admin_token_hash` (or `TOCSIN_ADMIN_TOKEN_HASH`) only **seeds**
that credential on first boot:

```sh
# pick a password, hash it for the seed config:
openssl rand -hex 24 | tee /dev/stderr | tocsin token hash
# → put the hash in admin_token_hash (or TOCSIN_ADMIN_TOKEN_HASH)
```

After the first start the database row is authoritative: change the password
in the UI (**Settings** tab) or via `ChangeAdminPassword` — editing the
config hash does nothing. A password change also rotates the JWT signing
secret, which **instantly logs out every session** (browser cookies and API
tokens alike). Locked out completely? Delete the `admin_credentials` row and
restart: the credential re-seeds from the config hash.

The web UI (Vue 3 + Tailwind/shadcn-style dark theme, embedded in the binary, same listener) covers
status (sync health, verification, per-channel joined/encrypted state,
delivery counters), channel CRUD, token CRUD (plaintext shown exactly once),
test notifications, the password change, the bot's Matrix profile (display
name and avatar, Settings tab), and a Docs tab documenting every ingest
endpoint (how it works, sender-side configuration, priority mapping). Channel rooms are shown by their
canonical alias (`#notifs:example.org`) when one is set, by raw ID
otherwise; the ID is in the alias's tooltip and a click on either copies
the room ID. The API is plain Connect JSON — curl works:

```sh
JWT=$(curl -s -X POST http://localhost:8686/notifier.v1.AdminService/Login \
  -H 'Content-Type: application/json' -d '{"password": "<admin-password>"}' | jq -r .token)
curl -X POST http://localhost:8686/notifier.v1.AdminService/CreateChannel \
  -H "Authorization: Bearer $JWT" -H 'Content-Type: application/json' \
  -d '{"name": "infra", "roomId": "!room:example.org"}'
```

Ingest tokens are random 256-bit values stored as SHA-256 (argon2 is
deliberately reserved for the admin password: a KDF per alertmanager burst
would be self-inflicted DoS).

## Configuration

See [config.example.yaml](config.example.yaml). One database (`sqlite` or
`postgres`) holds both the E2EE crypto store and the channel/token store
(GORM, auto-migrated). Every key can be overridden via `TOCSIN_*`
env vars.

Operational flow: create an **encrypted, named** room, invite the bot (it
joins on its own), map it to a channel in the UI (joined-but-unmapped rooms
are offered as one-click suggestions), mint a token, point your producer at
the endpoint. The name is a real requirement: a nameless room with two
members is indistinguishable from a direct message — e.g. the DM a client
creates to verify the bot — so such rooms are listed separately as
`DM with @user:server` and are not offered for mapping.

Auto-join is gated by inviter homeserver: invites from servers outside
`matrix.allowed_servers` (default: the bot's own homeserver) are declined
and logged, so federated strangers cannot pull the bot into rooms.

## Room commands

In any room it's joined to, the bot reacts to commands — only to messages
that arrived **encrypted**, and only after it started (no backfill replay):

- `!notify ping` — liveness check
- `!notify status` — device, verification, delivery stats
- `!notify test` — send a test notification to the current room
- `!notify help` — command list

## Identity, the recovery key, and resets

On first start the bot generates the account's cross-signing keys and writes
the **recovery key** to `<data_dir>/recovery.key`. That file is the bot's
identity anchor: **back it up now** (password manager, vault — anywhere
durable). Everything else is replaceable; this file is what makes a rebuilt
bot *the same* bot. It is standard SSSS — Element accepts it as a Security
Key for the bot's account.

| What                        | Where                        | If lost                          |
|-----------------------------|------------------------------|----------------------------------|
| Recovery key                | `<data_dir>/recovery.key`    | see "lost everything" below      |
| Pickle key (encrypts the DB)| `<data_dir>/pickle.key`      | crypto DB unusable — same as lost DB |
| Crypto store (olm sessions) | the configured database      | recreated on start (new device)  |
| Channels + tokens           | the configured database      | recreate via UI/API              |

Do **not** back up the crypto store or the pickle key: megolm sessions
ratchet forward on every send, so restoring a snapshot replays ratchet
state and desyncs the device from the homeserver. `recovery.key` is the
single file to protect — and the single file that suffices.

### Verifying the recovery key still works

```sh
tocsin verify-identity --config config.yaml
```

logs in as a **temporary device** (removed afterwards), unlocks the
server-side SSSS with `recovery.key`, decrypts the private cross-signing
keys and checks the derived master key against the one published on the
server. One line of output, exit 0/1 — run it from cron so a silently
mismatched key (e.g. after an identity reset from another session) is
discovered before a disaster, not during one.

### Reset with the recovery key (crypto DB / pickle key / host lost)

Nothing to do — this is automatic. Keep (or restore) `recovery.key` in
`data_dir`, point the bot at an empty database, and start it. It logs in as
a new device, fetches the cross-signing keys from the server using the
recovery key, signs the new device, and comes up verified:

```
INFO logged in user_id=@notifier:... device_id=NEWDEVICE
INFO device verified with existing recovery key
```

The Matrix identity is unchanged: clients keep trusting the bot, no shields
turn red. Messages sent *before* the rebuild can no longer be decrypted by
the bot (send-mostly, so nothing is affected in practice — commands only
react to messages that arrive after startup anyway).

The reverse mismatch is also guarded: if a `recovery.key` sits on disk but
the server has **no** cross-signing keys (stale data dir pointed at a new or
wiped server), the bot refuses to overwrite the file — move it away or run
`--reset-identity` to state your intent.

### Reset when you COMPLETELY lost everything (recovery key included)

Without the recovery key the bot cannot prove it owns the existing
cross-signing keys, and it will refuse to start:

```
cross-signing keys exist on the server but the recovery key is unavailable
```

The way out is to burn the old identity and mint a new one. Run **once**:

```sh
tocsin --config config.yaml --reset-identity
```

This burns the old identity completely (everything authenticated with the
bot's password):

1. **Logs out every other device** of the account (`/delete_devices`): their
   access tokens are revoked, their device keys removed, and they receive no
   future megolm sessions.
2. **Replaces the account's cross-signing keys** on the server, signs the
   current device with the new keys.
3. Writes a **new** `recovery.key`. Back that one up too, the old one is now
   worthless.

Consequences of a reset — this is why it's a flag and not automatic:

- Anyone who verified the bot sees an identity change: Element shows the
  classic "identity has changed" warning and the bot must be re-trusted.
- Old encrypted history stays undecryptable for the bot. Room membership,
  the account, and its password are untouched.
- Nothing retroactively hides what a logged-in device already decrypted
  while it was live; if the *password* itself is compromised, rotate it too
  (the reset does not change it).

Do **not** leave `--reset-identity` in a service unit / restart loop: every
start would mint a fresh identity. Run it once, then start the bot normally.

## Dev stack

Requires Docker, `jq`, Go and Node. Ports published on localhost:

| Service       | URL                     |
|---------------|-------------------------|
| Synapse       | `http://localhost:8008` |
| Element Web   | `http://localhost:8009` |
| synapse-admin | `http://localhost:8010` |
| Prometheus    | `http://localhost:9090` |
| Postgres      | `localhost:5432`        |
| bot (UI+API+ingest) | `http://localhost:8686` |

```sh
make dev-up    # Synapse + Postgres + Element + synapse-admin, accounts, encrypted room (alias #notifications:localhost), config.dev.yaml
make run-dev   # build (UI included) and run the bot
make dev-seed  # create a "notifications" channel + ingest token via the admin API
make dev-down  # stop containers (state kept)
make dev-nuke  # full reset: containers, volumes, keys, room, bot state
```

Dev credentials: Matrix admin `admin`/`admin` (Element + synapse-admin),
bot admin password `dev-admin-token` (web UI at `http://localhost:8686`;
seeded on first boot — if you change it in the UI, `dev-nuke` resets it).

Send a test notification with the token `make dev-seed` printed:

```sh
curl -X POST 'http://localhost:8686/message?token=tcsn_...' \
  -F title='Hello' -F message='**It works!**' -F priority=5
```

To drive bot commands or SAS verification from the CLI through real E2EE:

```sh
go run -tags goolm ./dev/cmdclient -room "$(cat dev/.room_id)" -message '!notify status'
go run -tags goolm ./dev/cmdclient -room "$(cat dev/.room_id)" -verify   # asserts mutual cross-signing
```

## Sending from the CLI

The binary doubles as a client for scripts and cron jobs:

```sh
export TOCSIN_URL=https://notifier.example.org
export TOCSIN_TOKEN=tcsn_your-ingest-token
tocsin send -t "Backup done" "**42 GB** copied"
echo "or pipe the body in" | tocsin send -t "From cron"
```

`--url`/`--token` flags override the env vars; `--priority` sets the Gotify
priority.

## Alertmanager receiver config

The notifier speaks the Alertmanager webhook format natively — point a
receiver straight at it (token in the URL):

```yaml
receivers:
  - name: tocsin
    webhook_configs:
      - url: https://notifier.example.org/alertmanager?token=tcsn_your-token
        send_resolved: true
route:
  routes:
    - receiver: tocsin
      continue: true   # also fall through to your other receivers
```

## CI

GitHub Actions (mirroring `thomas-maurice/cortex`):

- **test** — on every PR and non-master push: UI build + vitest, `go build`/`go vet`/golangci-lint/buf lint, `go test -race`, and a short fuzz
  pass over the ingest parsers (`make fuzz`, the code that chews untrusted
  webhook bodies; `make fuzz FUZZTIME=5m` for a longer local hunt).
- **build** — on master pushes and `v*` tags: runs the test workflow, then
  builds and pushes a multi-arch (amd64/arm64) image to
  `ghcr.io/thomas-maurice/tocsin` — `:latest` + `:sha-…` on master,
  `:X.Y.Z`/`:X.Y` on tags.
- **dependabot** — weekly grouped dependency PRs (Go modules, ui npm,
  actions, Dockerfile images); the testcontainers Synapse E2E job is what
  makes merging mautrix bumps safe.

The Docker image runs as a non-root user with `/data` as the volume for
`data_dir`; mount your config at `/config/config.yaml` (or override the
command). It is built for `linux/amd64` only (CGO/SQLite makes arm64 under
qemu emulation prohibitively slow); add `linux/arm64` back to the build
workflow's `platforms` if you need it.

## Building

mautrix-go's pure-Go olm implementation is behind a build tag; always build
with `-tags goolm` (the Makefile does). The admin UI is Vue 3 + TypeScript,
built with Vite (`npm run build` typechecks via `vue-tsc` first) and
embedded via `go:embed` — `make build` builds it automatically if missing;
`make ui` rebuilds it explicitly. `make proto` regenerates both the Go RPC
stubs (`gen/`) and the typed TypeScript Connect client (`ui/src/gen/`,
consumed via `@connectrpc/connect-web`); neither is ever edited by hand.

```sh
make build && make test
```
