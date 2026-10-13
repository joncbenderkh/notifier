// SPDX-License-Identifier: GPL-3.0-or-later
//
// Dead-man's switches (issue #29). A job pings its heartbeat's secret URL
// on every run; the cron trigger calls checkHeartbeats every minute and
// alerts the heartbeat's channel, once, when a check-in is overdue. A later
// ping announces the recovery.

import type { Delivery, Deps, Payload } from "./app";
import { addSubscribers, publisher } from "./publish";
import type { Target } from "./publish";
import * as store from "./store";
import type { Heartbeat } from "./store";
import { newId } from "./token";

export const HEARTBEAT_SECRET_PREFIX = "nh_";
export const MAX_HEARTBEATS_PER_USER = 20;
export const MIN_INTERVAL_SECONDS = 60;
export const MAX_INTERVAL_SECONDS = 30 * 24 * 60 * 60;
export const MIN_GRACE_SECONDS = 60;
export const MAX_GRACE_SECONDS = 24 * 60 * 60;
export const DEFAULT_GRACE_SECONDS = 5 * 60;
/** Heartbeats alerted per cron run; the rest wait a minute for the next. */
const CHECK_BATCH = 100;
/** How long an alert that reached no device waits before the next try. */
const RETRY_SECONDS = 60;
/** How long one attempt holds a pending notice before another may try it. */
const LEASE_SECONDS = 5 * 60;

/** A coarse, time-zone-free length of time: "45 s", "5 min", "3 h 10 min", "2 d 4 h". */
export function duration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  const d = Math.floor(s / 86400);
  const h = Math.floor((s % 86400) / 3600);
  const m = Math.floor((s % 3600) / 60);
  if (d > 0) return h > 0 ? `${d} d ${h} h` : `${d} d`;
  if (h > 0) return m > 0 ? `${h} h ${m} min` : `${h} h`;
  if (m > 0) return `${m} min`;
  return `${s} s`;
}

/** A recovery or failure notice, kept as JSON in pending_notice until delivered. */
interface Notice {
  /** Unique, so two notices with the same text are still told apart. */
  id: string;
  /** A missed check-in supersedes a pending recovery, but not a failure's details. */
  kind: "recovered" | "failed";
  title: string;
  body: string;
  priority: Payload["priority"];
}

export function heartbeatAlerts(deps: Deps) {
  const { publish } = publisher(deps);

  /**
   * Publishes to the heartbeat's channel. False when there were devices to
   * reach but none took the message, so a retry could still succeed; a
   * channel nobody listens to counts as done.
   */
  async function notify(env: Env, hb: Heartbeat, title: string, body: string, priority: Payload["priority"]): Promise<boolean> {
    // A channel deleted since [hb] was read: its heartbeats moved to default.
    const ch =
      (await store.ownedChannel(env.DB, hb.user_id, hb.channel_id)) ??
      (await store.ownedChannel(env.DB, hb.user_id, store.defaultChannelId(hb.user_id)));
    if (!ch) {
      console.warn("heartbeat channel missing", hb.id, hb.channel_id);
      return false;
    }
    const targets = new Map<string, Target>();
    await addSubscribers(env.DB, [ch], targets);
    if (targets.size === 0) {
      // Nobody listens, unless the channel was deleted during the lookup.
      if (await store.ownedChannel(env.DB, hb.user_id, ch.id)) return true;
      const fallback = await store.ownedChannel(env.DB, hb.user_id, store.defaultChannelId(hb.user_id));
      if (fallback) await addSubscribers(env.DB, [fallback], targets);
      if (targets.size === 0) return fallback !== null;
    }
    const p: Payload = { id: newId("msg_"), title, priority, sent_at: deps.now() };
    if (body !== "") p.body = body;
    const { deliveries, members } = await publish(env, hb.user_id, targets.values(), p);
    const taken = (s: Delivery["status"]) => s === "sent" || s === "queued";
    return deliveries.some((d) => taken(d.status)) || (members.sent ?? 0) + (members.queued ?? 0) > 0;
  }

  /**
   * Sends a pending notice and clears it, or schedules its next try. The
   * caller holds its lease, so no other attempt sends it meanwhile.
   */
  async function attempt(env: Env, hb: Heartbeat, notice: string): Promise<void> {
    let delivered = false;
    try {
      const n = JSON.parse(notice) as Notice;
      delivered = await notify(env, hb, n.title, n.body, n.priority);
    } catch (err) {
      console.error("heartbeat notice failed", hb.id, err);
    }
    if (delivered) await store.clearPendingNotice(env.DB, hb.id, notice);
    else await store.deferNotice(env.DB, hb.id, notice, deps.now() + RETRY_SECONDS);
  }

  /**
   * Queues a notice for the ping numbered [seq], replacing any older one,
   * unless a later ping has announced something or changed the state since. Unless [later], it is leased and tried at
   * once; whatever reaches no device the cron trigger retries.
   */
  async function keepOrSend(env: Env, hb: Heartbeat, seq: number, notice: Omit<Notice, "id">, later: boolean): Promise<void> {
    const raw = JSON.stringify({ id: newId("ntc_"), ...notice });
    const state = notice.kind === "recovered" ? "up" : "failed";
    if (!(await store.setPendingNotice(env.DB, hb.id, state, seq, raw, deps.now() + (later ? 0 : LEASE_SECONDS)))) return;
    if (!later) await attempt(env, hb, raw);
  }

  return {
    missed(env: Env, hb: Heartbeat) {
      const now = deps.now();
      const body =
        hb.last_ping_at === null
          ? `No check-in since it was created ${duration(now - hb.created_at)} ago; expected every ${duration(hb.interval_s)}.`
          : `Last check-in ${duration(now - hb.last_ping_at)} ago; expected every ${duration(hb.interval_s)}.`;
      return notify(env, hb, `${hb.name}: missed check-in`, body, "high");
    },

    /**
     * [left] is the state the ping numbered [seq] ended, and when it began.
     * [later] keeps the notice for the cron trigger without trying now.
     */
    recovered(env: Env, hb: Heartbeat, seq: number, left: Pick<Heartbeat, "state" | "changed_at">, later = false) {
      const how = left.state === "failed" ? "Failed" : "Down";
      const body = `${how} for ${duration(deps.now() - left.changed_at)}.`;
      return keepOrSend(env, hb, seq, { kind: "recovered", title: `${hb.name}: checked in again`, body, priority: "default" }, later);
    },

    /** For the ping numbered [seq]; [later] keeps the notice for the cron trigger without trying now. */
    failed(env: Env, hb: Heartbeat, seq: number, details: string, later = false) {
      const notice: Omit<Notice, "id"> = { kind: "failed", title: `${hb.name}: failed`, body: details || "The job reported a failure.", priority: "high" };
      return keepOrSend(env, hb, seq, notice, later);
    },

    /**
     * Alerts every overdue heartbeat once. An alert that reached no device is
     * undone and retried by the next run; a failure for one heartbeat does
     * not stop the rest.
     */
    async check(env: Env) {
      const now = deps.now();
      // One failing query must not skip the other loop for this minute.
      const due = await store.dueHeartbeats(env.DB, now, CHECK_BATCH).catch((err: unknown) => {
        console.error("heartbeat check failed", err);
        return [];
      });
      for (const hb of due) {
        const dropped = await store.markDown(env.DB, hb.id, now).catch(() => null);
        if (!dropped) continue;
        let delivered = false;
        try {
          delivered = await this.missed(env, hb);
        } catch (err) {
          console.error("heartbeat alert failed", hb.id, err);
        }
        if (!delivered) {
          await store.unmarkDown(env.DB, hb, dropped, now, now + RETRY_SECONDS).catch((err: unknown) => console.error("heartbeat retry not scheduled", hb.id, err));
        }
      }
      const pending = await store.pendingNotices(env.DB, now, CHECK_BATCH).catch((err: unknown) => {
        console.error("heartbeat notice check failed", err);
        return [];
      });
      for (const hb of pending) {
        const notice = hb.pending_notice!;
        try {
          if (await store.deferNotice(env.DB, hb.id, notice, now + LEASE_SECONDS, now)) await attempt(env, hb, notice);
        } catch (err) {
          console.error("heartbeat notice retry failed", hb.id, err);
        }
      }
    },
  };
}
