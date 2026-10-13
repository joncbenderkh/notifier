// SPDX-License-Identifier: GPL-3.0-or-later
//
// Sealing a message for each device and handing it to its transport, shared
// by sends through the API, heartbeat alerts and inbound webhooks.

import type { Delivery, Deps, Payload } from "./app";
import { fromBase64, toBase64 } from "./encoding";
import { UnregisteredError } from "./push";
import { seal } from "./seal";
import * as store from "./store";
import type { Channel, Device } from "./store";

export const MESSAGE_TTL_SECONDS = 24 * 60 * 60;
const ENVELOPE_VERSION = "1";

/** How a member sees a channel: bare when they own it, "owner/name" otherwise. */
export const channelLabel = (ch: Channel, viewer: string) =>
  ch.owner_id === viewer ? ch.name : `${ch.owner_name}/${ch.name}`;

/** A device to send to, and the channel it was reached through, if any. */
export interface Target {
  device: Device;
  channel?: Channel;
}

/**
 * Adds each subscriber of [channels] to [targets], once: a device keeps the
 * first requested channel it subscribes to.
 */
export async function addSubscribers(db: D1Database, channels: readonly Channel[], targets: Map<string, Target>) {
  for (const ch of channels) {
    for (const d of await store.subscribers(db, ch.id)) {
      if (!targets.has(d.id)) targets.set(d.id, { device: d, channel: ch });
    }
  }
}

export interface Published {
  /** The sender's own devices. */
  deliveries: Delivery[];
  /** Other members' devices are theirs to know: only counts by status. */
  members: Partial<Record<Delivery["status"], number>>;
}

export function publisher(deps: Deps) {
  async function deliver(env: Env, d: Device, p: Payload): Promise<Delivery> {
    if (d.transport === "fcm" && d.fcm_token === "") return { device_id: d.id, status: "unreachable" };
    try {
      const envelope = await seal(fromBase64(d.seal_key), d.id, new TextEncoder().encode(JSON.stringify(p)));
      const data = { v: ENVELOPE_VERSION, n: toBase64(envelope.nonce), c: toBase64(envelope.ciphertext) };
      if (d.transport === "ws") {
        const status = await deps.stream(env, d.id).deliver({
          id: p.id,
          frame: JSON.stringify({ type: "message", id: p.id, ...data }),
          expiresAt: deps.now() + MESSAGE_TTL_SECONDS,
        });
        return { device_id: d.id, status };
      }
      await deps.sender(env).send({ token: d.fcm_token, high: p.priority === "high", ttlSeconds: MESSAGE_TTL_SECONDS, data });
      return { device_id: d.id, status: "sent" };
    } catch (err) {
      if (err instanceof UnregisteredError) {
        console.warn("push token unregistered; clearing", d.id);
        // Other devices may already have been sent to; never turn this into a 500.
        await store.setFcmToken(env.DB, d.id, "").catch((dbErr: unknown) => {
          console.error("clear push token failed", d.id, dbErr);
        });
        return { device_id: d.id, status: "unreachable" };
      }
      console.error("push send failed", d.id, err);
      return { device_id: d.id, status: "failed", error: "push transport error" };
    }
  }

  /** Sends [base] from [userId] to every target, each labelled with its channel as its owner names it. */
  async function publish(env: Env, userId: string, targets: Iterable<Target>, base: Payload): Promise<Published> {
    // A failed warm-up (including a missing or invalid FCM_SERVICE_ACCOUNT)
    // is not fatal: each send retries and reports per device.
    try {
      await deps.sender(env).prepare?.();
    } catch (err) {
      console.error("push prepare failed", err);
    }
    const all = await Promise.all(
      [...targets].map(async ({ device: d, channel: ch }) => ({
        own: d.user_id === userId,
        delivery: await deliver(env, d, ch ? { ...base, channel: channelLabel(ch, d.user_id), channel_id: ch.id } : base),
      })),
    );
    const deliveries = all.filter((x) => x.own).map((x) => x.delivery);
    const members: Published["members"] = {};
    for (const { own, delivery } of all) {
      if (!own) members[delivery.status] = (members[delivery.status] ?? 0) + 1;
    }
    return { deliveries, members };
  }

  /**
   * Publishes [p] from [userId] to one of their channels, or to their
   * default channel if it has been deleted (its heartbeats and hooks moved
   * there). False when there were devices to reach but none took the
   * message, so a retry could still succeed; a channel nobody listens to
   * counts as done.
   */
  async function publishToChannel(env: Env, userId: string, channelId: string, p: Payload): Promise<boolean> {
    const ch =
      (await store.ownedChannel(env.DB, userId, channelId)) ??
      (await store.ownedChannel(env.DB, userId, store.defaultChannelId(userId)));
    if (!ch) {
      console.warn("channel missing", userId, channelId);
      return false;
    }
    const targets = new Map<string, Target>();
    await addSubscribers(env.DB, [ch], targets);
    if (targets.size === 0) {
      // Nobody listens, unless the channel was deleted during the lookup.
      if (await store.ownedChannel(env.DB, userId, ch.id)) return true;
      const fallback = await store.ownedChannel(env.DB, userId, store.defaultChannelId(userId));
      if (fallback) await addSubscribers(env.DB, [fallback], targets);
      if (targets.size === 0) return fallback !== null;
    }
    const { deliveries, members } = await publish(env, userId, targets.values(), p);
    const taken = (s: Delivery["status"]) => s === "sent" || s === "queued";
    return deliveries.some((d) => taken(d.status)) || (members.sent ?? 0) + (members.queued ?? 0) > 0;
  }

  return { deliver, publish, publishToChannel };
}
