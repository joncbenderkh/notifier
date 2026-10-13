// SPDX-License-Identifier: GPL-3.0-or-later
//
// The WebSocket transport, for devices FCM cannot reach (the Linux daemon).
// One Durable Object per device holds its socket and a short queue, so a
// message sent while the device is asleep or offline arrives when it
// reconnects. Delivery is at least once: an entry stays queued until the
// device acks its ID, and the device drops IDs it has already shown.

import { DurableObject } from "cloudflare:workers";

/** Entries beyond this are dropped, oldest first. */
export const MAX_QUEUED_MESSAGES = 200;

/** Close codes the daemon acts on: stop rather than reconnect. */
export const CLOSE_REPLACED = 4000;
export const CLOSE_UNPAIRED = 4001;

const UNPAIRED_KEY = "unpaired";

export interface StreamMessage {
  id: string;
  /** The JSON text frame sent to the device. */
  frame: string;
  /** Unix seconds. */
  expiresAt: number;
}

export type StreamStatus = "sent" | "queued";

/** What the API needs from a device's stream; tests substitute a fake. */
export interface Stream {
  deliver(message: StreamMessage): Promise<StreamStatus>;
  close(): Promise<void>;
  fetch(request: Request): Promise<Response>;
}

export class DeviceStream extends DurableObject<Env> {
  constructor(ctx: DurableObjectState, env: Env) {
    super(ctx, env);
    ctx.storage.sql.exec(
      `CREATE TABLE IF NOT EXISTS queue (
         seq        INTEGER PRIMARY KEY AUTOINCREMENT,
         id         TEXT NOT NULL UNIQUE,
         frame      TEXT NOT NULL,
         expires_at INTEGER NOT NULL
       )`,
    );
    // Answered without waking the object, so keepalives cost nothing.
    ctx.setWebSocketAutoResponse(new WebSocketRequestResponsePair("ping", "pong"));
  }

  /** Queues [message] and, if the device is connected, sends it now. */
  async deliver(message: StreamMessage): Promise<StreamStatus> {
    const sql = this.ctx.storage.sql;
    sql.exec("INSERT OR IGNORE INTO queue (id, frame, expires_at) VALUES (?, ?, ?)", message.id, message.frame, message.expiresAt);
    sql.exec(
      "DELETE FROM queue WHERE seq NOT IN (SELECT seq FROM queue ORDER BY seq DESC LIMIT ?)",
      MAX_QUEUED_MESSAGES,
    );
    await this.#scheduleExpiry();
    const socket = this.#open();
    if (!socket) return "queued";
    try {
      socket.send(message.frame);
      return "sent";
    } catch {
      return "queued";
    }
  }

  /** Accepts the device's connection, replacing any older one, and replays the queue. */
  override fetch(request: Request): Response {
    if (request.headers.get("upgrade")?.toLowerCase() !== "websocket") {
      return new Response("expected a WebSocket upgrade", { status: 426 });
    }
    // A connection authenticated just before the device was unpaired.
    if (this.ctx.storage.kv.get(UNPAIRED_KEY)) return new Response("device unpaired", { status: 404 });
    for (const old of this.ctx.getWebSockets()) old.close(CLOSE_REPLACED, "replaced by a newer connection");
    const [client, server] = Object.values(new WebSocketPair()) as [WebSocket, WebSocket];
    this.ctx.acceptWebSocket(server);
    this.#purge();
    for (const { frame } of this.ctx.storage.sql.exec<{ frame: string }>("SELECT frame FROM queue ORDER BY seq")) {
      server.send(frame);
    }
    return new Response(null, { status: 101, webSocket: client });
  }

  /** An ack, {"ack":"msg_…"}, removes that message from the queue; anything else is ignored. */
  override webSocketMessage(_ws: WebSocket, message: string | ArrayBuffer): void {
    if (typeof message !== "string") return;
    let ack: unknown;
    try {
      ack = (JSON.parse(message) as { ack?: unknown }).ack;
    } catch {
      return;
    }
    if (typeof ack === "string") this.ctx.storage.sql.exec("DELETE FROM queue WHERE id = ?", ack);
  }

  /** Completes a close the device starts; the runtime does not answer it for hibernated sockets. */
  override webSocketClose(ws: WebSocket): void {
    try {
      ws.close(1000, "closed");
    } catch {
      // Already closed.
    }
  }

  override async alarm(): Promise<void> {
    this.#purge();
    await this.#scheduleExpiry();
  }

  /**
   * Disconnects and forgets the device; called when it is unpaired. Only a
   * marker stays, so a connection that raced the unpairing is refused.
   */
  async close(): Promise<void> {
    for (const ws of this.ctx.getWebSockets()) ws.close(CLOSE_UNPAIRED, "device unpaired");
    await this.ctx.storage.deleteAlarm();
    await this.ctx.storage.deleteAll();
    this.ctx.storage.kv.put(UNPAIRED_KEY, true);
  }

  #open(): WebSocket | undefined {
    return this.ctx.getWebSockets().find((ws) => ws.readyState === WebSocket.OPEN);
  }

  #purge(): void {
    this.ctx.storage.sql.exec("DELETE FROM queue WHERE expires_at <= ?", Math.floor(Date.now() / 1000));
  }

  async #scheduleExpiry(): Promise<void> {
    const { next } = this.ctx.storage.sql.exec<{ next: number | null }>("SELECT min(expires_at) AS next FROM queue").one();
    if (next === null) await this.ctx.storage.deleteAlarm();
    else await this.ctx.storage.setAlarm(next * 1000);
  }
}
