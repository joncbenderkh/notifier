// SPDX-License-Identifier: GPL-3.0-or-later
//
// Bearer secrets the server issues. Every secret carries 256 bits of
// entropy, so an unsalted SHA-256 digest is a sufficient at-rest form: it
// cannot be brute-forced, and lookups by digest need no constant-time compare.

import { toBase32, toHex } from "./encoding";

export const API_KEY_PREFIX = "nk_";
export const DEVICE_TOKEN_PREFIX = "nd_";

// Crockford base32: no I, L, O, or U, so codes survive being read aloud.
const PAIRING_ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

function randomBytes(n: number): Uint8Array {
  return crypto.getRandomValues(new Uint8Array(n));
}

export function newSecret(prefix: string): string {
  return prefix + toBase32(randomBytes(32));
}

export function newId(prefix: string): string {
  return prefix + toBase32(randomBytes(10));
}

/** A short, human-typeable code like "7KQ2-MX9D". */
export function newPairingCode(): string {
  const chars = Array.from(randomBytes(8), (v) => PAIRING_ALPHABET[v & 31]);
  return `${chars.slice(0, 4).join("")}-${chars.slice(4).join("")}`;
}

/** Maps user input onto the canonical code, forgiving case, dashes, and look-alikes. */
export function normalizePairingCode(input: string): string {
  const code = input
    .toUpperCase()
    .replace(/[-\s]/g, "")
    .replace(/[IL]/g, "1")
    .replace(/O/g, "0");
  return code.length === 8 ? `${code.slice(0, 4)}-${code.slice(4)}` : code;
}

export async function sha256Hex(secret: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(secret));
  return toHex(new Uint8Array(digest));
}
