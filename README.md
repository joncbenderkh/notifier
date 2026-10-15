# notifier

Send authenticated, end-to-end encrypted push notifications to your Android
phone with one command or one HTTP call.

```sh
notify send --to pixel --high "Backup finished" "2.4 GB in 3m12s"
```

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/notifications \
  -H "Authorization: Bearer $NOTIFIER_API_KEY" \
  -d '{"title": "Backup finished", "body": "2.4 GB in 3m12s", "priority": "high"}'
```

- **`server/`** — a Cloudflare Worker (TypeScript, Hono, D1) that issues
  credentials, pairs phones, and relays notifications through Firebase Cloud
  Messaging (or, for Linux desktops, its own WebSocket transport).
- **`android/`** — a Kotlin + Jetpack Compose app that pairs with the server,
  decrypts incoming messages, and shows them as notifications.
- **`notify/`** — `notify`, a single Go binary for sending from scripts and
  terminals.
- **`notifyd/`** — `notifyd`, a daemon that shows notifications on a Linux
  desktop (dunst, mako, GNOME, KDE, ...).

## Security model

| Credential   | Held by                         | Allows                                         |
|--------------|---------------------------------|------------------------------------------------|
| Admin token  | Server operator (Worker secret) | Manage users (list, reissue token, disable); mint invites |
| User token   | Each user, from sign-up         | Pair devices, manage their API keys and devices, invite, send |
| API key      | A user's sending script/service | Send notifications; list paired devices (IDs and names) |
| Device token | The paired phone                | Update its own FCM token, send itself a test, unpair |
| Pairing code | User → phone, once              | Register one device, within 10 minutes         |

API keys and device tokens are 256-bit random secrets stored server-side only
as SHA-256 digests, and each can be revoked independently.

Every notification is sealed with **AES-256-GCM under a per-device key**
generated at pairing time, with the device ID as associated data. FCM only
ever carries ciphertext, so Google can neither read nor forge messages, and a
payload sealed for one device will not open on another. On the phone, the
pairing secrets are encrypted with an Android Keystore key and excluded from
backups. Release builds of the app refuse plain-HTTP servers.

## Setup

### 1. Firebase

1. In the [Firebase console](https://console.firebase.google.com/), create a
   project and add an Android app with package `com.joncbenderkh.notifier`.
2. **Server:** Project settings →
   [Service accounts](https://console.firebase.google.com/project/_/settings/serviceaccounts/adminsdk)
   → *Generate new private key*. Keep the JSON out of the repository.
3. **App:** from the Android app's `google-services.json`, put these in
   `~/.gradle/gradle.properties` (or export them as `NOTIFIER_FIREBASE_*`
   environment variables):

   ```properties
   notifier.firebase.projectId=<project_info.project_id>
   notifier.firebase.appId=<client[0].client_info.mobilesdk_app_id>
   notifier.firebase.apiKey=<client[0].api_key[0].current_key>
   notifier.firebase.senderId=<project_info.project_number>
   ```

Reference: [Add Firebase to Android](https://firebase.google.com/docs/android/setup),
[FCM HTTP v1 sending](https://firebase.google.com/docs/cloud-messaging/send/v1-api).

### 2. Server (Cloudflare Worker)

```sh
cd server
npm install
npx wrangler login
npx wrangler d1 create notifier        # paste the database_id into wrangler.jsonc
npx wrangler d1 migrations apply notifier --remote
openssl rand -hex 32 | tee /dev/stderr | npx wrangler secret put ADMIN_TOKEN   # the operator's token
npx wrangler secret put FCM_SERVICE_ACCOUNT < /path/to/notifier-firebase-adminsdk.json
npx wrangler deploy
```

After that, `.github/workflows/deploy.yml` redeploys on every push to `main`
that touches `server/`, given `CLOUDFLARE_API_TOKEN` and
`CLOUDFLARE_ACCOUNT_ID` repository secrets.

Local development: copy `server/.dev.vars.example` to `server/.dev.vars`,
then `npm run db:migrate:local && npm run dev`. `PUSH_TRANSPORT=log` logs
messages instead of delivering them.

### 3. Create a user

A deployment serves several users; each owns their own phones and API keys.
`ADMIN_TOKEN` is the operator's credential: it manages users and mints
invites, and nothing else. People join with a single-use invite (valid seven
days), and the server generates their token:

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/invites -H "Authorization: Bearer $ADMIN_TOKEN"
# {"code":"0Z5H-QCEA-3JX4","expires_at":"..."}

curl -X POST https://notifier.joncbender.workers.dev/v1/users -d '{"invite": "0Z5H-QCEA-3JX4", "name": "alice"}'
# {"user_id":"usr_...","name":"alice","token":"nu_..."}   <- shown only once
```

Or, with the CLI, which saves the token to its config file:
`notify --url https://notifier.joncbender.workers.dev signup 0Z5H-QCEA-3JX4 alice`.

Users can invite others too (`POST /v1/invites` with their own token; at most
ten each, used or not). `GET /v1/users` shows who invited whom; disabling a
user does not disable the users they invited. A lost token cannot be
recovered, only replaced: the operator calls `POST /v1/users/{id}/token`,
which revokes the old one. That also means the operator can take over any
account, though not without its owner's token stopping working.

Upgrading a deployment from before users existed moves its phones and API
keys to a user `usr_owner`. Nobody can sign in as it until the operator issues
its token:

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/users/usr_owner/token -H "Authorization: Bearer $ADMIN_TOKEN"
```

Paired phones and existing API keys keep working throughout.

### 4. Pair the phone

Create a single-use pairing code (valid 10 minutes) and show it as a QR code,
either way:

- **Browser:** open `https://notifier.joncbender.workers.dev/pair`, paste your
  user token, and click *Create pairing code*. The token stays in the page's
  memory and is sent only to that server.
- **Terminal:** `notify pair` prints the QR code (it needs your user token;
  see [the CLI](#notify-cli)).

In the app, tap **Scan QR code** (or scan with the phone's camera app, which
opens the `notifier://pair?...` link in Notifier). Scanning only fills in the
form: check the server URL, then tap **Pair** and **Send test notification**.
The code can also be typed by hand.

The raw API works too:

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/pairings -H "Authorization: Bearer $NOTIFIER_TOKEN"
# {"code":"7KQ2-MX9D","expires_at":"...","uri":"notifier://pair?server=...&code=7KQ2-MX9D"}
```

### 5. Create an API key and send

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/keys \
  -H "Authorization: Bearer $NOTIFIER_TOKEN" -d '{"name": "laptop"}'
# {"id":"key_...","key":"nk_...", ...}   <- the key is shown only once

export NOTIFIER_URL=https://notifier.joncbender.workers.dev NOTIFIER_API_KEY=nk_...
notify devices
notify send --to pixel "Hello"
```

### 6. Channels

Messages are published to channels, and devices subscribe to the channels
they want. Every user has a `default` channel, and new devices subscribe to
it and cannot leave it, so a message sent without a channel reaches all of
a user's devices (and only theirs: `default` cannot be shared).

```sh
T="Authorization: Bearer $NOTIFIER_TOKEN"; S=https://notifier.joncbender.workers.dev
curl -X POST $S/v1/channels -H "$T" -d '{"name": "homelab"}'          # {"id":"ch_...",...}
curl -X POST $S/v1/channels/ch_.../invites -H "$T"                    # share with another user
curl -X POST $S/v1/notifications -H "$T" -d '{"title": "disk full", "channels": ["homelab"]}'
```

- **Sharing:** the channel's owner invites other users. Each invite admits
  one user (`notifier://join?...`, valid 7 days); a channel has at most 20
  members, owner included. Members choose which of their devices
  subscribe, and only the owner publishes. The owner can remove members
  (`GET /v1/channels/{id}/members` lists them), and members can leave.
- **Scoped keys:** an API key created with `{"name": "ci", "channels":
  ["ci"]}` can publish only to those channels, and cannot address devices
  directly.
- **The payload:** each delivered message carries the channel it came
  through, named as the receiving user sees it (`homelab`, or
  `alice/homelab` for a channel joined from Alice).
- **In the app:** the paired screen lists the channels you belong to, with a
  switch each (`default` is always on), and joins a channel by scanning an
  invite's QR code, opening its `notifier://join` link (it asks first), or
  typing the code. Each channel other than `default` gets its own group in
  Android's notification settings, so it can be muted or given a sound.
  Notifications play notifier's own sounds: one for messages, a more
  insistent one for high priority, and one for questions, each its own
  Android channel. A sound you chose for a channel before is kept.

### Heartbeats

A heartbeat catches the job that silently stops running. The job pings a
secret URL after each successful run; when no ping arrives within the
interval plus a grace period, the server sends one high-priority alert to
the heartbeat's channel, and the next ping announces the recovery:

```sh
notify heartbeat create --every 1d --grace 30m backup   # prints the ping URL, once
# crontab:
0 3 * * *  backup.sh && curl -fsS -m 10 --retry 3 https://…/v1/ping/nh_… >/dev/null
```

A job can also report a failure straight away: `…/fail` alerts at once,
and a POSTed body (`backup.sh 2>&1 | tail -20 | curl -fsS --data-binary @-
…/fail`, up to 1 MiB) becomes the message, cleaned and cut to its last
2048 bytes. A heartbeat that never checks in alerts too, one interval plus
grace after it was created, and so does one that reported a failure and
then stopped running. `notify heartbeats` lists them with their state;
`notify heartbeat delete NAME` removes one. Deleting a channel moves its
heartbeats to `default`.

The URL is the credential, so treat it like an API key. The server checks
every minute; intervals run from 1 minute to 30 days, grace from 1 minute
to a day (default 5 minutes), and each user has up to 20 heartbeats.

### Webhooks

A hook lets a monitoring tool notify you in its own alert format, with no
glue code. Its URL is secret and publishes to one of your channels:

```sh
notify hook create -c alerts alertmanager prometheus   # prints the URL, once
```

```yaml
# alertmanager.yml
receivers:
  - name: notifier
    webhook_configs:
      - url: https://…/v1/hooks/alertmanager/nw_…
```

| Format | For | Title | Priority |
|---|---|---|---|
| `alertmanager` | Prometheus Alertmanager | `[FIRING:2] HostDown` or `[RESOLVED] HostDown` | high when a firing alert's `severity` label is `critical`, `high`, `page` or `error` |
| `grafana` | a Grafana contact point of type Webhook | Grafana's own title | as `alertmanager` |
| `generic` | anything that can POST JSON | `{"title", "body"?, "priority"?}` (`default` or `high`) | as given |

The body lists one line per alert (its `summary` or `description` and
`instance`), ending with "and N more" when they do not fit. Unknown fields
are ignored, and text is cleaned and cut to the usual limits rather than
refused, since a tool cannot be asked to fix it. Payloads may be up to
1 MiB. When devices exist but none took the alert, the hook answers 503 so
the tool retries, which can repeat an alert. `notify hooks` lists them;
`notify hook delete NAME` removes one. Deleting a channel moves its hooks
to `default`. The URL is the credential, so treat it like an API key; each
user has up to 20 hooks, and they count toward the send limit.

### Questions

A script can ask and wait for the answer:

```sh
if [ "$(notify ask -o yes,no "Deploy prod?")" = yes ]; then make deploy; fi
```

The question goes out like any message, with up to three answers (each at
most 32 bytes) shown as buttons on your devices. The first one tapped wins:
`notify ask` prints it on standard output (and who answered on standard
error) and exits 0. It exits 4 when nobody answered within `--wait`
(default 10 minutes, at most 24 hours). Only your own devices may answer;
on a shared channel, `--anyone` lets other members' devices answer too.

Each question carries a one-time key, inside the sealed message. The
device seals the answer under it, so the server holds the answer only in a
form it cannot read, and forgets it an hour after the question expires.
Devices that predate questions show the question without buttons. The
Android app shows the answers as buttons on the notification, and on the
question in its history until it expires; either way, the notification
then says what became of the answer. notifyd shows them too (see
[notifyd](#linux-desktop-notifyd)).

### Receipts

A script can wait until someone has seen a message, and escalate if not:

```sh
notify send --high --wait-seen=5m "Disk full" "/ at 98%" || page-someone
```

`--wait-seen` (default 10 minutes, 10 seconds to 24 hours) asks the devices
to report when they show and open the message, then waits until one opens
it: it prints which (`opened on Pixel`) and exits 0, or exits 4 when none
did in time. If it heard from the server only after the deadline
and only another member's device had opened the message by then, it cannot
tell when, and exits 1. The duration follows `=` (`--wait-seen=5m`): a
separate word is the title. `--receipts` asks for the reports without
waiting. Both print the message ID; `notify status ID` then shows when
each of your devices showed and opened it.

Your own devices always report. Other members' devices on a shared
channel report only as its owner sets it: `notify channel receipts NAME
off` (the default), `optional` (or `on`), or `required`. On an optional
channel each member turns them off or on for all their devices, with
`notify channel receipts NAME off|on` or the switch under the channel in
the app; where the owner requires them, they cannot. You see only how
many of their devices showed and opened a message, but on a channel with
one other member that count tells you when they did.

Opened means an explicit act: tapping the notification, answering its
question, or tapping it in the Android app's history; on a desktop,
clicking the notification (or its "Mark seen" action). dunst's default
left click only closes it; middle-click it, or set `mouse_left_click =
do_action, close_current` in `dunstrc`. A desktop whose notification
service has no actions reports only that it showed the message. Only messages sent this way are reported, and the server keeps
the receipts for a day. Devices that predate receipts never report.

### History

The app keeps the notifications it received, so one dismissed by accident
can still be read. Tap **History** on the paired screen, or tap any
notification. Entries are newest first, with their channel and time. Tap
one to show the whole body or delete it, or use **Clear all**.

The history exists only on that phone. It is encrypted with an Android
Keystore key, excluded from backups, and limited to the last 500
notifications within 30 days. Unpairing clears it. The server never had a
copy: it relays sealed messages and keeps no content.

## `notify` CLI

```
notify send [flags] TITLE [BODY | -]  send
notify ask -o A,B [flags] QUESTION [BODY | -]   ask; prints the answer (see Questions)
notify status ID                      when devices showed and opened it (see Receipts)
notify devices                        list your paired devices
notify pair                           print a pairing QR code
notify signup [--force] INVITE NAME   create a user; saves the token to the config file
notify invite                         mint an invite for someone else
notify config init [--force]          write the config file from --url/--token/--key
notify keys                           list API keys
notify key create [-c CHANNEL]... NAME   print a new API key, optionally scoped
notify key delete NAME|ID
notify channels                       list channels you own or joined
notify channel create|delete|invite|members|leave NAME
notify channel join CODE
notify channel remove NAME MEMBER
notify channel receipts NAME [SETTING]   show or set members' receipts (see Receipts)
notify heartbeats | heartbeat create|delete ...   see Heartbeats
notify hooks | hook create|delete ...             see Webhooks
notify completion bash|zsh|fish|powershell        shell completion script
notify version

send flags:
  --to DEVICE      device name (case-insensitive) or ID; repeatable or
                   comma-separated (a name containing a comma needs its ID)
  -c, --channel C  one of your own channels; repeatable or comma-separated.
                   Without --to or -c: the config file's, else the default
                   channel (every paired device)
  --high           high priority (heads-up, may wake the phone)
  --receipts       ask devices to report when they show and open it; prints
                   its ID for `notify status`
  --wait-seen[=D]  then wait until a device opens it (default 10m, 10s to
                   24h); exits 4 if none does

ask flags: those of send, plus
  -o, --options A  the answers to offer, at most 3; repeatable or comma-separated
  --wait D         how long the question stays open (default 10m, 10s to 24h)
  --anyone         let other channel members' devices answer too

Flags for every command:
  --url URL        server; must be https except for localhost or a loopback
                   IP, and redirects are refused
  --key KEY        API key, for sending and `devices`
  --token TOK      user token, for everything (sending included)
  --timeout D      request timeout (default 30s), counted from when the
                   request starts, not while waiting for stdin
```

Every command has `--help` (`notify pair -h`, `notify hook create --help`).

Settings come from flags, then the environment (`NOTIFIER_URL`,
`NOTIFIER_API_KEY`, `NOTIFIER_TOKEN`), then the config file
`~/.config/notifier/config.json` (`$XDG_CONFIG_HOME` is honoured, and
`$NOTIFIER_CONFIG` names another file). Sending uses an API key if one is
set at the highest level that sets any credential, else the user token.
`NOTIFIER_ADMIN_TOKEN`, the name `notify pair` used before users existed,
is still read as a user token (with a warning), but only when it holds one
(`nu_...`); an operator token there is ignored, never sent.

```json
{
  "url": "https://notifier.joncbender.workers.dev",
  "token": "nu_...",
  "api_key": "nk_...",
  "channels": ["homelab"],
  "to": ["pixel"],
  "priority": "high"
}
```

Every field is optional and unknown ones are rejected. A file holding a
token or key must be readable only by you (`chmod 600`); `notify signup` and
`notify config init` write it that way. `notify signup` replaces the
previous account's `api_key`, `to` and `channels`, which belong to that
account. The desktop daemon (#7) will read
the same file.

A new user's whole setup, with an invite from the operator or another user:

```sh
notify --url https://notifier.joncbender.workers.dev signup 0Z5H-QCEA-3JX4 alice
notify pair                                # scan with the phone
notify channel create homelab
notify key create -c homelab homelab-cron  # an API key that may only post there
notify send -c homelab "disk full"
```

Only `notify send` sends; a first word is always a command, and an unknown
one is an error. The flags of `send` come before TITLE: everything from
TITLE on is message text, so a body taken from a variable can never act as
a flag. Put `--` before a title that starts with `-`
(`notify send -- "$TITLE" "$BODY"` is safe for any title). Long flags take
two dashes since 0.4.0 (`--high`, not `-high`); an old spelling is refused
with a hint.

A `BODY` of `-` reads standard input, so a long job can report when it
finishes: `make test 2>&1 | tail -5 | notify send "Tests" -`. Only the last 8 KiB
of a piped stream is held in memory. Terminal escapes (colours) are
removed, carriage returns become newlines, invalid UTF-8 and other
characters the server refuses are replaced, and text over
the server's limits is shortened and marked with `…`: a title (256 bytes)
keeps its start, a body (2048 bytes) keeps its end, where a log's error
usually is.

Exit status is 0 when every one of your targeted devices accepted the
message. That includes a desktop that is offline: its queue holds the
message, reported as `queued`, so a desktop abandoned without unpairing
still counts as reached (unpair it with `DELETE /v1/devices/{id}`). Exit
status is 3 when nothing reached one of them (a delivery failed, a phone
was unreachable or unpaired before sending, or no phone matched or
subscribed), or when the message reached no device at all; 1 on other
errors; and 2 on usage errors. `notify ask` also exits 4 when its question
expired unanswered, and `notify send --wait-seen` when no device opened the
message in time; with `--wait-seen`, the exit status says only whether it
was seen. Other members' devices on a shared channel
are reported as counts and do not affect the exit status unless none got
the message.

Install a tagged release with
`go install github.com/joncbenderkh/notifier/notify@latest` (the module lives
in a subdirectory, so its versions are the `notify/vX.Y.Z` tags), or download
a prebuilt binary from the
[releases](https://github.com/joncbenderkh/notifier/releases).

## Linux desktop: `notifyd`

`notifyd` pairs a Linux desktop like a phone and shows each message through
the desktop's notification service (`org.freedesktop.Notifications`). FCM
does not reach desktops, so it receives over the server's
[WebSocket transport](#websocket-transport). Messages sent while the
machine is asleep or offline are queued for up to a day.

```sh
go install github.com/joncbenderkh/notifier/notifyd@latest
notify pair                       # prints the QR code and a notifier://pair link
notifyd pair 'notifier://pair?server=...&code=...'   # or: notifyd pair --server URL CODE
install -Dm644 notifyd/notifyd.service ~/.config/systemd/user/notifyd.service
systemctl --user enable --now notifyd
notifyd test                      # sends this desktop a test notification
notifyd history                   # what it received, newest first (-n COUNT, --json)
notifyd reply ID ANSWER           # answers a question from the history
```

The unit runs `~/go/bin/notifyd`, where `go install` puts it; with a
downloaded binary from the releases, edit `ExecStart` to point at it.

The pairing is kept in `~/.config/notifier/notifyd.json`, mode 0600 (set
`NOTIFYD_STATE` to move it). `notifyd unpair` removes it, both from the
server and locally.

`notifyd history` lists the messages this desktop received, so one that
was dismissed can still be read, including one the notification service
refused to show. The list is limited to the last 500 messages within 30
days. It lives only on this machine, in
`~/.local/state/notifier/notifyd-history.jsonl` with mode 0600 (set
`NOTIFYD_HISTORY` to move it). `notifyd history delete ID…` removes
messages, `notifyd history clear` empties it, and pairing or unpairing
clears it too.

`notifyd-history` shows the history in a window (GTK 4 and libadwaita
1.9 or later): the list beside the selected message, search, a channel
filter, delete with undo, and clear all. It follows new messages as they
arrive. Building it needs the libadwaita headers (`dnf install
libadwaita-devel` on Fedora):

```sh
go install github.com/joncbenderkh/notifier/notifyd/cmd/notifyd-history@latest
```

The unit starts with `graphical-session.target`. On a window manager that
does not reach that target (plain i3 or sway), use `default.target` in the
unit instead; systemd restarts `notifyd` until the notification daemon is
up.

A question (`notify ask`) shows its answers as notification actions, if
the notification service supports them (dunst lists them in its context
menu, GNOME and KDE as buttons). It stays until answered or until the
question closes; once answered, it is replaced by the outcome ("Answered:
yes", "Already answered"). Without actions, or after a restart of
`notifyd`, which forgets the buttons of notifications already shown,
answer with `notifyd reply ID ANSWER`: `notifyd history` shows the ID and
the answers.

A message sent with receipts (see [Receipts](#receipts)) is reported shown
once the notification service takes it, and opened when you click it,
which needs a service with actions.

High-priority messages are shown with critical urgency. A message sent to a
channel carries the category `notifier.<channel>`, so notification daemon
rules can treat channels differently. For dunst:

```ini
[notifier-homelab]
category = notifier.homelab
urgency = low
```

Each message names one of notifier's sounds in the `sound-file` hint: one
for messages, a more insistent one for high priority, and one for open
questions. `notifyd run` writes them to `$XDG_CACHE_HOME/notifier/sounds/`.
The outcome of an answer is silent. Services that play sounds use the
hint; others, such as dunst, ignore it. `notifyd run --no-sound` leaves the
hint out, so the service's own sound settings apply.

For i3blocks, `notifyd/contrib/i3blocks/` has a status bar block:
- `NTF 3` counts the messages the history window has not listed yet, and
  turns red as `NTF off` when the service is not running.
- Left-click opens the history window: `notifyd-history` when installed,
  else a simpler `zenity` one (`show-history`).
- Middle-click restarts the service.

It needs `jq`, and `zenity` for the fallback window:

```sh
mkdir -p ~/.config/i3blocks/notifier
cp notifyd/contrib/i3blocks/{notifier,show-history} ~/.config/i3blocks/notifier/
```

```ini
# ~/.config/i3blocks/config, with command=$SCRIPT_DIR/$BLOCK_NAME/$BLOCK_NAME
[notifier]
interval=5
```

The service runs `notifyd run`. A second `notifyd run` with the same state
file refuses to start while it is running, since the server keeps one
connection per device and would hand it to the newcomer; `notifyd` alone
only prints its usage.

`notifyd` stops, and systemd does not restart it, in three cases:
- the device was unpaired on the server (exit status 4); pair it again
  with `notifyd pair --force LINK`;
- another `notifyd` connected as the same device (exit status 5);
- it is not paired at all (exit status 4).

A message the notification service refuses outright is logged and
dropped, so it cannot hold up the ones behind it. A message in a newer
envelope format stays queued on the server until `notifyd` is updated.

## API

Credentials: **operator** (`ADMIN_TOKEN`), **user** (`nu_`, from sign-up),
**API key** (`nk_`, a user's, for scripts), **device** (`nd_`, a paired
phone's). Everything a user or API key touches is scoped to that user;
another user's device or key IDs answer 404.

| Method & path                     | Auth                 | Body / notes |
|-----------------------------------|----------------------|--------------|
| `POST /v1/notifications`          | User or API key      | `{"title", "body"?, "priority"?: "default"\|"high", "channels"?: [names], "device_ids"?: [...]}` — reaches devices subscribed to the named channels (the caller's own) plus the listed devices, each once; with neither, the `default` channel. Returns per-device delivery status for the sender's own devices, and `members` (counts by status) for other members'. At most 60 a minute per user (429 after). An optional `"question": {"options": [1–3 answers], "key": <32 bytes, base64>, "expires_in"?: 10–86400 (600), "anyone"?: false}` asks for an answer: see [Replies](#replies). `"receipts": true` asks the devices to report when they show and open it. |
| `GET /v1/notifications/{message id}?wait=N` | User or API key | For a message sent with `receipts`, within a day: waits up to N seconds (0–50) until a device opens it; `{"id", "status": "pending"\|"opened", "devices": [{"device_id", "name", "shown_at"?, "opened_at"?}], "members": {"shown", "opened"}}`, the sender's own devices listed and other members' counted |
| `GET /v1/replies/{message id}?wait=N` | User or API key  | Waits up to N seconds (0–50) for the answer; `{"status": "pending"\|"expired"}` or `{"status": "answered", "by", "replied_at", "n", "c"}` |
| `GET /v1/devices`                 | User or API key      | `transport` (`fcm` or `ws`); activity timestamps only with a user token |
| `POST /v1/pairings`               | User                 | Returns a single-use code valid for 10 minutes and its `notifier://pair` link (`uri`); `?qr=svg` adds `qr_svg`. At most 20 devices and 5 unused codes per user. |
| `GET /pair`                       | —                    | Static page that calls `POST /v1/pairings` with a pasted user token and shows the QR code (`/admin/pair` redirects here) |
| `DELETE /v1/devices/{id}`         | User                 | |
| `POST /v1/keys`                   | User                 | `{"name", "channels"?: [names]}`; channels limit what the key may publish to. At most 20 per user |
| `GET /v1/keys`                    | User                 | |
| `DELETE /v1/keys/{id}`            | User                 | |
| `GET /v1/user`                    | User                 | Who am I |
| `POST /v1/channels`               | User                 | `{"name"}` (`[a-z0-9][a-z0-9_-]*`, ≤32); at most 20 besides `default`, and 50 owned or joined, per user |
| `GET /v1/channels`                | User                 | Channels owned or joined, each with `receipts` (`off`, `optional`, `required`) and, for joined ones, `receipts_declined` |
| `PATCH /v1/channels/{id}`         | User (owner)         | `{"receipts": "off"\|"optional"\|"required"}`: whether other members' devices report receipts; not `default` |
| `DELETE /v1/channels/{id}`        | User (owner)         | Not `default` |
| `POST /v1/channels/{id}/invites`  | User (owner)         | Not `default`; single-use, 7 days; returns `uri` (`notifier://join?...`), `?qr=svg` adds `qr_svg` |
| `POST /v1/memberships`            | User                 | `{"invite"}`: join a channel |
| `PATCH /v1/memberships/{channel id}` | User (member)   | `{"receipts_declined": bool}`: decline receipts on an optional channel (409 where required) |
| `GET /v1/channels/{id}/members`   | User (owner)         | Members, with `id`, `name`, `joined_at` |
| `DELETE /v1/channels/{id}/members/{user_id}` | User (owner, or the member leaving) | Also unsubscribes that member's devices |
| `PUT /v1/devices/{id}/channels`   | User                 | `{"channel_ids"}`: set a device's subscriptions (must include `default`) |
| `POST /v1/invites`                | User or operator     | Single-use code valid for 7 days |
| `POST /v1/users`                  | Invite               | `{"invite", "name"}`; returns the new user's token, once |
| `GET /v1/users`                   | Operator             | |
| `POST /v1/users/{id}/token`       | Operator             | Issues a new token, revoking the old one |
| `POST /v1/users/{id}/disable`     | Operator             | Rejects the user's token, API keys, and devices; `/enable` undoes it |
| `POST /v1/devices`                | Pairing code         | Used by the app: `{"code", "name", "fcm_token"}`; the Linux daemon sends `{"code", "name", "transport": "ws"}` instead |
| `PUT /v1/device`                  | Device               | Used by the app: `{"fcm_token"}` (409 for a `ws` device) |
| `GET /v1/device/stream`           | Device (`ws`)        | WebSocket upgrade: see [WebSocket transport](#websocket-transport) |
| `POST /v1/device/test`            | Device               | Used by the app |
| `DELETE /v1/device`               | Device               | Used by the app (unpair) |
| `GET /v1/device/channels`         | Device               | Channels its owner can subscribe to, with `subscribed` |
| `PUT /v1/device/channels`         | Device               | `{"channel_ids"}` (must include `default`) |
| `POST /v1/device/memberships`     | Device               | `{"invite"}`: its owner joins, and the device subscribes |
| `PATCH /v1/device/memberships/{channel id}` | Device   | As `PATCH /v1/memberships/{channel id}`, for its owner |
| `POST /v1/replies/{message id}`   | Device               | `{"n", "c"}`: the sealed answer to a question the device may answer. 201; 409 if already answered, 410 if expired. Counts as opening it |
| `POST /v1/device/receipts`        | Device               | `{"message_id", "state": "shown"\|"opened"}` for a message sent with `receipts` (its payload has `"receipts": true`); the first time of each stays. 204; 404 if it is not tracked or did not reach this device |
| `POST /v1/heartbeats`             | User                 | `{"name", "interval", "grace"?, "channel"?}` (seconds; grace defaults to 300, channel to `default`). Returns `ping_url`, once. At most 20 per user |
| `GET /v1/heartbeats`              | User                 | With `state` (`up`, `down`, `failed`), `last_ping_at`, `due_at` |
| `DELETE /v1/heartbeats/{id}`      | User                 | |
| `GET`/`POST /v1/ping/{secret}`    | The URL              | A check-in; announces a recovery from `down` or `failed` |
| `GET`/`POST /v1/ping/{secret}/fail` | The URL            | Reports a failure at once; a POSTed body becomes the message |
| `POST /v1/hooks`                  | User                 | `{"name", "format", "channel"?}` (`alertmanager`, `grafana` or `generic`; channel defaults to `default`). Returns `url`, once. At most 20 per user |
| `GET /v1/hooks`                   | User                 | With `format`, `channel`, `last_used_at` |
| `DELETE /v1/hooks/{id}`           | User                 | |
| `POST /v1/hooks/{format}/{secret}` | The URL             | An alert in the hook's format; 503 asks the tool to retry |
| `GET /healthz`                    | —                    | |

Limits: title 256 bytes, body 2048 bytes. Titles and bodies may not
contain control characters other than newline and tab, nor bidi
embeddings, overrides or isolates (U+202A–U+202E, U+2066–U+2069); names
of users, devices and keys, once their surrounding whitespace is trimmed,
may not contain any of them. Such a request
answers 400. A message for a phone must also fit FCM's 4 KiB once
escaped as JSON and sealed, which text full of quotes or backslashes, or a
question's answers, can exceed; that also answers 400 (the desktop's
WebSocket transport has no such limit).

### WebSocket transport

FCM does not reach Linux desktops, so a device paired with
`"transport": "ws"` receives over a WebSocket instead
(`GET /v1/device/stream`, with its device token as the Bearer). Each such
device has a queue on the server. Every message is queued, and also sent
right away when the device is connected, as a text frame:

```json
{"type": "message", "id": "msg_...", "v": "1", "n": "<base64 nonce>", "c": "<base64 ciphertext>"}
```

`v`, `n` and `c` are the same sealed envelope that FCM carries. The device
answers `{"ack": "msg_..."}` once it has shown the message, which removes
it from the queue. Messages are not acknowledged until shown, so a message
can arrive twice; the device drops IDs it has already seen.

On connect, the server replays everything still queued, oldest first.
Queued messages expire after 24 hours, and a queue holds at most the 200
newest. While the device is offline, its deliveries report `queued`
instead of `sent`.

The text frame `ping` is answered with `pong`, for keepalives. A newer
connection for the same device closes the older one with code 4000.
Unpairing closes the connection with code 4001 and drops the queue; if
that fails, the unpair answers 503 and can be retried.

### Replies

A message sent with `question` carries, for each device allowed to answer
(the sender's own, and with `anyone` other members' too), an extra field
in its sealed payload:

```json
"reply": {"options": ["yes", "no"], "key": "<base64, 32 bytes>", "expires_at": 1791590400}
```

The device answers with `POST /v1/replies/{message id}`, sealing
`{"answer": "yes"}` with AES-256-GCM under `key`, a fresh 12-byte nonce,
and `reply:` + the message ID as associated data; `n` and `c` are the
base64 nonce and ciphertext (with its tag). The first answer wins. The
server cannot open it: the asker, who chose the key, opens it from
`GET /v1/replies/{message id}`, where `by` names the answering device (or,
for another member's, that member).

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
