// SPDX-License-Identifier: GPL-3.0-or-later
//
// What notification text and names may hold. Senders choose it, and it is
// shown on phones, desktops and terminals.

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
 * Makes text a client cannot clean itself (a log posted with curl) safe to
 * send: escapes removed, carriage returns made newlines, other unsafe
 * characters replaced with U+FFFD, and only the last [maxBytes] kept, since
 * a log's error is usually at its end.
 */
export function cleanTail(raw: string, maxBytes: number): string {
  // Cleaning only shrinks text, and each kept character takes at least one
  // byte, so this much of the end is plenty; the rest cannot be kept anyway.
  const window = 4 * maxBytes;
  const cut = raw.length > window;
  const s = (cut ? raw.slice(-window) : raw)
    .toWellFormed()
    .replace(TERMINAL_ESCAPE, "")
    .replace(/\r\n?/g, "\n")
    .replace(new RegExp(UNSAFE_IN_TEXT.source, "gu"), "\uFFFD")
    .replace(/\n+$/, "");
  if (!cut && utf8Length(s) <= maxBytes) return s;
  const marker = "…";
  const chars = [...s];
  let bytes = utf8Length(marker);
  let start = chars.length;
  while (start > 0 && bytes + utf8Length(chars[start - 1]!) <= maxBytes) bytes += utf8Length(chars[--start]!);
  return marker + chars.slice(start).join("");
}
