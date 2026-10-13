// SPDX-License-Identifier: GPL-3.0-or-later
//
// notifier's HTTP API. Four credentials guard it:
// - the operator's admin token (the ADMIN_TOKEN secret): user management
//   and invites, nothing else;
// - a user's token (nu_): pairing, keys, devices, invites, and sending, all
//   within that user's own devices;
// - a user's API keys (nk_): sending and listing devices, for scripts,
//   optionally limited to some of the user's channels;
// - a device's token (nd_): a paired phone or desktop managing its own
//   registration and, for the WebSocket transport, receiving messages.

import { Hono } from "hono";
import type { Context, MiddlewareHandler } from "hono";
import { bodyLimit } from "hono/body-limit";
import { HTTPException } from "hono/http-exception";
import {
  DEFAULT_GRACE_SECONDS,
  HEARTBEAT_SECRET_PREFIX,
  heartbeatAlerts,
  MAX_GRACE_SECONDS,
  MAX_HEARTBEATS_PER_USER,
  MAX_INTERVAL_SECONDS,
  MIN_GRACE_SECONDS,
  MIN_INTERVAL_SECONDS,
} from "./heartbeats";
import { pairPageRoutes } from "./pairPage";
import { toBase64 } from "./encoding";
import { joinUri, pairingUri, qrSvg } from "./pairing";
import type { Sender } from "./push";
import { addSubscribers, channelLabel, publisher } from "./publish";
import type { Target } from "./publish";
import type { Stream } from "./stream";
import { newSealKey } from "./seal";
import { cleanTail, UNSAFE_IN_NAME, UNSAFE_IN_TEXT, utf8Length } from "./text";
import * as store from "./store";
import type { Channel, Device, User } from "./store";
import {
  API_KEY_PREFIX,
  DEVICE_TOKEN_PREFIX,
  newId,
  newInviteCode,
  newPairingCode,
  newSecret,
  normalizeInviteCode,
  normalizePairingCode,
  sha256Hex,
  USER_TOKEN_PREFIX,
} from "./token";

export const PAIRING_TTL_SECONDS = 10 * 60;
export const INVITE_TTL_SECONDS = 7 * 24 * 60 * 60;
export { MESSAGE_TTL_SECONDS } from "./publish";
export const MAX_DEVICES_PER_USER = 20;
export const MAX_API_KEYS_PER_USER = 20;
export const MAX_INVITES_PER_USER = 10;
export const MAX_PENDING_PAIRINGS_PER_USER = 5;
export const MAX_CHANNELS_PER_USER = 20;
export const MAX_MEMBERSHIPS_PER_USER = 50;
/** Members, owner included; bounds one send's fan-out (FCM calls per request). */
export const MAX_MEMBERS_PER_CHANNEL = 20;
export const MAX_PENDING_INVITES_PER_CHANNEL = 10;
export const MAX_CHANNELS_PER_REQUEST = 20;
const CHANNEL_NAME = /^[a-z0-9][a-z0-9_-]{0,31}$/;
const MAX_REQUEST_BYTES = 16 * 1024;
const MAX_PING_BYTES = 1024 * 1024;
const MAX_TITLE_BYTES = 256;
const MAX_BODY_BYTES = 2048;
const MAX_NAME_BYTES = 64;
const MIN_ADMIN_TOKEN_LENGTH = 32;

export interface Deps {
  sender: (env: Env) => Sender;
  /** The Durable Object holding a WebSocket-transport device's socket and queue. */
  stream: (env: Env, deviceId: string) => Stream;
  /** Unix seconds. */
  now: () => number;
}

type AppEnv = {
  Bindings: Env;
  Variables: {
    /** The caller's user; "" when the operator acts. */
    userId: string;
    user: User;
    device: Device;
    /** True for a user token, false for an API key. */
    viaUserToken: boolean;
    /** Channel IDs an API key is limited to; empty means unlimited. */
    keyScope: string[];
  };
};

/** The plaintext sealed into each push; the Android app decodes the same shape. */
export interface Payload {
  id: string;
  title: string;
  body?: string;
  priority: "default" | "high";
  sent_at: number;
  /** The channel it was published to, as this device's owner names it. */
  channel?: string;
  channel_id?: string;
}

export interface Delivery {
  device_id: string;
  /** "queued": held for a WebSocket device that is offline, for up to a day. */
  status: "sent" | "queued" | "failed" | "unreachable";
  error?: string;
}

const iso = (seconds: number) => new Date(seconds * 1000).toISOString().replace(".000Z", "Z");

function fail(status: 400 | 401 | 403 | 404 | 409 | 426 | 429 | 503, message: string): never {
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
  if (UNSAFE_IN_NAME.test(name)) fail(400, "name must not contain control or bidi override characters");
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

async function userFromToken(c: Context<AppEnv>, now: number): Promise<User | null> {
  const secret = bearer(c);
  if (!secret.startsWith(USER_TOKEN_PREFIX)) return null;
  return store.userByTokenHash(c.env.DB, await sha256Hex(secret), now);
}

async function apiKeyFromToken(c: Context<AppEnv>, now: number): Promise<store.ApiKey | null> {
  const secret = bearer(c);
  if (!secret.startsWith(API_KEY_PREFIX)) return null;
  return store.apiKeyByHash(c.env.DB, await sha256Hex(secret), now);
}

function stringList(raw: unknown, field: string): string[] {
  if (raw === undefined || raw === null) return [];
  if (!Array.isArray(raw) || !raw.every((v) => typeof v === "string")) fail(400, `${field} must be an array of strings`);
  return raw;
}

function channelNames(raw: unknown): string[] {
  const names = [...new Set(stringList(raw, "channels"))];
  if (names.length > MAX_CHANNELS_PER_REQUEST) fail(400, `at most ${MAX_CHANNELS_PER_REQUEST} channels per request`);
  const bad = names.find((n) => !CHANNEL_NAME.test(n));
  if (bad !== undefined) fail(400, `invalid channel name "${bad}"`);
  return names;
}

/** Invites, channel invites, and pairing codes share an alphabet; prefixes keep them apart. */
const channelInviteHash = (code: string) => sha256Hex(`channel-invite:${normalizeInviteCode(code)}`);

/** Invites share the code alphabet with pairing codes; the prefix keeps the two apart. */
const inviteHash = (code: string) => sha256Hex(`invite:${normalizeInviteCode(code)}`);

export function createApp(deps: Deps): Hono<AppEnv> {
  const app = new Hono<AppEnv>();

  const operator: MiddlewareHandler<AppEnv> = async (c, next) => {
    if (!(await isAdmin(c))) fail(401, "invalid admin token");
    await next();
  };

  const user: MiddlewareHandler<AppEnv> = async (c, next) => {
    const found = await userFromToken(c, deps.now());
    if (!found) fail(401, "invalid user token");
    c.set("user", found);
    c.set("userId", found.id);
    await next();
  };

  const userOrApiKey: MiddlewareHandler<AppEnv> = async (c, next) => {
    const found = await userFromToken(c, deps.now());
    const key = found ? null : await apiKeyFromToken(c, deps.now());
    if (!found && !key) fail(401, "invalid user token or API key");
    c.set("userId", found?.id ?? key!.user_id);
    c.set("viaUserToken", found !== null);
    c.set("keyScope", key ? await store.apiKeyScope(c.env.DB, key.id) : []);
    await next();
  };

  const userOrOperator: MiddlewareHandler<AppEnv> = async (c, next) => {
    if (await isAdmin(c)) {
      c.set("userId", "");
    } else {
      const found = await userFromToken(c, deps.now());
      if (!found) fail(401, "invalid user token or admin token");
      c.set("userId", found.id);
    }
    await next();
  };

  const device: MiddlewareHandler<AppEnv> = async (c, next) => {
    const secret = bearer(c);
    const found = secret.startsWith(DEVICE_TOKEN_PREFIX)
      ? await store.deviceByTokenHash(c.env.DB, await sha256Hex(secret), deps.now())
      : null;
    if (!found) fail(401, "invalid device token");
    c.set("device", found);
    c.set("userId", found.user_id);
    await next();
  };

  /** One user's scripts or phones must not use up the FCM quota for everyone. */
  async function rateLimit(env: Env, userId: string): Promise<void> {
    if (!(await env.NOTIFY_LIMITER.limit({ key: userId })).success) {
      fail(429, "too many notifications; try again in a minute");
    }
  }

  /** Resolves channel names among the user's own channels; any unknown name is a 404. */
  async function ownedChannels(db: D1Database, userId: string, names: readonly string[]): Promise<Channel[]> {
    return Promise.all(
      names.map(async (name) => (await store.ownedChannelNamed(db, userId, name)) ?? fail(404, `you have no channel named "${name}"`)),
    );
  }

  /** Checks that [userId] belongs to every channel in [ids]; others are a 404. */
  async function memberOfAll(db: D1Database, userId: string, raw: unknown): Promise<string[]> {
    const ids = [...new Set(stringList(raw, "channel_ids"))];
    const mine = new Set((await store.memberships(db, userId)).map((ch) => ch.id));
    const missing = ids.find((id) => !mine.has(id));
    if (missing !== undefined) fail(404, `not a member of channel ${missing}`);
    return ids;
  }

  /** The channel IDs a device asks to subscribe to; a send without channels relies on default staying in. */
  async function subscriptionIds(db: D1Database, userId: string, raw: unknown): Promise<string[]> {
    const ids = await memberOfAll(db, userId, raw);
    if (!ids.includes(store.defaultChannelId(userId))) fail(400, "devices always subscribe to the default channel");
    return ids;
  }

  async function deviceChannels(db: D1Database, d: Device) {
    const subscribed = new Set(await store.deviceSubscriptions(db, d.id));
    return (await store.memberships(db, d.user_id)).map((ch) => ({
      id: ch.id,
      name: channelLabel(ch, d.user_id),
      subscribed: subscribed.has(ch.id),
    }));
  }

  /**
   * Unpairs a device. A WebSocket device's stream is closed first, while the
   * row still exists, so a failure leaves the device in place for a retry
   * instead of an open socket and queue nothing can reach any more.
   */
  async function unpair(env: Env, userId: string, deviceId: string, transport: Device["transport"]): Promise<boolean> {
    if (transport === "ws") {
      try {
        await deps.stream(env, deviceId).close();
      } catch (err) {
        console.error("close stream failed", deviceId, err);
        fail(503, "could not disconnect the device; try again");
      }
    }
    return store.deleteDevice(env.DB, userId, deviceId);
  }

  const { deliver, publish } = publisher(deps);
  const alerts = heartbeatAlerts(deps);

  /** Whole seconds within [min, max], or [fallback] when absent. */
  function seconds(raw: unknown, field: string, min: number, max: number, fallback?: number): number {
    if ((raw === undefined || raw === null) && fallback !== undefined) return fallback;
    if (typeof raw !== "number" || !Number.isInteger(raw) || raw < min || raw > max) {
      fail(400, `${field} must be whole seconds from ${min} to ${max}`);
    }
    return raw;
  }

  // A failure report may post a whole log, of which only the tail is kept.
  const tooLarge = (c: Context) => c.json({ error: "request too large" }, 413);
  const apiLimit: MiddlewareHandler<AppEnv> = bodyLimit({ maxSize: MAX_REQUEST_BYTES, onError: tooLarge });
  const pingLimit: MiddlewareHandler<AppEnv> = bodyLimit({ maxSize: MAX_PING_BYTES, onError: tooLarge });
  const limit: MiddlewareHandler<AppEnv> = (c, next) =>
    c.req.path.startsWith("/v1/ping/") ? pingLimit(c, next) : apiLimit(c, next);
  app.use("*", limit);

  app.get("/healthz", (c) => c.json({ status: "ok" }));
  pairPageRoutes(app);

  // --- users (operator) ---

  app.get("/v1/users", operator, async (c) => {
    const users = await store.listUsers(c.env.DB);
    return c.json(
      users.map((u) => ({
        id: u.id,
        name: u.name,
        devices: u.devices,
        created_at: iso(u.created_at),
        last_seen_at: iso(u.last_seen_at),
        ...(u.disabled_at === null ? {} : { disabled_at: iso(u.disabled_at) }),
        ...(u.invited_by ? { invited_by: u.invited_by } : {}),
      })),
    );
  });

  // Issues a fresh token, revoking the old one: recovery for a lost token,
  // and how the operator first signs in as the migrated owner.
  app.post("/v1/users/:id/token", operator, async (c) => {
    const id = c.req.param("id");
    const token = newSecret(USER_TOKEN_PREFIX);
    if (!(await store.setUserTokenHash(c.env.DB, id, await sha256Hex(token)))) fail(404, "not found");
    return c.json({ user_id: id, token });
  });

  app.post("/v1/users/:id/disable", operator, async (c) => {
    if (!(await store.setUserDisabled(c.env.DB, c.req.param("id"), deps.now()))) fail(404, "not found");
    return c.body(null, 204);
  });

  app.post("/v1/users/:id/enable", operator, async (c) => {
    if (!(await store.setUserDisabled(c.env.DB, c.req.param("id"), null))) fail(404, "not found");
    return c.body(null, 204);
  });

  // --- invites and sign-up ---

  app.post("/v1/invites", userOrOperator, async (c) => {
    const now = deps.now();
    const code = newInviteCode();
    const expiresAt = now + INVITE_TTL_SECONDS;
    const hash = await inviteHash(code);
    if (!(await store.createInvite(c.env.DB, hash, c.get("userId"), now, expiresAt, MAX_INVITES_PER_USER))) {
      fail(403, `at most ${MAX_INVITES_PER_USER} invites per user, counting those already used`);
    }
    return c.json({ code, expires_at: iso(expiresAt) }, 201);
  });

  app.post("/v1/users", async (c) => {
    const req = await readJson<{ invite: string; name: string }>(c, ["invite", "name"]);
    const u = { id: newId("usr_"), name: validName(req.name) };
    const token = newSecret(USER_TOKEN_PREFIX);
    const codeHash = await inviteHash(typeof req.invite === "string" ? req.invite : "");
    if (!(await store.redeemInvite(c.env.DB, codeHash, deps.now(), u, await sha256Hex(token)))) {
      fail(403, "invite is invalid, expired, or already used");
    }
    console.log("user signed up", u.id, u.name);
    return c.json({ user_id: u.id, name: u.name, token }, 201);
  });

  app.get("/v1/user", user, (c) => {
    const u = c.get("user");
    return c.json({ id: u.id, name: u.name, created_at: iso(u.created_at) });
  });

  // --- a user's devices and keys ---

  app.post("/v1/pairings", user, async (c) => {
    const userId = c.get("userId");
    // An early, clear answer; redeeming enforces the cap atomically.
    if ((await store.listDevices(c.env.DB, userId)).length >= MAX_DEVICES_PER_USER) {
      fail(403, `at most ${MAX_DEVICES_PER_USER} paired devices per user`);
    }
    const code = newPairingCode();
    const now = deps.now();
    const expiresAt = now + PAIRING_TTL_SECONDS;
    const hash = await sha256Hex(code);
    if (!(await store.createPairing(c.env.DB, hash, userId, now, expiresAt, MAX_PENDING_PAIRINGS_PER_USER))) {
      fail(403, `at most ${MAX_PENDING_PAIRINGS_PER_USER} unused pairing codes at a time`);
    }
    const uri = pairingUri(new URL(c.req.url).origin, code);
    // ?qr=svg also returns the link as a QR code, for the pairing page.
    const qr = c.req.query("qr") === "svg" ? { qr_svg: qrSvg(uri) } : {};
    return c.json({ code, expires_at: iso(expiresAt), uri, ...qr }, 201);
  });

  app.get("/v1/devices", userOrApiKey, async (c) => {
    const devices = await store.listDevices(c.env.DB, c.get("userId"));
    // Scripts only need enough to address a phone; activity timestamps are
    // for the user.
    const full = c.get("viaUserToken");
    return c.json(
      devices.map((d) => ({
        id: d.id,
        name: d.name,
        transport: d.transport,
        // A WebSocket device's queue holds messages while it is offline.
        reachable: d.transport === "ws" || d.fcm_token !== "",
        ...(full ? { created_at: iso(d.created_at), last_seen_at: iso(d.last_seen_at) } : {}),
      })),
    );
  });

  app.delete("/v1/devices/:id", user, async (c) => {
    const id = c.req.param("id");
    const transport = await store.deviceTransport(c.env.DB, c.get("userId"), id);
    if (transport === null || !(await unpair(c.env, c.get("userId"), id, transport))) fail(404, "not found");
    return c.body(null, 204);
  });

  app.post("/v1/keys", user, async (c) => {
    const req = await readJson<{ name: string; channels: string[] }>(c, ["name", "channels"]);
    const scope = await ownedChannels(c.env.DB, c.get("userId"), channelNames(req.channels));
    const key = {
      id: newId("key_"),
      user_id: c.get("userId"),
      name: validName(req.name),
      created_at: deps.now(),
      last_used_at: null,
    };
    const secret = newSecret(API_KEY_PREFIX);
    const scopeIds = scope.map((ch) => ch.id);
    if (!(await store.createApiKey(c.env.DB, key, await sha256Hex(secret), MAX_API_KEYS_PER_USER, scopeIds))) {
      fail(403, `at most ${MAX_API_KEYS_PER_USER} API keys per user`);
    }
    const channels = scope.map((ch) => ch.name);
    return c.json({ id: key.id, name: key.name, key: secret, created_at: iso(key.created_at), channels }, 201);
  });

  app.get("/v1/keys", user, async (c) => {
    const userId = c.get("userId");
    const keys = await store.listApiKeys(c.env.DB, userId);
    const names = new Map((await store.memberships(c.env.DB, userId)).map((ch) => [ch.id, ch.name]));
    const scopes = await Promise.all(keys.map((k) => store.apiKeyScope(c.env.DB, k.id)));
    return c.json(
      keys.map((k, i) => ({
        id: k.id,
        name: k.name,
        created_at: iso(k.created_at),
        ...(k.last_used_at === null ? {} : { last_used_at: iso(k.last_used_at) }),
        channels: scopes[i]!.map((id) => names.get(id) ?? id),
      })),
    );
  });

  app.delete("/v1/keys/:id", user, async (c) => {
    if (!(await store.deleteApiKey(c.env.DB, c.get("userId"), c.req.param("id")))) fail(404, "not found");
    return c.body(null, 204);
  });

  // --- channels ---

  app.post("/v1/channels", user, async (c) => {
    const req = await readJson<{ name: string }>(c, ["name"]);
    if (typeof req.name !== "string" || req.name === "") fail(400, "name is required");
    const [name] = channelNames([req.name]);
    if (name === undefined) fail(400, "name is required");
    const userId = c.get("userId");
    const ch = { id: newId("ch_"), owner_id: userId, name, created_at: deps.now() };
    if (!(await store.createChannel(c.env.DB, ch, MAX_CHANNELS_PER_USER, MAX_MEMBERSHIPS_PER_USER))) {
      if (await store.ownedChannelNamed(c.env.DB, userId, name)) fail(409, `you already have a channel named "${name}"`);
      fail(
        403,
        `at most ${MAX_CHANNELS_PER_USER} channels besides default, and ${MAX_MEMBERSHIPS_PER_USER} owned or joined, per user`,
      );
    }
    return c.json({ id: ch.id, name, created_at: iso(ch.created_at) }, 201);
  });

  app.get("/v1/channels", user, async (c) => {
    const userId = c.get("userId");
    const channels = await store.memberships(c.env.DB, userId);
    return c.json(
      channels.map((ch) => ({
        id: ch.id,
        name: channelLabel(ch, userId),
        owned: ch.owner_id === userId,
        members: ch.members,
      })),
    );
  });

  app.delete("/v1/channels/:id", user, async (c) => {
    if (!(await store.deleteChannel(c.env.DB, c.get("userId"), c.req.param("id")))) {
      fail(404, "not found (the default channel cannot be deleted)");
    }
    return c.body(null, 204);
  });

  app.post("/v1/channels/:id/invites", user, async (c) => {
    const ch = await store.ownedChannel(c.env.DB, c.get("userId"), c.req.param("id"));
    if (!ch) fail(404, "not found");
    // A send without channels goes to default, so sharing it would leak those.
    if (ch.id === store.defaultChannelId(ch.owner_id)) fail(400, "the default channel cannot be shared");
    const now = deps.now();
    const code = newInviteCode();
    const expiresAt = now + INVITE_TTL_SECONDS;
    const hash = await channelInviteHash(code);
    if (!(await store.createChannelInvite(c.env.DB, hash, ch.id, now, expiresAt, MAX_PENDING_INVITES_PER_CHANNEL))) {
      fail(403, `at most ${MAX_PENDING_INVITES_PER_CHANNEL} unused invites per channel`);
    }
    const uri = joinUri(new URL(c.req.url).origin, code);
    const qr = c.req.query("qr") === "svg" ? { qr_svg: qrSvg(uri) } : {};
    return c.json({ code, channel: ch.name, expires_at: iso(expiresAt), uri, ...qr }, 201);
  });

  /** Joins a channel for [userId]; the response names it as that user sees it. */
  async function join(db: D1Database, userId: string, raw: unknown) {
    const result = await store.joinChannel(
      db,
      await channelInviteHash(typeof raw === "string" ? raw : ""),
      userId,
      deps.now(),
      MAX_MEMBERSHIPS_PER_USER,
      MAX_MEMBERS_PER_CHANNEL,
    );
    if (result === "invalid") fail(403, "channel invite is invalid, expired, or already used");
    if (result === "full") fail(403, `at most ${MAX_MEMBERSHIPS_PER_USER} channels per user`);
    if (result === "channel full") fail(403, `at most ${MAX_MEMBERS_PER_CHANNEL} members per channel`);
    return result.channel;
  }

  app.post("/v1/memberships", user, async (c) => {
    const req = await readJson<{ invite: string }>(c, ["invite"]);
    const userId = c.get("userId");
    const ch = await join(c.env.DB, userId, req.invite);
    return c.json({ id: ch.id, name: channelLabel(ch, userId) }, 201);
  });

  app.get("/v1/channels/:id/members", user, async (c) => {
    const ch = await store.ownedChannel(c.env.DB, c.get("userId"), c.req.param("id"));
    if (!ch) fail(404, "not found");
    const list = await store.members(c.env.DB, ch.id);
    return c.json(list.map((m) => ({ id: m.id, name: m.name, joined_at: iso(m.joined_at) })));
  });

  // The owner removes a member, or a member leaves.
  app.delete("/v1/channels/:id/members/:userId", user, async (c) => {
    const me = c.get("userId");
    const channelId = c.req.param("id");
    const target = c.req.param("userId");
    const owner = (await store.ownedChannel(c.env.DB, me, channelId)) !== null;
    if ((!owner && target !== me) || !(await store.removeMember(c.env.DB, channelId, target))) fail(404, "not found");
    return c.body(null, 204);
  });

  app.put("/v1/devices/:id/channels", user, async (c) => {
    const req = await readJson<{ channel_ids: string[] }>(c, ["channel_ids"]);
    const userId = c.get("userId");
    const d = (await store.listDevices(c.env.DB, userId)).find((x) => x.id === c.req.param("id"));
    if (!d) fail(404, "not found");
    await store.setSubscriptions(c.env.DB, d.id, await subscriptionIds(c.env.DB, userId, req.channel_ids));
    return c.json(await deviceChannels(c.env.DB, d));
  });

  // --- device ---

  app.get("/v1/device/channels", device, async (c) => c.json(await deviceChannels(c.env.DB, c.get("device"))));

  app.put("/v1/device/channels", device, async (c) => {
    const req = await readJson<{ channel_ids: string[] }>(c, ["channel_ids"]);
    const d = c.get("device");
    await store.setSubscriptions(c.env.DB, d.id, await subscriptionIds(c.env.DB, d.user_id, req.channel_ids));
    return c.json(await deviceChannels(c.env.DB, d));
  });

  // A phone joins a channel for its owner and subscribes itself.
  app.post("/v1/device/memberships", device, async (c) => {
    const req = await readJson<{ invite: string }>(c, ["invite"]);
    const d = c.get("device");
    const ch = await join(c.env.DB, d.user_id, req.invite);
    await store.subscribe(c.env.DB, d.id, ch.id);
    return c.json({ id: ch.id, name: channelLabel(ch, d.user_id) }, 201);
  });

  app.post("/v1/devices", async (c) => {
    const req = await readJson<{ code: string; name: string; fcm_token: string; transport: string }>(c, [
      "code",
      "name",
      "fcm_token",
      "transport",
    ]);
    const name = validName(req.name);
    const transport = req.transport ?? "fcm";
    if (transport !== "fcm" && transport !== "ws") fail(400, 'transport must be "fcm" or "ws"');
    let fcmToken = "";
    if (transport === "ws") {
      if (req.fcm_token !== undefined) fail(400, "fcm_token is only for the fcm transport");
    } else {
      if (typeof req.fcm_token !== "string" || req.fcm_token === "") fail(400, "fcm_token is required");
      fcmToken = req.fcm_token;
    }
    const d: Omit<Device, "user_id"> = {
      id: newId("dev_"),
      name,
      seal_key: toBase64(newSealKey()),
      fcm_token: fcmToken,
      transport,
      created_at: 0,
      last_seen_at: 0,
    };
    const secret = newSecret(DEVICE_TOKEN_PREFIX);
    const codeHash = await sha256Hex(normalizePairingCode(typeof req.code === "string" ? req.code : ""));
    if (!(await store.redeemPairing(c.env.DB, codeHash, deps.now(), d, await sha256Hex(secret), MAX_DEVICES_PER_USER))) {
      fail(403, `pairing code is invalid, expired, or already used, or its user has ${MAX_DEVICES_PER_USER} devices`);
    }
    console.log("device paired", d.id, d.name);
    return c.json({ device_id: d.id, device_token: secret, seal_key: d.seal_key }, 201);
  });

  app.put("/v1/device", device, async (c) => {
    if (c.get("device").transport !== "fcm") fail(409, "this device uses the WebSocket transport");
    const req = await readJson<{ fcm_token: string }>(c, ["fcm_token"]);
    if (typeof req.fcm_token !== "string" || req.fcm_token === "") fail(400, "fcm_token is required");
    await store.setFcmToken(c.env.DB, c.get("device").id, req.fcm_token);
    return c.body(null, 204);
  });

  app.delete("/v1/device", device, async (c) => {
    const d = c.get("device");
    await unpair(c.env, d.user_id, d.id, d.transport);
    return c.body(null, 204);
  });

  // The WebSocket transport: the device's Durable Object takes the connection.
  app.get("/v1/device/stream", device, async (c) => {
    const d = c.get("device");
    if (d.transport !== "ws") fail(409, "this device receives through FCM");
    if (c.req.header("upgrade")?.toLowerCase() !== "websocket") fail(426, "expected a WebSocket upgrade");
    return deps.stream(c.env, d.id).fetch(c.req.raw);
  });

  app.post("/v1/device/test", device, async (c) => {
    await rateLimit(c.env, c.get("userId"));
    const p: Payload = {
      id: newId("msg_"),
      title: "Test notification",
      body: "Delivered and decrypted end to end.",
      priority: "default",
      sent_at: deps.now(),
    };
    return c.json({ id: p.id, deliveries: [await deliver(c.env, c.get("device"), p)] });
  });

  // --- heartbeats ---

  app.post("/v1/heartbeats", user, async (c) => {
    const req = await readJson<{ name: string; interval: number; grace: number; channel: string }>(c, [
      "name",
      "interval",
      "grace",
      "channel",
    ]);
    const userId = c.get("userId");
    const name = validName(req.name);
    const interval = seconds(req.interval, "interval", MIN_INTERVAL_SECONDS, MAX_INTERVAL_SECONDS);
    const grace = seconds(req.grace, "grace", MIN_GRACE_SECONDS, MAX_GRACE_SECONDS, DEFAULT_GRACE_SECONDS);
    const [channel] = await ownedChannels(c.env.DB, userId, channelNames([req.channel ?? "default"]));
    const now = deps.now();
    const hb: store.Heartbeat = {
      id: newId("hb_"),
      user_id: userId,
      channel_id: channel!.id,
      name,
      interval_s: interval,
      grace_s: grace,
      created_at: now,
      last_ping_at: null,
      due_at: now + interval + grace,
      state: "up",
      changed_at: now,
      pending_notice: null,
      notice_due_at: null,
    };
    const secret = newSecret(HEARTBEAT_SECRET_PREFIX);
    const created = await store.createHeartbeat(c.env.DB, hb, await sha256Hex(secret), MAX_HEARTBEATS_PER_USER);
    if (created === "exists") fail(409, `you already have a heartbeat named "${name}"`);
    if (created === "full") fail(403, `at most ${MAX_HEARTBEATS_PER_USER} heartbeats per user`);
    if (created === "gone") fail(404, `you have no channel named "${channel!.name}"`);
    return c.json(
      {
        id: hb.id,
        name,
        channel: channel!.name,
        interval,
        grace,
        ping_url: `${new URL(c.req.url).origin}/v1/ping/${secret}`,
        created_at: iso(now),
      },
      201,
    );
  });

  app.get("/v1/heartbeats", user, async (c) => {
    const heartbeats = await store.listHeartbeats(c.env.DB, c.get("userId"));
    return c.json(
      heartbeats.map((hb) => ({
        id: hb.id,
        name: hb.name,
        channel: hb.channel_name,
        interval: hb.interval_s,
        grace: hb.grace_s,
        state: hb.state,
        ...(hb.last_ping_at === null ? {} : { last_ping_at: iso(hb.last_ping_at) }),
        due_at: iso(hb.due_at),
        created_at: iso(hb.created_at),
      })),
    );
  });

  app.delete("/v1/heartbeats/:id", user, async (c) => {
    if (!(await store.deleteHeartbeat(c.env.DB, c.get("userId"), c.req.param("id")))) fail(404, "not found");
    return c.body(null, 204);
  });

  // The secret in the path is the credential, so cron jobs need only curl.
  app.on(["GET", "POST"], ["/v1/ping/:secret", "/v1/ping/:secret/fail"], async (c) => {
    const secret = c.req.param("secret");
    const hb = secret.startsWith(HEARTBEAT_SECRET_PREFIX)
      ? await store.heartbeatBySecretHash(c.env.DB, await sha256Hex(secret))
      : null;
    if (!hb) fail(404, "no such heartbeat");
    // Link checkers and unfurlers send HEAD; only a real request counts.
    if (c.req.method === "HEAD") return c.body(null, 200);
    if (!(await c.env.NOTIFY_LIMITER.limit({ key: `ping:${hb.id}` })).success) fail(429, "too many pings; try again in a minute");
    const failed = c.req.path.endsWith("/fail");
    const details = failed && c.req.method === "POST" ? cleanTail(await c.req.text(), MAX_BODY_BYTES) : "";
    const { left, seq } = await store.recordPing(c.env.DB, hb.id, deps.now(), failed);
    // Every reported failure is news; a plain ping only when it ends a
    // failure or outage. Past the user's send limit, the notice waits for
    // the cron trigger (one a minute per heartbeat) instead of going out now.
    if (failed || left) {
      const later = !(await c.env.NOTIFY_LIMITER.limit({ key: hb.user_id })).success;
      if (failed) await alerts.failed(c.env, hb, seq, details, later);
      else await alerts.recovered(c.env, hb, seq, left!, later);
    }
    return c.json({ state: failed ? "failed" : "up" });
  });

  // --- notifications ---

  app.post("/v1/notifications", userOrApiKey, async (c) => {
    const userId = c.get("userId");
    await rateLimit(c.env, userId);
    const req = await readJson<{
      title: string;
      body: string;
      priority: string;
      device_ids: string[];
      channels: string[];
    }>(c, ["title", "body", "priority", "device_ids", "channels"]);
    const title = typeof req.title === "string" ? req.title : "";
    const body = typeof req.body === "string" ? req.body : "";
    if (title.trim() === "") fail(400, "title is required");
    if (utf8Length(title) > MAX_TITLE_BYTES) fail(400, "title is too long");
    if (utf8Length(body) > MAX_BODY_BYTES) fail(400, "body is too long");
    if (!title.isWellFormed() || !body.isWellFormed()) fail(400, "title and body must be valid UTF-8");
    if (UNSAFE_IN_TEXT.test(title) || UNSAFE_IN_TEXT.test(body)) {
      fail(400, "title and body must not contain control or bidi override characters (newlines and tabs are fine)");
    }
    const priority =
      req.priority === undefined || req.priority === null || req.priority === "" ? "default" : req.priority;
    if (priority !== "default" && priority !== "high") fail(400, 'priority must be "default" or "high"');
    const ids = stringList(req.device_ids, "device_ids");
    let names = channelNames(req.channels);
    const scope = c.get("keyScope");
    if (scope.length > 0 && ids.length > 0) fail(403, "this API key may only publish to its channels");
    if (names.length === 0 && ids.length === 0) names = ["default"];
    const channels = await ownedChannels(c.env.DB, userId, names);
    const outside = channels.find((ch) => scope.length > 0 && !scope.includes(ch.id));
    if (outside) fail(403, `this API key may not publish to "${outside.name}"`);

    // Each device gets the message once, labelled with the first requested
    // channel it subscribes to; devices named directly carry no channel.
    const targets = new Map<string, Target>();
    await addSubscribers(c.env.DB, channels, targets);
    if (ids.length > 0) {
      const own = await store.listDevices(c.env.DB, userId);
      const unknown = ids.find((id) => !own.some((d) => d.id === id));
      if (unknown !== undefined) fail(404, `no paired device ${unknown}`);
      for (const d of own) {
        if (ids.includes(d.id) && !targets.has(d.id)) targets.set(d.id, { device: d });
      }
    }
    if (targets.size === 0) fail(404, "no matching paired devices");

    const base: Payload = { id: newId("msg_"), title, priority, sent_at: deps.now() };
    if (body !== "") base.body = body;
    const { deliveries, members } = await publish(c.env, userId, targets.values(), base);
    return c.json({ id: base.id, deliveries, ...(Object.keys(members).length > 0 ? { members } : {}) });
  });

  app.notFound((c) => c.json({ error: "not found" }, 404));
  app.onError((err, c) => {
    if (err instanceof HTTPException) return c.json({ error: err.message }, err.status);
    console.error("internal error", err);
    return c.json({ error: "internal error" }, 500);
  });

  return app;
}
