// SPDX-License-Identifier: GPL-3.0-or-later
//
// notifier's HTTP API. Three credentials guard it: the operator's admin
// token (pairing, key and device management), per-sender API keys (sending
// notifications, listing devices), and per-device tokens (a paired phone
// managing its own registration).

import { Hono } from "hono";
import type { Context, MiddlewareHandler } from "hono";
import { bodyLimit } from "hono/body-limit";
import { HTTPException } from "hono/http-exception";
import { fromBase64, toBase64 } from "./encoding";
import { UnregisteredError } from "./push";
import type { Sender } from "./push";
import { newSealKey, seal } from "./seal";
import * as store from "./store";
import type { Device } from "./store";
import {
  API_KEY_PREFIX,
  DEVICE_TOKEN_PREFIX,
  newId,
  newPairingCode,
  newSecret,
  normalizePairingCode,
  sha256Hex,
} from "./token";

export const PAIRING_TTL_SECONDS = 10 * 60;
export const MESSAGE_TTL_SECONDS = 24 * 60 * 60;
const MAX_REQUEST_BYTES = 16 * 1024;
const MAX_TITLE_BYTES = 256;
const MAX_BODY_BYTES = 2048;
const MAX_NAME_BYTES = 64;
const ENVELOPE_VERSION = "1";
const MIN_ADMIN_TOKEN_LENGTH = 32;

export interface Deps {
  sender: (env: Env) => Sender;
  /** Unix seconds. */
  now: () => number;
}

type AppEnv = { Bindings: Env; Variables: { device: Device; admin: boolean } };

/** The plaintext sealed into each push; the Android app decodes the same shape. */
export interface Payload {
  id: string;
  title: string;
  body?: string;
  priority: "default" | "high";
  sent_at: number;
}

export interface Delivery {
  device_id: string;
  status: "sent" | "failed" | "unreachable";
  error?: string;
}

const utf8Length = (s: string) => new TextEncoder().encode(s).length;
const iso = (seconds: number) => new Date(seconds * 1000).toISOString().replace(".000Z", "Z");

function fail(status: 400 | 401 | 403 | 404, message: string): never {
  throw new HTTPException(status, { message });
}

/** Parses a JSON object body, rejecting unknown fields so a typo cannot silently change meaning. */
async function readJson<T extends object>(c: Context, fields: readonly (keyof T & string)[]): Promise<Partial<T>> {
  let value: unknown;
  try {
    value = await c.req.json();
  } catch {
    fail(400, "invalid JSON body");
  }
  if (typeof value !== "object" || value === null || Array.isArray(value)) fail(400, "invalid JSON body");
  const unknown = Object.keys(value).find((k) => !(fields as readonly string[]).includes(k));
  if (unknown !== undefined) fail(400, `invalid JSON body: unknown field "${unknown}"`);
  return value;
}

function validName(raw: unknown): string {
  const name = typeof raw === "string" ? raw.trim() : "";
  if (name === "" || utf8Length(name) > MAX_NAME_BYTES) {
    fail(400, "name is required and must be at most 64 bytes");
  }
  if (!name.isWellFormed()) fail(400, "name must be valid UTF-8");
  return name;
}

function bearer(c: Context): string {
  const match = /^bearer\s+(.+)$/i.exec(c.req.header("authorization") ?? "");
  return match?.[1]?.trim() ?? "";
}

let reportedBadAdminToken = false;

async function isAdmin(c: Context<AppEnv>): Promise<boolean> {
  // Fail closed: without this, an unset secret and a missing header would
  // both hash the empty string and compare equal.
  if (typeof c.env.ADMIN_TOKEN !== "string" || c.env.ADMIN_TOKEN.length < MIN_ADMIN_TOKEN_LENGTH) {
    if (!reportedBadAdminToken) {
      console.error(`ADMIN_TOKEN secret must be set to at least ${MIN_ADMIN_TOKEN_LENGTH} characters`);
      reportedBadAdminToken = true;
    }
    return false;
  }
  const encoder = new TextEncoder();
  const [got, want] = await Promise.all(
    [bearer(c), c.env.ADMIN_TOKEN].map((s) => crypto.subtle.digest("SHA-256", encoder.encode(s))),
  );
  return crypto.subtle.timingSafeEqual(got!, want!);
}

async function isApiKey(c: Context<AppEnv>, now: number): Promise<boolean> {
  const secret = bearer(c);
  if (!secret.startsWith(API_KEY_PREFIX)) return false;
  return (await store.apiKeyByHash(c.env.DB, await sha256Hex(secret), now)) !== null;
}

export function createApp(deps: Deps): Hono<AppEnv> {
  const app = new Hono<AppEnv>();

  const admin: MiddlewareHandler<AppEnv> = async (c, next) => {
    if (!(await isAdmin(c))) fail(401, "invalid admin token");
    await next();
  };

  const apiKey: MiddlewareHandler<AppEnv> = async (c, next) => {
    if (!(await isApiKey(c, deps.now()))) fail(401, "invalid API key");
    await next();
  };

  const adminOrApiKey: MiddlewareHandler<AppEnv> = async (c, next) => {
    const admin = await isAdmin(c);
    if (!admin && !(await isApiKey(c, deps.now()))) fail(401, "invalid admin token or API key");
    c.set("admin", admin);
    await next();
  };

  const device: MiddlewareHandler<AppEnv> = async (c, next) => {
    const secret = bearer(c);
    const found = secret.startsWith(DEVICE_TOKEN_PREFIX)
      ? await store.deviceByTokenHash(c.env.DB, await sha256Hex(secret), deps.now())
      : null;
    if (!found) fail(401, "invalid device token");
    c.set("device", found);
    await next();
  };

  async function deliver(env: Env, d: Device, p: Payload): Promise<Delivery> {
    if (d.fcm_token === "") return { device_id: d.id, status: "unreachable" };
    try {
      const envelope = await seal(fromBase64(d.seal_key), d.id, new TextEncoder().encode(JSON.stringify(p)));
      await deps.sender(env).send({
        token: d.fcm_token,
        high: p.priority === "high",
        ttlSeconds: MESSAGE_TTL_SECONDS,
        data: { v: ENVELOPE_VERSION, n: toBase64(envelope.nonce), c: toBase64(envelope.ciphertext) },
      });
      return { device_id: d.id, status: "sent" };
    } catch (err) {
      if (err instanceof UnregisteredError) {
        console.warn("push token unregistered; clearing", d.id);
        // Other devices may already have been sent to; never turn this into a 500.
        await store.setFcmToken(env.DB, d.id, "").catch((dbErr: unknown) => {
          console.error("clear push token failed", d.id, dbErr);
        });
        return { device_id: d.id, status: "unreachable" };
      }
      console.error("push send failed", d.id, err);
      return { device_id: d.id, status: "failed", error: "push transport error" };
    }
  }

  app.use("*", bodyLimit({ maxSize: MAX_REQUEST_BYTES, onError: (c) => c.json({ error: "request too large" }, 413) }));

  app.get("/healthz", (c) => c.json({ status: "ok" }));

  // --- admin ---

  app.post("/v1/pairings", admin, async (c) => {
    const code = newPairingCode();
    const expiresAt = deps.now() + PAIRING_TTL_SECONDS;
    await store.createPairing(c.env.DB, await sha256Hex(code), expiresAt);
    return c.json({ code, expires_at: iso(expiresAt) }, 201);
  });

  app.get("/v1/devices", adminOrApiKey, async (c) => {
    const devices = await store.listDevices(c.env.DB);
    // Senders only need enough to address a phone; activity timestamps are
    // for the operator.
    const admin = c.get("admin");
    return c.json(
      devices.map((d) => ({
        id: d.id,
        name: d.name,
        reachable: d.fcm_token !== "",
        ...(admin ? { created_at: iso(d.created_at), last_seen_at: iso(d.last_seen_at) } : {}),
      })),
    );
  });

  app.delete("/v1/devices/:id", admin, async (c) => {
    if (!(await store.deleteDevice(c.env.DB, c.req.param("id")))) fail(404, "not found");
    return c.body(null, 204);
  });

  app.post("/v1/keys", admin, async (c) => {
    const req = await readJson<{ name: string }>(c, ["name"]);
    const key = { id: newId("key_"), name: validName(req.name), created_at: deps.now(), last_used_at: null };
    const secret = newSecret(API_KEY_PREFIX);
    await store.createApiKey(c.env.DB, key, await sha256Hex(secret));
    return c.json({ id: key.id, name: key.name, key: secret, created_at: iso(key.created_at) }, 201);
  });

  app.get("/v1/keys", admin, async (c) => {
    const keys = await store.listApiKeys(c.env.DB);
    return c.json(
      keys.map((k) => ({
        id: k.id,
        name: k.name,
        created_at: iso(k.created_at),
        ...(k.last_used_at === null ? {} : { last_used_at: iso(k.last_used_at) }),
      })),
    );
  });

  app.delete("/v1/keys/:id", admin, async (c) => {
    if (!(await store.deleteApiKey(c.env.DB, c.req.param("id")))) fail(404, "not found");
    return c.body(null, 204);
  });

  // --- device ---

  app.post("/v1/devices", async (c) => {
    const req = await readJson<{ code: string; name: string; fcm_token: string }>(c, ["code", "name", "fcm_token"]);
    const name = validName(req.name);
    if (typeof req.fcm_token !== "string" || req.fcm_token === "") fail(400, "fcm_token is required");
    const d: Device = {
      id: newId("dev_"),
      name,
      seal_key: toBase64(newSealKey()),
      fcm_token: req.fcm_token,
      created_at: 0,
      last_seen_at: 0,
    };
    const secret = newSecret(DEVICE_TOKEN_PREFIX);
    const codeHash = await sha256Hex(normalizePairingCode(typeof req.code === "string" ? req.code : ""));
    if (!(await store.redeemPairing(c.env.DB, codeHash, deps.now(), d, await sha256Hex(secret)))) {
      fail(403, "pairing code is invalid, expired, or already used");
    }
    console.log("device paired", d.id, d.name);
    return c.json({ device_id: d.id, device_token: secret, seal_key: d.seal_key }, 201);
  });

  app.put("/v1/device", device, async (c) => {
    const req = await readJson<{ fcm_token: string }>(c, ["fcm_token"]);
    if (typeof req.fcm_token !== "string" || req.fcm_token === "") fail(400, "fcm_token is required");
    await store.setFcmToken(c.env.DB, c.get("device").id, req.fcm_token);
    return c.body(null, 204);
  });

  app.delete("/v1/device", device, async (c) => {
    await store.deleteDevice(c.env.DB, c.get("device").id);
    return c.body(null, 204);
  });

  app.post("/v1/device/test", device, async (c) => {
    const p: Payload = {
      id: newId("msg_"),
      title: "Test notification",
      body: "Delivered and decrypted end to end.",
      priority: "default",
      sent_at: deps.now(),
    };
    return c.json({ id: p.id, deliveries: [await deliver(c.env, c.get("device"), p)] });
  });

  // --- notifications ---

  app.post("/v1/notifications", apiKey, async (c) => {
    const req = await readJson<{ title: string; body: string; priority: string; device_ids: string[] }>(c, [
      "title",
      "body",
      "priority",
      "device_ids",
    ]);
    const title = typeof req.title === "string" ? req.title : "";
    const body = typeof req.body === "string" ? req.body : "";
    if (title.trim() === "") fail(400, "title is required");
    if (utf8Length(title) > MAX_TITLE_BYTES) fail(400, "title is too long");
    if (utf8Length(body) > MAX_BODY_BYTES) fail(400, "body is too long");
    if (!title.isWellFormed() || !body.isWellFormed()) fail(400, "title and body must be valid UTF-8");
    const priority =
      req.priority === undefined || req.priority === null || req.priority === "" ? "default" : req.priority;
    if (priority !== "default" && priority !== "high") fail(400, 'priority must be "default" or "high"');
    const ids = req.device_ids ?? [];
    if (!Array.isArray(ids) || !ids.every((id) => typeof id === "string")) {
      fail(400, "device_ids must be an array of strings");
    }

    const all = await store.listDevices(c.env.DB);
    const targets = ids.length === 0 ? all : all.filter((d) => ids.includes(d.id));
    if (targets.length === 0) fail(404, "no matching paired devices");

    const p: Payload = { id: newId("msg_"), title, priority, sent_at: deps.now() };
    if (body !== "") p.body = body;
    // A failed warm-up (including a missing or invalid FCM_SERVICE_ACCOUNT)
    // is not fatal: each send retries and reports per device.
    try {
      await deps.sender(c.env).prepare?.();
    } catch (err) {
      console.error("push prepare failed", err);
    }
    const deliveries = await Promise.all(targets.map((d) => deliver(c.env, d, p)));
    return c.json({ id: p.id, deliveries });
  });

  app.notFound((c) => c.json({ error: "not found" }, 404));
  app.onError((err, c) => {
    if (err instanceof HTTPException) return c.json({ error: err.message }, err.status);
    console.error("internal error", err);
    return c.json({ error: "internal error" }, 500);
  });

  return app;
}
