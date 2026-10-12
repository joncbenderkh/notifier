// SPDX-License-Identifier: GPL-3.0-or-later
// D1 persistence for users, invites, devices, API keys, and pending pairings.
// Times are Unix seconds. Everything a user owns is looked up by user ID, and
// the credentials of a disabled user resolve to nothing.

export interface User {
  id: string;
  name: string;
  created_at: number;
  last_seen_at: number;
  disabled_at: number | null;
}

export interface Device {
  id: string;
  user_id: string;
  name: string;
  /** base64 AES-256 key */
  seal_key: string;
  fcm_token: string;
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
const DEVICE_COLUMNS = "d.id, d.user_id, d.name, d.seal_key, d.fcm_token, d.created_at, d.last_seen_at";
const API_KEY_COLUMNS = "k.id, k.user_id, k.name, k.created_at, k.last_used_at";

/** Joins the owner of the rows aliased [table], keeping only enabled owners. */
const activeOwner = (table: string) => `JOIN users u ON u.id = ${table}.user_id AND u.disabled_at IS NULL`;

// --- users and invites ---

/**
 * Stores an invite unless its creator already has [maxPending] unused ones;
 * the operator (createdBy "") is not capped. The check and the insert are one
 * statement, so concurrent requests cannot overshoot the cap.
 */
export async function createInvite(
  db: D1Database,
  codeHash: string,
  createdBy: string,
  now: number,
  expiresAt: number,
  maxPending: number,
): Promise<boolean> {
  const result = await db
    .prepare(
      `INSERT INTO invites (code_hash, created_by, expires_at)
       SELECT ?, ?, ?
       WHERE ? = '' OR (SELECT count(*) FROM invites WHERE created_by = ? AND expires_at > ?) < ?`,
    )
    .bind(codeHash, createdBy, expiresAt, createdBy, createdBy, now, maxPending)
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
        `INSERT INTO users (id, name, token_hash, created_at, last_seen_at)
         SELECT ?, ?, ?, ?, ?
         WHERE EXISTS (
           SELECT 1 FROM invites i
           WHERE i.code_hash = ? AND i.expires_at > ?
             AND (i.created_by = '' OR i.created_by IN (SELECT id FROM users WHERE disabled_at IS NULL))
         )`,
      )
      .bind(user.id, user.name, tokenHash, now, now, codeHash, now),
    db.prepare("DELETE FROM invites WHERE code_hash = ? OR expires_at <= ?").bind(codeHash, now),
  ]);
  return (inserted?.meta.changes ?? 0) === 1;
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
      `SELECT ${USER_COLUMNS}, (SELECT count(*) FROM devices WHERE user_id = users.id) AS devices
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
        `INSERT INTO devices (id, user_id, name, token_hash, seal_key, fcm_token, created_at, last_seen_at)
         SELECT ?, p.user_id, ?, ?, ?, ?, ?, ?
         FROM pairings p ${activeOwner("p")}
         WHERE p.code_hash = ? AND p.expires_at > ?
           AND (SELECT count(*) FROM devices WHERE user_id = p.user_id) < ?`,
      )
      .bind(device.id, device.name, tokenHash, device.seal_key, device.fcm_token, now, now, codeHash, now, maxDevices),
    db.prepare("DELETE FROM pairings WHERE code_hash = ? OR expires_at <= ?").bind(codeHash, now),
  ]);
  return (inserted?.meta.changes ?? 0) === 1;
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

export async function deleteDevice(db: D1Database, userId: string, id: string): Promise<boolean> {
  const result = await db.prepare("DELETE FROM devices WHERE id = ? AND user_id = ?").bind(id, userId).run();
  return result.meta.changes === 1;
}

// --- API keys ---

/** Stores a key unless its user already has [max]; atomic, like createInvite. */
export async function createApiKey(db: D1Database, key: ApiKey, keyHash: string, max: number): Promise<boolean> {
  const result = await db
    .prepare(
      `INSERT INTO api_keys (id, user_id, name, key_hash, created_at)
       SELECT ?, ?, ?, ?, ?
       WHERE (SELECT count(*) FROM api_keys WHERE user_id = ?) < ?`,
    )
    .bind(key.id, key.user_id, key.name, keyHash, key.created_at, key.user_id, max)
    .run();
  return result.meta.changes === 1;
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
  const result = await db.prepare("DELETE FROM api_keys WHERE id = ? AND user_id = ?").bind(id, userId).run();
  return result.meta.changes === 1;
}
