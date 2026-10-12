# notifier

Send authenticated, end-to-end encrypted push notifications to your Android
phone with one command or one HTTP call.

```sh
notify -to pixel -high "Backup finished" "2.4 GB in 3m12s"
```

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/notifications \
  -H "Authorization: Bearer $NOTIFIER_API_KEY" \
  -d '{"title": "Backup finished", "body": "2.4 GB in 3m12s", "priority": "high"}'
```

- **`server/`** — a Cloudflare Worker (TypeScript, Hono, D1) that issues
  credentials, pairs phones, and relays notifications through Firebase Cloud
  Messaging.
- **`android/`** — a Kotlin + Jetpack Compose app that pairs with the server,
  decrypts incoming messages, and shows them as notifications.
- **`notify/`** — `notify`, a single Go binary for sending from scripts and
  terminals.

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
- **Terminal:** `NOTIFIER_ADMIN_TOKEN=nu_... notify pair` prints the QR code.

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
notify -to pixel "Hello"
```

### 6. Channels

Messages are published to channels, and devices subscribe to the channels
they want. Every user has a `default` channel, and new devices subscribe to
it, so a message sent without a channel reaches all of a user's devices
(and only theirs: `default` cannot be shared).

```sh
T="Authorization: Bearer $NOTIFIER_TOKEN"; S=https://notifier.joncbender.workers.dev
curl -X POST $S/v1/channels -H "$T" -d '{"name": "homelab"}'          # {"id":"ch_...",...}
curl -X POST $S/v1/channels/ch_.../invites -H "$T"                    # share with another user
curl -X POST $S/v1/notifications -H "$T" -d '{"title": "disk full", "channels": ["homelab"]}'
```

- **Sharing:** the channel's owner invites other users. Each invite admits
  one user (`notifier://join?...`, valid 7 days). Members choose which of
  their devices subscribe, and only the owner publishes. The owner can remove
  members (`GET /v1/channels/{id}/members` lists them), and members can
  leave.
- **Scoped keys:** an API key created with `{"name": "ci", "channels":
  ["ci"]}` can publish only to those channels, and cannot address devices
  directly.
- **The payload:** each delivered message carries the channel it came
  through, named as the receiving user sees it (`homelab`, or
  `alice/homelab` for a channel joined from Alice).

## `notify` CLI

```
notify [flags] TITLE [BODY | -]
notify devices
notify pair        # needs a user token in NOTIFIER_ADMIN_TOKEN; prints a pairing QR code
notify version

  -to PHONE   device name (case-insensitive) or ID; repeatable or
              comma-separated (a name containing a comma needs its ID).
              Default: every paired phone.
  -high       high priority (heads-up, may wake the phone)
  -url URL    server (default $NOTIFIER_URL); must be https except for
              localhost or a loopback IP, and redirects are refused
  -key KEY    API key (default $NOTIFIER_API_KEY)
  -timeout    request timeout (default 30s), counted from when the request
              starts, not while waiting for stdin
```

Flags must come before TITLE; everything from TITLE on is message text, so a
body taken from a variable can never act as a flag. Put `--` before a title
that starts with `-` or is exactly `devices`, `pair`, or `version`.

A `BODY` of `-` reads standard input, so a long job can report when it
finishes: `make test 2>&1 | tail -5 | notify "Tests" -`. Only the last 8 KiB
of a piped stream is held in memory. Invalid UTF-8 is replaced, and text over
the server's limits is shortened and marked with `…`: a title (256 bytes)
keeps its start, a body (2048 bytes) keeps its end, where a log's error
usually is.

Exit status is 0 when every targeted phone accepted the message; 3 when
nothing reached some phone (a delivery failed, a phone was unreachable or
unpaired before sending, or no paired phone matched, with or without `-to`);
1 on other errors; and 2 on usage errors.

Install a tagged release with
`go install github.com/joncbenderkh/notifier/notify@latest` (the module lives
in a subdirectory, so its versions are the `notify/vX.Y.Z` tags), or download
a prebuilt binary from the
[releases](https://github.com/joncbenderkh/notifier/releases).

## API

Credentials: **operator** (`ADMIN_TOKEN`), **user** (`nu_`, from sign-up),
**API key** (`nk_`, a user's, for scripts), **device** (`nd_`, a paired
phone's). Everything a user or API key touches is scoped to that user;
another user's device or key IDs answer 404.

| Method & path                     | Auth                 | Body / notes |
|-----------------------------------|----------------------|--------------|
| `POST /v1/notifications`          | User or API key      | `{"title", "body"?, "priority"?: "default"\|"high", "channels"?: [names], "device_ids"?: [...]}` — reaches devices subscribed to the named channels (the caller's own) plus the listed devices, each once; with neither, the `default` channel. Returns per-device delivery status. At most 60 a minute per user (429 after). |
| `GET /v1/devices`                 | User or API key      | Activity timestamps only with a user token |
| `POST /v1/pairings`               | User                 | Returns a single-use code valid for 10 minutes and its `notifier://pair` link (`uri`); `?qr=svg` adds `qr_svg`. At most 20 devices and 5 unused codes per user. |
| `GET /pair`                       | —                    | Static page that calls `POST /v1/pairings` with a pasted user token and shows the QR code (`/admin/pair` redirects here) |
| `DELETE /v1/devices/{id}`         | User                 | |
| `POST /v1/keys`                   | User                 | `{"name", "channels"?: [names]}`; channels limit what the key may publish to. At most 20 per user |
| `GET /v1/keys`                    | User                 | |
| `DELETE /v1/keys/{id}`            | User                 | |
| `GET /v1/user`                    | User                 | Who am I |
| `POST /v1/channels`               | User                 | `{"name"}` (`[a-z0-9][a-z0-9_-]*`, ≤32); at most 20 per user |
| `GET /v1/channels`                | User                 | Channels owned or joined |
| `DELETE /v1/channels/{id}`        | User (owner)         | Not `default` |
| `POST /v1/channels/{id}/invites`  | User (owner)         | Not `default`; single-use, 7 days; returns `uri` (`notifier://join?...`), `?qr=svg` adds `qr_svg` |
| `POST /v1/memberships`            | User                 | `{"invite"}`: join a channel |
| `GET /v1/channels/{id}/members`   | User (owner)         | Members, with `id`, `name`, `joined_at` |
| `DELETE /v1/channels/{id}/members/{user_id}` | User (owner, or the member leaving) | Also unsubscribes that member's devices |
| `PUT /v1/devices/{id}/channels`   | User                 | `{"channel_ids"}`: set a device's subscriptions |
| `POST /v1/invites`                | User or operator     | Single-use code valid for 7 days |
| `POST /v1/users`                  | Invite               | `{"invite", "name"}`; returns the new user's token, once |
| `GET /v1/users`                   | Operator             | |
| `POST /v1/users/{id}/token`       | Operator             | Issues a new token, revoking the old one |
| `POST /v1/users/{id}/disable`     | Operator             | Rejects the user's token, API keys, and devices; `/enable` undoes it |
| `POST /v1/devices`                | Pairing code         | Used by the app: `{"code", "name", "fcm_token"}` |
| `PUT /v1/device`                  | Device               | Used by the app: `{"fcm_token"}` |
| `POST /v1/device/test`            | Device               | Used by the app |
| `DELETE /v1/device`               | Device               | Used by the app (unpair) |
| `GET /v1/device/channels`         | Device               | Channels its owner can subscribe to, with `subscribed` |
| `PUT /v1/device/channels`         | Device               | `{"channel_ids"}` |
| `POST /v1/device/memberships`     | Device               | `{"invite"}`: its owner joins, and the device subscribes |
| `GET /healthz`                    | —                    | |

Limits: title 256 bytes, body 2048 bytes.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
