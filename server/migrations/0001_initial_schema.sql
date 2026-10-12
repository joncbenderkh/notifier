-- SPDX-License-Identifier: GPL-3.0-or-later
CREATE TABLE devices (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  seal_key     TEXT NOT NULL, -- base64, 32 bytes
  fcm_token    TEXT NOT NULL DEFAULT '',
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL
);

CREATE TABLE api_keys (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  key_hash     TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER
);

CREATE TABLE pairings (
  code_hash  TEXT PRIMARY KEY,
  expires_at INTEGER NOT NULL
);
