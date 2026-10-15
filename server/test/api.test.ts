// SPDX-License-Identifier: GPL-3.0-or-later
import { env, runDurableObjectAlarm } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import {
  createApp,
  INVITE_TTL_SECONDS,
  MAX_API_KEYS_PER_USER,
  MAX_CHANNELS_PER_REQUEST,
  MAX_CHANNELS_PER_USER,
  MAX_DEVICES_PER_USER,
  MAX_INVITES_PER_USER,
  MAX_MEMBERS_PER_CHANNEL,
  MAX_PENDING_INVITES_PER_CHANNEL,
  MAX_PENDING_PAIRINGS_PER_USER,
  PAIRING_TTL_SECONDS,
} from "../src/app";
import { NO_RECEIPTS, type Delivery, type Payload } from "../src/app";
import { fromBase64, toBase64 } from "../src/encoding";
import { heartbeatAlerts, MAX_HEARTBEATS_PER_USER } from "../src/heartbeats";
import { MAX_HOOKS_PER_USER } from "../src/hooks";
import { MAX_FCM_DATA_BYTES, sealedDataBytes } from "../src/publish";
import { UnregisteredError } from "../src/push";
import type { PushMessage, Sender } from "../src/push";
import { open, seal } from "../src/seal";
import type { Stream, StreamMessage } from "../src/stream";

const ADMIN = env.ADMIN_TOKEN;

/** The real question boxes: they take the app's clock, so the fake one works. */
const replies = (e: Env, id: string) => e.REPLY_BOX.get(e.REPLY_BOX.idFromName(id));
const receipts = (e: Env, id: string) => e.RECEIPT_BOX.get(e.RECEIPT_BOX.idFromName(id));

class FakeSender implements Sender {
  sent: PushMessage[] = [];
  unregistered = new Set<string>();
  send(message: PushMessage): Promise<void> {
    if (this.unregistered.has(message.token)) return Promise.reject(new UnregisteredError("gone"));
    this.sent.push(message);
    return Promise.resolve();
  }
}

class FakeStreams {
  delivered = new Map<string, StreamMessage[]>();
  connected = new Set<string>();
  closed: string[] = [];
  failing = false;
  closeFails = false;
  for(deviceId: string): Stream {
    return {
      deliver: (m) => {
        if (this.failing) return Promise.reject(new Error("object unavailable"));
        this.delivered.set(deviceId, [...(this.delivered.get(deviceId) ?? []), m]);
        return Promise.resolve(this.connected.has(deviceId) ? "sent" : "queued");
      },
      close: () => {
        if (this.closeFails) return Promise.reject(new Error("object unavailable"));
        this.closed.push(deviceId);
        return Promise.resolve();
      },
      fetch: () => Promise.resolve(new Response(`stream of ${deviceId}`)),
    };
  }
}

interface Registration {
  device_id: string;
  device_token: string;
  seal_key: string;
}

interface NotifyResponse {
  id: string;
  deliveries: Delivery[];
}

let USER: string;
let sender: FakeSender;
let streams: FakeStreams;
let now: number;
let app: ReturnType<typeof createApp>;

beforeEach(async () => {
  await env.DB.batch([
    env.DB.prepare("DELETE FROM devices"),
    env.DB.prepare("DELETE FROM api_keys"),
    env.DB.prepare("DELETE FROM pairings"),
    env.DB.prepare("DELETE FROM users"),
    env.DB.prepare("DELETE FROM invites"),
    ...["channels", "channel_members", "subscriptions", "channel_invites", "api_key_channels", "heartbeats", "hooks"].map((t) =>
      env.DB.prepare(`DELETE FROM ${t}`),
    ),
  ]);
  sender = new FakeSender();
  streams = new FakeStreams();
  now = 1_790_000_000;
  app = createApp({ sender: () => sender, stream: (_, id) => streams.for(id), replies, receipts, now: () => now });
  USER = await signup("alice");
});

async function call<T = unknown>(
  method: string,
  path: string,
  bearer: string,
  body: unknown,
  wantStatus: number,
  bindings: Env = env,
): Promise<T> {
  const headers: Record<string, string> = { "content-type": "application/json" };
  if (bearer) headers.authorization = `Bearer ${bearer}`;
  const res = await app.request(
    path,
    { method, headers, body: body === undefined ? undefined : JSON.stringify(body) },
    bindings,
  );
  const text = await res.text();
  expect(res.status, `${method} ${path}: ${text}`).toBe(wantStatus);
  return (text ? JSON.parse(text) : undefined) as T;
}

async function invite(by = ADMIN): Promise<string> {
  return (await call<{ code: string }>("POST", "/v1/invites", by, undefined, 201)).code;
}

/** Signs up a new user through an operator invite and returns their token. */
async function signup(name: string): Promise<string> {
  const { token } = await call<{ token: string }>("POST", "/v1/users", "", { invite: await invite(), name }, 201);
  return token;
}

async function pair(name: string, fcmToken: string, owner = USER): Promise<Registration> {
  const { code } = await call<{ code: string }>("POST", "/v1/pairings", owner, undefined, 201);
  return call<Registration>("POST", "/v1/devices", "", { code, name, fcm_token: fcmToken }, 201);
}

async function apiKey(owner = USER): Promise<string> {
  const { key } = await call<{ key: string }>("POST", "/v1/keys", owner, { name: "ci" }, 201);
  return key;
}

describe("notifications", () => {
  it("seals the payload separately for each device", async () => {
    const phone = await pair("phone", "fcm-phone");
    const tablet = await pair("tablet", "fcm-tablet");
    const resp = await call<NotifyResponse>(
      "POST",
      "/v1/notifications",
      await apiKey(),
      { title: "Build done", body: "main is green", priority: "high" },
      200,
    );

    expect(resp.deliveries.map((d) => d.status)).toEqual(["sent", "sent"]);
    for (const [i, reg] of [phone, tablet].entries()) {
      const m = sender.sent[i]!;
      expect(m.high).toBe(true);
      const plaintext = await open(fromBase64(reg.seal_key), reg.device_id, {
        nonce: fromBase64(m.data.n!),
        ciphertext: fromBase64(m.data.c!),
      });
      const p = JSON.parse(new TextDecoder().decode(plaintext)) as Payload;
      expect(p).toMatchObject({ id: resp.id, title: "Build done", body: "main is green", priority: "high" });
    }
  });

  it("refuses text that FCM could not carry once escaped and sealed", async () => {
    const phone = await pair("phone", "fcm-phone");
    await call("POST", "/v1/notifications", USER, { title: "t", body: '"'.repeat(2048) }, 400);
    expect(sender.sent).toHaveLength(0);

    // The estimate is exact: the largest message that fits arrives at 4 KiB or less.
    const body = "x".repeat(1548);
    await call("POST", "/v1/notifications", USER, { title: "t".repeat(256), body: '"'.repeat(500) + body }, 200);
    const m = sender.sent[0]!;
    const bytes = Object.entries(m.data).reduce((sum, [k, v]) => sum + k.length + v.length, 0);
    expect(bytes).toBe(sealedDataBytes(await payloadFor(phone, 0)));
    expect(bytes).toBeLessThanOrEqual(MAX_FCM_DATA_BYTES);

    // A WebSocket device has no such limit, and a phone without a token
    // (skipped as unreachable) does not hold it back.
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", USER, undefined, 201);
    const desktop = await call<Registration>("POST", "/v1/devices", "", { code, name: "desktop", transport: "ws" }, 201);
    await env.DB.prepare("UPDATE devices SET fcm_token = '' WHERE id = ?").bind(phone.device_id).run();
    const sent = await call<NotifyResponse>("POST", "/v1/notifications", USER, { title: "t", body: '"'.repeat(2048) }, 200);
    expect(sent.deliveries).toEqual([
      { device_id: phone.device_id, status: "unreachable" },
      { device_id: desktop.device_id, status: "queued" },
    ]);
  });

  it.each(["", null])("treats priority %j as default", async (priority) => {
    await pair("phone", "fcm-phone");
    await call("POST", "/v1/notifications", await apiKey(), { title: "hi", priority }, 200);
    expect(sender.sent[0]!.high).toBe(false);
  });

  it("reports per-device failures when the sender cannot be built", async () => {
    await pair("phone", "fcm-phone");
    const key = await apiKey();
    app = createApp({
      sender: () => {
        throw new Error("FCM_SERVICE_ACCOUNT secret is not set");
      },
      stream: (_, id) => streams.for(id),
      replies,
      receipts,
      now: () => now,
    });
    const resp = await call<NotifyResponse>("POST", "/v1/notifications", key, { title: "hi" }, 200);
    expect(resp.deliveries.map((d) => d.status)).toEqual(["failed"]);
  });

  it("still answers when clearing an unregistered token fails", async () => {
    await pair("phone", "fcm-gone");
    sender.unregistered.add("fcm-gone");
    const key = await apiKey();
    const failingDb = new Proxy(env.DB, {
      get(target, prop, receiver) {
        if (prop !== "prepare") return Reflect.get(target, prop, receiver) as unknown;
        return (sql: string) =>
          sql.startsWith("UPDATE devices SET fcm_token")
            ? { bind: () => ({ run: () => Promise.reject(new Error("D1 unavailable")) }) }
            : target.prepare(sql);
      },
    });
    const resp = await call<NotifyResponse>(
      "POST",
      "/v1/notifications",
      key,
      { title: "hi" },
      200,
      { ...env, DB: failingDb },
    );
    expect(resp.deliveries[0]!.status).toBe("unreachable");
  });

  it("rejects oversized requests", async () => {
    const res = await app.request(
      "/v1/notifications",
      { method: "POST", headers: { "content-type": "application/json" }, body: "x".repeat(20_000) },
      env,
    );
    expect(res.status).toBe(413);
  });

  it("targets only the selected devices", async () => {
    await pair("phone", "fcm-phone");
    const tablet = await pair("tablet", "fcm-tablet");
    await call("POST", "/v1/notifications", await apiKey(), { title: "hi", device_ids: [tablet.device_id] }, 200);
    expect(sender.sent.map((m) => m.token)).toEqual(["fcm-tablet"]);
  });

  it("clears tokens the transport reports as unregistered", async () => {
    await pair("phone", "fcm-gone");
    sender.unregistered.add("fcm-gone");
    const resp = await call<NotifyResponse>("POST", "/v1/notifications", await apiKey(), { title: "hi" }, 200);
    expect(resp.deliveries[0]!.status).toBe("unreachable");
    const devices = await call<{ reachable: boolean }[]>("GET", "/v1/devices", USER, undefined, 200);
    expect(devices[0]!.reachable).toBe(false);
  });

  it.each([
    { title: " " },
    { title: "hi", priority: "urgent" },
    { title: "x".repeat(257) },
    { title: "hi", device_ids: "dev_x" },
    { title: "hi", device_id: "dev_x" },
    { title: "hi", priority: 0 },
    { title: "hi", priority: false },
    { title: "a\ud800" },
    { title: "hi", body: "b\udc00" },
  ])("rejects invalid request %j", async (body) => {
    await pair("phone", "fcm");
    await call("POST", "/v1/notifications", await apiKey(), body, 400);
  });
});

describe("authentication", () => {
  it("accepts each credential only where it belongs", async () => {
    const reg = await pair("phone", "fcm-phone");
    const key = await apiKey();
    const note = { title: "hi" };

    await call("POST", "/v1/pairings", "wrong", undefined, 401);
    await call("POST", "/v1/pairings", key, undefined, 401);
    await call("POST", "/v1/pairings", ADMIN, undefined, 401);
    await call("POST", "/v1/keys", key, { name: "x" }, 401);
    await call("GET", "/v1/users", USER, undefined, 401);
    await call("POST", "/v1/invites", key, undefined, 401);
    await call("POST", "/v1/notifications", "", note, 401);
    await call("POST", "/v1/notifications", ADMIN, note, 401);
    await call("POST", "/v1/notifications", reg.device_token, note, 401);
    await call("POST", "/v1/notifications", "nk_unknown", note, 401);
    await call("POST", "/v1/notifications", "nu_unknown", note, 401);
    await call("GET", "/v1/devices", ADMIN, undefined, 401);
    await call("PUT", "/v1/device", key, { fcm_token: "x" }, 401);
    await call("GET", "/v1/devices", reg.device_token, undefined, 401);
    await call("DELETE", `/v1/devices/${reg.device_id}`, key, undefined, 401);
  });

  it("lets API keys list devices without activity timestamps", async () => {
    const reg = await pair("phone", "fcm-phone");
    const devices = await call<object[]>("GET", "/v1/devices", await apiKey(), undefined, 200);
    expect(devices).toEqual([{ id: reg.device_id, name: "phone", transport: "fcm", reachable: true }]);
    const full = await call<object[]>("GET", "/v1/devices", USER, undefined, 200);
    expect(full[0]).toHaveProperty("last_seen_at");
  });

  it.each([
    ["unset", undefined],
    ["empty", ""],
    ["too short", "short"],
  ])("fails closed when ADMIN_TOKEN is %s", async (_, token) => {
    const bindings = { ...env, ADMIN_TOKEN: token } as Env;
    await call("GET", "/v1/users", "", undefined, 401, bindings);
    await call("GET", "/v1/users", token ?? "", undefined, 401, bindings);
    await call("POST", "/v1/invites", "", undefined, 401, bindings);
  });

  it("tolerates extra whitespace after Bearer", async () => {
    const res = await app.request("/v1/keys", { headers: { authorization: `bearer   ${USER}` } }, env);
    expect(res.status).toBe(200);
  });

  it("rejects names that are not valid UTF-8", async () => {
    const { error } = await call<{ error: string }>("POST", "/v1/keys", USER, { name: "a\ud800" }, 400);
    expect(error).toBe("name must be valid UTF-8");
  });

  it.each([
    ["NUL", "a\u0000b"],
    ["an escape", "\u001b[2Jcleared"],
    ["a carriage return", "ok\rspoofed"],
    ["DEL", "a\u007fb"],
    ["a C1 control", "a\u009bb"],
    ["a bidi override", "invoice\u202Efdp.exe"],
    ["a bidi isolate", "a\u2066b\u2069"],
  ])("rejects %s in names and message text", async (_, text) => {
    await pair("phone", "fcm-phone");
    const { error } = await call<{ error: string }>("POST", "/v1/keys", USER, { name: text }, 400);
    expect(error).toBe("name must not contain control or bidi override characters");
    for (const req of [{ title: text }, { title: "ok", body: text }]) {
      const res = await call<{ error: string }>("POST", "/v1/notifications", USER, req, 400);
      expect(res.error).toMatch(/must not contain control or bidi override characters/);
    }
  });

  it("keeps newlines, tabs, bidi marks and joiners", async () => {
    await pair("phone", "fcm-phone");
    const body = "line one\n\tline two \u200F\u05E9\u05DC\u05D5\u05DD\u200E \u{1F468}\u200D\u{1F4BB}";
    await call("POST", "/v1/notifications", USER, { title: "\u05E9\u05DC\u05D5\u05DD \u{1F44B}", body }, 200);
    await call("POST", "/v1/keys", USER, { name: "\u05E9\u05DC\u05D5\u05DD" }, 201);
    await call("POST", "/v1/keys", USER, { name: "line\nbreak" }, 400);
  });

  it("rejects a revoked API key", async () => {
    await pair("phone", "fcm-phone");
    const { id, key } = await call<{ id: string; key: string }>("POST", "/v1/keys", USER, { name: "ci" }, 201);
    await call("DELETE", `/v1/keys/${id}`, USER, undefined, 204);
    await call("POST", "/v1/notifications", key, { title: "hi" }, 401);
  });
});

describe("pairing", () => {
  it("redeems a code once", async () => {
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", USER, undefined, 201);
    const req = { code: code.toLowerCase().replace("-", ""), name: "phone", fcm_token: "fcm" };
    await call("POST", "/v1/devices", "", req, 201);
    await call("POST", "/v1/devices", "", req, 403);
  });

  it("rejects an expired code", async () => {
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", USER, undefined, 201);
    now += PAIRING_TTL_SECONDS;
    await call("POST", "/v1/devices", "", { code, name: "phone", fcm_token: "fcm" }, 403);
  });
});

describe("device self-management", () => {
  it("updates its token, sends itself a test, and unpairs", async () => {
    const reg = await pair("phone", "fcm-old");
    await call("PUT", "/v1/device", reg.device_token, { fcm_token: "fcm-new" }, 204);
    await call("POST", "/v1/device/test", reg.device_token, undefined, 200);
    expect(sender.sent[0]!.token).toBe("fcm-new");

    await call("DELETE", "/v1/device", reg.device_token, undefined, 204);
    await call("POST", "/v1/device/test", reg.device_token, undefined, 401);
  });
});

describe("WebSocket transport", () => {
  async function pairDesktop(name = "desktop", owner = USER): Promise<Registration> {
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", owner, undefined, 201);
    return call<Registration>("POST", "/v1/devices", "", { code, name, transport: "ws" }, 201);
  }

  const stream = (token: string, headers: Record<string, string> = { upgrade: "websocket" }) =>
    app.request("/v1/device/stream", { headers: { authorization: `Bearer ${token}`, ...headers } }, env);

  it("pairs without an FCM token and counts as reachable", async () => {
    const desktop = await pairDesktop();
    const devices = await call<object[]>("GET", "/v1/devices", await apiKey(), undefined, 200);
    expect(devices).toEqual([{ id: desktop.device_id, name: "desktop", transport: "ws", reachable: true }]);
  });

  it.each([
    { transport: "ws", fcm_token: "x" },
    { transport: "ws", fcm_token: "" },
    { transport: "apns", fcm_token: "x" },
    { transport: "fcm" },
  ])("rejects pairing request %j", async (fields) => {
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", USER, undefined, 201);
    await call("POST", "/v1/devices", "", { code, name: "d", ...fields }, 400);
  });

  it("queues a sealed frame, reporting sent only while connected", async () => {
    const desktop = await pairDesktop();
    await pair("phone", "fcm-phone");
    const key = await apiKey();

    const queued = await call<NotifyResponse>("POST", "/v1/notifications", key, { title: "hi", priority: "high" }, 200);
    expect(queued.deliveries.map((d) => d.status).sort()).toEqual(["queued", "sent"]);
    expect(sender.sent.map((m) => m.token)).toEqual(["fcm-phone"]);

    const [m] = streams.delivered.get(desktop.device_id)!;
    expect(m!.id).toBe(queued.id);
    expect(m!.expiresAt).toBe(now + 24 * 60 * 60);
    const frame = JSON.parse(m!.frame) as Record<string, string>;
    expect(Object.keys(frame).sort()).toEqual(["c", "id", "n", "type", "v"]);
    expect(frame).toMatchObject({ type: "message", id: queued.id, v: "1" });
    const plaintext = await open(fromBase64(desktop.seal_key), desktop.device_id, {
      nonce: fromBase64(frame.n!),
      ciphertext: fromBase64(frame.c!),
    });
    expect(JSON.parse(new TextDecoder().decode(plaintext))).toMatchObject({ id: queued.id, title: "hi", priority: "high" });

    streams.connected.add(desktop.device_id);
    const sent = await call<NotifyResponse>("POST", "/v1/notifications", key, { title: "again" }, 200);
    expect(sent.deliveries.map((d) => d.status)).toEqual(["sent", "sent"]);
  });

  it("reports a failing stream as failed", async () => {
    await pairDesktop();
    streams.failing = true;
    const resp = await call<NotifyResponse>("POST", "/v1/notifications", USER, { title: "hi" }, 200);
    expect(resp.deliveries).toEqual([expect.objectContaining({ status: "failed" })]);
  });

  it("hands the upgrade to the device's own stream", async () => {
    const desktop = await pairDesktop();
    const phone = await pair("phone", "fcm-phone");

    const res = await stream(desktop.device_token);
    expect(res.status).toBe(200);
    expect(await res.text()).toBe(`stream of ${desktop.device_id}`);
    expect((await stream(desktop.device_token, {})).status).toBe(426);
    expect((await stream(phone.device_token)).status).toBe(409);
    expect((await stream(await apiKey())).status).toBe(401);
    expect((await stream(USER)).status).toBe(401);
    await call("PUT", "/v1/device", desktop.device_token, { fcm_token: "x" }, 409);
  });

  it("closes the stream when the device is unpaired", async () => {
    const a = await pairDesktop("a");
    const b = await pairDesktop("b");
    const phone = await pair("phone", "fcm-phone");
    await call("DELETE", `/v1/devices/${a.device_id}`, USER, undefined, 204);
    await call("DELETE", "/v1/device", b.device_token, undefined, 204);
    await call("DELETE", `/v1/devices/${phone.device_id}`, USER, undefined, 204);
    expect(streams.closed).toEqual([a.device_id, b.device_id]);
    expect((await stream(b.device_token)).status).toBe(401);
  });

  it("keeps the device for a retry when its stream cannot be closed", async () => {
    const desktop = await pairDesktop();
    streams.closeFails = true;
    await call("DELETE", `/v1/devices/${desktop.device_id}`, USER, undefined, 503);
    await call("DELETE", "/v1/device", desktop.device_token, undefined, 503);
    expect(await call<object[]>("GET", "/v1/devices", USER, undefined, 200)).toHaveLength(1);

    streams.closeFails = false;
    await call("DELETE", `/v1/devices/${desktop.device_id}`, USER, undefined, 204);
    expect(streams.closed).toEqual([desktop.device_id]);
  });

  it("does not close another user's device stream", async () => {
    const desktop = await pairDesktop();
    await call("DELETE", `/v1/devices/${desktop.device_id}`, await signup("mallory"), undefined, 404);
    expect(streams.closed).toEqual([]);
  });
});

describe("QR pairing", () => {
  it("returns a pairing link for this origin, and a QR on request", async () => {
    const plain = await call<{ code: string; uri: string; qr_svg?: string }>("POST", "/v1/pairings", USER, undefined, 201);
    expect(plain.uri).toBe(`notifier://pair?server=http%3A%2F%2Flocalhost&code=${plain.code}`);
    expect(plain.qr_svg).toBeUndefined();

    const withQr = await call<{ qr_svg: string }>("POST", "/v1/pairings?qr=svg", USER, undefined, 201);
    expect(withQr.qr_svg).toMatch(/^<svg [^>]*viewBox/);
  });

  it("serves the pairing page with a strict policy and no secrets", async () => {
    const old = await app.request("/admin/pair", {}, env);
    expect(old.status).toBe(301);
    expect(old.headers.get("location")).toBe("/pair");

    const page = await app.request("/pair", {}, env);
    expect(page.status).toBe(200);
    const csp = page.headers.get("content-security-policy") ?? "";
    expect(csp).toContain("default-src 'none'");
    expect(csp).toContain("script-src 'self'");
    expect(csp).not.toContain("unsafe-inline");
    expect(page.headers.get("cache-control")).toBe("no-store");
    expect(page.headers.get("referrer-policy")).toBe("no-referrer");
    const html = await page.text();
    expect(html).not.toContain(ADMIN);
    expect(html).not.toContain(USER);
    expect(html).not.toMatch(/<script>|<style>|\son\w+=/);

    for (const [path, type] of [
      ["/pair.js", "text/javascript"],
      ["/pair.css", "text/css"],
    ]) {
      const res = await app.request(path!, {}, env);
      expect(res.status).toBe(200);
      expect(res.headers.get("content-type")).toContain(type);
    }
  });
});

describe("users", () => {
  it("keeps each user's devices and keys to themselves", async () => {
    const bob = await signup("bob");
    const alicePhone = await pair("phone", "fcm-alice");
    const bobPhone = await pair("phone", "fcm-bob", bob);
    const aliceKey = await apiKey();

    const listed = await call<{ id: string }[]>("GET", "/v1/devices", bob, undefined, 200);
    expect(listed.map((d) => d.id)).toEqual([bobPhone.device_id]);

    // Another user's device is "not found", never "forbidden".
    await call("DELETE", `/v1/devices/${alicePhone.device_id}`, bob, undefined, 404);
    const { id: keyId } = (await call<{ id: string }[]>("GET", "/v1/keys", USER, undefined, 200))[0]!;
    await call("DELETE", `/v1/keys/${keyId}`, bob, undefined, 404);
    expect(await call<object[]>("GET", "/v1/keys", bob, undefined, 200)).toEqual([]);

    // Sending reaches only the sender's devices, even when naming another's.
    await call("POST", "/v1/notifications", aliceKey, { title: "hi", device_ids: [bobPhone.device_id] }, 404);
    await call(
      "POST",
      "/v1/notifications",
      aliceKey,
      { title: "hi", device_ids: [alicePhone.device_id, bobPhone.device_id] },
      404,
    );
    await call("POST", "/v1/notifications", aliceKey, { title: "hi" }, 200);
    await call("POST", "/v1/notifications", bob, { title: "hi" }, 200);
    expect(sender.sent.map((m) => m.token)).toEqual(["fcm-alice", "fcm-bob"]);
  });

  it("signs up once per invite, before it expires", async () => {
    const code = await invite();
    const req = { invite: code.toLowerCase().replaceAll("-", " "), name: "carol" };
    const { user_id, token } = await call<{ user_id: string; token: string }>("POST", "/v1/users", "", req, 201);
    expect(token).toMatch(/^nu_/);
    await call("POST", "/v1/users", "", req, 403);
    expect(await call("GET", "/v1/user", token, undefined, 200)).toMatchObject({ id: user_id, name: "carol" });

    const late = await invite(USER);
    now += INVITE_TTL_SECONDS;
    await call("POST", "/v1/users", "", { invite: late, name: "dave" }, 403);
    await call("POST", "/v1/users", "", { invite: "AAAA-BBBB-CCCC", name: "eve" }, 403);
    await call("POST", "/v1/users", "", { invite: late, name: "eve", admin: true }, 400);
  });

  it("does not accept a pairing code as an invite", async () => {
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", USER, undefined, 201);
    await call("POST", "/v1/users", "", { invite: code, name: "mallory" }, 403);
  });

  it("lets the operator list users, reissue tokens, and disable users", async () => {
    const reg = await pair("phone", "fcm-phone");
    const key = await apiKey();
    const users = await call<{ id: string; name: string; devices: number }[]>("GET", "/v1/users", ADMIN, undefined, 200);
    expect(users).toMatchObject([{ name: "alice", devices: 1 }]);
    const id = users[0]!.id;

    const { token } = await call<{ token: string }>("POST", `/v1/users/${id}/token`, ADMIN, undefined, 200);
    await call("GET", "/v1/user", USER, undefined, 401);
    await call("GET", "/v1/user", token, undefined, 200);
    await call("POST", "/v1/users/usr_missing/token", ADMIN, undefined, 404);

    await call("POST", `/v1/users/${id}/disable`, ADMIN, undefined, 204);
    await call("GET", "/v1/user", token, undefined, 401);
    await call("POST", "/v1/notifications", key, { title: "hi" }, 401);
    await call("POST", "/v1/device/test", reg.device_token, undefined, 401);

    await call("POST", `/v1/users/${id}/enable`, ADMIN, undefined, 204);
    await call("POST", "/v1/notifications", key, { title: "hi" }, 200);
  });

  it("caps devices at redemption, not only when a code is made", async () => {
    for (let i = 0; i < MAX_DEVICES_PER_USER - 1; i++) {
      await env.DB.prepare(
        "INSERT INTO devices (id, user_id, name, token_hash, seal_key, created_at, last_seen_at) SELECT ?, id, 'x', ?, 'AAAA', 0, 0 FROM users",
      )
        .bind(`dev_${i}`, `h${i}`)
        .run();
    }
    const codes: string[] = [];
    for (let i = 0; i < 2; i++) {
      codes.push((await call<{ code: string }>("POST", "/v1/pairings", USER, undefined, 201)).code);
    }
    await call("POST", "/v1/devices", "", { code: codes[0], name: "a", fcm_token: "x" }, 201);
    await call("POST", "/v1/devices", "", { code: codes[1], name: "b", fcm_token: "x" }, 403);
    await call("POST", "/v1/pairings", USER, undefined, 403);
  });

  it("caps pending pairing codes per user", async () => {
    for (let i = 0; i < MAX_PENDING_PAIRINGS_PER_USER; i++) await call("POST", "/v1/pairings", USER, undefined, 201);
    await call("POST", "/v1/pairings", USER, undefined, 403);
    now += PAIRING_TTL_SECONDS;
    await call("POST", "/v1/pairings", USER, undefined, 201);
  });

  it("voids the invites of a disabled user", async () => {
    const code = await invite(USER);
    const { id } = await call<{ id: string }>("GET", "/v1/user", USER, undefined, 200);
    await call("POST", `/v1/users/${id}/disable`, ADMIN, undefined, 204);
    await call("POST", "/v1/users", "", { invite: code, name: "alt" }, 403);
  });

  it("caps API keys and invites per user, counting used invites", async () => {
    for (let i = 0; i < MAX_API_KEYS_PER_USER; i++) await apiKey();
    await call("POST", "/v1/keys", USER, { name: "one more" }, 403);

    // Redeeming one's own invites must not free their slots.
    for (let i = 0; i < MAX_INVITES_PER_USER; i++) {
      await call("POST", "/v1/users", "", { invite: await invite(USER), name: `alt${i}` }, 201);
    }
    await call("POST", "/v1/invites", USER, undefined, 403);
    await invite(ADMIN);

    const { id } = await call<{ id: string }>("GET", "/v1/user", USER, undefined, 200);
    const users = await call<{ name: string; invited_by?: string }[]>("GET", "/v1/users", ADMIN, undefined, 200);
    expect(users.filter((u) => u.invited_by === id)).toHaveLength(MAX_INVITES_PER_USER);
    expect(users.find((u) => u.name === "alice")?.invited_by).toBeUndefined();
  });

  it("rate-limits notifications per user", async () => {
    const reg = await pair("phone", "fcm-phone");
    const keys: string[] = [];
    const limited = {
      ...env,
      NOTIFY_LIMITER: {
        limit: ({ key }: { key: string }) => {
          keys.push(key);
          return Promise.resolve({ success: false });
        },
      },
    } as Env;
    await call("POST", "/v1/notifications", USER, { title: "hi" }, 429, limited);
    await call("POST", "/v1/device/test", reg.device_token, undefined, 429, limited);
    const { id } = await call<{ id: string }>("GET", "/v1/user", USER, undefined, 200);
    expect(keys).toEqual([id, id]);
    expect(sender.sent).toEqual([]);
  });
});

interface ChannelView {
  id: string;
  name: string;
  subscribed?: boolean;
}

async function payloadFor(reg: Registration, index: number): Promise<Payload> {
  const m = sender.sent[index]!;
  const plaintext = await open(fromBase64(reg.seal_key), reg.device_id, {
    nonce: fromBase64(m.data.n!),
    ciphertext: fromBase64(m.data.c!),
  });
  return JSON.parse(new TextDecoder().decode(plaintext)) as Payload;
}

async function channel(name: string, owner = USER): Promise<string> {
  return (await call<{ id: string }>("POST", "/v1/channels", owner, { name }, 201)).id;
}

describe("channels", () => {
  it("subscribes new devices to the user's default channel", async () => {
    const reg = await pair("phone", "fcm-phone");
    const channels = await call<ChannelView[]>("GET", "/v1/device/channels", reg.device_token, undefined, 200);
    expect(channels).toEqual([{ id: expect.stringMatching(/^ch_default_/) as string, name: "default", subscribed: true }]);

    await call("POST", "/v1/notifications", USER, { title: "hi" }, 200);
    expect((await payloadFor(reg, 0)).channel).toBe("default");
  });

  it("lets a member subscribe to a shared channel, but only the owner publish", async () => {
    const bob = await signup("bob");
    const homelab = await channel("homelab");
    const bobPhone = await pair("phone", "fcm-bob", bob);

    const { code, uri } = await call<{ code: string; uri: string }>(
      "POST",
      `/v1/channels/${homelab}/invites`,
      USER,
      undefined,
      201,
    );
    expect(uri).toBe(`notifier://join?server=http%3A%2F%2Flocalhost&code=${code}`);
    await call("POST", `/v1/channels/${homelab}/invites`, bob, undefined, 404);

    // Joining from the phone subscribes that phone.
    const joined = await call<ChannelView>("POST", "/v1/device/memberships", bobPhone.device_token, { invite: code }, 201);
    expect(joined).toEqual({ id: homelab, name: "alice/homelab" });
    await call("POST", "/v1/memberships", bob, { invite: code }, 403);

    // The sender learns how other members' devices fared, not their IDs.
    const sent = await call<NotifyResponse & { members?: object }>(
      "POST",
      "/v1/notifications",
      USER,
      { title: "disk full", channels: ["homelab"] },
      200,
    );
    expect(sent).toMatchObject({ deliveries: [], members: { sent: 1 } });
    expect(sender.sent.map((m) => m.token)).toEqual(["fcm-bob"]);
    expect((await payloadFor(bobPhone, 0)).channel).toBe("alice/homelab");

    // Bob receives but cannot publish: his "homelab" would be his own.
    await call("POST", "/v1/notifications", bob, { title: "x", channels: ["homelab"] }, 404);

    // Removing Bob stops delivery, even though his phone subscribed.
    const members = await call<{ id: string; name: string }[]>("GET", `/v1/channels/${homelab}/members`, USER, undefined, 200);
    expect(members.map((m) => m.name)).toEqual(["alice", "bob"]);
    await call("GET", `/v1/channels/${homelab}/members`, bob, undefined, 404);
    const bobId = members.find((m) => m.name === "bob")!.id;
    await call("DELETE", `/v1/channels/${homelab}/members/${bobId}`, USER, undefined, 204);
    await call("POST", "/v1/notifications", USER, { title: "again", channels: ["homelab"] }, 404);
  });

  it("lets a member leave, but not remove others", async () => {
    const bob = await signup("bob");
    const carol = await signup("carol");
    const homelab = await channel("homelab");
    const ids: Record<string, string> = {};
    for (const [name, token] of [
      ["bob", bob],
      ["carol", carol],
    ] as const) {
      const { code } = await call<{ code: string }>("POST", `/v1/channels/${homelab}/invites`, USER, undefined, 201);
      await call("POST", "/v1/memberships", token, { invite: code }, 201);
      ids[name] = (await call<{ id: string }>("GET", "/v1/user", token, undefined, 200)).id;
    }
    await call("DELETE", `/v1/channels/${homelab}/members/${ids.carol}`, bob, undefined, 404);
    await call("DELETE", `/v1/channels/${homelab}/members/${ids.bob}`, bob, undefined, 204);
    const aliceId = (await call<{ id: string }>("GET", "/v1/user", USER, undefined, 200)).id;
    await call("DELETE", `/v1/channels/${homelab}/members/${aliceId}`, USER, undefined, 404);
  });

  it("delivers once per device across several channels", async () => {
    const ci = await channel("ci");
    await channel("alerts");
    const reg = await pair("phone", "fcm-phone");
    const all = await call<ChannelView[]>("GET", "/v1/device/channels", reg.device_token, undefined, 200);
    await call("PUT", "/v1/device/channels", reg.device_token, { channel_ids: all.map((ch) => ch.id) }, 200);

    const resp = await call<NotifyResponse>(
      "POST",
      "/v1/notifications",
      USER,
      { title: "hi", channels: ["ci", "alerts", "ci"], device_ids: [reg.device_id] },
      200,
    );
    expect(resp.deliveries).toHaveLength(1);
    expect(await payloadFor(reg, 0)).toMatchObject({ channel: "ci", channel_id: ci });
  });

  it("limits a scoped API key to its channels", async () => {
    const ci = await channel("ci");
    await channel("homelab");
    const reg = await pair("phone", "fcm-phone");
    const { key, channels } = await call<{ key: string; channels: string[] }>(
      "POST",
      "/v1/keys",
      USER,
      { name: "ci", channels: ["ci"] },
      201,
    );
    expect(channels).toEqual(["ci"]);
    const all = await call<ChannelView[]>("GET", "/v1/device/channels", reg.device_token, undefined, 200);
    await call("PUT", "/v1/device/channels", reg.device_token, { channel_ids: all.map((ch) => ch.id) }, 200);

    await call("POST", "/v1/notifications", key, { title: "ok", channels: ["ci"] }, 200);
    await call("POST", "/v1/notifications", key, { title: "no", channels: ["homelab"] }, 403);
    await call("POST", "/v1/notifications", key, { title: "no" }, 403);
    await call("POST", "/v1/notifications", key, { title: "no", channels: ["ci"], device_ids: [reg.device_id] }, 403);
    await call("POST", "/v1/keys", USER, { name: "x", channels: ["nope"] }, 404);
    expect(await call<{ channels: string[] }[]>("GET", "/v1/keys", USER, undefined, 200)).toMatchObject([
      { name: "ci", channels: ["ci"] },
    ]);

    // Deleting its only channel leaves the key scoped to nothing, not unscoped.
    await call("DELETE", `/v1/channels/${ci}`, USER, undefined, 204);
    await call("POST", "/v1/notifications", key, { title: "no" }, 403);
    await call("POST", "/v1/notifications", key, { title: "no", device_ids: [reg.device_id] }, 403);
    await call("POST", "/v1/notifications", key, { title: "no", channels: ["homelab"] }, 403);
  });

  it("refuses subscriptions to channels the owner has not joined", async () => {
    const bob = await signup("bob");
    const secret = await channel("secret", bob);
    const reg = await pair("phone", "fcm-phone");
    await call("PUT", "/v1/device/channels", reg.device_token, { channel_ids: [secret] }, 404);
    await call("PUT", `/v1/devices/${reg.device_id}/channels`, USER, { channel_ids: [secret] }, 404);
    await call("PUT", `/v1/devices/${reg.device_id}/channels`, bob, { channel_ids: [] }, 404);

    // A send without channels goes to default, so no device may leave it.
    await call("PUT", `/v1/devices/${reg.device_id}/channels`, USER, { channel_ids: [] }, 400);
    await call("PUT", "/v1/device/channels", reg.device_token, { channel_ids: [] }, 400);
    const ci = await channel("ci");
    const all = await call<ChannelView[]>("GET", "/v1/device/channels", reg.device_token, undefined, 200);
    const def = all.find((ch) => ch.name === "default");
    const subs = await call<ChannelView[]>("PUT", "/v1/device/channels", reg.device_token, { channel_ids: [def!.id] }, 200);
    expect(subs.filter((ch) => ch.subscribed).map((ch) => ch.id)).toEqual([def!.id]);
    expect(subs.some((ch) => ch.id === ci)).toBe(true);
  });

  it("caps channels, invites, members, and channels per request", async () => {
    for (let i = 0; i < MAX_CHANNELS_PER_USER; i++) await channel(`c${i}`);
    await call("POST", "/v1/channels", USER, { name: "one-more" }, 403);
    await call("POST", "/v1/channels", USER, {}, 400);

    const names = Array.from({ length: MAX_CHANNELS_PER_REQUEST + 1 }, (_, i) => `c${i}`);
    await call("POST", "/v1/notifications", USER, { title: "x", channels: names }, 400);

    const c0 = (await call<{ id: string; name: string }[]>("GET", "/v1/channels", USER, undefined, 200)).find(
      (ch) => ch.name === "c0",
    )!.id;
    const invites: string[] = [];
    for (let i = 0; i < MAX_PENDING_INVITES_PER_CHANNEL; i++) {
      invites.push((await call<{ code: string }>("POST", `/v1/channels/${c0}/invites`, USER, undefined, 201)).code);
    }
    await call("POST", `/v1/channels/${c0}/invites`, USER, undefined, 403);

    // Fill the channel (the owner counts), then one invite too many.
    for (let i = 1; i < MAX_MEMBERS_PER_CHANNEL; i++) {
      const code = invites.pop() ?? (await call<{ code: string }>("POST", `/v1/channels/${c0}/invites`, USER, undefined, 201)).code;
      await call("POST", "/v1/memberships", await signup(`m${i}`), { invite: code }, 201);
    }
    const last = await call<{ code: string }>("POST", `/v1/channels/${c0}/invites`, USER, undefined, 201);
    await call("POST", "/v1/memberships", await signup("late"), { invite: last.code }, 403);
  });

  it("validates channel names and protects the default channel", async () => {
    await call("POST", "/v1/channels", USER, { name: "Has Spaces" }, 400);
    await channel("ci");
    await call("POST", "/v1/channels", USER, { name: "ci" }, 409);
    const list = await call<{ id: string; name: string; owned: boolean }[]>("GET", "/v1/channels", USER, undefined, 200);
    const def = list.find((ch) => ch.name === "default")!;
    await call("DELETE", `/v1/channels/${def.id}`, USER, undefined, 404);
    await call("POST", `/v1/channels/${def.id}/invites`, USER, undefined, 400);
    await call("POST", "/v1/channels", USER, { name: "default" }, 409);
    await call("POST", "/v1/notifications", USER, { title: "x", channels: ["../etc"] }, 400);
  });
});

describe("heartbeats", () => {
  interface Created {
    id: string;
    ping_url: string;
  }
  interface Listed {
    name: string;
    channel: string;
    state: string;
    last_ping_at?: string;
    due_at: string;
  }

  const create = (body: object, status = 201) => call<Created>("POST", "/v1/heartbeats", USER, body, status);
  const check = () => heartbeatAlerts({ sender: () => sender, stream: (_, id) => streams.for(id), replies, receipts, now: () => now }).check(env);

  async function ping(url: string, suffix = "", body?: string, wantStatus = 200): Promise<void> {
    const res = await app.request(new URL(url).pathname + suffix, { method: body === undefined ? "GET" : "POST", body }, env);
    expect(res.status, await res.text()).toBe(wantStatus);
  }

  it("creates, lists and deletes heartbeats", async () => {
    const hb = await create({ name: "backup", interval: 86400 });
    expect(hb.ping_url).toMatch(/^http:\/\/localhost\/v1\/ping\/nh_[a-z2-7]+$/);
    let list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list).toEqual([
      expect.objectContaining({ name: "backup", channel: "default", state: "up", interval: 86400, grace: 300 }),
    ]);
    expect(list[0]).not.toHaveProperty("last_ping_at");

    now += 60;
    await ping(hb.ping_url);
    list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]!.last_ping_at).toBeDefined();
    expect(Date.parse(list[0]!.due_at) / 1000).toBe(now + 86400 + 300);

    // Others cannot see or delete it; API keys cannot manage heartbeats.
    const bob = await signup("bob");
    expect(await call("GET", "/v1/heartbeats", bob, undefined, 200)).toEqual([]);
    await call("DELETE", `/v1/heartbeats/${hb.id}`, bob, undefined, 404);
    await call("GET", "/v1/heartbeats", await apiKey(), undefined, 401);
    await call("DELETE", `/v1/heartbeats/${hb.id}`, USER, undefined, 204);
    await ping(hb.ping_url, "", undefined, 404);
  });

  it("validates and caps heartbeats", async () => {
    await create({ name: "x", interval: 59 }, 400);
    await create({ name: "x", interval: 60.5 }, 400);
    await create({ name: "x", interval: 3600, grace: 86401 }, 400);
    await create({ name: "x", interval: 3600, channel: "nope" }, 404);
    await create({ name: "x\u001b", interval: 3600 }, 400);
    await create({ name: "x", interval: 3600, extra: 1 }, 400);
    await create({ name: "x", interval: 3600 });
    await create({ name: "x", interval: 3600 }, 409);
    for (let i = 1; i < MAX_HEARTBEATS_PER_USER; i++) await create({ name: `job${i}`, interval: 3600 });
    await create({ name: "one-too-many", interval: 3600 }, 403);
    await ping("http://localhost/v1/ping/nh_NOTASECRET", "", undefined, 404);
    await ping("http://localhost/v1/ping/nk_NOTASECRET", "", undefined, 404);
  });

  it("alerts a missed check-in once, then announces the recovery", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "backup", interval: 3600, grace: 60 });
    await ping(hb.ping_url);

    now += 3600 + 59;
    await check();
    expect(sender.sent).toHaveLength(0);

    now += 1;
    await check();
    await check(); // an overlapping run alerts nothing more
    expect(sender.sent).toHaveLength(1);
    expect(await payloadFor(phone, 0)).toMatchObject({
      title: "backup: missed check-in",
      body: "Last check-in 1 h 1 min ago; expected every 1 h.",
      priority: "high",
      channel: "default",
    });
    expect(sender.sent[0]!.high).toBe(true);

    now += 7200;
    await ping(hb.ping_url);
    await ping(hb.ping_url);
    expect(sender.sent).toHaveLength(2);
    expect(await payloadFor(phone, 1)).toMatchObject({
      title: "backup: checked in again",
      body: "Down for 2 h.",
      priority: "default",
    });
  });

  it("alerts a heartbeat that never checked in", async () => {
    const phone = await pair("phone", "fcm-phone");
    await create({ name: "renew", interval: 86400 });
    now += 86400 + 300;
    await check();
    expect(await payloadFor(phone, 0)).toMatchObject({
      title: "renew: missed check-in",
      body: "No check-in since it was created 1 d ago; expected every 1 d.",
    });
  });

  it("reports failures with the posted log, cleaned", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    await ping(hb.ping_url, "/fail", "\u001b[31mFAIL\u001b[0m step 3\r\nexit\u0000 1\n");
    await ping(hb.ping_url, "/fail");
    expect(await payloadFor(phone, 0)).toMatchObject({ title: "build: failed", body: "FAIL step 3\nexit\uFFFD 1", priority: "high" });
    expect(await payloadFor(phone, 1)).toMatchObject({ body: "The job reported a failure." });
    let list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]!.state).toBe("failed");

    now += 600;
    await ping(hb.ping_url);
    expect(await payloadFor(phone, 2)).toMatchObject({ title: "build: checked in again", body: "Failed for 10 min." });
    list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]!.state).toBe("up");
  });

  it("still alerts a failed heartbeat whose job stops running", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    await ping(hb.ping_url, "/fail");
    now += 3600 + 300;
    await check();
    expect(await payloadFor(phone, 1)).toMatchObject({ title: "build: missed check-in" });
  });

  it("retries a missed-check-in alert that reached no device", async () => {
    const phone = await pair("phone", "fcm-phone");
    await create({ name: "backup", interval: 3600 });
    now += 3600 + 300;
    const failing: Sender = { send: () => Promise.reject(new Error("FCM down")) };
    await heartbeatAlerts({ sender: () => failing, stream: (_, id) => streams.for(id), replies, receipts, now: () => now }).check(env);
    let list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]!.state).toBe("up");
    // Retried behind heartbeats that went overdue earlier.
    expect(Date.parse(list[0]!.due_at) / 1000).toBe(now + 60);

    now += 60;
    await check();
    expect(await payloadFor(phone, 0)).toMatchObject({ title: "backup: missed check-in" });
    list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]!.state).toBe("down");
  });

  it("retries a recovery notice that reached no device", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "backup", interval: 3600 });
    now += 3600 + 300;
    await check();
    const failing: Sender = { send: () => Promise.reject(new Error("FCM down")) };
    const downApp = createApp({ sender: () => failing, stream: (_, id) => streams.for(id), replies, receipts, now: () => now });
    now += 600;
    const res = await downApp.request(new URL(hb.ping_url).pathname, {}, env);
    expect(res.status).toBe(200);
    expect(sender.sent).toHaveLength(1); // only the missed check-in so far

    now += 60;
    await check();
    expect(await payloadFor(phone, 1)).toMatchObject({ title: "backup: checked in again", body: "Down for 10 min." });
    await check();
    expect(sender.sent).toHaveLength(2); // delivered once, then cleared
  });

  it("never delivers a notice a newer one replaced", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "backup", interval: 3600 });
    now += 3600 + 300;
    await check();
    const failing: Sender = { send: () => Promise.reject(new Error("FCM down")) };
    const downApp = createApp({ sender: () => failing, stream: (_, id) => streams.for(id), replies, receipts, now: () => now });
    await downApp.request(new URL(hb.ping_url).pathname, {}, env); // recovery kept pending
    await ping(hb.ping_url, "/fail"); // delivered, replacing it
    now += 600;
    await check();
    await Promise.all([check(), check()]); // overlapping runs
    expect(await Promise.all(sender.sent.map((_, i) => payloadFor(phone, i).then((p) => p.title)))).toEqual([
      "backup: missed check-in",
      "backup: failed",
    ]);
  });

  /** Resolves once [ready] holds, yielding to other requests meanwhile. */
  async function until(ready: () => boolean): Promise<void> {
    while (!ready()) await new Promise((r) => setTimeout(r, 1));
  }

  /** A promise to hold an operation at, and the function that lets it go on. */
  function gate(): [Promise<void>, () => void] {
    let release!: () => void;
    return [new Promise<void>((r) => (release = r)), () => release()];
  }

  const titles = (phone: Registration) => Promise.all(sender.sent.map((_, i) => payloadFor(phone, i).then((p) => p.title)));
  const depsWith = (s: Sender) => ({ sender: () => s, stream: (_: Env, id: string) => streams.for(id), replies, receipts, now: () => now });

  it("drops a recovery notice that a failure overtook", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "backup", interval: 3600 });
    now += 3600 + 300;
    await check();
    // The recovery ping stalls at the send limiter while a failure comes in.
    const owner = await userId();
    const [held, release] = gate();
    let reached = false;
    const stalled = {
      ...env,
      NOTIFY_LIMITER: {
        limit: async ({ key }: { key: string }) => {
          if (key === owner) {
            reached = true;
            await held;
          }
          return { success: true };
        },
      },
    } as Env;
    const recovery = app.request(new URL(hb.ping_url).pathname, {}, stalled);
    await until(() => reached);
    await ping(hb.ping_url, "/fail");
    release();
    expect((await recovery).status).toBe(200);
    now += 600;
    await check();
    expect(await titles(phone)).toEqual(["backup: missed check-in", "backup: failed"]);
    const list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]).toMatchObject({ state: "failed" });
  });

  it("keeps a newer failure's log over a stalled older one", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    // Failure A stalls at the send limiter; failure B is recorded after it
    // and its delivery fails, so its notice waits for the cron trigger.
    const owner = await userId();
    const [held, release] = gate();
    let reached = false;
    const stalled = {
      ...env,
      NOTIFY_LIMITER: {
        limit: async ({ key }: { key: string }) => {
          if (key === owner && !reached) {
            reached = true;
            await held;
          }
          return { success: true };
        },
      },
    } as Env;
    const path = new URL(hb.ping_url).pathname + "/fail";
    const first = app.request(path, { method: "POST", body: "old error" }, stalled);
    await until(() => reached);
    const failing: Sender = { send: () => Promise.reject(new Error("FCM down")) };
    await createApp(depsWith(failing)).request(path, { method: "POST", body: "new error" }, env);
    release();
    expect((await first).status).toBe(200);
    now += 60;
    await check();
    const bodies = await Promise.all(sender.sent.map((_, i) => payloadFor(phone, i).then((p) => p.body)));
    expect(bodies).toEqual(["new error"]);
  });

  it("delivers each of two identical failure notices", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    // The first failure's delivery stalls; the second's fails meanwhile.
    const [held, release] = gate();
    let reached = false;
    const slow: Sender = {
      send: async (m) => {
        reached = true;
        await held;
        await sender.send(m);
      },
    };
    const failing: Sender = { send: () => Promise.reject(new Error("FCM down")) };
    const first = createApp(depsWith(slow)).request(new URL(hb.ping_url).pathname + "/fail", {}, env);
    await until(() => reached);
    await createApp(depsWith(failing)).request(new URL(hb.ping_url).pathname + "/fail", {}, env);
    release();
    expect((await first).status).toBe(200);
    now += 60;
    await check();
    expect(await titles(phone)).toEqual(["build: failed", "build: failed"]);
  });

  it("does not resend a failure notice delivered while a missed alert was undone", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    const failing: Sender = { send: () => Promise.reject(new Error("FCM down")) };
    await createApp(depsWith(failing)).request(new URL(hb.ping_url).pathname + "/fail", {}, env); // kept pending
    now += 3600 + 300;
    // Run A's missed alert stalls, then fails; run B delivers the pending
    // failure notice meanwhile.
    const [held, release] = gate();
    let reached = false;
    const flaky: Sender = {
      send: async (m) => {
        if (reached) return sender.send(m);
        reached = true;
        await held;
        throw new Error("FCM down");
      },
    };
    const runA = heartbeatAlerts(depsWith(flaky)).check(env);
    await until(() => reached);
    await check();
    release();
    await runA;
    now += 60;
    await check();
    await check();
    const got = await titles(phone);
    expect(got.filter((t) => t === "build: failed")).toHaveLength(1);
    expect(got).toContain("build: missed check-in");
  });

  it("ignores HEAD requests to ping URLs", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    for (const suffix of ["", "/fail"]) {
      const res = await app.request(new URL(hb.ping_url).pathname + suffix, { method: "HEAD" }, env);
      expect(res.status).toBe(200);
    }
    expect(sender.sent).toHaveLength(0);
    const list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]).toMatchObject({ state: "up" });
    expect(list[0]).not.toHaveProperty("last_ping_at");
    void phone;
  });

  it("keeps a pending failure notice when the missed alert fails too", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    const failing: Sender = { send: () => Promise.reject(new Error("FCM down")) };
    const deps = { sender: () => failing, stream: (_: Env, id: string) => streams.for(id), replies, receipts, now: () => now };
    await createApp(deps).request(new URL(hb.ping_url).pathname + "/fail", { method: "POST", body: "the log" }, env);
    now += 3600 + 300;
    await heartbeatAlerts(deps).check(env); // missed alert fails and is undone
    now += 60;
    await check();
    const titles = await Promise.all(sender.sent.map((_, i) => payloadFor(phone, i).then((p) => [p.title, p.body])));
    expect(titles).toContainEqual(["build: failed", "the log"]);
    expect(titles).toContainEqual(["build: missed check-in", expect.any(String)]);
  });

  it("accepts a failure log larger than an API request", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    await ping(hb.ping_url, "/fail", "log line\n".repeat(10_000) + "the error\n");
    expect((await payloadFor(phone, 0)).body).toMatch(/^….*the error$/s);
  });

  it("keeps the tail of a long failure log", async () => {
    const phone = await pair("phone", "fcm-phone");
    const hb = await create({ name: "build", interval: 3600 });
    await ping(hb.ping_url, "/fail", "x".repeat(5000) + "\nthe error");
    const body = (await payloadFor(phone, 0)).body!;
    expect(body.startsWith("…")).toBe(true);
    expect(body.endsWith("the error")).toBe(true);
    expect(new TextEncoder().encode(body).length).toBeLessThanOrEqual(2048);
  });

  it("alerts the channel's subscribers, and default once the channel is deleted", async () => {
    const phone = await pair("phone", "fcm-phone");
    const ops = await channel("ops");
    await call("PUT", "/v1/device/channels", phone.device_token, { channel_ids: [`ch_default_${await userId()}`] }, 200);
    const hb = await create({ name: "backup", interval: 3600, channel: "ops" });
    now += 3600 + 300;
    await check();
    expect(sender.sent).toHaveLength(0); // the phone does not subscribe to ops

    await call("DELETE", `/v1/channels/${ops}`, USER, undefined, 204);
    const list = await call<Listed[]>("GET", "/v1/heartbeats", USER, undefined, 200);
    expect(list[0]!.channel).toBe("default");
    await ping(hb.ping_url);
    expect(await payloadFor(phone, 0)).toMatchObject({ title: "backup: checked in again", channel: "default" });
  });

  it("ignores a disabled user's heartbeats", async () => {
    await pair("phone", "fcm-phone");
    const hb = await create({ name: "backup", interval: 3600 });
    await call("POST", `/v1/users/${await userId()}/disable`, ADMIN, undefined, 204);
    await ping(hb.ping_url, "", undefined, 404);
    now += 3600 + 300;
    await check();
    expect(sender.sent).toHaveLength(0);
  });
});

describe("inbound webhooks", () => {
  interface Created {
    id: string;
    format: string;
    channel: string;
    url: string;
  }
  interface Listed {
    name: string;
    format: string;
    channel: string;
    last_used_at?: string;
  }

  const create = (body: object, status = 201) => call<Created>("POST", "/v1/hooks", USER, body, status);

  async function post(url: string, body: unknown, wantStatus = 200, bindings: Env = env): Promise<void> {
    const res = await app.request(new URL(url).pathname, { method: "POST", body: typeof body === "string" ? body : JSON.stringify(body) }, bindings);
    expect(res.status, await res.text()).toBe(wantStatus);
  }

  const firing = {
    version: "4",
    status: "firing",
    groupLabels: { alertname: "DiskFull" },
    commonLabels: { alertname: "DiskFull", severity: "critical" },
    alerts: [
      { status: "firing", labels: { alertname: "DiskFull", severity: "critical", instance: "nas-01" }, annotations: { summary: "Disk almost full" } },
    ],
  };

  it("creates, lists and deletes hooks", async () => {
    const hook = await create({ name: "am", format: "alertmanager" });
    expect(hook).toMatchObject({ format: "alertmanager", channel: "default" });
    expect(hook.url).toMatch(/^http:\/\/localhost\/v1\/hooks\/alertmanager\/nw_[a-z2-7]+$/);
    let list = await call<Listed[]>("GET", "/v1/hooks", USER, undefined, 200);
    expect(list).toEqual([expect.objectContaining({ name: "am", format: "alertmanager", channel: "default" })]);
    expect(list[0]).not.toHaveProperty("last_used_at");

    await post(hook.url, firing);
    list = await call<Listed[]>("GET", "/v1/hooks", USER, undefined, 200);
    expect(list[0]!.last_used_at).toBeDefined();

    // Others cannot see or delete it; API keys cannot manage hooks.
    const bob = await signup("bob");
    expect(await call("GET", "/v1/hooks", bob, undefined, 200)).toEqual([]);
    await call("DELETE", `/v1/hooks/${hook.id}`, bob, undefined, 404);
    await call("GET", "/v1/hooks", await apiKey(), undefined, 401);
    await call("DELETE", `/v1/hooks/${hook.id}`, USER, undefined, 204);
    await post(hook.url, firing, 404);
  });

  it("validates and caps hooks", async () => {
    await create({ name: "x", format: "nagios" }, 400);
    await create({ name: "x", format: "toString" }, 400);
    await create({ name: "x" }, 400);
    await create({ name: "x", format: "generic", channel: "nope" }, 404);
    await create({ name: "x\u001b", format: "generic" }, 400);
    await create({ name: "x", format: "generic", extra: 1 }, 400);
    await create({ name: "x", format: "generic" });
    await create({ name: "x", format: "grafana" }, 409);
    for (let i = 1; i < MAX_HOOKS_PER_USER; i++) await create({ name: `hook${i}`, format: "generic" });
    await create({ name: "one-too-many", format: "generic" }, 403);
    await post("http://localhost/v1/hooks/generic/nw_NOTASECRET", { title: "x" }, 404);
    await post("http://localhost/v1/hooks/generic/nh_NOTASECRET", { title: "x" }, 404);
  });

  it("publishes an Alertmanager alert to the hook's channel", async () => {
    const phone = await pair("phone", "fcm-phone");
    const { id } = await call<{ id: string }>("POST", "/v1/channels", USER, { name: "alerts" }, 201);
    const hook = await create({ name: "am", format: "alertmanager", channel: "alerts" });
    await call("PUT", `/v1/devices/${phone.device_id}/channels`, USER, { channel_ids: [`ch_default_${await userId()}`, id] }, 200);

    await post(hook.url, firing);
    expect(await payloadFor(phone, 0)).toMatchObject({
      title: "[FIRING:1] DiskFull",
      body: "Disk almost full (nas-01)",
      priority: "high",
      channel: "alerts",
    });
    expect(sender.sent[0]!.high).toBe(true);

    await post(hook.url, { ...firing, status: "resolved", alerts: [{ ...firing.alerts[0], status: "resolved" }] });
    expect(await payloadFor(phone, 1)).toMatchObject({ title: "[RESOLVED] DiskFull", priority: "default" });
  });

  it("checks the format in the path and the payload", async () => {
    await pair("phone", "fcm-phone");
    const hook = await create({ name: "g", format: "generic" });
    await post(hook.url.replace("/generic/", "/grafana/"), { title: "x" }, 404);
    await post(hook.url, "not json", 400);
    await post(hook.url, { body: "no title" }, 400);
    await post(hook.url, { title: "x", priority: "urgent" }, 400);
    await post(hook.url, { title: "\u001b[2J" }, 400);
    expect(sender.sent).toHaveLength(0);
    await post(hook.url, { title: "Deploy done", body: "v1.2.3", unknown: "ignored" });
    expect(sender.sent).toHaveLength(1);
  });

  it("asks the tool to retry when no device took the alert", async () => {
    const reg = await pair("phone", "fcm-phone");
    const hook = await create({ name: "am", format: "alertmanager" });
    const failing = { send: () => Promise.reject(new Error("FCM down")) };
    app = createApp({ sender: () => failing, stream: (_, id) => streams.for(id), replies, receipts, now: () => now });
    await post(hook.url, firing, 503);
    // Nobody listening is not worth a retry.
    await call("DELETE", `/v1/devices/${reg.device_id}`, USER, undefined, 204);
    await post(hook.url, firing, 200);
  });

  it("rate-limits per user and ignores disabled users", async () => {
    await pair("phone", "fcm-phone");
    const hook = await create({ name: "am", format: "alertmanager" });
    const keys: string[] = [];
    const limited = {
      ...env,
      NOTIFY_LIMITER: {
        limit: ({ key }: { key: string }) => {
          keys.push(key);
          return Promise.resolve({ success: false });
        },
      },
    } as Env;
    await post(hook.url, firing, 429, limited);
    expect(keys).toEqual([await userId()]);
    await call("POST", `/v1/users/${await userId()}/disable`, ADMIN, undefined, 204);
    await post(hook.url, firing, 404);
    expect(sender.sent).toHaveLength(0);
  });

  it("moves to the default channel when its channel is deleted", async () => {
    const phone = await pair("phone", "fcm-phone");
    const { id } = await call<{ id: string }>("POST", "/v1/channels", USER, { name: "alerts" }, 201);
    const hook = await create({ name: "am", format: "alertmanager", channel: "alerts" });
    await call("DELETE", `/v1/channels/${id}`, USER, undefined, 204);
    expect(await call<Listed[]>("GET", "/v1/hooks", USER, undefined, 200)).toEqual([expect.objectContaining({ channel: "default" })]);
    await post(hook.url, firing);
    expect(await payloadFor(phone, 0)).toMatchObject({ title: "[FIRING:1] DiskFull", channel: "default" });
  });

  it("accepts payloads up to 1 MiB", async () => {
    await pair("phone", "fcm-phone");
    const hook = await create({ name: "am", format: "alertmanager" });
    const many = { ...firing, alerts: Array.from({ length: 2000 }, (_, i) => ({ ...firing.alerts[0], labels: { ...firing.alerts[0]!.labels, instance: `host-${i}` } })) };
    await post(hook.url, many);
    await post(hook.url, { ...firing, padding: "x".repeat(1024 * 1024) }, 413);
  });
});

describe("questions", () => {
  const KEY = toBase64(new Uint8Array(32).fill(7));
  type Answered = { status: string; by?: string; replied_at?: number; n?: string; c?: string };

  const ask = (body: object, wantStatus = 200, token = USER) =>
    call<NotifyResponse>(
      "POST",
      "/v1/notifications",
      token,
      { title: "Deploy prod?", question: { options: ["yes", "no"], key: KEY, expires_in: 600 }, ...body },
      wantStatus,
    );

  async function answer(reg: Registration, id: string, text: string, wantStatus = 201) {
    const env = await seal(fromBase64(KEY), `reply:${id}`, new TextEncoder().encode(JSON.stringify({ answer: text })));
    return call("POST", `/v1/replies/${id}`, reg.device_token, { n: toBase64(env.nonce), c: toBase64(env.ciphertext) }, wantStatus);
  }

  const poll = (id: string, wait = 0, token = USER, wantStatus = 200) =>
    call<Answered>("GET", `/v1/replies/${id}?wait=${wait}`, token, undefined, wantStatus);

  it("puts the options and key in the payload and returns the first answer, sealed", async () => {
    const phone = await pair("phone", "fcm-phone");
    const laptop = await pair("laptop", "fcm-laptop");
    const { id } = await ask({});
    expect((await payloadFor(phone, 0)).reply).toEqual({ options: ["yes", "no"], key: KEY, expires_at: now + 600 });
    // Whitespace, which atob skips, is not carried into the payload.
    await ask({ question: { options: ["ok"], key: KEY.replace("=", "\n".repeat(4000) + "=") } });
    expect((await payloadFor(phone, 2)).reply?.key).toBe(KEY);
    expect(await poll(id)).toEqual({ status: "pending" });

    await answer(phone, id, "no");
    await answer(laptop, id, "yes", 409);
    const got = await poll(id, 0, await apiKey());
    expect(got).toMatchObject({ status: "answered", by: "phone", replied_at: now });
    const plaintext = await open(fromBase64(KEY), `reply:${id}`, { nonce: fromBase64(got.n!), ciphertext: fromBase64(got.c!) });
    expect(JSON.parse(new TextDecoder().decode(plaintext))).toEqual({ answer: "no" });
  });

  it("wakes a waiting poll when the answer arrives", async () => {
    const phone = await pair("phone", "fcm-phone");
    const { id } = await ask({});
    const waiting = poll(id, 30);
    await new Promise((r) => setTimeout(r, 50));
    await answer(phone, id, "yes");
    expect(await waiting).toMatchObject({ status: "answered", by: "phone" });
  });

  it("refuses answers after expiry and reports it", async () => {
    const phone = await pair("phone", "fcm-phone");
    const { id } = await ask({ question: { options: ["ok"], key: KEY, expires_in: 10 } });
    now += 10;
    await answer(phone, id, "ok", 410);
    expect(await poll(id)).toEqual({ status: "expired" });
  });

  it("lets other members' devices answer only when anyone is set", async () => {
    const bob = await signup("bob");
    const homelab = await channel("homelab");
    const bobPhone = await pair("phone", "fcm-bob", bob);
    const { code } = await call<{ code: string }>("POST", `/v1/channels/${homelab}/invites`, USER, undefined, 201);
    await call("POST", "/v1/device/memberships", bobPhone.device_token, { invite: code }, 201);
    const own = await pair("laptop", "fcm-laptop");
    await call("PUT", "/v1/device/channels", own.device_token, { channel_ids: [`ch_default_${await userId()}`, homelab] }, 200);

    const mine = await ask({ channels: ["homelab"] });
    expect((await payloadFor(bobPhone, 0)).reply).toBeUndefined();
    expect((await payloadFor(own, 1)).reply).toBeDefined();
    await answer(bobPhone, mine.id, "yes", 404);

    const anyone = await ask({ channels: ["homelab"], question: { options: ["yes", "no"], key: KEY, anyone: true } });
    expect((await payloadFor(bobPhone, 2)).reply).toMatchObject({ options: ["yes", "no"], expires_at: now + 600 });
    await answer(bobPhone, anyone.id, "yes");
    expect(await poll(anyone.id)).toMatchObject({ status: "answered", by: "bob" });
  });

  it("refuses a question that makes the message too large for FCM, before opening it", async () => {
    await pair("phone", "fcm-phone");
    const options = ["a", "b", "c"].map((x) => x.repeat(32));
    const big = { title: "t".repeat(256), body: '"'.repeat(500) + "x".repeat(1548) };
    const { error } = await call<{ error: string }>("POST", "/v1/notifications", USER, { ...big, question: { options, key: KEY } }, 400);
    expect(error).toMatch(/too long once encoded/);
    expect(sender.sent).toHaveLength(0);
  });

  it("refuses a question none of the asker's devices can answer", async () => {
    const bob = await signup("bob");
    const homelab = await channel("homelab");
    const bobPhone = await pair("phone", "fcm-bob", bob);
    const { code } = await call<{ code: string }>("POST", `/v1/channels/${homelab}/invites`, USER, undefined, 201);
    await call("POST", "/v1/device/memberships", bobPhone.device_token, { invite: code }, 201);
    await ask({ channels: ["homelab"] }, 404);
    expect(sender.sent).toHaveLength(0);
  });

  it("hides questions from other users and other devices", async () => {
    const phone = await pair("phone", "fcm-phone");
    const bob = await signup("bob");
    const bobPhone = await pair("phone", "fcm-bob", bob);
    const { id } = await ask({});
    await poll(id, 0, bob, 404);
    await answer(bobPhone, id, "yes", 404);
    await poll("msg_aaaaaaaaaaaaaaaa", 0, USER, 404);
    await call("POST", `/v1/replies/${id}`, USER, { n: "", c: "" }, 401);
    await answer(phone, id, "yes");
  });

  it.each([
    [{ options: [] }],
    [{ options: ["a", "b", "c", "d"] }],
    [{ options: ["a", "a"] }],
    [{ options: [" "] }],
    [{ options: ["two\nlines"] }],
    [{ options: ["x".repeat(33)] }],
    [{ options: ["yes"], key: "short" }],
    [{ options: ["yes"], expires_in: 9 }],
    [{ options: ["yes"], expires_in: 86_401 }],
    [{ options: ["yes"], expires_in: 1.5 }],
    [{ options: ["yes"], anyone: "yes" }],
    [{ options: ["yes"], extra: 1 }],
  ])("rejects the question %j", async (q) => {
    await pair("phone", "fcm-phone");
    await ask({ question: { key: KEY, ...q } }, 400);
    expect(sender.sent).toHaveLength(0);
  });

  it("checks the reply's shape and the wait", async () => {
    const phone = await pair("phone", "fcm-phone");
    const { id } = await ask({});
    await call("POST", `/v1/replies/${id}`, phone.device_token, { n: "AAAA", c: "AAAA" }, 400);
    await call("POST", `/v1/replies/${id}`, phone.device_token, { n: toBase64(new Uint8Array(12)), c: "" }, 400);
    await call("POST", `/v1/replies/${id}`, phone.device_token, { n: toBase64(new Uint8Array(12)), c: "AA==" }, 400);
    await call("POST", `/v1/replies/${id}`, phone.device_token, { n: toBase64(new Uint8Array(12)), c: toBase64(new Uint8Array(16)) }, 400);
    expect(await poll(id)).toEqual({ status: "pending" });
    await poll(id, 51, USER, 400);
    await call("GET", `/v1/replies/${id}?wait=-1`, USER, undefined, 400);
  });
});

describe("receipts", () => {
  type Receipts = {
    id: string;
    status: string;
    devices: { device_id: string; name: string; shown_at?: number; opened_at?: number }[];
    members: { shown: number; opened: number };
  };

  const send = (body: object = {}, token = USER) =>
    call<NotifyResponse>("POST", "/v1/notifications", token, { title: "Disk full", receipts: true, ...body }, 200);
  const mark = (reg: Registration, id: string, state: string, wantStatus = 204) =>
    call("POST", "/v1/device/receipts", reg.device_token, { message_id: id, state }, wantStatus);
  const read = (id: string, wait = 0, token = USER, wantStatus = 200) =>
    call<Receipts>("GET", `/v1/notifications/${id}?wait=${wait}`, token, undefined, wantStatus);

  it("tracks only messages sent with receipts", async () => {
    const phone = await pair("phone", "fcm-phone");
    const { id } = await send();
    expect((await payloadFor(phone, 0)).receipts).toBe(true);
    const plain = await call<NotifyResponse>("POST", "/v1/notifications", USER, { title: "hi" }, 200);
    expect((await payloadFor(phone, 1)).receipts).toBeUndefined();
    await mark(phone, plain.id, "shown", 404);
    expect(await read(plain.id, 0, USER, 404)).toEqual({ error: NO_RECEIPTS });
    expect(await read("msg_typo", 0, USER, 404)).toEqual({ error: NO_RECEIPTS });
    expect(await read(id)).toEqual({
      id,
      status: "pending",
      devices: [{ device_id: phone.device_id, name: "phone" }],
      members: { shown: 0, opened: 0 },
    });
  });

  it("keeps the first time of each state, and opening implies showing", async () => {
    const phone = await pair("phone", "fcm-phone");
    const laptop = await pair("laptop", "fcm-laptop");
    const { id } = await send();
    await mark(phone, id, "shown");
    now += 5;
    await mark(phone, id, "opened");
    now += 5;
    await mark(phone, id, "shown");
    await mark(phone, id, "opened");
    await mark(laptop, id, "opened");
    const got = await read(id, 0, await apiKey());
    expect(got.status).toBe("opened");
    expect(got.devices).toEqual(
      expect.arrayContaining([
        { device_id: phone.device_id, name: "phone", shown_at: now - 10, opened_at: now - 5 },
        { device_id: laptop.device_id, name: "laptop", shown_at: now, opened_at: now },
      ]),
    );
  });

  it("wakes a waiting poll when a device opens the message, not when it shows it", async () => {
    const phone = await pair("phone", "fcm-phone");
    const { id } = await send();
    let settled = false;
    const waiting = read(id, 30).then((r) => ((settled = true), r));
    await new Promise((r) => setTimeout(r, 50));
    await mark(phone, id, "shown");
    await new Promise((r) => setTimeout(r, 50));
    expect(settled).toBe(false);
    await mark(phone, id, "opened");
    expect(await waiting).toMatchObject({ status: "opened" });
  });

  it("counts other members' devices without naming them", async () => {
    const bob = await signup("bob");
    const homelab = await channel("homelab");
    const bobPhone = await pair("phone", "fcm-bob", bob);
    const { code } = await call<{ code: string }>("POST", `/v1/channels/${homelab}/invites`, USER, undefined, 201);
    await call("POST", "/v1/device/memberships", bobPhone.device_token, { invite: code }, 201);
    const { id } = await send({ channels: ["homelab"] });
    expect((await payloadFor(bobPhone, 0)).receipts).toBe(true);
    await mark(bobPhone, id, "opened");
    expect(await read(id)).toEqual({ id, status: "opened", devices: [], members: { shown: 1, opened: 1 } });
    await read(id, 0, bob, 404);
  });

  it("counts a late answer, after the question is gone, as opening it", async () => {
    const KEY = toBase64(new Uint8Array(32).fill(7));
    const phone = await pair("phone", "fcm-phone");
    const { id } = await send({ question: { options: ["ok"], key: KEY, expires_in: 10 } });
    expect(await runDurableObjectAlarm(replies(env, id))).toBe(true);
    const sealed = await seal(fromBase64(KEY), `reply:${id}`, new TextEncoder().encode(JSON.stringify({ answer: "ok" })));
    await call("POST", `/v1/replies/${id}`, phone.device_token, { n: toBase64(sealed.nonce), c: toBase64(sealed.ciphertext) }, 404);
    expect((await read(id)).status).toBe("opened");
  });

  it("refuses devices the message did not reach, and bad requests", async () => {
    const phone = await pair("phone", "fcm-phone");
    const bob = await signup("bob");
    const bobPhone = await pair("phone", "fcm-bob", bob);
    const { id } = await send();
    await mark(bobPhone, id, "opened", 404);
    await mark(phone, id, "read", 400);
    await mark(phone, "msg_bad", "shown", 404);
    await call("POST", "/v1/device/receipts", phone.device_token, { message_id: id, state: "shown", extra: 1 }, 400);
    await call("POST", "/v1/device/receipts", USER, { message_id: id, state: "shown" }, 401);
    await call("POST", "/v1/notifications", USER, { title: "x", receipts: "yes" }, 400);
    await read(id, 51, USER, 400);
    expect((await read(id)).status).toBe("pending");
  });

  it("counts answering a question as opening it, even when another answered first", async () => {
    const KEY = toBase64(new Uint8Array(32).fill(7));
    const phone = await pair("phone", "fcm-phone");
    const laptop = await pair("laptop", "fcm-laptop");
    const { id } = await send({ question: { options: ["ok"], key: KEY } });
    const sealed = await seal(fromBase64(KEY), `reply:${id}`, new TextEncoder().encode(JSON.stringify({ answer: "ok" })));
    const body = { n: toBase64(sealed.nonce), c: toBase64(sealed.ciphertext) };
    await call("POST", `/v1/replies/${id}`, phone.device_token, body, 201);
    now += 5;
    await call("POST", `/v1/replies/${id}`, laptop.device_token, body, 409);
    expect((await read(id)).devices).toEqual(
      expect.arrayContaining([
        { device_id: phone.device_id, name: "phone", shown_at: now - 5, opened_at: now - 5 },
        { device_id: laptop.device_id, name: "laptop", shown_at: now, opened_at: now },
      ]),
    );
  });
});

async function userId(): Promise<string> {
  return (await call<{ id: string }>("GET", "/v1/user", USER, undefined, 200)).id;
}
