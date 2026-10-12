// SPDX-License-Identifier: GPL-3.0-or-later
// The links a phone scans: notifier://pair?server=<origin>&code=<code> to
// pair, and notifier://join?... to join a channel. The Android app parses
// both (NotifierLink.kt).

import { renderSVG } from "uqr";

function notifierUri(action: "pair" | "join", server: string, code: string): string {
  return `notifier://${action}?server=${encodeURIComponent(server)}&code=${encodeURIComponent(code)}`;
}

export const pairingUri = (server: string, code: string) => notifierUri("pair", server, code);
export const joinUri = (server: string, code: string) => notifierUri("join", server, code);

export function qrSvg(data: string): string {
  return renderSVG(data, { ecc: "M", border: 4 });
}
