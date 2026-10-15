// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import java.net.IDN
import java.net.URI
import java.net.URISyntaxException

/** Which links a message may open, and how to name where they lead. */
object Links {
    /**
     * [raw] if a message may open it: http or https, with a host and no
     * credentials. The server accepts no other, but older or other
     * servers might send one.
     */
    fun web(raw: String?): String? {
        val uri = parse(raw) ?: return null
        val scheme = uri.scheme?.lowercase()
        if (scheme != "http" && scheme != "https") return null
        // URI reports no user info for a host it cannot parse ("a@b_c.example"), so look for it.
        val authority = uri.rawAuthority
        if (authority.isNullOrEmpty() || '@' in authority) return null
        return raw
    }

    /**
     * Where [url] leads, for showing before it is opened: its host, in
     * ASCII, so a look-alike in another script shows as punycode, as the
     * browser's address bar would.
     */
    fun host(url: String): String {
        val uri = parse(url) ?: return url
        val name = uri.host ?: uri.rawAuthority?.replace(PORT, "") ?: return url
        return try {
            IDN.toASCII(name, IDN.ALLOW_UNASSIGNED)
        } catch (_: IllegalArgumentException) {
            name
        }
    }

    private val PORT = Regex(":\\d*$")

    private fun parse(raw: String?): URI? =
        try {
            raw?.let(::URI)
        } catch (_: URISyntaxException) {
            null
        }
}
