// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import java.net.URI
import java.net.URISyntaxException
import java.net.URLDecoder

/**
 * A pairing link, `notifier://pair?server=<url>&code=<code>`, as shown in QR
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
            val schemeOk = serverUri.scheme == "https" || (allowHttp && serverUri.scheme == "http")
            if (!schemeOk || serverUri.host.isNullOrEmpty()) return null
            return PairingLink(server, code)
        }
    }
}
