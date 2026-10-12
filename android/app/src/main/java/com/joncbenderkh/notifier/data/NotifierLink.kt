// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import java.net.URI
import java.net.URISyntaxException
import java.net.URLDecoder

/**
 * A link the app scans or opens, `notifier://<action>?server=<origin>&code=<code>`:
 * [Pair] (from the server's /pair page and `notify pair`) or [Join] (a channel
 * invite, from `notify channel invite`). Mirrors `server/src/pairing.ts`.
 */
sealed interface NotifierLink {
    val serverUrl: String
    val code: String

    data class Pair(override val serverUrl: String, override val code: String) : NotifierLink
    data class Join(override val serverUrl: String, override val code: String) : NotifierLink

    companion object {
        /** Parses [raw], or returns null if it is not a usable Notifier link. */
        fun parse(raw: String, allowHttp: Boolean): NotifierLink? {
            val uri = try {
                URI(raw.trim())
            } catch (_: URISyntaxException) {
                return null
            }
            if (!uri.scheme.equals("notifier", ignoreCase = true)) return null
            val make: (String, String) -> NotifierLink = when (uri.host) {
                "pair" -> ::Pair
                "join" -> ::Join
                else -> return null
            }
            val params = uri.rawQuery.orEmpty().split('&').mapNotNull { pair ->
                val (key, value) = pair.split('=', limit = 2).takeIf { it.size == 2 } ?: return@mapNotNull null
                URLDecoder.decode(key, "UTF-8") to URLDecoder.decode(value, "UTF-8")
            }.toMap()
            val server = origin(params["server"] ?: return null, allowHttp) ?: return null
            val code = params["code"]?.trim()?.takeIf { it.isNotEmpty() } ?: return null
            return make(server, code)
        }

        /**
         * Normalizes a server URL to its origin (lowercase scheme and host, no
         * default port, no trailing slash), as the server spells it in links;
         * null if it is not a bare http(s) origin.
         */
        fun origin(url: String, allowHttp: Boolean): String? {
            val serverUri = try {
                URI(url.trim().trimEnd('/'))
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
            val defaultPort = if (scheme == "https") 443 else 80
            val port = if (serverUri.port == -1 || serverUri.port == defaultPort) "" else ":${serverUri.port}"
            return "$scheme://${serverUri.host.lowercase()}$port"
        }
    }
}
