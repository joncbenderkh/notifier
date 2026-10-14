// SPDX-License-Identifier: GPL-3.0-or-later
//
// Replies to a question (`notify ask`). One Durable Object per question
// holds which devices may answer, until when, and the first answer, sealed
// by the device under a key only the asker and those devices hold. The
// asker long-polls it; an answer wakes every waiting poll at once.

import { DurableObject } from "cloudflare:workers";

/** How long an answered or expired box is kept for a poll to collect it. */
export const REPLY_GRACE_SECONDS = 60 * 60;

export interface Responder {
  device_id: string;
  /** Who answered, as the asker sees it: their device's name, or another member's user name. */
  label: string;
}

export interface Question {
  userId: string;
  /** Unix seconds. */
  expiresAt: number;
  responders: Responder[];
}

export interface Answer {
  by: string;
  replied_at: number;
  n: string;
  c: string;
}

export type AnswerResult = "ok" | "answered" | "expired" | "forbidden";

export type ReplyState =
  | { status: "missing" | "pending" | "expired" }
  | ({ status: "answered" } & Answer);

/** What the API needs from a question's box; `now` is the caller's clock, in Unix seconds. */
export interface Replies {
  open(question: Question, now: number): Promise<void>;
  answer(deviceId: string, n: string, c: string, now: number): Promise<AnswerResult>;
  wait(userId: string, seconds: number, now: number): Promise<ReplyState>;
}

const QUESTION_KEY = "question";
const ANSWER_KEY = "answer";

export class ReplyBox extends DurableObject<Env> implements Replies {
  #waiters = new Set<() => void>();

  async open(question: Question, now: number): Promise<void> {
    this.ctx.storage.kv.put(QUESTION_KEY, question);
    await this.ctx.storage.setAlarm(Date.now() + (question.expiresAt - now + REPLY_GRACE_SECONDS) * 1000);
  }

  /** Stores the first answer from a device allowed to give one. */
  answer(deviceId: string, n: string, c: string, now: number): Promise<AnswerResult> {
    const q = this.ctx.storage.kv.get<Question>(QUESTION_KEY);
    const responder = q?.responders.find((r) => r.device_id === deviceId);
    if (!q || !responder) return Promise.resolve("forbidden");
    if (this.ctx.storage.kv.get(ANSWER_KEY)) return Promise.resolve("answered");
    if (now >= q.expiresAt) return Promise.resolve("expired");
    this.ctx.storage.kv.put<Answer>(ANSWER_KEY, { by: responder.label, replied_at: now, n, c });
    for (const wake of [...this.#waiters]) wake();
    return Promise.resolve("ok");
  }

  /** The state for [userId], after waiting up to [seconds] for an answer. */
  async wait(userId: string, seconds: number, now: number): Promise<ReplyState> {
    const q = this.ctx.storage.kv.get<Question>(QUESTION_KEY);
    if (!q || q.userId !== userId) return { status: "missing" };
    const wait = Math.min(seconds, q.expiresAt - now);
    if (!this.ctx.storage.kv.get(ANSWER_KEY) && wait > 0) {
      await new Promise<void>((resolve) => {
        const done = () => {
          clearTimeout(timer);
          this.#waiters.delete(done);
          resolve();
        };
        const timer = setTimeout(done, wait * 1000);
        this.#waiters.add(done);
      });
    }
    const answer = this.ctx.storage.kv.get<Answer>(ANSWER_KEY);
    if (answer) return { status: "answered", ...answer };
    return { status: now + wait >= q.expiresAt ? "expired" : "pending" };
  }

  override async alarm(): Promise<void> {
    await this.ctx.storage.deleteAll();
  }
}
