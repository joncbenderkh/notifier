// SPDX-License-Identifier: GPL-3.0-or-later
//
// Bearer secrets the server issues. Every secret carries 256 bits of
// entropy, so an unsalted SHA-256 digest is a sufficient at-rest form: it
// cannot be brute-forced, and lookups by digest need no constant-time compare.

import { toBase32, toHex } from "./encoding";

export const API_KEY_PREFIX = "nk_";
export const DEVICE_TOKEN_PREFIX = "nd_";
export const USER_TOKEN_PREFIX = "nu_";

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

/** A short, human-typeable code like "7KQ2-MX9D", in groups of four. */
function newCode(groups: number): string {
  const chars = Array.from(randomBytes(groups * 4), (v) => PAIRING_ALPHABET[v & 31]);
  return Array.from({ length: groups }, (_, i) => chars.slice(i * 4, i * 4 + 4).join("")).join("-");
}

/** Maps user input onto the canonical code, forgiving case, dashes, and look-alikes. */
function normalizeCode(input: string, groups: number): string {
  const code = input
    .toUpperCase()
    .replace(/[-\s]/g, "")
    .replace(/[IL]/g, "1")
    .replace(/O/g, "0");
  return code.length === groups * 4 ? code.match(/.{4}/g)!.join("-") : code;
}

/** Pairing codes live ten minutes, so 40 bits is plenty. */
export const newPairingCode = () => newCode(2);
export const normalizePairingCode = (input: string) => normalizeCode(input, 2);

/** Invites live for days and create accounts, so they get 60 bits. */
export const newInviteCode = () => newCode(3);
export const normalizeInviteCode = (input: string) => normalizeCode(input, 3);

export async function sha256Hex(secret: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(secret));
  return toHex(new Uint8Array(digest));
}
