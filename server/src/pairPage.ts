// SPDX-License-Identifier: GPL-3.0-or-later
//
// A static page for creating a pairing code and showing it as a QR code. The
// page itself is public and holds no secrets; the user pastes their token,
// which the page keeps in memory only and sends solely to this origin's
// POST /v1/pairings.

import type { Hono } from "hono";
import { secureHeaders } from "hono/secure-headers";

const PAGE = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Pair a phone · notifier</title>
<link rel="stylesheet" href="/pair.css">
<script src="/pair.js" defer></script>
</head>
<body>
<main>
  <h1>Pair a phone</h1>
  <form id="form" autocomplete="off">
    <label for="token">Your token</label>
    <input id="token" type="password" required pattern="nu_[a-z2-7]+" placeholder="nu_…" spellcheck="false">
    <button id="create" type="submit">Create pairing code</button>
  </form>
  <p id="status" role="status"></p>
  <section id="result" hidden>
    <img id="qr" alt="Pairing QR code" width="280" height="280">
    <p>Scan with the Notifier app, or enter by hand:</p>
    <dl>
      <dt>Server URL</dt><dd id="server"></dd>
      <dt>Pairing code</dt><dd id="code"></dd>
    </dl>
    <p id="expiry"></p>
  </section>
</main>
</body>
</html>
`;

const SCRIPT = `"use strict";
(() => {
  const $ = (id) => document.getElementById(id);
  let timer;

  function showStatus(text) {
    $("status").textContent = text;
  }

  function expire() {
    clearInterval(timer);
    $("result").hidden = true;
    $("qr").removeAttribute("src");
    showStatus("That code expired. Create a new one.");
  }

  $("form").addEventListener("submit", async (event) => {
    event.preventDefault();
    $("create").disabled = true;
    showStatus("Creating…");
    let response, body;
    try {
      response = await fetch("/v1/pairings?qr=svg", {
        method: "POST",
        headers: { authorization: "Bearer " + $("token").value.trim() },
      });
      body = await response.json();
    } catch {
      showStatus("Could not reach the server.");
      return;
    } finally {
      $("create").disabled = false;
    }
    if (!response.ok) {
      showStatus(body.error || "HTTP " + response.status);
      return;
    }
    $("qr").src = "data:image/svg+xml;base64," + btoa(body.qr_svg);
    $("server").textContent = location.origin;
    $("code").textContent = body.code;
    $("result").hidden = false;
    showStatus("");
    const expiresAt = Date.parse(body.expires_at);
    const tick = () => {
      const left = Math.round((expiresAt - Date.now()) / 1000);
      if (left <= 0) return expire();
      $("expiry").textContent =
        "Expires in " + Math.floor(left / 60) + ":" + String(left % 60).padStart(2, "0") + " · single use";
    };
    clearInterval(timer);
    tick();
    timer = setInterval(tick, 1000);
  });
})();
`;

const STYLE = `:root { color-scheme: light dark; font-family: system-ui, sans-serif; }
body { margin: 0; display: grid; place-items: start center; }
main { width: min(28rem, 100% - 2rem); margin-block: 2rem; }
form { display: grid; gap: 0.5rem; }
input, button { font: inherit; padding: 0.5rem 0.75rem; border-radius: 0.5rem; }
button { cursor: pointer; }
#qr { display: block; margin: 1.5rem auto 0.5rem; background: white; padding: 0.5rem; border-radius: 0.5rem; }
dl { display: grid; grid-template-columns: auto 1fr; gap: 0.25rem 1rem; }
dt { opacity: 0.7; }
dd { margin: 0; font-family: ui-monospace, monospace; overflow-wrap: anywhere; }
#code { font-size: 1.5rem; letter-spacing: 0.1em; }
`;

const NO_STORE = { "cache-control": "no-store" };

const pageHeaders = secureHeaders({
  contentSecurityPolicy: {
    defaultSrc: ["'none'"],
    scriptSrc: ["'self'"],
    styleSrc: ["'self'"],
    imgSrc: ["data:"],
    connectSrc: ["'self'"],
    formAction: ["'none'"],
    baseUri: ["'none'"],
    frameAncestors: ["'none'"],
  },
  referrerPolicy: "no-referrer",
  xFrameOptions: "DENY",
  strictTransportSecurity: "max-age=31536000; includeSubDomains",
});

export function pairPageRoutes<E extends object>(app: Hono<E>): void {
  app.get("/pair", pageHeaders, (c) => c.html(PAGE, 200, NO_STORE));
  app.get("/pair.js", pageHeaders, (c) =>
    c.body(SCRIPT, 200, { ...NO_STORE, "content-type": "text/javascript; charset=utf-8" }),
  );
  app.get("/pair.css", pageHeaders, (c) => c.body(STYLE, 200, { ...NO_STORE, "content-type": "text/css; charset=utf-8" }));
  // The page's address before users existed.
  app.get("/admin/pair", (c) => c.redirect("/pair", 301));
}
