-- SPDX-License-Identifier: GPL-3.0-or-later
-- Dead-man's switches: a job pings its heartbeat's secret URL each run, and
-- the cron trigger alerts the channel once a heartbeat passes due_at
-- (last ping, or creation, plus interval and grace) without one.
CREATE TABLE heartbeats (
  id           TEXT PRIMARY KEY,
  user_id      TEXT NOT NULL,
  channel_id   TEXT NOT NULL,
  name         TEXT NOT NULL,
  secret_hash  TEXT NOT NULL UNIQUE,
  interval_s   INTEGER NOT NULL,
  grace_s      INTEGER NOT NULL,
  created_at   INTEGER NOT NULL,
  last_ping_at INTEGER,
  due_at       INTEGER NOT NULL,
  -- up: waiting for its next ping; down: missed one and alerted; failed:
  -- the job reported a failure. The cron trigger checks up and failed ones.
  state        TEXT NOT NULL DEFAULT 'up' CHECK (state IN ('up', 'down', 'failed')),
  -- When it went down or failed, to say for how long on recovery.
  changed_at   INTEGER NOT NULL,
  -- A recovery or failure notice that reached no device yet, as JSON
  -- {title, body, priority}; the cron trigger retries it from
  -- notice_due_at, which also leases it to one attempt at a time.
  pending_notice TEXT,
  notice_due_at  INTEGER,
  UNIQUE (user_id, name)
);

CREATE INDEX heartbeats_due ON heartbeats (state, due_at);
