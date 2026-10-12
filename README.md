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
| Admin token  | Server operator (Worker secret) | Create pairing codes and API keys; list/remove devices and keys |
| API key      | Each sending script/service     | Send notifications; list paired devices (IDs and names) |
| Device token | The paired phone                | Update its own FCM token, send itself a test, unpair |
| Pairing code | Operator → phone, once          | Register one device, within 10 minutes         |

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
openssl rand -hex 32 | tee /dev/stderr | npx wrangler secret put ADMIN_TOKEN
npx wrangler secret put FCM_SERVICE_ACCOUNT < /path/to/notifier-firebase-adminsdk.json
npx wrangler deploy
```

After that, `.github/workflows/deploy.yml` redeploys on every push to `main`
that touches `server/`, given `CLOUDFLARE_API_TOKEN` and
`CLOUDFLARE_ACCOUNT_ID` repository secrets.

Local development: copy `server/.dev.vars.example` to `server/.dev.vars`,
then `npm run db:migrate:local && npm run dev`. `PUSH_TRANSPORT=log` logs
messages instead of delivering them.

### 3. Pair the phone

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/pairings -H "Authorization: Bearer $ADMIN_TOKEN"
# {"code":"7KQ2-MX9D","expires_at":"..."}
```

Enter the server URL and code in the app, then tap **Send test notification**.

### 4. Create an API key and send

```sh
curl -X POST https://notifier.joncbender.workers.dev/v1/keys \
  -H "Authorization: Bearer $ADMIN_TOKEN" -d '{"name": "laptop"}'
# {"id":"key_...","key":"nk_...", ...}   <- the key is shown only once

export NOTIFIER_URL=https://notifier.joncbender.workers.dev NOTIFIER_API_KEY=nk_...
notify devices
notify -to pixel "Hello"
```

## `notify` CLI

```
notify [flags] TITLE [BODY | -]
notify devices
notify version

  -to PHONE   device name (case-insensitive) or ID; repeatable or
              comma-separated. Default: every paired phone.
  -high       high priority (heads-up, may wake the phone)
  -url URL    server (default $NOTIFIER_URL)
  -key KEY    API key (default $NOTIFIER_API_KEY)
  -timeout    request timeout (default 30s), counted from when the request
              starts, not while waiting for stdin
```

Flags must come before TITLE; everything from TITLE on is message text, so a
body taken from a variable can never act as a flag. Put `--` before a title
that starts with `-`. A `BODY` of `-` reads standard input, so a long job can
report when it finishes: `make test 2>&1 | tail -5 | notify "Tests" -`.
Bodies over the server's 2048-byte limit keep their last 2048 bytes (where a
log's error usually is), prefixed with `…`; invalid UTF-8 is replaced, and
only the last 8 KiB of a piped stream is ever held in memory. The server URL must be `https`,
except for `localhost`.

Exit status is 0 when every phone accepted the message; 3 when any delivery
failed, a phone was unreachable, or no paired phone matched; 1 on other
errors; and 2 on usage errors.

Install a tagged release with
`go install github.com/joncbenderkh/notifier/notify@latest` (the module lives
in a subdirectory, so its versions are the `notify/vX.Y.Z` tags), or download
a prebuilt binary from the
[releases](https://github.com/joncbenderkh/notifier/releases).

## API

| Method & path                | Auth             | Body / notes |
|------------------------------|------------------|--------------|
| `POST /v1/notifications`     | API key          | `{"title", "body"?, "priority"?: "default"\|"high", "device_ids"?: [...]}` — omitting `device_ids` sends to every paired device. Returns per-device delivery status. |
| `GET /v1/devices`            | Admin or API key | |
| `POST /v1/pairings`          | Admin            | Returns a single-use code valid for 10 minutes |
| `DELETE /v1/devices/{id}`    | Admin            | |
| `POST /v1/keys`              | Admin            | `{"name"}` |
| `GET /v1/keys`               | Admin            | |
| `DELETE /v1/keys/{id}`       | Admin            | |
| `POST /v1/devices`           | Pairing code     | Used by the app: `{"code", "name", "fcm_token"}` |
| `PUT /v1/device`             | Device           | Used by the app: `{"fcm_token"}` |
| `POST /v1/device/test`       | Device           | Used by the app |
| `DELETE /v1/device`          | Device           | Used by the app (unpair) |
| `GET /healthz`               | —                | |

Limits: title 256 bytes, body 2048 bytes.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
