// SPDX-License-Identifier: GPL-3.0-or-later
// The pairing link a phone scans: notifier://pair?server=<origin>&code=<code>.
// The Android app parses the same shape (PairingLink.kt).

import { renderSVG } from "uqr";

export function pairingUri(server: string, code: string): string {
  return `notifier://pair?server=${encodeURIComponent(server)}&code=${encodeURIComponent(code)}`;
}

export function qrSvg(data: string): string {
  return renderSVG(data, { ecc: "M", border: 4 });
}
