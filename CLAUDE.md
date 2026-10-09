# CLAUDE.md

Guidance for Claude Code (claude.ai/code) when working in this repository.

## What notifier is

A self-hosted service plus Android app for sending authenticated push
notifications to the user's own phone via an HTTP API. A monorepo:

- `server/` — Go HTTP API. Issues credentials, pairs devices, seals payloads,
  and hands them to FCM.
- `android/` — Kotlin + Jetpack Compose app. Pairs with a server, receives FCM
  data messages, decrypts them, and posts notifications.

See `README.md` for the user-facing setup and API table.

## Decisions

- **License:** GPL-3.0-or-later. `LICENSE` at repo root;
  `SPDX-License-Identifier: GPL-3.0-or-later` as the first line of every `.go`
  and `.kt` file.
- **Server stack:** Go (stdlib `net/http` with method/path patterns),
  SQLite via the pure-Go `modernc.org/sqlite` (so `CGO_ENABLED=0` static
  builds work), Firebase Admin SDK (`firebase.google.com/go/v4`) for FCM.
  Configuration is environment-only (see `cmd/notifier/main.go` doc comment).
- **Android stack:** Kotlin + Jetpack Compose, AGP 9 with built-in Kotlin
  (no `org.jetbrains.kotlin.android` plugin), Gradle Kotlin DSL with a version
  catalog. `minSdk = 26`, `compileSdk = 37.2`, `targetSdk = 37`. OkHttp +
  kotlinx.serialization for HTTP; no DI framework at this size.
- **Delivery:** Firebase Cloud Messaging, **data-only** messages (a
  `notification` block would be rendered by the system without the app
  decrypting it).
- **Firebase client config** is injected at build time from Gradle properties
  `notifier.firebase.{projectId,appId,apiKey,senderId}` or
  `NOTIFIER_FIREBASE_*` env vars into `BuildConfig`, and `NotifierApp`
  initializes `FirebaseApp` manually. There is deliberately no
  `google-services.json` and no google-services Gradle plugin, so the app
  builds (but cannot pair) without Firebase config — CI relies on this.
- **Credentials:** admin token (env var), API keys (`nk_` prefix), device
  tokens (`nd_` prefix), pairing codes (Crockford base32 `XXXX-XXXX`, 10 min,
  single use). Secrets are stored only as unsalted SHA-256 digests — fine
  because they are 256-bit random; don't reuse that scheme for anything
  low-entropy.
- **Wire format (envelope v1):** FCM data `{"v":"1","n":<b64 nonce>,"c":<b64
  ciphertext>}`; AES-256-GCM, 12-byte nonce, 128-bit tag, per-device key,
  AAD = device ID. Plaintext is the JSON `api.Payload` / Kotlin `Payload`.
  A shared test vector in `server/internal/seal/seal_test.go` and
  `android/.../push/SealedMessageTest.kt` pins both sides — change the format
  only by bumping `v` and updating both.
- **Phone-side secret storage:** `PairingStore` encrypts the pairing JSON with
  an AES-GCM Android Keystore key; backups and device transfer are disabled
  for it (a restored copy could not be decrypted anyway).
- **Application id / package:** `com.joncbenderkh.notifier`.
- **Schema migrations:** `store.migrations` is append-only, tracked with
  SQLite `PRAGMA user_version`. Never edit a shipped migration.

## Layout

- `server/cmd/notifier` — entry point, env config, graceful shutdown.
- `server/internal/api` — handlers and auth middleware; `api_test.go` drives
  the whole flow over `httptest` with a fake `push.Sender`.
- `server/internal/store` — SQLite persistence.
- `server/internal/seal` — envelope encryption.
- `server/internal/token` — secret/ID/pairing-code generation and hashing.
- `server/internal/push` — `Sender` interface, FCM implementation, and a `Log`
  sender (`NOTIFIER_PUSH=log`) for development.
- `android/app/src/main/java/com/joncbenderkh/notifier/`
  - `data/` — `PairingStore`, `ServerClient` (blocking; callers pick the thread).
  - `push/` — `PushService` (FCM), `SealedMessage` (pure JVM, unit-tested),
    `Notifications` (channels `default` and `high`).
  - `ui/` — `MainViewModel`, `NotifierScreen`, theme.

## Commands

Server (from `server/`):

- Build: `go build ./...`
- Test: `go test -race ./...`
- Lint: `gofmt -l .` (must print nothing) and `go vet ./...`
- Run locally without Firebase: `NOTIFIER_ADMIN_TOKEN=<32+ chars> NOTIFIER_PUSH=log go run ./cmd/notifier`

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
`versionName` in `android/app/build.gradle.kts` and `version` in
`server/cmd/notifier/main.go` must match; the tag `vX.Y.Z` mirrors them
(release builds also inject it into the server via `-ldflags -X`).
`versionCode` increments by one each release.

## Working on notifier

- Never commit to `main`. Use a worktree per change (`gwa <branch> <comment>`,
  siblings of `main/`), push the branch, open a PR, and report the PR URL.
- Conventional Commits, imperative, ≤50-char subject, no attribution lines.
- English only.
