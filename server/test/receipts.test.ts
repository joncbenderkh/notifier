// SPDX-License-Identifier: GPL-3.0-or-later
import { env, runDurableObjectAlarm, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { ReceiptBox } from "../src/receipts";

let messages = 0;
const newBox = () => env.RECEIPT_BOX.get(env.RECEIPT_BOX.idFromName(`msg_${++messages}`));
const NOW = 1_790_000_000;
const tracked = {
  userId: "usr_a",
  expiresAt: NOW + 86_400,
  recipients: [
    { device_id: "dev_a", name: "phone", own: true },
    { device_id: "dev_b", own: false },
  ],
};

describe("ReceiptBox", () => {
  it("wakes every waiting poll when a device opens the message", async () => {
    const box = newBox();
    await box.open(tracked, NOW);
    const polls = [box.wait("usr_a", 30), box.wait("usr_a", 30)];
    expect(await box.mark("dev_b", "opened", NOW + 5)).toBe(true);
    const opened = {
      status: "opened",
      devices: [{ device_id: "dev_a", name: "phone" }],
      members: { tracked: 1, shown: 1, opened: 1 },
    };
    expect(await Promise.all(polls)).toEqual([opened, opened]);
  });

  it("answers pending after the wait", async () => {
    const box = newBox();
    await box.open(tracked, NOW);
    await box.mark("dev_a", "shown", NOW);
    expect(await box.wait("usr_a", 0)).toEqual({
      status: "pending",
      devices: [{ device_id: "dev_a", name: "phone", shown_at: NOW }],
      members: { tracked: 1, shown: 0, opened: 0 },
    });
  });

  it("knows only its sender and recipients", async () => {
    const box = newBox();
    expect(await box.wait("usr_a", 0)).toEqual({ status: "missing" });
    expect(await box.mark("dev_a", "shown", NOW)).toBe(false);
    await box.open(tracked, NOW);
    expect(await box.wait("usr_b", 0)).toEqual({ status: "missing" });
    expect(await box.mark("dev_c", "shown", NOW)).toBe(false);
  });

  it("answers missing to a poll that outlived the message", async () => {
    const box = newBox();
    await box.open(tracked, NOW);
    await box.mark("dev_a", "shown", NOW);
    // Far longer than the test may take: the alarm must wake it.
    const poll = box.wait("usr_a", 30);
    expect(await runDurableObjectAlarm(box)).toBe(true);
    expect(await poll).toEqual({ status: "missing" });
  });

  it("forgets the message when its alarm fires", async () => {
    const box = newBox();
    await box.open(tracked, NOW);
    const alarm = await runInDurableObject(box, (_: ReceiptBox, state) => state.storage.getAlarm());
    expect(alarm).toBeGreaterThan(Date.now() + 86_400_000);
    expect(await runDurableObjectAlarm(box)).toBe(true);
    expect(await box.wait("usr_a", 0)).toEqual({ status: "missing" });
  });
});
