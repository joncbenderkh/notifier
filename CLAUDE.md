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
- `notify/` — Go CLI that sends notifications with an API key.

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
- **CLI stack:** Go stdlib only (`flag`, `net/http`). `-to` accepts names or
  IDs; names resolve through `GET /v1/devices`, which API keys may call.
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
- **Credentials:** admin token (Worker secret), API keys (`nk_` prefix), device
  tokens (`nd_` prefix), pairing codes (Crockford base32 `XXXX-XXXX`, 10 min,
  single use). Secrets are stored only as unsalted SHA-256 digests — fine
  because they are 256-bit random; don't reuse that scheme for anything
  low-entropy.
- **Wire format (envelope v1):** FCM data `{"v":"1","n":<b64 nonce>,"c":<b64
  ciphertext>}`; AES-256-GCM, 12-byte nonce, 128-bit tag, per-device key,
  AAD = device ID. Plaintext is the JSON `Payload` (`server/src/app.ts`,
  Kotlin `Payload`). A shared test vector in `server/test/seal.test.ts` and
  `android/.../push/SealedMessageTest.kt` pins both sides — change the format
  only by bumping `v` and updating both.
- **Phone-side secret storage:** `PairingStore` encrypts the pairing JSON with
  an AES-GCM Android Keystore key; backups and device transfer are disabled
  for it (a restored copy could not be decrypted anyway).
- **Application id / package:** `com.joncbenderkh.notifier`.
- **Schema migrations:** `server/migrations/NNNN_*.sql`, applied by
  `wrangler d1 migrations apply`. Append only; never edit a shipped one, and
  keep them additive (deploy.yml migrates before publishing the new code).

## Layout

- `server/src/app.ts` — `createApp(deps)`: routes and auth middleware.
  Admin auth fails closed unless `ADMIN_TOKEN` is ≥32 characters; request
  bodies reject unknown fields; API keys see only `id`/`name`/`reachable`
  from `GET /v1/devices`. The
  sender and clock are injected so `test/api.test.ts` drives the whole flow
  against D1 with a fake sender.
- `server/src/{store,seal,token,push,encoding}.ts` — D1 queries, envelope
  encryption, secret generation/hashing, FCM + log senders, byte encodings.
- `server/src/index.ts` — the deployed entry point.
- `notify/` — `main.go` (flags, output, exit codes), `client.go` (API client,
  name → ID resolution); `main_test.go` runs it against an `httptest` server.
- `android/app/src/main/java/com/joncbenderkh/notifier/`
  - `data/` — `PairingStore`, `ServerClient` (blocking; callers pick the thread).
  - `push/` — `PushService` (FCM), `SealedMessage` (pure JVM, unit-tested),
    `Notifications` (channels `default` and `high`).
  - `ui/` — `MainViewModel`, `NotifierScreen`, theme.

## Commands

Server (from `server/`):

- Check: `npm run typecheck && npm run lint && npm test && npm run build`
- Local: `cp .dev.vars.example .dev.vars`, `npm run db:migrate:local`, `npm run dev`
- Deploy: automatic via `.github/workflows/deploy.yml` on push to `main`
  (needs `CLOUDFLARE_API_TOKEN` / `CLOUDFLARE_ACCOUNT_ID` secrets); manual
  `npm run db:migrate:remote && npm run deploy`.

CLI (from `notify/`):

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
`server/package.json`, and `version` in `notify/main.go` must match; the tag
`vX.Y.Z` mirrors them (release builds also inject it into `notify` via
`-ldflags -X`). Because `notify/` is a nested Go module, also push
`notify/vX.Y.Z` on the same commit, or `go install …/notify@latest` resolves
to an untagged pseudo-version. The Worker deploys continuously from `main`,
independent of tags. `versionCode` increments by one each release.

## Working on notifier

- Never commit to `main`. Use a worktree per change (`gwa <branch> <comment>`,
  siblings of `main/`), push the branch, open a PR, and report the PR URL.
- Conventional Commits, imperative, ≤50-char subject, no attribution lines.
- English only.
