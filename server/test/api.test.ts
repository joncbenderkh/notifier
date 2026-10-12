// SPDX-License-Identifier: GPL-3.0-or-later
import { env } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import {
  createApp,
  INVITE_TTL_SECONDS,
  MAX_API_KEYS_PER_USER,
  MAX_DEVICES_PER_USER,
  MAX_PENDING_INVITES_PER_USER,
  MAX_PENDING_PAIRINGS_PER_USER,
  PAIRING_TTL_SECONDS,
} from "../src/app";
import type { Delivery, Payload } from "../src/app";
import { fromBase64 } from "../src/encoding";
import { UnregisteredError } from "../src/push";
import type { PushMessage, Sender } from "../src/push";
import { open } from "../src/seal";

const ADMIN = env.ADMIN_TOKEN;

class FakeSender implements Sender {
  sent: PushMessage[] = [];
  unregistered = new Set<string>();
  send(message: PushMessage): Promise<void> {
    if (this.unregistered.has(message.token)) return Promise.reject(new UnregisteredError("gone"));
    this.sent.push(message);
    return Promise.resolve();
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
let now: number;
let app: ReturnType<typeof createApp>;

beforeEach(async () => {
  await env.DB.batch([
    env.DB.prepare("DELETE FROM devices"),
    env.DB.prepare("DELETE FROM api_keys"),
    env.DB.prepare("DELETE FROM pairings"),
    env.DB.prepare("DELETE FROM users"),
    env.DB.prepare("DELETE FROM invites"),
  ]);
  sender = new FakeSender();
  now = 1_790_000_000;
  app = createApp({ sender: () => sender, now: () => now });
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
    expect(devices).toEqual([{ id: reg.device_id, name: "phone", reachable: true }]);
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

  it("caps API keys and pending invites per user", async () => {
    for (let i = 0; i < MAX_API_KEYS_PER_USER; i++) await apiKey();
    await call("POST", "/v1/keys", USER, { name: "one more" }, 403);

    for (let i = 0; i < MAX_PENDING_INVITES_PER_USER; i++) await invite(USER);
    await call("POST", "/v1/invites", USER, undefined, 403);
    await invite(ADMIN);
  });

  it("rate-limits notifications per user", async () => {
    await pair("phone", "fcm-phone");
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
    expect(keys).toEqual([(await call<{ id: string }>("GET", "/v1/user", USER, undefined, 200)).id]);
    expect(sender.sent).toEqual([]);
  });
});
