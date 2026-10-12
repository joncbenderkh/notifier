// SPDX-License-Identifier: GPL-3.0-or-later
import { applyD1Migrations, env } from "cloudflare:test";
import { beforeEach, describe, expect, it } from "vitest";
import { createApp } from "../src/app";
import { LogSender } from "../src/push";
import { sha256Hex } from "../src/token";

const [initial, users, channels] = env.TEST_MIGRATIONS;

/** Rebuilds the database at the pre-user schema. */
beforeEach(async () => {
  const tables = [
    "devices",
    "api_keys",
    "pairings",
    "users",
    "invites",
    "channels",
    "channel_members",
    "subscriptions",
    "channel_invites",
    "api_key_channels",
    "d1_migrations",
  ];
  await env.DB.batch(tables.map((t) => env.DB.prepare(`DROP TABLE IF EXISTS ${t}`)));
  await applyD1Migrations(env.DB, [initial!]);
});

describe("0002_users", () => {
  it("gives existing devices and keys to an owner reachable only by reissue", async () => {
    await env.DB.batch([
      env.DB.prepare(
        `INSERT INTO devices (id, name, token_hash, seal_key, fcm_token, created_at, last_seen_at)
         VALUES ('dev_old', 'phone', 'h1', 'AAAA', 'fcm-old', 1, 1)`,
      ),
      env.DB.prepare(
        `INSERT INTO api_keys (id, name, key_hash, created_at) VALUES ('key_old', 'ci', ?, 1)`,
      ).bind(await sha256Hex("nk_old")),
    ]);
    await applyD1Migrations(env.DB, [initial!, users!]);
    // What the previous Worker version writes before the new one is live.
    await env.DB.prepare("INSERT INTO pairings (code_hash, expires_at) VALUES ('p', 9999999999)").run();

    const owner = await env.DB.prepare("SELECT id, name, disabled_at FROM users").all();
    expect(owner.results).toEqual([{ id: "usr_owner", name: "owner", disabled_at: null }]);
    const owned = await env.DB.prepare(
      "SELECT (SELECT user_id FROM devices) AS d, (SELECT user_id FROM api_keys) AS k, (SELECT user_id FROM pairings) AS p",
    ).first();
    expect(owned).toEqual({ d: "usr_owner", k: "usr_owner", p: "usr_owner" });

    // Today's code runs on the fully migrated schema.
    await applyD1Migrations(env.DB, env.TEST_MIGRATIONS);

    const app = createApp({ sender: () => new LogSender(), now: () => 1_790_000_000 });
    const req = (path: string, token: string, method = "GET") =>
      app.request(path, { method, headers: { authorization: `Bearer ${token}` } }, env);
    expect((await req("/v1/devices", "nk_old")).status).toBe(200);

    const res = await req("/v1/users/usr_owner/token", env.ADMIN_TOKEN, "POST");
    const { token } = await res.json<{ token: string }>();
    const devices = await (await req("/v1/devices", token)).json<{ id: string }[]>();
    expect(devices.map((d) => d.id)).toEqual(["dev_old"]);
  });

  it("creates the owner even for an empty database", async () => {
    await applyD1Migrations(env.DB, [initial!, users!]);
    expect(await env.DB.prepare("SELECT id FROM users").first("id")).toBe("usr_owner");
  });
});

describe("0003_channels", () => {
  it("gives every user a default channel that their devices subscribe to", async () => {
    await applyD1Migrations(env.DB, [initial!, users!]);
    await env.DB.batch([
      env.DB.prepare(
        "INSERT INTO users (id, name, token_hash, created_at, last_seen_at) VALUES ('usr_b', 'bob', 'hb', 1, 1)",
      ),
      env.DB.prepare(
        `INSERT INTO devices (id, user_id, name, token_hash, seal_key, created_at, last_seen_at)
         VALUES ('dev_a', 'usr_owner', 'a', 'h1', 'AAAA', 1, 1), ('dev_b', 'usr_b', 'b', 'h2', 'AAAA', 1, 1)`,
      ),
    ]);
    await applyD1Migrations(env.DB, [initial!, users!, channels!]);
    // Rows the previous Worker writes before the new code is published.
    await env.DB.batch([
      env.DB.prepare(
        "INSERT INTO users (id, name, token_hash, created_at, last_seen_at) VALUES ('usr_c', 'carol', 'hc', 2, 2)",
      ),
      env.DB.prepare(
        `INSERT INTO devices (id, user_id, name, token_hash, seal_key, created_at, last_seen_at)
         VALUES ('dev_c', 'usr_c', 'c', 'h3', 'AAAA', 2, 2)`,
      ),
    ]);

    const subs = await env.DB.prepare(
      `SELECT s.device_id, c.owner_id, c.name FROM subscriptions s
       JOIN channels c ON c.id = s.channel_id
       JOIN channel_members m ON m.channel_id = c.id AND m.user_id = c.owner_id
       ORDER BY s.device_id`,
    ).all();
    expect(subs.results).toEqual([
      { device_id: "dev_a", owner_id: "usr_owner", name: "default" },
      { device_id: "dev_b", owner_id: "usr_b", name: "default" },
      { device_id: "dev_c", owner_id: "usr_c", name: "default" },
    ]);
  });
});
