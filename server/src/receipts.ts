// SPDX-License-Identifier: GPL-3.0-or-later
//
// Receipts for a message the sender asked to track (`notify send
// --wait-seen`). One Durable Object per message holds who it went to and
// when each device showed and opened it. The sender long-polls it until a
// device opens the message; other members' devices are theirs to know, so
// the sender sees only counts for them.

import { DurableObject } from "cloudflare:workers";

/** How long a box outlives its messages, for a poll to collect the last receipts. */
export const RECEIPT_GRACE_SECONDS = 60 * 60;

export interface Recipient {
  device_id: string;
  /** The device's name, for the sender's own devices. */
  name?: string;
  own: boolean;
}

export interface Tracked {
  userId: string;
  /** Unix seconds: when the message leaves the queues. */
  expiresAt: number;
  recipients: Recipient[];
}

export type ReceiptState = "shown" | "opened";

interface Times {
  shown_at?: number;
  opened_at?: number;
}

export interface DeviceReceipt extends Times {
  device_id: string;
  name: string;
}

export type Receipts =
  | { status: "missing" }
  | {
      /** "opened" once any device opened the message. */
      status: "pending" | "opened";
      devices: DeviceReceipt[];
      /** Other members' devices: how many report receipts, and how many did. */
      members: { tracked: number; shown: number; opened: number };
    };

/** What the API needs from a message's box; `now` is the caller's clock, in Unix seconds. */
export interface ReceiptBook {
  open(tracked: Tracked, now: number): Promise<void>;
  /** False when the message is not tracked or [deviceId] did not receive it. */
  mark(deviceId: string, state: ReceiptState, now: number): Promise<boolean>;
  wait(userId: string, seconds: number): Promise<Receipts>;
}

const TRACKED_KEY = "tracked";
const TIMES_KEY = "times";

export class ReceiptBox extends DurableObject<Env> implements ReceiptBook {
  /** Waiting polls; [gone] when the box was deleted. */
  #waiters = new Set<(gone?: boolean) => void>();

  async open(tracked: Tracked, now: number): Promise<void> {
    this.ctx.storage.kv.put(TRACKED_KEY, tracked);
    await this.ctx.storage.setAlarm(Date.now() + (tracked.expiresAt - now + RECEIPT_GRACE_SECONDS) * 1000);
  }

  /** Records [state] for [deviceId]; the first time of each stays, and opening implies showing. */
  mark(deviceId: string, state: ReceiptState, now: number): Promise<boolean> {
    const t = this.ctx.storage.kv.get<Tracked>(TRACKED_KEY);
    if (!t?.recipients.some((r) => r.device_id === deviceId)) return Promise.resolve(false);
    const all = this.ctx.storage.kv.get<Record<string, Times>>(TIMES_KEY) ?? {};
    const times = { ...all[deviceId] };
    // Already recorded: nothing to write, nor anyone to wake.
    if (times.shown_at !== undefined && (state === "shown" || times.opened_at !== undefined)) return Promise.resolve(true);
    times.shown_at ??= now;
    if (state === "opened") times.opened_at ??= now;
    this.ctx.storage.kv.put(TIMES_KEY, { ...all, [deviceId]: times });
    for (const wake of [...this.#waiters]) wake();
    return Promise.resolve(true);
  }

  /** The receipts for [userId], after waiting up to [seconds] for a device to open the message. */
  async wait(userId: string, seconds: number): Promise<Receipts> {
    const t = this.ctx.storage.kv.get<Tracked>(TRACKED_KEY);
    if (!t || t.userId !== userId) return { status: "missing" };
    if (!this.#opened() && seconds > 0) {
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer);
          this.#waiters.delete(check);
          resolve();
        };
        // Any receipt wakes the poll; only an opening, or the box's deletion, ends it.
        const check = (gone?: boolean) => {
          if (gone || this.#opened()) done();
        };
        const timer = setTimeout(done, seconds * 1000);
        this.#waiters.add(check);
      });
      // Deleted while waiting: gone, as for a poll that came later.
      if (!this.ctx.storage.kv.get(TRACKED_KEY)) return { status: "missing" };
    }
    const all = this.ctx.storage.kv.get<Record<string, Times>>(TIMES_KEY) ?? {};
    const devices: DeviceReceipt[] = [];
    const members = { tracked: 0, shown: 0, opened: 0 };
    for (const r of t.recipients) {
      const times = all[r.device_id] ?? {};
      if (r.own) {
        devices.push({ device_id: r.device_id, name: r.name ?? r.device_id, ...times });
      } else {
        members.tracked++;
        if (times.shown_at !== undefined) members.shown++;
        if (times.opened_at !== undefined) members.opened++;
      }
    }
    return { status: this.#opened() ? "opened" : "pending", devices, members };
  }

  #opened(): boolean {
    const all = this.ctx.storage.kv.get<Record<string, Times>>(TIMES_KEY) ?? {};
    return Object.values(all).some((t) => t.opened_at !== undefined);
  }

  override async alarm(): Promise<void> {
    await this.ctx.storage.deleteAll();
    for (const wake of [...this.#waiters]) wake(true);
  }
}
