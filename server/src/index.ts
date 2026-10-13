// SPDX-License-Identifier: GPL-3.0-or-later
import { createApp } from "./app";
import { senderFor } from "./push";

export { DeviceStream } from "./stream";

export default createApp({
  sender: senderFor,
  stream: (env, deviceId) => env.DEVICE_STREAM.get(env.DEVICE_STREAM.idFromName(deviceId)),
  now: () => Math.floor(Date.now() / 1000),
});
