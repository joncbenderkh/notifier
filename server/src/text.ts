// SPDX-License-Identifier: GPL-3.0-or-later
//
// What notification text and names may hold. Senders choose it, and it is
// shown on phones, desktops and terminals.

export const MAX_TITLE_BYTES = 256;
export const MAX_BODY_BYTES = 2048;

export const utf8Length = (s: string) => new TextEncoder().encode(s).length;

/**
 * Characters a client could be spoofed or driven by: C0 and C1 controls
 * and DEL (terminal escapes), and the bidi embeddings, overrides and
 * isolates (reordering what is shown). Bidi marks and joiners stay
 * allowed: right-to-left text and emoji sequences need them.
 */
export const UNSAFE_IN_NAME = /[\p{Cc}\u202A-\u202E\u2066-\u2069]/u;
/** As in names, but a message may span lines and hold tabs. */
export const UNSAFE_IN_TEXT = /[^\P{Cc}\t\n]|[\u202A-\u202E\u2066-\u2069]/u;

// ANSI escape sequences: CSI, OSC, nF, and the two-byte ones (as notify's clean).
// eslint-disable-next-line no-control-regex -- matching control characters is the point
const TERMINAL_ESCAPE = /\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[ -/]+[0-~]|[@-Z\\-_])/g;

/**
 * Makes text a client cannot clean itself (a log posted with curl, a
 * monitoring tool's alert) safe to send: escapes removed, carriage returns
 * made newlines, other unsafe characters replaced with U+FFFD, and at most
 * [maxBytes] kept, marked "…" where cut. [keep] says which end survives: a
 * log's error is usually at its end, an alert's summary at its start.
 */
export function cleanText(raw: string, maxBytes: number, keep: "head" | "tail"): string {
  // The whole text is cleaned before cutting, since escapes or blank lines
  // could fill any window of the raw text. Every step is linear, so even a
  // 1 MiB body takes milliseconds.
  const s = trimNewlines(
    raw
      .toWellFormed()
      .replace(TERMINAL_ESCAPE, "")
      .replace(/\r\n?/g, "\n")
      .replace(new RegExp(UNSAFE_IN_TEXT.source, "gu"), "\uFFFD"),
    keep === "head",
  );
  if (utf8Length(s) <= maxBytes) return s;
  const marker = "…";
  let bytes = utf8Length(marker);
  if (keep === "tail") {
    let start = s.length;
    for (const ch of [...s.slice(-maxBytes)].reverse()) {
      if (bytes + utf8Length(ch) > maxBytes) break;
      bytes += utf8Length(ch);
      start -= ch.length;
    }
    return marker + s.slice(start);
  }
  let end = 0;
  for (const ch of s.slice(0, maxBytes)) {
    if (bytes + utf8Length(ch) > maxBytes) break;
    bytes += utf8Length(ch);
    end += ch.length;
  }
  return s.slice(0, end) + marker;
}

/**
 * Drops trailing newlines, and leading ones too when [leading]. A loop, as
 * /\n+$/ backtracks quadratically over long runs of newlines mid-text.
 */
function trimNewlines(s: string, leading: boolean): string {
  let start = 0;
  let end = s.length;
  while (end > 0 && s[end - 1] === "\n") end--;
  while (leading && start < end && s[start] === "\n") start++;
  return s.slice(start, end);
}

/** [cleanText] keeping the end, as for a log. */
export const cleanTail = (raw: string, maxBytes: number) => cleanText(raw, maxBytes, "tail");
