// SPDX-License-Identifier: GPL-3.0-or-later
import { createApp } from "./app";
import { senderFor } from "./push";

export default createApp({ sender: senderFor, now: () => Math.floor(Date.now() / 1000) });
