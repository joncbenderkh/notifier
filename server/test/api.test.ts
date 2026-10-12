// SPDX-License-Identifier: GPL-3.0-or-later
import { env } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import { createApp, PAIRING_TTL_SECONDS } from "../src/app";
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

let sender: FakeSender;
let now: number;
let app: ReturnType<typeof createApp>;

beforeEach(async () => {
  await env.DB.batch([
    env.DB.prepare("DELETE FROM devices"),
    env.DB.prepare("DELETE FROM api_keys"),
    env.DB.prepare("DELETE FROM pairings"),
  ]);
  sender = new FakeSender();
  now = 1_790_000_000;
  app = createApp({ sender: () => sender, now: () => now });
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

async function pair(name: string, fcmToken: string): Promise<Registration> {
  const { code } = await call<{ code: string }>("POST", "/v1/pairings", ADMIN, undefined, 201);
  return call<Registration>("POST", "/v1/devices", "", { code, name, fcm_token: fcmToken }, 201);
}

async function apiKey(): Promise<string> {
  const { key } = await call<{ key: string }>("POST", "/v1/keys", ADMIN, { name: "ci" }, 201);
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
    const devices = await call<{ reachable: boolean }[]>("GET", "/v1/devices", ADMIN, undefined, 200);
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
    await call("POST", "/v1/notifications", "", note, 401);
    await call("POST", "/v1/notifications", ADMIN, note, 401);
    await call("POST", "/v1/notifications", reg.device_token, note, 401);
    await call("POST", "/v1/notifications", "nk_unknown", note, 401);
    await call("PUT", "/v1/device", key, { fcm_token: "x" }, 401);
    await call("GET", "/v1/devices", reg.device_token, undefined, 401);
    await call("DELETE", `/v1/devices/${reg.device_id}`, key, undefined, 401);
  });

  it("lets API keys list devices without activity timestamps", async () => {
    const reg = await pair("phone", "fcm-phone");
    const devices = await call<object[]>("GET", "/v1/devices", await apiKey(), undefined, 200);
    expect(devices).toEqual([{ id: reg.device_id, name: "phone", reachable: true }]);
    const full = await call<object[]>("GET", "/v1/devices", ADMIN, undefined, 200);
    expect(full[0]).toHaveProperty("last_seen_at");
  });

  it.each([
    ["unset", undefined],
    ["empty", ""],
    ["too short", "short"],
  ])("fails closed when ADMIN_TOKEN is %s", async (_, token) => {
    const bindings = { ...env, ADMIN_TOKEN: token } as Env;
    await call("POST", "/v1/pairings", "", undefined, 401, bindings);
    await call("POST", "/v1/pairings", token ?? "", undefined, 401, bindings);
    await call("POST", "/v1/keys", "", { name: "x" }, 401, bindings);
  });

  it("tolerates extra whitespace after Bearer", async () => {
    const res = await app.request("/v1/keys", { headers: { authorization: `bearer   ${ADMIN}` } }, env);
    expect(res.status).toBe(200);
  });

  it("rejects names that are not valid UTF-8", async () => {
    const { error } = await call<{ error: string }>("POST", "/v1/keys", ADMIN, { name: "a\ud800" }, 400);
    expect(error).toBe("name must be valid UTF-8");
  });

  it("rejects a revoked API key", async () => {
    await pair("phone", "fcm-phone");
    const { id, key } = await call<{ id: string; key: string }>("POST", "/v1/keys", ADMIN, { name: "ci" }, 201);
    await call("DELETE", `/v1/keys/${id}`, ADMIN, undefined, 204);
    await call("POST", "/v1/notifications", key, { title: "hi" }, 401);
  });
});

describe("pairing", () => {
  it("redeems a code once", async () => {
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", ADMIN, undefined, 201);
    const req = { code: code.toLowerCase().replace("-", ""), name: "phone", fcm_token: "fcm" };
    await call("POST", "/v1/devices", "", req, 201);
    await call("POST", "/v1/devices", "", req, 403);
  });

  it("rejects an expired code", async () => {
    const { code } = await call<{ code: string }>("POST", "/v1/pairings", ADMIN, undefined, 201);
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
    const plain = await call<{ code: string; uri: string; qr_svg?: string }>("POST", "/v1/pairings", ADMIN, undefined, 201);
    expect(plain.uri).toBe(`notifier://pair?server=http%3A%2F%2Flocalhost&code=${plain.code}`);
    expect(plain.qr_svg).toBeUndefined();

    const withQr = await call<{ qr_svg: string }>("POST", "/v1/pairings?qr=svg", ADMIN, undefined, 201);
    expect(withQr.qr_svg).toMatch(/^<svg [^>]*viewBox/);
  });

  it("serves the admin page with a strict policy and no secrets", async () => {
    const page = await app.request("/admin/pair", {}, env);
    expect(page.status).toBe(200);
    const csp = page.headers.get("content-security-policy") ?? "";
    expect(csp).toContain("default-src 'none'");
    expect(csp).toContain("script-src 'self'");
    expect(csp).not.toContain("unsafe-inline");
    expect(page.headers.get("cache-control")).toBe("no-store");
    expect(page.headers.get("referrer-policy")).toBe("no-referrer");
    const html = await page.text();
    expect(html).not.toContain(ADMIN);
    expect(html).not.toMatch(/<script>|<style>|\son\w+=/);

    for (const [path, type] of [
      ["/admin/pair.js", "text/javascript"],
      ["/admin/pair.css", "text/css"],
    ]) {
      const res = await app.request(path!, {}, env);
      expect(res.status).toBe(200);
      expect(res.headers.get("content-type")).toContain(type);
    }
  });
});
