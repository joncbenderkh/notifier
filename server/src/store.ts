// SPDX-License-Identifier: GPL-3.0-or-later
// D1 persistence for users, invites, devices, API keys, pending pairings,
// and channels.
// Times are Unix seconds. Everything a user owns is looked up by user ID, and
// the credentials of a disabled user resolve to nothing.

export interface User {
  id: string;
  name: string;
  created_at: number;
  last_seen_at: number;
  disabled_at: number | null;
  invited_by?: string | null;
}

export interface Device {
  id: string;
  user_id: string;
  name: string;
  /** base64 AES-256 key */
  seal_key: string;
  fcm_token: string;
  transport: "fcm" | "ws";
  created_at: number;
  last_seen_at: number;
}

export interface ApiKey {
  id: string;
  user_id: string;
  name: string;
  created_at: number;
  last_used_at: number | null;
}

const USER_COLUMNS = "id, name, created_at, last_seen_at, disabled_at";
const DEVICE_COLUMNS = "d.id, d.user_id, d.name, d.seal_key, d.fcm_token, d.transport, d.created_at, d.last_seen_at";
const API_KEY_COLUMNS = "k.id, k.user_id, k.name, k.created_at, k.last_used_at";

/** Joins the owner of the rows aliased [table], keeping only enabled owners. */
const activeOwner = (table: string) => `JOIN users u ON u.id = ${table}.user_id AND u.disabled_at IS NULL`;

// --- users and invites ---

/**
 * Stores an invite unless its creator has already issued [max], counting both
 * unused invites and the users who signed up with one (otherwise a user could
 * redeem their own invites for unlimited accounts). The operator (createdBy
 * "") is not capped. The check and the insert are one statement, so
 * concurrent requests cannot overshoot the cap.
 */
export async function createInvite(
  db: D1Database,
  codeHash: string,
  createdBy: string,
  now: number,
  expiresAt: number,
  max: number,
): Promise<boolean> {
  const result = await db
    .prepare(
      `INSERT INTO invites (code_hash, created_by, expires_at)
       SELECT ?, ?, ?
       WHERE ? = ''
          OR (SELECT count(*) FROM invites WHERE created_by = ? AND expires_at > ?)
           + (SELECT count(*) FROM users WHERE invited_by = ?) < ?`,
    )
    .bind(codeHash, createdBy, expiresAt, createdBy, createdBy, now, createdBy, max)
    .run();
  return result.meta.changes === 1;
}

/**
 * Consumes an invite and creates the user it admits, atomically, like
 * redeemPairing. Returns false when the invite is unknown, used, or expired,
 * or its creator has since been disabled (or else a disabled user could
 * return under a new account).
 */
export async function redeemInvite(
  db: D1Database,
  codeHash: string,
  now: number,
  user: Pick<User, "id" | "name">,
  tokenHash: string,
): Promise<boolean> {
  const [inserted] = await db.batch([
    db
      .prepare(
        `INSERT INTO users (id, name, token_hash, created_at, last_seen_at, invited_by)
         SELECT ?, ?, ?, ?, ?, NULLIF(i.created_by, '')
         FROM invites i
         WHERE i.code_hash = ? AND i.expires_at > ?
           AND (i.created_by = '' OR i.created_by IN (SELECT id FROM users WHERE disabled_at IS NULL))`,
      )
      .bind(user.id, user.name, tokenHash, now, now, codeHash, now),
    db.prepare("DELETE FROM invites WHERE code_hash = ? OR expires_at <= ?").bind(codeHash, now),
  ]);
  return (inserted?.meta.changes ?? 0) > 0; // also counts rows a trigger writes
}

export async function userByTokenHash(db: D1Database, tokenHash: string, now: number): Promise<User | null> {
  const user = await db
    .prepare(`SELECT ${USER_COLUMNS} FROM users WHERE token_hash = ? AND disabled_at IS NULL`)
    .bind(tokenHash)
    .first<User>();
  if (user) await db.prepare("UPDATE users SET last_seen_at = ? WHERE id = ?").bind(now, user.id).run();
  return user;
}

export async function listUsers(db: D1Database): Promise<(User & { devices: number })[]> {
  const { results } = await db
    .prepare(
      `SELECT ${USER_COLUMNS}, invited_by, (SELECT count(*) FROM devices WHERE user_id = users.id) AS devices
       FROM users ORDER BY created_at`,
    )
    .all<User & { devices: number }>();
  return results;
}

export async function setUserTokenHash(db: D1Database, id: string, tokenHash: string): Promise<boolean> {
  const result = await db.prepare("UPDATE users SET token_hash = ? WHERE id = ?").bind(tokenHash, id).run();
  return result.meta.changes === 1;
}

export async function setUserDisabled(db: D1Database, id: string, disabledAt: number | null): Promise<boolean> {
  const result = await db.prepare("UPDATE users SET disabled_at = ? WHERE id = ?").bind(disabledAt, id).run();
  return result.meta.changes === 1;
}

// --- devices ---

/** Stores a pairing code unless its user already has [maxPending] live ones; atomic, like createInvite. */
export async function createPairing(
  db: D1Database,
  codeHash: string,
  userId: string,
  now: number,
  expiresAt: number,
  maxPending: number,
): Promise<boolean> {
  const result = await db
    .prepare(
      `INSERT INTO pairings (code_hash, user_id, expires_at)
       SELECT ?, ?, ?
       WHERE (SELECT count(*) FROM pairings WHERE user_id = ? AND expires_at > ?) < ?`,
    )
    .bind(codeHash, userId, expiresAt, userId, now, maxPending)
    .run();
  return result.meta.changes === 1;
}

/**
 * Consumes a pairing code and creates the device it authorizes, owned by the
 * user who created the code, atomically: a code is redeemable once, only
 * before it expires, and only while its user has fewer than [maxDevices].
 * Returns false otherwise.
 */
export async function redeemPairing(
  db: D1Database,
  codeHash: string,
  now: number,
  device: Omit<Device, "user_id">,
  tokenHash: string,
  maxDevices: number,
): Promise<boolean> {
  const [inserted] = await db.batch([
    db
      .prepare(
        `INSERT INTO devices (id, user_id, name, token_hash, seal_key, fcm_token, transport, created_at, last_seen_at)
         SELECT ?, p.user_id, ?, ?, ?, ?, ?, ?, ?
         FROM pairings p ${activeOwner("p")}
         WHERE p.code_hash = ? AND p.expires_at > ?
           AND (SELECT count(*) FROM devices WHERE user_id = p.user_id) < ?`,
      )
      .bind(
        device.id,
        device.name,
        tokenHash,
        device.seal_key,
        device.fcm_token,
        device.transport,
        now,
        now,
        codeHash,
        now,
        maxDevices,
      ),
    db.prepare("DELETE FROM pairings WHERE code_hash = ? OR expires_at <= ?").bind(codeHash, now),
  ]);
  return (inserted?.meta.changes ?? 0) > 0; // also counts rows a trigger writes
}

export async function deviceByTokenHash(db: D1Database, tokenHash: string, now: number): Promise<Device | null> {
  const device = await db
    .prepare(`SELECT ${DEVICE_COLUMNS} FROM devices d ${activeOwner("d")} WHERE d.token_hash = ?`)
    .bind(tokenHash)
    .first<Device>();
  if (device) await db.prepare("UPDATE devices SET last_seen_at = ? WHERE id = ?").bind(now, device.id).run();
  return device;
}

export async function listDevices(db: D1Database, userId: string): Promise<Device[]> {
  const { results } = await db
    .prepare(`SELECT ${DEVICE_COLUMNS} FROM devices d WHERE d.user_id = ? ORDER BY d.created_at`)
    .bind(userId)
    .all<Device>();
  return results;
}

export async function setFcmToken(db: D1Database, deviceId: string, fcmToken: string): Promise<boolean> {
  const result = await db.prepare("UPDATE devices SET fcm_token = ? WHERE id = ?").bind(fcmToken, deviceId).run();
  return result.meta.changes === 1;
}

/** The transport of the user's device [id], or null when they have no such device. */
export async function deviceTransport(db: D1Database, userId: string, id: string): Promise<Device["transport"] | null> {
  return db
    .prepare("SELECT transport FROM devices WHERE id = ? AND user_id = ?")
    .bind(id, userId)
    .first<Device["transport"]>("transport");
}

export async function deleteDevice(db: D1Database, userId: string, id: string): Promise<boolean> {
  const [deleted] = await db.batch([
    db.prepare("DELETE FROM devices WHERE id = ? AND user_id = ?").bind(id, userId),
    db.prepare("DELETE FROM subscriptions WHERE device_id = ? AND NOT EXISTS (SELECT 1 FROM devices WHERE id = ?)").bind(id, id),
  ]);
  return deleted?.meta.changes === 1;
}

// --- API keys ---

/**
 * Stores a key, limited to [channelIds] when any are given, unless its user
 * already has [max]; atomic, like createInvite. The key and its scope are
 * one batch, so a key can never exist without the scope it was given.
 */
export async function createApiKey(
  db: D1Database,
  key: ApiKey,
  keyHash: string,
  max: number,
  channelIds: readonly string[],
): Promise<boolean> {
  const [inserted] = await db.batch([
    db
      .prepare(
        `INSERT INTO api_keys (id, user_id, name, key_hash, created_at)
         SELECT ?, ?, ?, ?, ?
         WHERE (SELECT count(*) FROM api_keys WHERE user_id = ?) < ?`,
      )
      .bind(key.id, key.user_id, key.name, keyHash, key.created_at, key.user_id, max),
    ...channelIds.map((channelId) =>
      db
        .prepare("INSERT INTO api_key_channels (key_id, channel_id) SELECT id, ? FROM api_keys WHERE id = ?")
        .bind(channelId, key.id),
    ),
  ]);
  return inserted?.meta.changes === 1;
}

/** The channel IDs a key is limited to; empty means unscoped. */
export async function apiKeyScope(db: D1Database, keyId: string): Promise<string[]> {
  const { results } = await db
    .prepare("SELECT channel_id FROM api_key_channels WHERE key_id = ? ORDER BY channel_id")
    .bind(keyId)
    .all<{ channel_id: string }>();
  return results.map((r) => r.channel_id);
}

export async function apiKeyByHash(db: D1Database, keyHash: string, now: number): Promise<ApiKey | null> {
  const key = await db
    .prepare(`SELECT ${API_KEY_COLUMNS} FROM api_keys k ${activeOwner("k")} WHERE k.key_hash = ?`)
    .bind(keyHash)
    .first<ApiKey>();
  if (key) await db.prepare("UPDATE api_keys SET last_used_at = ? WHERE id = ?").bind(now, key.id).run();
  return key;
}

export async function listApiKeys(db: D1Database, userId: string): Promise<ApiKey[]> {
  const { results } = await db
    .prepare(`SELECT ${API_KEY_COLUMNS} FROM api_keys k WHERE k.user_id = ? ORDER BY k.created_at`)
    .bind(userId)
    .all<ApiKey>();
  return results;
}

export async function deleteApiKey(db: D1Database, userId: string, id: string): Promise<boolean> {
  const [deleted] = await db.batch([
    db.prepare("DELETE FROM api_keys WHERE id = ? AND user_id = ?").bind(id, userId),
    db.prepare("DELETE FROM api_key_channels WHERE key_id = ? AND NOT EXISTS (SELECT 1 FROM api_keys WHERE id = ?)").bind(id, id),
  ]);
  return deleted?.meta.changes === 1;
}

// --- channels ---

export interface Channel {
  id: string;
  owner_id: string;
  owner_name: string;
  name: string;
  created_at: number;
}

/** A channel as one of its members sees it. */
export interface Membership extends Channel {
  members: number;
}

// The Android app recognizes a default channel by this prefix (ChannelRoute.kt);
// newId("ch_") cannot produce it, since its base32 has no "_".
export const defaultChannelId = (userId: string) => `ch_default_${userId}`;

const CHANNEL_COLUMNS = "c.id, c.owner_id, o.name AS owner_name, c.name, c.created_at";

/**
 * Creates a channel and makes its owner a member, unless the owner already
 * owns [max] channels or one with this name. Returns false in either case.
 */
export async function createChannel(
  db: D1Database,
  channel: Pick<Channel, "id" | "owner_id" | "name" | "created_at">,
  max: number,
  maxMemberships: number,
): Promise<boolean> {
  const [inserted] = await db.batch([
    db
      .prepare(
        `INSERT OR IGNORE INTO channels (id, owner_id, name, created_at)
         SELECT ?, ?, ?, ?
         WHERE (SELECT count(*) FROM channels WHERE owner_id = ? AND name != 'default') < ?
           AND (SELECT count(*) FROM channel_members WHERE user_id = ?) < ?`,
      )
      .bind(
        channel.id,
        channel.owner_id,
        channel.name,
        channel.created_at,
        channel.owner_id,
        max,
        channel.owner_id,
        maxMemberships,
      ),
    db
      .prepare("INSERT INTO channel_members (channel_id, user_id, joined_at) SELECT id, owner_id, created_at FROM channels WHERE id = ?")
      .bind(channel.id),
  ]);
  return inserted?.meta.changes === 1;
}

export async function ownedChannelNamed(db: D1Database, ownerId: string, name: string): Promise<Channel | null> {
  return db
    .prepare(`SELECT ${CHANNEL_COLUMNS} FROM channels c JOIN users o ON o.id = c.owner_id WHERE c.owner_id = ? AND c.name = ?`)
    .bind(ownerId, name)
    .first<Channel>();
}

export async function ownedChannel(db: D1Database, ownerId: string, id: string): Promise<Channel | null> {
  return db
    .prepare(`SELECT ${CHANNEL_COLUMNS} FROM channels c JOIN users o ON o.id = c.owner_id WHERE c.owner_id = ? AND c.id = ?`)
    .bind(ownerId, id)
    .first<Channel>();
}

/** Channels the user belongs to, owned or joined, in the order they joined. */
export async function memberships(db: D1Database, userId: string): Promise<Membership[]> {
  const { results } = await db
    .prepare(
      `SELECT ${CHANNEL_COLUMNS}, (SELECT count(*) FROM channel_members WHERE channel_id = c.id) AS members
       FROM channel_members m
       JOIN channels c ON c.id = m.channel_id
       JOIN users o ON o.id = c.owner_id
       WHERE m.user_id = ?
       ORDER BY m.joined_at, c.name`,
    )
    .bind(userId)
    .all<Membership>();
  return results;
}

/** Deletes a channel the user owns, other than their default, with everything that refers to it. */
export async function deleteChannel(db: D1Database, ownerId: string, id: string): Promise<boolean> {
  const [deleted] = await db.batch([
    db.prepare("DELETE FROM channels WHERE id = ? AND owner_id = ? AND name != 'default'").bind(id, ownerId),
    // api_key_channels rows stay: a key scoped only to deleted channels must
    // stay scoped (to nothing), not become unscoped.
    ...["channel_members", "subscriptions", "channel_invites"].map((table) =>
      db.prepare(`DELETE FROM ${table} WHERE channel_id = ? AND NOT EXISTS (SELECT 1 FROM channels WHERE id = ?)`).bind(id, id),
    ),
    // Monitoring must not stop silently: heartbeats fall back to default.
    db
      .prepare(
        `UPDATE heartbeats SET channel_id = 'ch_default_' || user_id
         WHERE channel_id = ? AND NOT EXISTS (SELECT 1 FROM channels WHERE id = ?)`,
      )
      .bind(id, id),
  ]);
  return deleted?.meta.changes === 1;
}

/** Stores a channel invite unless the channel already has [maxPending] unused ones. */
export async function createChannelInvite(
  db: D1Database,
  codeHash: string,
  channelId: string,
  now: number,
  expiresAt: number,
  maxPending: number,
): Promise<boolean> {
  const result = await db
    .prepare(
      `INSERT INTO channel_invites (code_hash, channel_id, expires_at)
       SELECT ?, ?, ?
       WHERE (SELECT count(*) FROM channel_invites WHERE channel_id = ? AND expires_at > ?) < ?`,
    )
    .bind(codeHash, channelId, expiresAt, channelId, now, maxPending)
    .run();
  return result.meta.changes === 1;
}

export type JoinResult = { channel: Channel; joined: boolean } | "invalid" | "full" | "channel full";

/**
 * Redeems a channel invite for [userId], atomically: the membership and the
 * invite's deletion are one batch, so an invite admits one user. A user who
 * is already a member keeps the invite unspent. "full" means the user
 * already belongs to [maxMemberships] channels, "channel full" that the
 * channel already has [maxMembers] members.
 */
export async function joinChannel(
  db: D1Database,
  codeHash: string,
  userId: string,
  now: number,
  maxMemberships: number,
  maxMembers: number,
): Promise<JoinResult> {
  const channel = await db
    .prepare(
      `SELECT ${CHANNEL_COLUMNS} FROM channel_invites i
       JOIN channels c ON c.id = i.channel_id
       JOIN users o ON o.id = c.owner_id AND o.disabled_at IS NULL
       WHERE i.code_hash = ? AND i.expires_at > ?`,
    )
    .bind(codeHash, now)
    .first<Channel>();
  if (!channel) return "invalid";
  const member = await db
    .prepare("SELECT 1 FROM channel_members WHERE channel_id = ? AND user_id = ?")
    .bind(channel.id, userId)
    .first();
  if (member) return { channel, joined: false };

  const [inserted] = await db.batch([
    db
      .prepare(
        `INSERT OR IGNORE INTO channel_members (channel_id, user_id, joined_at)
         SELECT ?, ?, ?
         WHERE EXISTS (SELECT 1 FROM channel_invites WHERE code_hash = ? AND expires_at > ?)
           AND (SELECT count(*) FROM channel_members WHERE user_id = ?) < ?
           AND (SELECT count(*) FROM channel_members WHERE channel_id = ?) < ?`,
      )
      .bind(channel.id, userId, now, codeHash, now, userId, maxMemberships, channel.id, maxMembers),
    db
      .prepare(
        `DELETE FROM channel_invites WHERE code_hash = ?
         AND EXISTS (SELECT 1 FROM channel_members WHERE channel_id = ? AND user_id = ? AND joined_at = ?)`,
      )
      .bind(codeHash, channel.id, userId, now),
  ]);
  if (inserted?.meta.changes === 1) return { channel, joined: true };
  const raced = await db
    .prepare("SELECT 1 FROM channel_members WHERE channel_id = ? AND user_id = ?")
    .bind(channel.id, userId)
    .first();
  if (raced) return { channel, joined: false };
  const stillValid = await db.prepare("SELECT 1 FROM channel_invites WHERE code_hash = ?").bind(codeHash).first();
  if (!stillValid) return "invalid";
  const joined = await db.prepare("SELECT count(*) AS n FROM channel_members WHERE user_id = ?").bind(userId).first<number>("n");
  return (joined ?? 0) >= maxMemberships ? "full" : "channel full";
}

export async function members(
  db: D1Database,
  channelId: string,
): Promise<{ id: string; name: string; joined_at: number }[]> {
  const { results } = await db
    .prepare(
      `SELECT u.id, u.name, m.joined_at FROM channel_members m JOIN users u ON u.id = m.user_id
       WHERE m.channel_id = ? ORDER BY m.joined_at, u.name`,
    )
    .bind(channelId)
    .all<{ id: string; name: string; joined_at: number }>();
  return results;
}

/**
 * Removes a member who is not the owner, and unsubscribes that member's
 * devices from the channel.
 */
export async function removeMember(db: D1Database, channelId: string, userId: string): Promise<boolean> {
  const [deleted] = await db.batch([
    db
      .prepare(
        `DELETE FROM channel_members WHERE channel_id = ? AND user_id = ?
         AND user_id != (SELECT owner_id FROM channels WHERE id = ?)`,
      )
      .bind(channelId, userId, channelId),
    db
      .prepare(
        `DELETE FROM subscriptions WHERE channel_id = ?
         AND device_id IN (SELECT id FROM devices WHERE user_id = ?)
         AND NOT EXISTS (SELECT 1 FROM channel_members WHERE channel_id = ? AND user_id = ?)`,
      )
      .bind(channelId, userId, channelId, userId),
  ]);
  return deleted?.meta.changes === 1;
}

export async function deviceSubscriptions(db: D1Database, deviceId: string): Promise<string[]> {
  const { results } = await db
    .prepare("SELECT channel_id FROM subscriptions WHERE device_id = ?")
    .bind(deviceId)
    .all<{ channel_id: string }>();
  return results.map((r) => r.channel_id);
}

/** Replaces a device's subscriptions. The caller checks that its owner belongs to every channel. */
export async function setSubscriptions(db: D1Database, deviceId: string, channelIds: readonly string[]): Promise<void> {
  await db.batch([
    db.prepare("DELETE FROM subscriptions WHERE device_id = ?").bind(deviceId),
    ...channelIds.map((channelId) =>
      db.prepare("INSERT OR IGNORE INTO subscriptions (device_id, channel_id) VALUES (?, ?)").bind(deviceId, channelId),
    ),
  ]);
}

export async function subscribe(db: D1Database, deviceId: string, channelId: string): Promise<void> {
  await db
    .prepare("INSERT OR IGNORE INTO subscriptions (device_id, channel_id) VALUES (?, ?)")
    .bind(deviceId, channelId)
    .run();
}

/**
 * The devices subscribed to [channelId] whose owners are still enabled
 * members of it: a removed member's stale subscription delivers nothing.
 */
export async function subscribers(db: D1Database, channelId: string): Promise<Device[]> {
  const { results } = await db
    .prepare(
      `SELECT ${DEVICE_COLUMNS} FROM subscriptions s
       JOIN devices d ON d.id = s.device_id
       JOIN channel_members m ON m.channel_id = s.channel_id AND m.user_id = d.user_id
       ${activeOwner("d")}
       WHERE s.channel_id = ?
       ORDER BY d.created_at`,
    )
    .bind(channelId)
    .all<Device>();
  return results;
}

// --- heartbeats ---

export interface Heartbeat {
  id: string;
  user_id: string;
  channel_id: string;
  name: string;
  interval_s: number;
  grace_s: number;
  created_at: number;
  last_ping_at: number | null;
  /** When the cron trigger alerts unless a ping comes first. */
  due_at: number;
  state: "up" | "down" | "failed";
  /** When it last went down, failed, or recovered. */
  changed_at: number;
  /** A notice that reached no device yet (JSON Notice), or null. */
  pending_notice: string | null;
  /** When the cron trigger may next try the pending notice. */
  notice_due_at: number | null;
}

const HEARTBEAT_COLUMNS =
  "h.id, h.user_id, h.channel_id, h.name, h.interval_s, h.grace_s, h.created_at, h.last_ping_at, h.due_at, h.state, h.changed_at, h.pending_notice, h.notice_due_at";

/**
 * Stores a heartbeat unless the user has one of that name ("exists"), already
 * has [max] ("full"), or its channel was deleted meanwhile ("gone").
 */
export async function createHeartbeat(
  db: D1Database,
  h: Heartbeat,
  secretHash: string,
  max: number,
): Promise<"created" | "exists" | "full" | "gone"> {
  const { meta } = await db
    .prepare(
      `INSERT INTO heartbeats (id, user_id, channel_id, name, secret_hash, interval_s, grace_s, created_at, due_at, state, changed_at)
       SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, 'up', ?
       WHERE (SELECT count(*) FROM heartbeats WHERE user_id = ?) < ?
         AND NOT EXISTS (SELECT 1 FROM heartbeats WHERE user_id = ? AND name = ?)
         AND EXISTS (SELECT 1 FROM channels WHERE id = ? AND owner_id = ?)`,
    )
    .bind(h.id, h.user_id, h.channel_id, h.name, secretHash, h.interval_s, h.grace_s, h.created_at, h.due_at, h.created_at, h.user_id, max, h.user_id, h.name, h.channel_id, h.user_id)
    .run();
  if (meta.changes === 1) return "created";
  const named = await db.prepare("SELECT 1 FROM heartbeats WHERE user_id = ? AND name = ?").bind(h.user_id, h.name).first();
  if (named) return "exists";
  const channel = await db.prepare("SELECT 1 FROM channels WHERE id = ?").bind(h.channel_id).first();
  return channel ? "full" : "gone";
}

/** The user's heartbeats with the name of the channel each alerts. */
export async function listHeartbeats(db: D1Database, userId: string): Promise<(Heartbeat & { channel_name: string })[]> {
  const { results } = await db
    .prepare(
      `SELECT ${HEARTBEAT_COLUMNS}, coalesce(c.name, 'default') AS channel_name FROM heartbeats h
       LEFT JOIN channels c ON c.id = h.channel_id
       WHERE h.user_id = ? ORDER BY h.name`,
    )
    .bind(userId)
    .all<Heartbeat & { channel_name: string }>();
  return results;
}

export async function deleteHeartbeat(db: D1Database, userId: string, id: string): Promise<boolean> {
  const { meta } = await db.prepare("DELETE FROM heartbeats WHERE id = ? AND user_id = ?").bind(id, userId).run();
  return meta.changes === 1;
}

/** The heartbeat a ping URL's secret belongs to; nothing for a disabled user's. */
export async function heartbeatBySecretHash(db: D1Database, secretHash: string): Promise<Heartbeat | null> {
  return db
    .prepare(`SELECT ${HEARTBEAT_COLUMNS} FROM heartbeats h ${activeOwner("h")} WHERE h.secret_hash = ?`)
    .bind(secretHash)
    .first<Heartbeat>();
}

/** What recordPing saw: the state a ping left, if it changed it, and its place in line. */
export interface Ping {
  /** The state the ping ended, with when it began; null if it changed nothing. */
  left: Pick<Heartbeat, "state" | "changed_at"> | null;
  /** Its ping_seq, which setPendingNotice requires to still be current. */
  seq: number;
}

/**
 * Records a check-in: the next one is due an interval (plus grace) from now.
 * [failed] marks it failed instead of up. Of two concurrent pings, only one
 * changes the state. A ping that announces something (a failure, or the end
 * of one or of an outage) advances ping_seq. The batch is one transaction,
 * so a cron run cannot change the state between the read and the update.
 */
export async function recordPing(db: D1Database, id: string, now: number, failed: boolean): Promise<Ping> {
  const state = failed ? "failed" : "up";
  const results = await db.batch([
    db.prepare("SELECT state, changed_at FROM heartbeats WHERE id = ?").bind(id),
    db
      .prepare("UPDATE heartbeats SET state = ?, changed_at = ?, ping_seq = ping_seq + 1 WHERE id = ? AND state != ?")
      .bind(state, now, id, state),
    db
      .prepare(
        `UPDATE heartbeats SET last_ping_at = ?, due_at = ? + interval_s + grace_s,
           ping_seq = ping_seq + ? WHERE id = ?`,
      )
      .bind(now, now, failed ? 1 : 0, id),
    db.prepare("SELECT ping_seq FROM heartbeats WHERE id = ?").bind(id),
  ]);
  const changed = results[1]?.meta.changes === 1;
  const left = changed ? ((results[0]?.results[0] as Ping["left"] | undefined) ?? null) : null;
  return { left, seq: (results[3]?.results[0] as { ping_seq: number } | undefined)?.ping_seq ?? 0 };
}

/**
 * Heartbeats of enabled users whose check-in is overdue, the most overdue
 * first. A failed one is checked too: a job that reported a failure and then
 * stopped running is still missing.
 */
export async function dueHeartbeats(db: D1Database, now: number, limit: number): Promise<Heartbeat[]> {
  const { results } = await db
    .prepare(
      `SELECT ${HEARTBEAT_COLUMNS} FROM heartbeats h ${activeOwner("h")}
       WHERE h.state IN ('up', 'failed') AND h.due_at <= ? ORDER BY h.due_at LIMIT ?`,
    )
    .bind(now, limit)
    .all<Heartbeat>();
  return results;
}

/** A pending recovery notice markDown removed, or nulls if it removed none. */
export type DroppedNotice = Pick<Heartbeat, "pending_notice" | "notice_due_at">;

/**
 * Marks an overdue heartbeat down; null if a ping or another run got there
 * first. A pending recovery notice is dropped, since the missed check-in
 * contradicts it, and returned so unmarkDown can put it back; a pending
 * failure notice, with the job's log, stays.
 */
export async function markDown(db: D1Database, id: string, now: number): Promise<DroppedNotice | null> {
  const recovered = "json_extract(pending_notice, '$.kind') = 'recovered'";
  const [before, changed] = await db.batch([
    db
      .prepare(
        `SELECT CASE WHEN ${recovered} THEN pending_notice END AS pending_notice,
           CASE WHEN ${recovered} THEN notice_due_at END AS notice_due_at
         FROM heartbeats WHERE id = ?`,
      )
      .bind(id),
    db
      .prepare(
        `UPDATE heartbeats SET state = 'down', changed_at = ?,
           notice_due_at = CASE WHEN ${recovered} THEN NULL ELSE notice_due_at END,
           pending_notice = CASE WHEN ${recovered} THEN NULL ELSE pending_notice END
         WHERE id = ? AND state IN ('up', 'failed') AND due_at <= ?`,
      )
      .bind(now, id, now),
  ]);
  if (changed?.meta.changes !== 1) return null;
  return (before?.results[0] as DroppedNotice | undefined) ?? { pending_notice: null, notice_due_at: null };
}

/**
 * Undoes markDown when its alert reached nobody, so a later run tries again;
 * not if a ping has come in since. The retry is due at [retryAt], behind
 * heartbeats that went overdue earlier, so failing ones cannot crowd the
 * rest out of a run. Only the notice markDown [dropped] is put back: a
 * failure notice it kept may have been delivered meanwhile.
 */
export async function unmarkDown(
  db: D1Database,
  hb: Heartbeat,
  dropped: DroppedNotice,
  markedAt: number,
  retryAt: number,
): Promise<void> {
  const restore = dropped.pending_notice === null ? 0 : 1;
  await db
    .prepare(
      `UPDATE heartbeats SET state = ?, changed_at = ?, due_at = ?,
         pending_notice = CASE WHEN ? THEN ? ELSE pending_notice END,
         notice_due_at = CASE WHEN ? THEN ? ELSE notice_due_at END
       WHERE id = ? AND state = 'down' AND changed_at = ?`,
    )
    .bind(hb.state, hb.changed_at, retryAt, restore, dropped.pending_notice, restore, dropped.notice_due_at, hb.id, markedAt)
    .run();
}

/**
 * Keeps [notice] as the heartbeat's pending notice, replacing an older one,
 * not to be tried by the cron trigger before [dueAt]. False, keeping
 * nothing, unless the heartbeat is still in [state], the one the notice
 * announces, and no later ping has announced anything since [seq]: a
 * request that stalled must not replace a newer notice.
 */
export async function setPendingNotice(
  db: D1Database,
  id: string,
  state: Heartbeat["state"],
  seq: number,
  notice: string,
  dueAt: number,
): Promise<boolean> {
  const { meta } = await db
    .prepare("UPDATE heartbeats SET pending_notice = ?, notice_due_at = ? WHERE id = ? AND state = ? AND ping_seq = ?")
    .bind(notice, dueAt, id, state, seq)
    .run();
  return meta.changes === 1;
}

/**
 * Moves a pending notice's next try to [dueAt] if it is still [notice] and,
 * when [ifDueBy] is given, due by then. As a claim, of two cron runs only
 * one gets true.
 */
export async function deferNotice(db: D1Database, id: string, notice: string, dueAt: number, ifDueBy?: number): Promise<boolean> {
  const { meta } = await db
    .prepare("UPDATE heartbeats SET notice_due_at = ? WHERE id = ? AND pending_notice = ? AND notice_due_at <= ?")
    .bind(dueAt, id, notice, ifDueBy ?? Number.MAX_SAFE_INTEGER)
    .run();
  return meta.changes === 1;
}

/** Clears a pending notice once delivered, unless a newer one replaced it meanwhile. */
export async function clearPendingNotice(db: D1Database, id: string, notice: string): Promise<void> {
  await db
    .prepare("UPDATE heartbeats SET pending_notice = NULL, notice_due_at = NULL WHERE id = ? AND pending_notice = ?")
    .bind(id, notice)
    .run();
}

/** Heartbeats of enabled users with a notice due for a retry, the longest waiting first. */
export async function pendingNotices(db: D1Database, now: number, limit: number): Promise<Heartbeat[]> {
  const { results } = await db
    .prepare(
      `SELECT ${HEARTBEAT_COLUMNS} FROM heartbeats h ${activeOwner("h")}
       WHERE h.pending_notice IS NOT NULL AND h.notice_due_at <= ? ORDER BY h.notice_due_at LIMIT ?`,
    )
    .bind(now, limit)
    .all<Heartbeat>();
  return results;
}
