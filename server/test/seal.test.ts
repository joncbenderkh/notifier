// SPDX-License-Identifier: GPL-3.0-or-later
import { describe, expect, it } from "vitest";
import { fromBase64, toBase64 } from "../src/encoding";
import { newSealKey, open, seal } from "../src/seal";

// The same vector is asserted by the Android app's SealedMessageTest, pinning
// the wire format on both sides.
const VECTOR = {
  key: "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=",
  nonce: "MDEyMzQ1Njc4OWFi",
  ciphertext:
    "TDM7F3Qdq2rkWAzPzRj+L2fQcGZjs92ayPLhYHDQE7A3+4ZuMxigPp5ZB5Zwv1bQKNFhPX3Zl57NQ/14A8U3Afl212jFtCR48XIXfkAPsHWEZmARTqUMCVVkb1gc7WGe8Sjshg42esye9bbN9nysV+9Pw6UNZw==",
  deviceId: "dev_test",
  plaintext: `{"id":"msg_test","title":"Hello","body":"From the test vector","priority":"high","sent_at":1791590400}`,
};

// A device seals its answer to a question under the asker's one-time key,
// with "reply:" + the message ID as associated data. The server never opens
// it; the notify CLI, notifyd and the Android app assert this vector.
const REPLY_VECTOR = {
  key: VECTOR.key,
  nonce: VECTOR.nonce,
  ciphertext: "TDMzHSVQ7HW1BXHCzRiocGE39XiLrrhG+LEwUrTqrw8=",
  aad: "reply:msg_test",
  plaintext: `{"answer":"yes"}`,
};

const encode = (s: string) => new TextEncoder().encode(s);

describe("seal", () => {
  it("produces the shared vector for a fixed nonce", async () => {
    const env = await seal(fromBase64(VECTOR.key), VECTOR.deviceId, encode(VECTOR.plaintext), fromBase64(VECTOR.nonce));
    expect(toBase64(env.ciphertext)).toBe(VECTOR.ciphertext);
  });

  it("opens the shared vector", async () => {
    const plaintext = await open(fromBase64(VECTOR.key), VECTOR.deviceId, {
      nonce: fromBase64(VECTOR.nonce),
      ciphertext: fromBase64(VECTOR.ciphertext),
    });
    expect(new TextDecoder().decode(plaintext)).toBe(VECTOR.plaintext);
  });

  it("produces the shared reply vector", async () => {
    const env = await seal(fromBase64(REPLY_VECTOR.key), REPLY_VECTOR.aad, encode(REPLY_VECTOR.plaintext), fromBase64(REPLY_VECTOR.nonce));
    expect(toBase64(env.ciphertext)).toBe(REPLY_VECTOR.ciphertext);
  });

  it("rejects a payload sealed for another device", async () => {
    const key = newSealKey();
    const env = await seal(key, "dev_a", encode("hello"));
    await expect(open(key, "dev_b", env)).rejects.toThrow();
  });

  it("rejects tampered ciphertext", async () => {
    const key = newSealKey();
    const env = await seal(key, "dev_a", encode("hello"));
    env.ciphertext[0]! ^= 1;
    await expect(open(key, "dev_a", env)).rejects.toThrow();
  });
});
