// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class NotifierLinkTest {
    @Test
    fun parsesTheServerFormat() {
        // Exactly what server/src/pairing.ts produces.
        val link = NotifierLink.parse(
            "notifier://pair?server=https%3A%2F%2Fnotifier.joncbender.workers.dev&code=7KQ2-MX9D",
            allowHttp = false,
        )
        assertEquals(NotifierLink.Pair("https://notifier.joncbender.workers.dev", "7KQ2-MX9D"), link)
    }

    @Test
    fun allowsHttpOnlyWhenPermitted() {
        val raw = "notifier://pair?server=http%3A%2F%2F127.0.0.1%3A8788&code=82VN-45JA"
        assertNull(NotifierLink.parse(raw, allowHttp = false))
        assertEquals(NotifierLink.Pair("http://127.0.0.1:8788", "82VN-45JA"), NotifierLink.parse(raw, allowHttp = true))
    }

    @Test
    fun normalizesTheOrigin() {
        assertEquals(
            NotifierLink.Pair("https://x.dev:8443", "A"),
            NotifierLink.parse("notifier://pair?server=HTTPS%3A%2F%2FX.dev%3A8443%2F&code=A", allowHttp = false),
        )
    }

    @Test
    fun rejectsOtherLinks() {
        for (raw in listOf(
            "https://notifier.joncbender.workers.dev",
            "notifier://unpair?server=https%3A%2F%2Fx.dev&code=A",
            "notifier://join?server=https%3A%2F%2Fx.dev%2Fpath&code=A",
            "notifier://pair?server=https%3A%2F%2Fx.dev",
            "notifier://pair?code=7KQ2-MX9D",
            "notifier://pair?server=ftp%3A%2F%2Fx.dev&code=A",
            "notifier://pair?server=javascript%3Aalert(1)&code=A",
            "not a uri at all %%",
            // Anything but a bare origin could disguise the real host.
            "notifier://pair?server=https%3A%2F%2Fnotifier.joncbender.workers.dev%40evil.example&code=A",
            "notifier://pair?server=https%3A%2F%2Fx.dev%2Fpath&code=A",
            "notifier://pair?server=https%3A%2F%2Fx.dev%3Fq%3D1&code=A",
            "notifier://pair?server=https%3A%2F%2Fx.dev%23frag&code=A",
            "notifier://pair?server=https%3A%2F%2F%40x.dev&code=A",
            "notifier://pair?server=https%3A%2F%2Fx.dev%3A99999&code=A",
        )) {
            assertNull(raw, NotifierLink.parse(raw, allowHttp = true))
        }
    }

    @Test
    fun normalizesTypedServerURLsLikeTheServer() {
        assertEquals("https://notifier.joncbender.workers.dev", NotifierLink.origin(" HTTPS://Notifier.JonCBender.workers.dev/ ", false))
        assertEquals("https://x.dev", NotifierLink.origin("https://x.dev:443", false))
        assertEquals("http://10.0.2.2:8787", NotifierLink.origin("http://10.0.2.2:8787", true))
        assertNull(NotifierLink.origin("http://x.dev", false))
        assertNull(NotifierLink.origin("https://x.dev/notifier", false))
    }

    @Test
    fun parsesJoinLinks() {
        // Exactly what server/src/pairing.ts produces for a channel invite.
        assertEquals(
            NotifierLink.Join("https://notifier.joncbender.workers.dev", "ABCD-EFGH-JKMN"),
            NotifierLink.parse(
                "notifier://join?server=https%3A%2F%2Fnotifier.joncbender.workers.dev&code=ABCD-EFGH-JKMN",
                allowHttp = false,
            ),
        )
    }
}
