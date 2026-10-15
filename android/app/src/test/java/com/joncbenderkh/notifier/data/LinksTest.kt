// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class LinksTest {
    @Test
    fun acceptsOnlyWebLinks() {
        for (ok in listOf("https://ci.example/7?a=b#c", "http://10.0.0.2:8080/", "HTTPS://CI.example")) {
            assertEquals(ok, Links.web(ok))
        }
        for (bad in listOf(
            null, "", "ci.example", "javascript:alert(1)", "intent://x#Intent;end", "file:///etc/passwd",
            "content://x/y", "https:///path", "https://user:pw@ci.example/", "https://ci.example/a b",
            "https:ci.example", "https://good.example@evil_host.example/", "https://user:pw@é.example/",
        )) {
            assertNull(bad, Links.web(bad))
        }
    }

    @Test
    fun namesTheHost() {
        assertEquals("ci.example", Links.host("https://ci.example/7"))
        assertEquals("10.0.0.2", Links.host("http://10.0.0.2:8080/"))
        assertEquals("evil_host.example", Links.host("https://evil_host.example:8443/"))
        // Cyrillic о: shown as the browser would, not as a look-alike of google.com.
        assertEquals("xn--ggle-55da.com", Links.host("https://g\u043e\u043egle.com/x"))
    }
}
