// SPDX-License-Identifier: GPL-3.0-or-later
//
// End-to-end payload encryption, so the push transport (FCM) can neither
// read nor forge notifications: AES-256-GCM under a per-device key, with the
// device ID as associated data so a payload sealed for one device fails to
// open on another. The Android app's SealedMessage is the other half.

export const KEY_BYTES = 32;
const NONCE_BYTES = 12;

export interface Envelope {
  nonce: Uint8Array;
  /** Ciphertext with the 16-byte GCM tag appended. */
  ciphertext: Uint8Array;
}

export function newSealKey(): Uint8Array {
  return crypto.getRandomValues(new Uint8Array(KEY_BYTES));
}

function importKey(key: Uint8Array, usage: "encrypt" | "decrypt"): Promise<CryptoKey> {
  if (key.length !== KEY_BYTES) throw new Error(`seal key must be ${KEY_BYTES} bytes`);
  return crypto.subtle.importKey("raw", key, "AES-GCM", false, [usage]);
}

export async function seal(
  key: Uint8Array,
  deviceId: string,
  plaintext: Uint8Array,
  nonce: Uint8Array = crypto.getRandomValues(new Uint8Array(NONCE_BYTES)),
): Promise<Envelope> {
  const ciphertext = await crypto.subtle.encrypt(
    { name: "AES-GCM", iv: nonce, additionalData: new TextEncoder().encode(deviceId) },
    await importKey(key, "encrypt"),
    plaintext,
  );
  return { nonce, ciphertext: new Uint8Array(ciphertext) };
}

export async function open(
  key: Uint8Array,
  deviceId: string,
  envelope: Envelope,
): Promise<Uint8Array> {
  const plaintext = await crypto.subtle.decrypt(
    { name: "AES-GCM", iv: envelope.nonce, additionalData: new TextEncoder().encode(deviceId) },
    await importKey(key, "decrypt"),
    envelope.ciphertext,
  );
  return new Uint8Array(plaintext);
}
