// SPDX-License-Identifier: GPL-3.0-or-later
//
// Hands sealed payloads to a delivery transport. FCM is called over its
// HTTP v1 API directly: the Firebase Admin SDK does not run on Workers, so
// the service account's OAuth token is minted here with Web Crypto.

import { fromBase64, toBase64Url } from "./encoding";

export interface PushMessage {
  token: string;
  data: Record<string, string>;
  high: boolean;
  ttlSeconds: number;
}

/** The transport no longer recognizes the device's push token; discard it. */
export class UnregisteredError extends Error {}

export interface Sender {
  send(message: PushMessage): Promise<void>;
}

export class LogSender implements Sender {
  send(message: PushMessage): Promise<void> {
    console.log("push (log transport)", JSON.stringify(message));
    return Promise.resolve();
  }
}

interface ServiceAccount {
  project_id: string;
  client_email: string;
  private_key: string;
}

const SCOPE = "https://www.googleapis.com/auth/firebase.messaging";
const TOKEN_URL = "https://oauth2.googleapis.com/token";

export class FcmSender implements Sender {
  readonly #account: ServiceAccount;
  readonly #fetch: typeof fetch;
  #accessToken?: { value: string; expiresAt: number };

  // The default wraps the global: Workers' fetch throws "Illegal invocation"
  // when called as a method of another object.
  constructor(serviceAccountJson: string, fetcher: typeof fetch = (input, init) => fetch(input, init)) {
    this.#account = JSON.parse(serviceAccountJson) as ServiceAccount;
    this.#fetch = fetcher;
  }

  async send(message: PushMessage): Promise<void> {
    const url = `https://fcm.googleapis.com/v1/projects/${this.#account.project_id}/messages:send`;
    // Data-only on purpose: a notification block would be rendered by the
    // system tray directly, bypassing the app's decryption.
    const response = await this.#fetch(url, {
      method: "POST",
      headers: {
        authorization: `Bearer ${await this.#token()}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({
        message: {
          token: message.token,
          data: message.data,
          android: { priority: message.high ? "HIGH" : "NORMAL", ttl: `${message.ttlSeconds}s` },
        },
      }),
    });
    if (response.ok) return;
    const body = await response.text();
    if (response.status === 404 || body.includes('"UNREGISTERED"')) throw new UnregisteredError(body);
    throw new Error(`FCM send failed: HTTP ${response.status}: ${body}`);
  }

  async #token(): Promise<string> {
    const now = Date.now();
    if (this.#accessToken && this.#accessToken.expiresAt > now + 60_000) return this.#accessToken.value;

    const response = await this.#fetch(TOKEN_URL, {
      method: "POST",
      headers: { "content-type": "application/x-www-form-urlencoded" },
      body: new URLSearchParams({
        grant_type: "urn:ietf:params:oauth:grant-type:jwt-bearer",
        assertion: await this.#assertion(Math.floor(now / 1000)),
      }),
    });
    if (!response.ok) throw new Error(`OAuth token exchange failed: HTTP ${response.status}: ${await response.text()}`);
    const { access_token, expires_in } = await response.json<{ access_token: string; expires_in: number }>();
    this.#accessToken = { value: access_token, expiresAt: now + expires_in * 1000 };
    return access_token;
  }

  async #assertion(iat: number): Promise<string> {
    const encode = (value: object) => toBase64Url(new TextEncoder().encode(JSON.stringify(value)));
    const unsigned =
      encode({ alg: "RS256", typ: "JWT" }) +
      "." +
      encode({ iss: this.#account.client_email, scope: SCOPE, aud: TOKEN_URL, iat, exp: iat + 3600 });
    const der = fromBase64(this.#account.private_key.replace(/-----[^-]+-----|\s/g, ""));
    const key = await crypto.subtle.importKey(
      "pkcs8",
      der,
      { name: "RSASSA-PKCS1-v1_5", hash: "SHA-256" },
      false,
      ["sign"],
    );
    const signature = await crypto.subtle.sign("RSASSA-PKCS1-v1_5", key, new TextEncoder().encode(unsigned));
    return `${unsigned}.${toBase64Url(new Uint8Array(signature))}`;
  }
}

// One sender per isolate, so the OAuth token is reused across requests.
let cached: { config: string; sender: Sender } | undefined;

export function senderFor(env: Env): Sender {
  const config = `${env.PUSH_TRANSPORT}:${env.FCM_SERVICE_ACCOUNT ?? ""}`;
  if (cached?.config !== config) {
    let sender: Sender;
    if ((env.PUSH_TRANSPORT as string) === "log") {
      sender = new LogSender();
    } else if (env.FCM_SERVICE_ACCOUNT) {
      sender = new FcmSender(env.FCM_SERVICE_ACCOUNT);
    } else {
      throw new Error("FCM_SERVICE_ACCOUNT secret is not set");
    }
    cached = { config, sender };
  }
  return cached.sender;
}
