-- SPDX-License-Identifier: GPL-3.0-or-later
CREATE TABLE channels (
  id         TEXT PRIMARY KEY,
  owner_id   TEXT NOT NULL,
  name       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (owner_id, name)
);

-- The owner is a member too, so "channels a user can receive" is one query.
CREATE TABLE channel_members (
  channel_id TEXT NOT NULL,
  user_id    TEXT NOT NULL,
  joined_at  INTEGER NOT NULL,
  PRIMARY KEY (channel_id, user_id)
);
CREATE INDEX channel_members_user_id ON channel_members (user_id);

CREATE TABLE subscriptions (
  device_id  TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  PRIMARY KEY (device_id, channel_id)
);
CREATE INDEX subscriptions_channel_id ON subscriptions (channel_id);

CREATE TABLE channel_invites (
  code_hash  TEXT PRIMARY KEY,
  channel_id TEXT NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX channel_invites_channel_id ON channel_invites (channel_id);

-- A key with no rows here may publish anywhere its user may.
CREATE TABLE api_key_channels (
  key_id     TEXT NOT NULL,
  channel_id TEXT NOT NULL,
  PRIMARY KEY (key_id, channel_id)
);

-- Every user gets a "default" channel that their devices subscribe to, so a
-- send without channels still reaches all of them.
INSERT INTO channels (id, owner_id, name, created_at)
SELECT 'ch_default_' || id, id, 'default', unixepoch() FROM users;
INSERT INTO channel_members (channel_id, user_id, joined_at)
SELECT 'ch_default_' || id, id, unixepoch() FROM users;
INSERT INTO subscriptions (device_id, channel_id)
SELECT id, 'ch_default_' || user_id FROM devices;

-- Keep that true for every later row, including those the previous Worker
-- writes between this migration and the deploy of the code that reads it.
CREATE TRIGGER users_default_channel AFTER INSERT ON users
BEGIN
  INSERT OR IGNORE INTO channels (id, owner_id, name, created_at)
  VALUES ('ch_default_' || NEW.id, NEW.id, 'default', NEW.created_at);
  INSERT OR IGNORE INTO channel_members (channel_id, user_id, joined_at)
  VALUES ('ch_default_' || NEW.id, NEW.id, NEW.created_at);
END;

CREATE TRIGGER devices_default_subscription AFTER INSERT ON devices
BEGIN
  INSERT OR IGNORE INTO subscriptions (device_id, channel_id)
  VALUES (NEW.id, 'ch_default_' || NEW.user_id);
END;
