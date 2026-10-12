// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import java.net.URI
import java.net.URISyntaxException
import java.net.URLDecoder

/**
 * A pairing link, `notifier://pair?server=<origin>&code=<code>`, as shown in QR
 * form by the server's admin page and `notify pair`. Mirrors
 * `server/src/pairing.ts`.
 */
data class PairingLink(val serverUrl: String, val code: String) {
    companion object {
        /** Parses [raw], or returns null if it is not a usable pairing link. */
        fun parse(raw: String, allowHttp: Boolean): PairingLink? {
            val uri = try {
                URI(raw.trim())
            } catch (_: URISyntaxException) {
                return null
            }
            if (!uri.scheme.equals("notifier", ignoreCase = true) || uri.host != "pair") return null
            val params = uri.rawQuery.orEmpty().split('&').mapNotNull { pair ->
                val (key, value) = pair.split('=', limit = 2).takeIf { it.size == 2 } ?: return@mapNotNull null
                URLDecoder.decode(key, "UTF-8") to URLDecoder.decode(value, "UTF-8")
            }.toMap()
            val server = params["server"]?.trimEnd('/') ?: return null
            val code = params["code"]?.trim()?.takeIf { it.isNotEmpty() } ?: return null
            val serverUri = try {
                URI(server)
            } catch (_: URISyntaxException) {
                return null
            }
            val scheme = serverUri.scheme?.lowercase()
            val schemeOk = scheme == "https" || (allowHttp && scheme == "http")
            // Only a bare origin: "https://trusted@evil.example" would show a
            // trusted name in the form while pairing with evil.example.
            val bareOrigin = serverUri.rawUserInfo == null && serverUri.rawPath.isNullOrEmpty() &&
                serverUri.rawQuery == null && serverUri.rawFragment == null
            if (!schemeOk || !bareOrigin || serverUri.host.isNullOrEmpty() || serverUri.port > 65535) return null
            val port = if (serverUri.port == -1) "" else ":${serverUri.port}"
            return PairingLink("$scheme://${serverUri.host.lowercase()}$port", code)
        }
    }
}
