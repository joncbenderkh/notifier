# notifier

Send authenticated, end-to-end encrypted push notifications to your Android
phone with one HTTP call.

```sh
curl -X POST https://notifier.example.com/v1/notifications \
  -H "Authorization: Bearer $NOTIFIER_API_KEY" \
  -d '{"title": "Backup finished", "body": "2.4 GB in 3m12s", "priority": "high"}'
```

- **`server/`** — a single Go binary (SQLite, no other services) that issues
  credentials, pairs phones, and relays notifications through Firebase Cloud
  Messaging.
- **`android/`** — a Kotlin + Jetpack Compose app that pairs with the server,
  decrypts incoming messages, and shows them as notifications.

## Security model

| Credential   | Held by                     | Allows                                         |
|--------------|-----------------------------|------------------------------------------------|
| Admin token  | Server operator (env var)   | Create pairing codes and API keys; list/remove devices and keys |
| API key      | Each sending script/service | `POST /v1/notifications` only                  |
| Device token | The paired phone            | Update its own FCM token, send itself a test, unpair |
| Pairing code | Operator → phone, once      | Register one device, within 10 minutes         |

API keys and device tokens are 256-bit random secrets stored server-side only
as SHA-256 digests, and each can be revoked independently.

Every notification is sealed with **AES-256-GCM under a per-device key**
generated at pairing time, with the device ID as associated data. FCM only
ever carries ciphertext, so Google can neither read nor forge messages, and a
payload sealed for one device will not open on another. On the phone, the
pairing secrets are encrypted with an Android Keystore key and excluded from
backups.

Run the server behind TLS (a reverse proxy such as Caddy); release builds of
the app refuse plain-HTTP servers.

## Setup

### 1. Firebase

1. Create a Firebase project and add an Android app with package
   `com.joncbenderkh.notifier`.
2. **Server:** Project settings → Service accounts → generate a private key
   (JSON). Point `GOOGLE_APPLICATION_CREDENTIALS` at it.
3. **App:** from the Android app's `google-services.json`, put these in
   `~/.gradle/gradle.properties` (or export them as `NOTIFIER_FIREBASE_*`
   environment variables):

   ```properties
   notifier.firebase.projectId=<project_info.project_id>
   notifier.firebase.appId=<client[0].client_info.mobilesdk_app_id>
   notifier.firebase.apiKey=<client[0].api_key[0].current_key>
   notifier.firebase.senderId=<project_info.project_number>
   ```

### 2. Server

```sh
export NOTIFIER_ADMIN_TOKEN=$(openssl rand -hex 32)
export GOOGLE_APPLICATION_CREDENTIALS=/path/to/service-account.json
cd server && go run ./cmd/notifier
```

| Variable                         | Default       |                                         |
|----------------------------------|---------------|-----------------------------------------|
| `NOTIFIER_ADMIN_TOKEN`           | — (required)  | At least 32 characters                  |
| `NOTIFIER_LISTEN`                | `:8080`       |                                         |
| `NOTIFIER_DB`                    | `notifier.db` | SQLite file                             |
| `NOTIFIER_PUSH`                  | `fcm`         | `log` prints instead of delivering (development) |
| `GOOGLE_APPLICATION_CREDENTIALS` | —             | Required for `fcm`                      |

Or with Docker:

```sh
docker build -t notifier server
docker run -d -p 8080:8080 -v notifier-data:/data \
  -e NOTIFIER_ADMIN_TOKEN -e GOOGLE_APPLICATION_CREDENTIALS=/creds.json \
  -v /path/to/service-account.json:/creds.json:ro notifier
```

### 3. Pair the phone

```sh
curl -X POST https://notifier.example.com/v1/pairings -H "Authorization: Bearer $NOTIFIER_ADMIN_TOKEN"
# {"code":"7KQ2-MX9D","expires_at":"..."}
```

Enter the server URL and code in the app, then tap **Send test notification**.

### 4. Create an API key

```sh
curl -X POST https://notifier.example.com/v1/keys \
  -H "Authorization: Bearer $NOTIFIER_ADMIN_TOKEN" -d '{"name": "backup-script"}'
# {"id":"key_...","key":"nk_...", ...}   <- the key is shown only once
```

## API

| Method & path                | Auth   | Body / notes |
|------------------------------|--------|--------------|
| `POST /v1/notifications`     | API key | `{"title", "body"?, "priority"?: "default"\|"high", "device_ids"?: [...]}` — omitting `device_ids` sends to every paired device. Returns per-device delivery status. |
| `POST /v1/pairings`          | Admin  | Returns a single-use code valid for 10 minutes |
| `GET /v1/devices`            | Admin  | |
| `DELETE /v1/devices/{id}`    | Admin  | |
| `POST /v1/keys`              | Admin  | `{"name"}` |
| `GET /v1/keys`               | Admin  | |
| `DELETE /v1/keys/{id}`       | Admin  | |
| `POST /v1/devices`           | Pairing code | Used by the app: `{"code", "name", "fcm_token"}` |
| `PUT /v1/device`             | Device | Used by the app: `{"fcm_token"}` |
| `POST /v1/device/test`       | Device | Used by the app |
| `DELETE /v1/device`          | Device | Used by the app (unpair) |
| `GET /healthz`               | —      | |

Limits: title 256 bytes, body 2048 bytes.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
