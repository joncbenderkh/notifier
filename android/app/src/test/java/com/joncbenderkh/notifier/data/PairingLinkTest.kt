// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class PairingLinkTest {
    @Test
    fun parsesTheServerFormat() {
        // Exactly what server/src/pairing.ts produces.
        val link = PairingLink.parse(
            "notifier://pair?server=https%3A%2F%2Fnotifier.joncbender.workers.dev&code=7KQ2-MX9D",
            allowHttp = false,
        )
        assertEquals(PairingLink("https://notifier.joncbender.workers.dev", "7KQ2-MX9D"), link)
    }

    @Test
    fun allowsHttpOnlyWhenPermitted() {
        val raw = "notifier://pair?server=http%3A%2F%2F127.0.0.1%3A8788&code=82VN-45JA"
        assertNull(PairingLink.parse(raw, allowHttp = false))
        assertEquals(PairingLink("http://127.0.0.1:8788", "82VN-45JA"), PairingLink.parse(raw, allowHttp = true))
    }

    @Test
    fun rejectsOtherLinks() {
        for (raw in listOf(
            "https://notifier.joncbender.workers.dev",
            "notifier://unpair?server=https%3A%2F%2Fx.dev&code=A",
            "notifier://pair?server=https%3A%2F%2Fx.dev",
            "notifier://pair?code=7KQ2-MX9D",
            "notifier://pair?server=ftp%3A%2F%2Fx.dev&code=A",
            "notifier://pair?server=javascript%3Aalert(1)&code=A",
            "not a uri at all %%",
        )) {
            assertNull(raw, PairingLink.parse(raw, allowHttp = true))
        }
    }
}
