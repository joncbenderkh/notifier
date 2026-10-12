// SPDX-License-Identifier: GPL-3.0-or-later
import { describe, expect, it } from "vitest";
import { toBase64 } from "../src/encoding";
import { FcmSender, UnregisteredError } from "../src/push";

async function serviceAccount() {
  const pair = (await crypto.subtle.generateKey(
    { name: "RSASSA-PKCS1-v1_5", modulusLength: 2048, publicExponent: new Uint8Array([1, 0, 1]), hash: "SHA-256" },
    true,
    ["sign", "verify"],
  )) as CryptoKeyPair;
  const der = new Uint8Array((await crypto.subtle.exportKey("pkcs8", pair.privateKey)) as ArrayBuffer);
  const pem = `-----BEGIN PRIVATE KEY-----\n${toBase64(der).replace(/(.{64})/g, "$1\n")}\n-----END PRIVATE KEY-----\n`;
  return {
    publicKey: pair.publicKey,
    json: JSON.stringify({ project_id: "demo-project", client_email: "sa@demo.iam", private_key: pem }),
  };
}

function base64UrlDecode(s: string): Uint8Array {
  const b64 = s.replace(/-/g, "+").replace(/_/g, "/");
  return Uint8Array.from(atob(b64 + "=".repeat((4 - (b64.length % 4)) % 4)), (c) => c.charCodeAt(0));
}

const message = { token: "fcm-token", data: { v: "1" }, high: true, ttlSeconds: 60 };

describe("FcmSender", () => {
  it("mints a signed OAuth assertion, caches the token, and sends data-only messages", async () => {
    const { publicKey, json } = await serviceAccount();
    const calls: Request[] = [];
    const fetcher = ((input: RequestInfo, init?: RequestInit) => {
      const req = new Request(input, init);
      calls.push(req.clone());
      if (req.url === "https://oauth2.googleapis.com/token") {
        return Promise.resolve(Response.json({ access_token: "access-1", expires_in: 3600 }));
      }
      return Promise.resolve(Response.json({ name: "projects/demo-project/messages/1" }));
    }) as typeof fetch;

    const sender = new FcmSender(json, fetcher);
    await sender.send(message);
    await sender.send(message);

    expect(calls.map((c) => new URL(c.url).pathname)).toEqual([
      "/token",
      "/v1/projects/demo-project/messages:send",
      "/v1/projects/demo-project/messages:send",
    ]);

    const assertion = (await calls[0]!.formData()).get("assertion") as string;
    const [header, claims, signature] = assertion.split(".");
    const valid = await crypto.subtle.verify(
      "RSASSA-PKCS1-v1_5",
      publicKey,
      base64UrlDecode(signature!),
      new TextEncoder().encode(`${header}.${claims}`),
    );
    expect(valid).toBe(true);
    expect(JSON.parse(new TextDecoder().decode(base64UrlDecode(claims!)))).toMatchObject({
      iss: "sa@demo.iam",
      scope: "https://www.googleapis.com/auth/firebase.messaging",
      aud: "https://oauth2.googleapis.com/token",
    });

    const send = calls[1]!;
    expect(send.headers.get("authorization")).toBe("Bearer access-1");
    expect(await send.json()).toEqual({
      message: { token: "fcm-token", data: { v: "1" }, android: { priority: "HIGH", ttl: "60s" } },
    });
  });

  it("maps UNREGISTERED to UnregisteredError", async () => {
    const { json } = await serviceAccount();
    const fetcher = ((input: RequestInfo) => {
      if (new Request(input).url.endsWith("/token")) {
        return Promise.resolve(Response.json({ access_token: "a", expires_in: 3600 }));
      }
      const body = { error: { status: "NOT_FOUND", details: [{ errorCode: "UNREGISTERED" }] } };
      return Promise.resolve(Response.json(body, { status: 404 }));
    }) as typeof fetch;
    await expect(new FcmSender(json, fetcher).send(message)).rejects.toBeInstanceOf(UnregisteredError);
  });
});
