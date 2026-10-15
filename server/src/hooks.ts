// SPDX-License-Identifier: GPL-3.0-or-later
//
// Inbound webhooks (issue #34). A monitoring tool posts its own alert format
// to /v1/hooks/<format>/<secret>, and the alert is published to the hook's
// channel. Parsing is lenient: these schemas evolve, so unknown fields are
// ignored, and text is cleaned and cut rather than rejected, since the
// sender cannot be told to fix it.

import type { Payload } from "./app";
import { cleanText, MAX_BODY_BYTES, MAX_TITLE_BYTES, utf8Length, webLink } from "./text";

export const HOOK_SECRET_PREFIX = "nw_";
export const MAX_HOOKS_PER_USER = 20;

/** What a hook publishes. */
export interface Message {
  title: string;
  body: string;
  priority: Payload["priority"];
  /** Where to look: dropped, not refused, when it is no web link. */
  url?: string;
}

/** A payload its format cannot read; answered with 400. */
export class PayloadError extends Error {}

/** Severities, as monitoring rules commonly label them, that page: high priority. */
const URGENT = new Set(["critical", "high", "page", "error"]);

type Json = Record<string, unknown>;
const isObject = (v: unknown): v is Json => typeof v === "object" && v !== null && !Array.isArray(v);
const text = (v: unknown): string | undefined => (typeof v === "string" && v.trim() !== "" ? v : undefined);
const field = (o: unknown, key: string): Json => (isObject(o) && isObject(o[key]) ? o[key] : {});

/**
 * [raw] on one line. Split rather than a regex trimming the spaces around
 * each line break, which backtracks quadratically on long runs of spaces.
 */
const oneLine = (raw: string) =>
  raw
    .split(/[\r\n]+/)
    .map((s) => s.trim())
    .filter((s) => s !== "")
    .join(" ");

/** A title on one line, cleaned and cut to fit. */
const titleOf = (raw: string) => cleanText(oneLine(raw), MAX_TITLE_BYTES, "head");

/** At most this much of each alert's line, so one long one cannot crowd out the rest. */
const MAX_LINE_BYTES = 512;

/** Lines that fit in a body, then "and k more" for the rest. */
function bodyOf(lines: readonly string[], more = 0): string {
  const kept: string[] = [];
  const tail = (n: number) => (n > 0 ? `and ${n} more` : "");
  for (const [i, raw] of lines.entries()) {
    const line = cleanText(oneLine(raw), MAX_LINE_BYTES, "head");
    const rest = tail(lines.length - i - 1 + more);
    const next = [...kept, line, ...(rest ? [rest] : [])].join("\n");
    if (utf8Length(next) > MAX_BODY_BYTES) {
      kept.push(tail(lines.length - i + more));
      return kept.join("\n");
    }
    kept.push(line);
  }
  if (more > 0) kept.push(tail(more));
  return kept.join("\n");
}

/** Alertmanager's webhook (version 4), which Grafana's extends. */
function alertmanager(json: unknown): Message {
  if (!isObject(json) || !Array.isArray(json.alerts) || json.alerts.length === 0) {
    throw new PayloadError("expected an Alertmanager webhook payload with alerts");
  }
  const alerts = json.alerts.filter(isObject);
  // Junk in place of alerts must not read as everything resolved.
  if (alerts.length === 0) throw new PayloadError("expected alerts to be objects");
  const firing = alerts.filter((a) => a.status !== "resolved");
  const resolved = json.status === "resolved" || firing.length === 0;
  const name =
    text(field(json, "groupLabels").alertname) ??
    text(field(json, "commonLabels").alertname) ??
    (alerts.length === 1 ? text(field(alerts[0], "labels").alertname) : undefined) ??
    "alerts";
  const lines = alerts.map((a) => {
    const labels = field(a, "labels");
    const notes = field(a, "annotations");
    const what = text(notes.summary) ?? text(notes.description) ?? text(labels.alertname) ?? "alert";
    const where = text(labels.instance);
    const line = where && !what.includes(where) ? `${what} (${where})` : what;
    return !resolved && a.status === "resolved" ? `Resolved: ${line}` : line;
  });
  const truncated = typeof json.truncatedAlerts === "number" && json.truncatedAlerts > 0 ? json.truncatedAlerts : 0;
  const urgent = firing.some((a) => URGENT.has(text(field(a, "labels").severity)?.toLowerCase() ?? ""));
  // One alert's own source (its rule or graph), else Alertmanager's page for the group.
  const single = alerts.length === 1 ? alerts[0] : firing.length === 1 ? firing[0] : undefined;
  const url = webLink(single?.generatorURL) ?? webLink(json.externalURL);
  return {
    title: titleOf(resolved ? `[RESOLVED] ${name}` : `[FIRING:${firing.length + truncated}] ${name}`),
    body: bodyOf(lines, truncated),
    priority: !resolved && urgent ? "high" : "default",
    ...(url !== undefined ? { url } : {}),
  };
}

/** Grafana's webhook contact point: Alertmanager's payload plus its own title. */
function grafana(json: unknown): Message {
  const m = alertmanager(json);
  const title = isObject(json) ? text(json.title) : undefined;
  return title ? { ...m, title: titleOf(title) } : m;
}

/** {"title": "…", "body": "…", "priority": "default" | "high", "url": "…"}, for anything else. */
function generic(json: unknown): Message {
  const title = isObject(json) ? text(json.title) : undefined;
  if (!isObject(json) || !title) throw new PayloadError("expected a JSON object with a title");
  const priority = json.priority ?? "default";
  if (priority !== "default" && priority !== "high") throw new PayloadError('priority must be "default" or "high"');
  const body = typeof json.body === "string" ? cleanText(json.body, MAX_BODY_BYTES, "head") : "";
  const url = webLink(json.url);
  return { title: titleOf(title), body, priority, ...(url !== undefined ? { url } : {}) };
}

/** Each format's parser, by the name in the hook's URL. */
export const FORMATS = { alertmanager, grafana, generic } satisfies Record<string, (json: unknown) => Message>;
export type Format = keyof typeof FORMATS;

export const isFormat = (s: unknown): s is Format => typeof s === "string" && Object.hasOwn(FORMATS, s);
