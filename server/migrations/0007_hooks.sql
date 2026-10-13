-- SPDX-License-Identifier: GPL-3.0-or-later
-- Inbound webhooks (issue #34): a monitoring tool posts its own alert
-- format to the hook's secret URL, and the alert is published to the
-- hook's channel. format is checked in code, not here, so a new format
-- needs no table rebuild.
CREATE TABLE hooks (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL,
  channel_id   TEXT NOT NULL,
  name         TEXT NOT NULL,
  format       TEXT NOT NULL,
  secret_hash  TEXT NOT NULL UNIQUE,
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,
  UNIQUE (user_id, name)
);
