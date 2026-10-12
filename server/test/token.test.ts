// SPDX-License-Identifier: GPL-3.0-or-later
import { describe, expect, it } from "vitest";
import { toBase32 } from "../src/encoding";
import { newPairingCode, newSecret, normalizePairingCode } from "../src/token";

describe("token", () => {
  it("generates Crockford pairing codes", () => {
    for (let i = 0; i < 100; i++) {
      expect(newPairingCode()).toMatch(/^[0-9A-HJKMNP-TV-Z]{4}-[0-9A-HJKMNP-TV-Z]{4}$/);
    }
  });

  it.each([
    ["7KQ2-MX9D", "7KQ2-MX9D"],
    ["7kq2mx9d", "7KQ2-MX9D"],
    [" 7kq2 mx9d", "7KQ2-MX9D"],
    ["OIL0-0000", "0110-0000"],
  ])("normalizes %j", (input, want) => {
    expect(normalizePairingCode(input)).toBe(want);
  });

  it("encodes RFC 4648 base32", () => {
    expect(toBase32(new TextEncoder().encode("foobar"))).toBe("mzxw6ytboi");
  });

  it("issues 256-bit secrets", () => {
    expect(newSecret("nk_")).toMatch(/^nk_[a-z2-7]{52}$/);
  });
});
