// SPDX-License-Identifier: GPL-3.0-or-later
import { env, runDurableObjectAlarm, runInDurableObject } from "cloudflare:test";
import { describe, expect, it, vi } from "vitest";
import { CLOSE_REPLACED, CLOSE_UNPAIRED, MAX_QUEUED_MESSAGES } from "../src/stream";
import type { DeviceStream, StreamMessage } from "../src/stream";

let devices = 0;
const newStream = () => env.DEVICE_STREAM.get(env.DEVICE_STREAM.idFromName(`dev_${++devices}`));
type Stub = ReturnType<typeof newStream>;

const nowSeconds = () => Math.floor(Date.now() / 1000);
const message = (id: string, expiresAt = nowSeconds() + 3600): StreamMessage => ({
  id,
  frame: JSON.stringify({ type: "message", id }),
  expiresAt,
});
const ids = (frames: string[]) => frames.map((f) => (JSON.parse(f) as { id: string }).id);

async function connect(stub: Stub) {
  const res = await stub.fetch("https://notifier.test/v1/device/stream", { headers: { upgrade: "websocket" } });
  expect(res.status).toBe(101);
  const ws = res.webSocket!;
  const frames: string[] = [];
  const closed = new Promise<CloseEvent>((resolve) => ws.addEventListener("close", resolve));
  ws.addEventListener("message", (e) => {
    frames.push(e.data as string);
  });
  ws.accept();
  return { ws, frames, closed };
}

const queued = (stub: Stub) =>
  runInDurableObject(stub, (_: DeviceStream, state) =>
    state.storage.sql
      .exec<{ id: string }>("SELECT id FROM queue ORDER BY seq")
      .toArray()
      .map((r) => r.id),
  );

describe("DeviceStream", () => {
  it("queues while offline and replays in order on connect", async () => {
    const stub = newStream();
    expect(await stub.deliver(message("msg_1"))).toBe("queued");
    expect(await stub.deliver(message("msg_2"))).toBe("queued");
    expect(await stub.deliver(message("msg_1"))).toBe("queued");

    const { frames } = await connect(stub);
    await vi.waitFor(() => expect(ids(frames)).toEqual(["msg_1", "msg_2"]));
  });

  it("sends live while connected and forgets what the device acks", async () => {
    const stub = newStream();
    const { ws, frames } = await connect(stub);
    expect(await stub.deliver(message("msg_1"))).toBe("sent");
    await vi.waitFor(() => expect(ids(frames)).toEqual(["msg_1"]));

    ws.send("not json");
    ws.send(JSON.stringify({ ack: 7 }));
    ws.send("ping");
    await vi.waitFor(() => expect(frames).toContain("pong"));
    expect(await queued(stub)).toEqual(["msg_1"]);

    ws.send(JSON.stringify({ ack: "msg_1" }));
    await vi.waitFor(async () => expect(await queued(stub)).toEqual([]));
  });

  it("lets a newer connection replace the old one", async () => {
    const stub = newStream();
    const first = await connect(stub);
    const second = await connect(stub);
    expect((await first.closed).code).toBe(CLOSE_REPLACED);
    expect(await stub.deliver(message("msg_1"))).toBe("sent");
    await vi.waitFor(() => expect(ids(second.frames)).toEqual(["msg_1"]));
    expect(first.frames).toEqual([]);
  });

  it("keeps only the newest messages", async () => {
    const stub = newStream();
    for (let i = 1; i <= MAX_QUEUED_MESSAGES + 5; i++) await stub.deliver(message(`msg_${i}`));
    const left = await queued(stub);
    expect(left).toHaveLength(MAX_QUEUED_MESSAGES);
    expect(left[0]).toBe("msg_6");
  });

  it("drops expired messages", async () => {
    const stub = newStream();
    await stub.deliver(message("msg_old", nowSeconds() - 1));
    await stub.deliver(message("msg_new"));
    await runDurableObjectAlarm(stub);
    expect(await queued(stub)).toEqual(["msg_new"]);
    const { frames } = await connect(stub);
    await vi.waitFor(() => expect(ids(frames)).toEqual(["msg_new"]));
  });

  it("disconnects and forgets an unpaired device", async () => {
    const stub = newStream();
    await stub.deliver(message("msg_1"));
    const { closed } = await connect(stub);
    await stub.close();
    expect((await closed).code).toBe(CLOSE_UNPAIRED);
    const left = await runInDurableObject(stub, async (_: DeviceStream, state) => ({
      tables: state.storage.sql.exec("SELECT name FROM sqlite_master WHERE name = 'queue'").toArray(),
      alarm: await state.storage.getAlarm(),
    }));
    expect(left).toEqual({ tables: [], alarm: null });
    const again = await stub.fetch("https://notifier.test/v1/device/stream", { headers: { upgrade: "websocket" } });
    expect(again.status).toBe(404);
  });

  it("answers a close the device starts", async () => {
    const stub = newStream();
    const { ws, closed } = await connect(stub);
    ws.close(1000, "bye");
    expect((await closed).code).toBe(1000);
  });

  it("refuses requests that are not upgrades", async () => {
    const res = await newStream().fetch("https://notifier.test/v1/device/stream");
    expect(res.status).toBe(426);
  });
});
