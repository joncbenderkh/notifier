-- SPDX-License-Identifier: GPL-3.0-or-later
-- Whether other members' devices report receipts on a channel (issue #33):
-- 'off', 'optional' (members who turn them on) or 'required'. No CHECK, as
-- with hooks.format, so a new setting needs no table rebuild; the Worker
-- validates it.
ALTER TABLE channels ADD COLUMN receipts TEXT NOT NULL DEFAULT 'off';

-- A member who turned receipts on for a channel where the owner offers
-- them; where the owner requires them, they apply regardless.
ALTER TABLE channel_members ADD COLUMN receipts_accepted INTEGER NOT NULL DEFAULT 0;
