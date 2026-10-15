// SPDX-License-Identifier: GPL-3.0-or-later
import type { Deps } from "./app";
import { createApp } from "./app";
import { heartbeatAlerts } from "./heartbeats";
import { senderFor } from "./push";

export { ReceiptBox } from "./receipts";
export { ReplyBox } from "./replies";
export { DeviceStream } from "./stream";

const deps: Deps = {
  sender: senderFor,
  stream: (env, deviceId) => env.DEVICE_STREAM.get(env.DEVICE_STREAM.idFromName(deviceId)),
  replies: (env, messageId) => env.REPLY_BOX.get(env.REPLY_BOX.idFromName(messageId)),
  receipts: (env, messageId) => env.RECEIPT_BOX.get(env.RECEIPT_BOX.idFromName(messageId)),
  now: () => Math.floor(Date.now() / 1000),
};
const app = createApp(deps);
const alerts = heartbeatAlerts(deps);

export default {
  fetch: app.fetch,
  // The cron trigger (wrangler.jsonc): every minute, alert overdue heartbeats.
  scheduled(_controller, env, ctx) {
    ctx.waitUntil(alerts.check(env));
  },
} satisfies ExportedHandler<Env>;
