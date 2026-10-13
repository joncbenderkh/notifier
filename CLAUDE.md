# CLAUDE.md

Guidance for Claude Code (claude.ai/code) when working in this repository.

## What notifier is

A self-hosted service plus Android app for sending authenticated push
notifications to the user's own phone via an HTTP API. A monorepo:

- `server/` — Cloudflare Worker (TypeScript, Hono, D1). Issues credentials,
  pairs devices, seals payloads, and hands them to FCM. Deployed at
  `https://notifier.joncbender.workers.dev`.
- `android/` — Kotlin + Jetpack Compose app. Pairs with a server, receives FCM
  data messages, decrypts them, and posts notifications.
- `notify/` — Go CLI that sends notifications and manages the account
  (sign-up, pairing, keys, channels).
- `notifyd/` — Go daemon for Linux desktops (issue #7). Pairs over the
  WebSocket transport and shows messages via `org.freedesktop.Notifications`.

See `README.md` for the user-facing setup and API table.

## Decisions

- **License:** GPL-3.0-or-later. `LICENSE` at repo root;
  `SPDX-License-Identifier: GPL-3.0-or-later` as the first line of every `.ts`,
  `.go`, and `.kt` file.
- **Server stack:** Cloudflare Workers + D1, Hono, plain D1 SQL (no ORM for
  three tables), vitest with `@cloudflare/vitest-plugin` — the same shape as
  the `hearth` project. It replaced an earlier Go server (in git history,
  before PR #3) so there is one implementation. The Firebase Admin SDK does
  not run on Workers, so `src/push.ts` mints the service-account OAuth token
  itself (RS256 JWT via Web Crypto) and calls the FCM HTTP v1 API; the token
  is cached per isolate. Never store the global `fetch` unbound in a field
  — Workers throws "Illegal invocation" (the unit tests inject a fetcher, so
  only a real run catches this).
- **Server config:** `PUSH_TRANSPORT` var (`fcm`, or `log` for local dev);
  secrets `ADMIN_TOKEN` (≥32 chars) and `FCM_SERVICE_ACCOUNT` (the
  service-account JSON verbatim), declared in `src/env.d.ts` because
  `wrangler types` cannot see secrets. Locally they live in the gitignored
  `server/.dev.vars`.
- **CLI stack:** `spf13/cobra` (issue #54; both `notify` and `notifyd`,
  each with a `cmd.go` holding the command tree), `net/http`, and
  `skip2/go-qrcode` for `notify pair`, which draws the QR with half blocks
  in explicit black/white ANSI colours so it scans on dark terminals too.
  Long flags take two dashes since 0.4.0; an old single-dash spelling is
  refused with a hint (`oldStyleFlag`). Handlers return exit codes, which
  `exitCode` errors carry through Cobra unchanged; Cobra's own errors exit
  2. A command that only groups others exits 2 when run alone. `--to` accepts names or IDs;
  names resolve through `GET /v1/devices`, which API keys may call.
  Settings: flags > env (`NOTIFIER_URL`, `NOTIFIER_API_KEY`,
  `NOTIFIER_TOKEN`; `NOTIFIER_ADMIN_TOKEN` as a legacy alias, honoured only
  when it starts with `nu_`, since it once held the operator secret) > JSON config
  at `$XDG_CONFIG_HOME/notifier/config.json` (issue #8; `notifyd` keeps
  its own `notifyd.json` instead, so neither program's strict parsing
  constrains the other). Unknown fields are rejected, and a file with credentials must
  be 0600; writes are atomic and 0600. Flags default to empty and resolve
  after parsing, so `--help` never prints a secret. Sending uses the API key or
  user token from the highest level that sets one; other commands need the
  user token. Only `notify send TITLE [BODY | -]` sends, so a first word is
  always a command; `send` stops parsing flags at TITLE
  (`SetInterspersed(false)`), so a body from a variable never acts as a
  flag. Sign-up rewrites the config with only the new account's
  settings.
- **QR pairing:** the link is `notifier://pair?server=<origin>&code=<code>`
  (and `notifier://join?...` for a channel invite), built by
  `server/src/pairing.ts` and parsed by Android's `NotifierLink.kt` (keep
  them in step). The Worker's `/pair` page (`src/pairPage.ts`) is
  static and secret-free under a strict CSP (no inline script or style);
  the user pastes their token, which never leaves page memory. The
  app scans with Google's code scanner (`play-services-code-scanner`; no
  camera permission), handles the link as a deep link, and takes it pasted
  (issue #41). Each way it only pre-fills the form — never pair without the user tapping Pair, or
  a hostile QR code could silently point the phone at another server. A
  join link likewise asks before joining, and only for the paired server.
- **Android channels:** each Notifier channel except the user's `default`
  gets an Android `NotificationChannelGroup` (`ch:<channel id>`) holding
  `<channel id>` and `<channel id>.high`, created on first message, so it
  can be muted or given a sound in system settings; `default` and direct
  sends keep the original `default`/`high` channels (`push/ChannelRoute.kt`).
  Keyed by channel ID, never the label, which is not unique. Loading the
  channel list deletes groups of channels the user no longer belongs to.
- **Android stack:** Kotlin + Jetpack Compose, AGP 9 with built-in Kotlin
  (no `org.jetbrains.kotlin.android` plugin), Gradle Kotlin DSL with a version
  catalog. `minSdk = 26`, `compileSdk = 37.2`, `targetSdk = 37`. OkHttp +
  kotlinx.serialization for HTTP; no DI framework at this size.
- **Delivery:** Firebase Cloud Messaging, **data-only** messages (a
  `notification` block would be rendered by the system without the app
  decrypting it).
- **notifyd (issue #7):** a separate Go module (`godbus/dbus/v5`,
  `coder/websocket`), Linux only. `notifyd pair` takes the
  `notifier://pair` link (bare-origin rules as in `NotifierLink.kt`; plain
  http only for loopback), registers with `"transport": "ws"`, and stores
  `{url, device_id, device_token, seal_key, name}` in 0600 `notifyd.json`
  (`NOTIFYD_STATE` overrides). It pings every 30 s and treats 75 s of
  silence as a dead connection. It reconnects with jittered exponential
  backoff (1 s to 5 min, reset after a connection that held a minute).

  A message is shown, then acked. If the desktop is missing or not
  answering (ServiceUnknown, NoReply, Timeout, Disconnected, or a closed
  bus, which `Notify` reconnects), the message is not acked, so it is
  replayed. If the service refuses that message (any other D-Bus error,
  wrapped as `errRejected`), it is acked and dropped; otherwise one bad
  message would block the queue behind it. NUL characters, which JSON
  allows but D-Bus forbids, are removed first. A message that cannot be
  opened is acked and dropped. A newer envelope version is left unacked,
  so it waits in the queue for an updated daemon. The last 500 IDs are
  remembered to drop replays. Dialling and every D-Bus call have
  timeouts. `pair --force` saves the new pairing before unpairing the old
  one.

  Bare `notifyd` prints usage instead of running (issue #22): the server
  keeps one connection per device, so a second daemon replaces the
  service's, which then exits 5 and stays down. For the same reason
  `notifyd run` takes a non-blocking `flock` on `<state file>.lock`
  (symlinks resolved; a copied state file is not caught) for its lifetime and exits 1 if another holds it; the unit's
  `Restart=on-failure` then retries every 10 s until it is free.

  History (issue #16): each opened message is appended, before it is
  shown (so refused ones are kept too), as a JSON line to 0600
  `$XDG_STATE_HOME/notifier/notifyd-history.jsonl` (`NOTIFYD_HISTORY`
  overrides). The file is opened per write and set to 0600 before
  appending. Every change takes a `flock` on `<file>.lock`, shared with
  `history clear`, `pair` and `unpair` in other processes. Under that lock
  the daemon checks that `notifyd.json` still holds the device it runs as,
  so a stale daemon cannot write into a new pairing's history, and a
  compaction cannot undo a clear. The file is not encrypted, since
  `notifyd.json` keeps the seal key in the clear beside it.

  Reading deduplicates by ID, keeping the first arrival, and returns the
  newest first: at most 500, none older than 30 days. Past 600 lines the
  file is rewritten to that. Pairing and unpairing clear it.
  `notifyd history` replaces control and format characters (escapes, bidi
  overrides) in what it prints, and `--json` escapes them, since senders
  choose the text. Its tests' `TestMain` points `NOTIFYD_STATE`
  and `NOTIFYD_HISTORY` at missing temporary files.

  Close 4001, 401/404 on connect, or no state file exits 4; close 4000
  exits 5. The
  unit's `RestartPreventExitStatus=4 5` stops systemd from looping on
  them. Notify mapping: summary = title, body = body (markup-escaped when
  the server advertises `body-markup`), urgency normal/critical by
  priority, and `category` `notifier.<channel label>`.
- **WebSocket transport (issue #7):** FCM cannot reach Linux desktops, so
  `devices.transport` is `fcm` or `ws` (migration 0004). A `ws` device has
  a SQLite-backed Durable Object (`src/stream.ts`, `DeviceStream`, bound as
  `DEVICE_STREAM`, keyed `idFromName(device id)`). The object holds the
  device's socket, through the Hibernation API, and its queue: every
  message is queued and also sent when the device is connected. The queue
  holds the 200 newest, each for 24 h, and an alarm purges expired
  messages. An entry is removed only when the device acks its ID, so
  delivery is at least once, and the device deduplicates by ID.

  Frames carry envelope v1 unchanged, plus the message ID. `ping` is
  auto-answered `pong` without waking the object. `webSocketClose`
  answers a close the client starts, because the runtime did not do so
  for hibernated sockets, despite `web_socket_auto_reply_to_close`.

  A new connection closes the old one (4000). Unpairing closes the stream
  *before* deleting the row (a failure answers 503 and keeps the device
  for a retry): the socket closes with 4001, `deleteAll` runs, and an
  `unpaired` marker stays in `ctx.storage.kv` so a connection that raced
  the unpairing gets 404. Delivery to an offline device reports `queued`,
  which the CLI counts as delivered. `Deps.stream` is injected so
  `api.test.ts` uses a fake; `test/stream.test.ts` drives the real object.

  **Rollback:** migration 0004 is harmless to the previous Worker, which
  ignores `transport` (a `ws` device just looks unreachable). But from the
  first deploy on, wrangler holds a Durable Object migration (`v1`) naming
  `DeviceStream`, so every later version must keep exporting the class.
  Removing it takes a `deleted_classes` migration, which destroys every
  queue.
- **Firebase client config** is injected at build time from Gradle properties
  `notifier.firebase.{projectId,appId,apiKey,senderId}` or
  `NOTIFIER_FIREBASE_*` env vars into `BuildConfig`, and `NotifierApp`
  initializes `FirebaseApp` manually. There is deliberately no
  `google-services.json` and no google-services Gradle plugin, so the app
  builds (but cannot pair) without Firebase config — CI relies on this.
- **Users:** a deployment serves several users (issue #9). Sign-up is by
  single-use invite (`XXXX-XXXX-XXXX`, 7 days), minted by the operator or a
  user; the server generates the user's token. Devices, API keys, and
  pairings carry `user_id`, and every store query is scoped by it; another
  user's IDs answer 404, never 403. Credentials of a disabled user resolve to
  nothing (the lookups join `users`). Per-user caps, enforced inside the INSERT so
  concurrent requests cannot overshoot: 20 devices, 20 API keys, 10 invites
  (pending plus redeemed, recorded in `users.invited_by`, so redeeming one's
  own invites cannot mint unlimited accounts), 5 pending pairing codes, and 60 notifications a minute (Workers Rate Limiting
  binding `NOTIFY_LIMITER`, approximate by design). Migration 0002 moved
  pre-user data to `usr_owner` (also the column default, for writes by the
  old Worker during deploy), reachable only after the operator reissues its
  token. **No rollback past migration 0002** (nor, after it, past PR #13,
  whose `> 0` checks the 0003 triggers need): the earlier Worker ignores
  `user_id` and `disabled_at`, so it would send to every user's phones and
  accept every user's API keys, disabled or not. Never run it against a
  database with more than one user. Disabling a user does not disable the
  users they invited. The operator can act as any user by reissuing their
  token (which revokes the old one), so `ADMIN_TOKEN` is not barred from
  sending, only from doing so unnoticed. OAuth sign-up is planned
  separately (issue #10).
- **Channels (issue #6):** messages are published to channels; devices
  subscribe. A channel has one owner, who alone publishes (with their token
  or an API key scoped through `api_key_channels`; an unscoped key may
  publish to any of the owner's channels and address devices directly).
  Other users join by single-use channel invite (hashed as
  `channel-invite:` + code) and choose which of their devices subscribe.
  Delivery joins `channel_members`, so a removed member's stale subscription
  delivers nothing. Each user has `ch_default_<user id>`, which cannot be
  shared or deleted, and every device subscribes to it when paired and
  cannot leave it; a send without `channels` or `device_ids` goes there.
  Triggers in `0003_channels.sql` create both, so rows the previous Worker
  writes during a deploy are covered too. D1 counts trigger rows in
  `meta.changes`, so those inserts are checked with `> 0`; that check
  shipped separately (PR #13) before 0003, because deploys migrate before
  publishing code. Deleting a channel keeps its `api_key_channels` rows,
  so a key scoped only to deleted channels stays scoped to nothing rather
  than becoming unscoped. Caps: 20 channels besides `default`, 50 owned or
  joined channels per user, 20 members per channel (bounding one send's
  FCM fan-out), 10 pending invites per channel, 20 channels per send. A
  send reports per-device status only for the sender's own devices, and
  counts for other members'. The sealed payload carries the optional
  `channel` (as the receiver names it: `homelab` or `alice/homelab`) and
  `channel_id`. User names are not unique, so the label is for display
  only; clients must key on `channel_id`.
- **Credentials:** operator token (`ADMIN_TOKEN` Worker secret: user
  management and invites only), user tokens (`nu_`), API keys (`nk_`, a
  user's), device tokens (`nd_`), pairing codes (Crockford base32
  `XXXX-XXXX`, 10 min, single use). Invite codes are hashed with an
  `invite:` prefix so they can never redeem as pairing codes. Secrets are stored only as unsalted SHA-256 digests — fine
  because they are 256-bit random; don't reuse that scheme for anything
  low-entropy.
- **Unsafe characters (issue #24):** the server answers 400 to a title or
  body holding a control character other than `\n` and `\t` (C0, DEL, C1;
  so `\r` too) or a bidi embedding, override or isolate (U+202A–U+202E,
  U+2066–U+2069), and to a user, device or key name holding any of them once trimmed.
  Bidi marks and joiners stay allowed (right-to-left text, emoji
  sequences). The `notify` CLI cleans text first (`clean` in `main.go`:
  strips ANSI escapes, turns `\r` into newlines, replaces the rest with
  U+FFFD), so piped logs keep working; it shipped in a release before
  the server check, which deploys on merge. Clients still filter what they
  display: older rows, a 24 h stream queue, and other servers can hold
  such text.
- **Heartbeats (issue #29):** dead-man's switches in `heartbeats`
  (migration 0005), checked by a cron trigger every minute
  (`wrangler.jsonc` `triggers`, `scheduled` in `index.ts`). A heartbeat is
  `up`, `down` or `failed`. The check alerts `up` and `failed` ones past
  `due_at` (last ping, or creation, plus interval and grace), so a job
  that reported a failure and then stopped is still caught. `markDown` is
  a conditional UPDATE, so overlapping runs alert once, and `down` is not
  checked, so there are no repeats; an alert that reached no device is
  undone (`unmarkDown`) and retried a minute later, queued behind
  heartbeats that went overdue earlier so failing ones cannot crowd them
  out; a channel deleted mid-check falls back to `default`. A ping
  (`/v1/ping/<nh_ secret>`; the secret is the credential, hashed like the
  others, and appears in Workers request logs, which only the operator
  reads) moves `due_at`. `recordPing` reads the state and changes it in
  one batch (a transaction), so of two pings only one announces the
  recovery, with the right duration. `/fail` is recorded first, then
  alerts every time. A recovery or failure notice is queued in
  `pending_notice` (a newer one replaces it; each carries a unique ID, so
  identical texts stay distinct; it is queued only while the heartbeat is
  still in the state it announces and no later ping announced anything,
  counted by `ping_seq` (migration 0006, additive), so a request that
  stalled cannot replace a newer ping's notice) and sent at once under a
  lease (`notice_due_at`), or, past the user's send limit, left for the
  cron trigger; whatever reaches no device the cron retries, oldest first,
  claiming each with a conditional UPDATE so overlapping runs send it
  once. A missed check-in drops a pending recovery notice but keeps a
  failure's, and `unmarkDown` restores only the recovery notice it
  dropped (read in the same batch), never a failure notice that may
  have been delivered meanwhile. HEAD requests to
  ping URLs (link checkers) record nothing; its body (up to 1 MiB,
  past the 16 KiB API limit) is cleaned server-side (`cleanTail` in
  `text.ts`), since curl cannot clean it. Pings are limited per heartbeat
  (`NOTIFY_LIMITER`, `ping:<id>`). The INSERT checks the channel still
  exists, so a concurrent channel deletion cannot strand a heartbeat. Alerts go through `publish.ts` like any
  send, to the heartbeat's channel; deleting that channel moves its
  heartbeats to `default` so monitoring never stops silently. Disabled
  users' heartbeats neither ping nor alert. Rollback leaves data
  intact (the previous Worker ignores the table), but redeploying it does
  not remove the cron trigger: wrangler changes schedules only when
  `triggers.crons` is set. Deploy `"triggers": {"crons": []}` with it, or
  remove the trigger in the dashboard, or the schedule keeps invoking a
  Worker with no `scheduled` handler every minute.
- **Inbound webhooks (issue #34):** `hooks` (migration 0007, additive,
  so rollback is safe) maps a secret (`nw_`, hashed like the others) to a
  user's channel and a `format`. `POST /v1/hooks/<format>/<secret>`
  parses the tool's payload (`src/hooks.ts` `FORMATS`: `alertmanager`,
  `grafana`, `generic`; Uptime Kuma and GitHub are to follow) and
  publishes through `publishToChannel` (`publish.ts`, shared with the
  heartbeat alerts: a deleted channel falls back to `default`, and
  deleting a channel moves its hooks there). `format` has no CHECK, so a
  new format needs no table rebuild; the path's format must match the
  hook's. Parsing is lenient, since these schemas evolve: unknown fields
  are ignored, and text is cleaned and cut (`cleanText`, keeping the
  head) instead of refused. The body limit is 1 MiB, as for pings. The
  send limit applies per user (429). When devices exist but none took
  the alert it answers 503 so the tool retries, which can repeat an alert
  on devices that did get it; no notice is queued server-side. A
  disabled user's hooks answer 404.
- **Wire format (envelope v1):** FCM data `{"v":"1","n":<b64 nonce>,"c":<b64
  ciphertext>}`; AES-256-GCM, 12-byte nonce, 128-bit tag, per-device key,
  AAD = device ID. Plaintext is the JSON `Payload` (`server/src/app.ts`,
  Kotlin `Payload`). A shared test vector in `server/test/seal.test.ts` and
  `android/.../push/SealedMessageTest.kt` pins both sides — change the format
  only by bumping `v` and updating both.
- **Phone-side secret storage:** `PairingStore` encrypts the pairing JSON with
  an AES-GCM Android Keystore key (`KeystoreCipher`); backups and device
  transfer are disabled for it (a restored copy could not be decrypted
  anyway).
- **Notification history (issue #16):** each device keeps what it
  received; the server stores no content. On Android, `History` (pure JVM)
  holds the entries, newest first, deduplicated by message ID, at most 500
  and none older than 30 days. `HistoryStore` is a process-wide singleton,
  since `PushService` and the UI both write. It encrypts the history under
  its own Keystore key (`notifier-history`) into `files/history.bin`
  (`AtomicFile`), which is excluded from backups. Every failure surfaces
  as an `IOException`. Unreadable data is moved aside
  (`history.unreadable.bin`) and a new history starts.

  `PushService` records before showing, so messages arrive in the history
  even without the notification permission, and it catches any failure,
  so the history can never cost a notification. Recording checks, under
  the store's lock, that the pairing that received the message is still
  current. Unpairing clears the pairing before the history, so a message
  in flight during an unpair cannot reach the next pairing's history.
  Opening the history prunes it, and each screen keeps its saved state
  through `rememberSaveableStateHolder`. Tapping a notification opens
  the history (`EXTRA_OPEN_HISTORY`; the `PendingIntent` uses
  `FLAG_UPDATE_CURRENT`, because Android matches cached intents ignoring
  extras). Unpairing clears the history.
- **Application id / package:** `com.joncbenderkh.notifier`.
- **Schema migrations:** `server/migrations/NNNN_*.sql`, applied by
  `wrangler d1 migrations apply`. Append only; never edit a shipped one, and
  keep them additive (deploy.yml migrates before publishing the new code).

## Layout

- `server/src/app.ts` — `createApp(deps)`: routes and auth middleware
  (`operator`, `user`, `userOrApiKey`, `userOrOperator`, `device`, each
  setting `userId`). Operator auth fails closed unless `ADMIN_TOKEN` is ≥32
  characters; request bodies reject unknown fields; API keys see only
  `id`/`name`/`reachable` from `GET /v1/devices`. The
  sender and clock are injected so `test/api.test.ts` drives the whole flow
  against D1 with a fake sender.
- `server/src/{store,seal,token,push,encoding}.ts` — D1 queries, envelope
  encryption, secret generation/hashing, FCM + log senders, byte encodings.
- `server/src/stream.ts` — `DeviceStream`, the Durable Object behind the
  WebSocket transport (exported from `index.ts`).
- `server/src/{pairing,pairPage}.ts` — the `notifier://pair` link and QR, and
  the static `/pair` page.
- `server/src/publish.ts` — sealing and fan-out to devices, shared by
  `POST /v1/notifications`, heartbeat alerts and inbound webhooks.
- `server/src/hooks.ts` — inbound webhook formats (payload → message).
- `server/src/heartbeats.ts` — heartbeat alerts and the cron check;
  `server/src/text.ts` — what text and names may hold, and `cleanTail`.
- `server/src/index.ts` — the deployed entry point (`fetch` and the
  `scheduled` cron handler).
- `notify/` — `cmd.go` (the Cobra command tree and flags), `main.go`
  (settings, sending, exit codes), `commands.go` (the other commands), `config.go` (config file), `client.go` (API
  client, name → ID resolution), `pair.go` (terminal QR). `main_test.go` and
  `commands_test.go` run it against `httptest` servers; `TestMain` clears
  `NOTIFIER_*` and points `NOTIFIER_CONFIG` at a missing file.
- `notifyd/` — `cmd.go` (the Cobra command tree: `pair`, `run`, `test`,
  `history`, `unpair`, `version`, `completion`), `main.go` (the commands and
  exit codes), `state.go` (`notifyd.json`, atomic
  writes), `history.go` (the history file), `link.go`
  (`notifier://pair` parsing), `seal.go` (envelope v1 open, pinned by the
  shared vector), `stream.go` (connection loop), `desktop.go` (D-Bus),
  `notifyd.service` (systemd user unit), `contrib/i3blocks/` (a status
  bar block and its zenity history viewer, plain shell over `notifyd
  history --json`; the viewer strips control and format characters, since
  senders choose the text). The tests use `httptest`
  WebSocket servers and a fake `Desktop`.
- `android/app/src/main/java/com/joncbenderkh/notifier/`
  - `data/` — `PairingStore` and `HistoryStore` (both over
    `KeystoreCipher`), `ServerClient` (blocking; callers pick the thread),
    `NotifierLink` and `History` (pure JVM, unit-tested).
  - `push/` — `PushService` (FCM), `SealedMessage` and `ChannelRoute` (pure
    JVM, unit-tested), `Notifications` (Android channels and groups).
  - `ui/` — `MainViewModel`, `NotifierScreen`, `HistoryScreen`, theme.

## Commands

Server (from `server/`):

- Check: `npm run typecheck && npm run lint && npm test && npm run build`
- Local: `cp .dev.vars.example .dev.vars`, `npm run db:migrate:local`, `npm run dev`
- Deploy: automatic via `.github/workflows/deploy.yml` on push to `main`
  (needs `CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID` secrets); manual
  `npm run db:migrate:remote && npm run deploy`.

CLI and daemon (from `notify/` or `notifyd/`):

- Test: `go test -race ./...`; lint: `gofmt -l .` (must print nothing), `go vet ./...`

Android (from `android/`, needs a full JDK 17+ — the system JDK 25 here is a
JRE without `javac`; use `JAVA_HOME=~/.jdks/jbr-21.0.11`):

- Build + test + lint: `./gradlew assembleDebug testDebugUnitTest lintDebug`
- Single test: `./gradlew :app:testDebugUnitTest --tests "*SealedMessageTest"`
- Release: `./gradlew assembleRelease bundleRelease`

## Signing

No release keystore exists yet. `.github/workflows/release.yml` builds an
**unsigned** APK/AAB (the job name says so) until a keystore and its secrets
(`KEYSTORE_BASE64`, `KEYSTORE_PASSWORD`, `KEY_ALIAS`, `KEY_PASSWORD`) are added.
Never generate a keystore unprompted — ask first; losing it is unrecoverable.
The release workflow also reads `NOTIFIER_FIREBASE_*` repo secrets; without
them the released APK cannot pair.

## Versioning

SemVer 2.0.0, starting at 0.1.0. One version for the whole repo:
`versionName` in `android/app/build.gradle.kts`, `version` in
`server/package.json`, and `version` in `notify/main.go` and
`notifyd/main.go` must match; the tag `vX.Y.Z` mirrors them (release builds
also inject it into both via `-ldflags -X`). Because `notify/` and
`notifyd/` are nested Go modules, also push `notify/vX.Y.Z` and
`notifyd/vX.Y.Z` on the same commit, or `go install …@latest` resolves to
an untagged pseudo-version. The Worker deploys continuously from `main`,
independent of tags. `versionCode` increments by one each release.

## Working on notifier

- Never commit to `main`. Use a worktree per change (`gwa <branch> <comment>`,
  siblings of `main/`), push the branch, open a PR, and report the PR URL.
- Conventional Commits, imperative, ≤50-char subject, no attribution lines.
- English only.
