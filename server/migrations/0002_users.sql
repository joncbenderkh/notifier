-- SPDX-License-Identifier: GPL-3.0-or-later
CREATE TABLE users (
  id           TEXT PRIMARY KEY,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_seen_at INTEGER NOT NULL,
  disabled_at  INTEGER
);

CREATE TABLE invites (
  code_hash  TEXT PRIMARY KEY,
  created_by TEXT NOT NULL, -- a user ID, or '' for the operator
  expires_at INTEGER NOT NULL
);

-- Everything that predates users belongs to one owner, and so does anything
-- the previous Worker version writes between this migration and the new
-- code going live (hence the column defaults). Its token hash is random, so
-- nobody can sign in as it until the operator reissues its token
-- (POST /v1/users/usr_owner/token).
INSERT INTO users (id, name, token_hash, created_at, last_seen_at)
VALUES ('usr_owner', 'owner', lower(hex(randomblob(32))), unixepoch(), unixepoch());

ALTER TABLE devices ADD COLUMN user_id TEXT NOT NULL DEFAULT 'usr_owner';
ALTER TABLE api_keys ADD COLUMN user_id TEXT NOT NULL DEFAULT 'usr_owner';
ALTER TABLE pairings ADD COLUMN user_id TEXT NOT NULL DEFAULT 'usr_owner';

CREATE INDEX devices_user_id ON devices (user_id);
CREATE INDEX api_keys_user_id ON api_keys (user_id);
CREATE INDEX invites_created_by ON invites (created_by);
