-- SPDX-License-Identifier: GPL-3.0-or-later
-- How a device receives: FCM (Android) or the WebSocket stream (the Linux
-- daemon, which FCM cannot reach). Rows the previous Worker writes during a
-- deploy take the default.
ALTER TABLE devices ADD COLUMN transport TEXT NOT NULL DEFAULT 'fcm' CHECK (transport IN ('fcm', 'ws'));
