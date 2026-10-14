// SPDX-License-Identifier: GPL-3.0-or-later
import { env, runDurableObjectAlarm, runInDurableObject } from "cloudflare:test";
import { describe, expect, it } from "vitest";
import type { ReplyBox } from "../src/replies";

let questions = 0;
const newBox = () => env.REPLY_BOX.get(env.REPLY_BOX.idFromName(`msg_${++questions}`));
const NOW = 1_790_000_000;
const question = { userId: "usr_a", expiresAt: NOW + 600, responders: [{ device_id: "dev_a", label: "phone" }] };

describe("ReplyBox", () => {
  it("wakes every waiting poll with the first answer", async () => {
    const box = newBox();
    await box.open(question, NOW);
    const polls = [box.wait("usr_a", 30, NOW), box.wait("usr_a", 30, NOW)];
    expect(await box.answer("dev_a", "n1", "c1", NOW + 5)).toBe("ok");
    expect(await box.answer("dev_a", "n2", "c2", NOW + 6)).toBe("answered");
    const answered = { status: "answered", by: "phone", replied_at: NOW + 5, n: "n1", c: "c1" };
    expect(await Promise.all(polls)).toEqual([answered, answered]);
    expect(await box.wait("usr_a", 30, NOW + 7)).toEqual(answered);
  });

  it("answers pending after the wait, and expired at the deadline", async () => {
    const box = newBox();
    await box.open({ ...question, expiresAt: NOW + 1 }, NOW);
    expect(await box.wait("usr_a", 0, NOW)).toEqual({ status: "pending" });
    expect(await box.wait("usr_a", 30, NOW)).toEqual({ status: "expired" });
    expect(await box.answer("dev_a", "n", "c", NOW + 1)).toBe("expired");
  });

  it("knows only its asker and responders", async () => {
    const box = newBox();
    expect(await box.wait("usr_a", 0, NOW)).toEqual({ status: "missing" });
    expect(await box.answer("dev_a", "n", "c", NOW)).toBe("forbidden");
    await box.open(question, NOW);
    expect(await box.wait("usr_b", 0, NOW)).toEqual({ status: "missing" });
    expect(await box.answer("dev_b", "n", "c", NOW)).toBe("forbidden");
  });

  it("forgets the question when its alarm fires", async () => {
    const box = newBox();
    await box.open(question, NOW);
    const alarm = await runInDurableObject(box, (_: ReplyBox, state) => state.storage.getAlarm());
    expect(alarm).toBeGreaterThan(Date.now() + 600_000);
    expect(await runDurableObjectAlarm(box)).toBe(true);
    expect(await box.wait("usr_a", 0, NOW)).toEqual({ status: "missing" });
  });
});
