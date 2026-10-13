-- SPDX-License-Identifier: GPL-3.0-or-later
-- Counts the pings that announce something (failures, and pings that end a
-- failure or outage), so a request that stalled before queueing its notice
-- cannot replace the notice of a newer one.
ALTER TABLE heartbeats ADD COLUMN ping_seq INTEGER NOT NULL DEFAULT 0;
