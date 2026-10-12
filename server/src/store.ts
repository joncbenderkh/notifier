// SPDX-License-Identifier: GPL-3.0-or-later
// D1 persistence for devices, API keys, and pending pairings. Times are Unix seconds.

export interface Device {
  id: string;
  name: string;
  /** base64 AES-256 key */
  seal_key: string;
  fcm_token: string;
  created_at: number;
  last_seen_at: number;
}

export interface ApiKey {
  id: string;
  name: string;
  created_at: number;
  last_used_at: number | null;
}

const DEVICE_COLUMNS = "id, name, seal_key, fcm_token, created_at, last_seen_at";

export async function createPairing(db: D1Database, codeHash: string, expiresAt: number): Promise<void> {
  await db.prepare("INSERT INTO pairings (code_hash, expires_at) VALUES (?, ?)").bind(codeHash, expiresAt).run();
}

/**
 * Consumes a pairing code and creates the device it authorizes, atomically:
 * a code is redeemable once, and only before it expires. Returns false when
 * the code is unknown, used, or expired.
 */
export async function redeemPairing(
  db: D1Database,
  codeHash: string,
  now: number,
  device: Device,
  tokenHash: string,
): Promise<boolean> {
  const [inserted] = await db.batch([
    db
      .prepare(
        `INSERT INTO devices (id, name, token_hash, seal_key, fcm_token, created_at, last_seen_at)
         SELECT ?, ?, ?, ?, ?, ?, ?
         WHERE EXISTS (SELECT 1 FROM pairings WHERE code_hash = ? AND expires_at > ?)`,
      )
      .bind(device.id, device.name, tokenHash, device.seal_key, device.fcm_token, now, now, codeHash, now),
    db.prepare("DELETE FROM pairings WHERE code_hash = ? OR expires_at <= ?").bind(codeHash, now),
  ]);
  return (inserted?.meta.changes ?? 0) === 1;
}

export async function deviceByTokenHash(db: D1Database, tokenHash: string, now: number): Promise<Device | null> {
  const device = await db
    .prepare(`SELECT ${DEVICE_COLUMNS} FROM devices WHERE token_hash = ?`)
    .bind(tokenHash)
    .first<Device>();
  if (device) await db.prepare("UPDATE devices SET last_seen_at = ? WHERE id = ?").bind(now, device.id).run();
  return device;
}

export async function listDevices(db: D1Database): Promise<Device[]> {
  const { results } = await db.prepare(`SELECT ${DEVICE_COLUMNS} FROM devices ORDER BY created_at`).all<Device>();
  return results;
}

export async function setFcmToken(db: D1Database, deviceId: string, fcmToken: string): Promise<boolean> {
  const result = await db.prepare("UPDATE devices SET fcm_token = ? WHERE id = ?").bind(fcmToken, deviceId).run();
  return result.meta.changes === 1;
}

export async function deleteDevice(db: D1Database, id: string): Promise<boolean> {
  const result = await db.prepare("DELETE FROM devices WHERE id = ?").bind(id).run();
  return result.meta.changes === 1;
}

export async function createApiKey(db: D1Database, key: ApiKey, keyHash: string): Promise<void> {
  await db
    .prepare("INSERT INTO api_keys (id, name, key_hash, created_at) VALUES (?, ?, ?, ?)")
    .bind(key.id, key.name, keyHash, key.created_at)
    .run();
}

export async function apiKeyByHash(db: D1Database, keyHash: string, now: number): Promise<ApiKey | null> {
  const key = await db
    .prepare("SELECT id, name, created_at, last_used_at FROM api_keys WHERE key_hash = ?")
    .bind(keyHash)
    .first<ApiKey>();
  if (key) await db.prepare("UPDATE api_keys SET last_used_at = ? WHERE id = ?").bind(now, key.id).run();
  return key;
}

export async function listApiKeys(db: D1Database): Promise<ApiKey[]> {
  const { results } = await db
    .prepare("SELECT id, name, created_at, last_used_at FROM api_keys ORDER BY created_at")
    .all<ApiKey>();
  return results;
}

export async function deleteApiKey(db: D1Database, id: string): Promise<boolean> {
  const result = await db.prepare("DELETE FROM api_keys WHERE id = ?").bind(id).run();
  return result.meta.changes === 1;
}
