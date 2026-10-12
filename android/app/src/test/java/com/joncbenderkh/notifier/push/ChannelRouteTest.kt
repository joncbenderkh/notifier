// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ChannelRouteTest {
    private val json = Json { ignoreUnknownKeys = true }

    private fun payload(priority: String = "default", channel: String? = null, channelId: String? = null) =
        Payload("msg_1", "Hi", priority = priority, sentAt = 1, channel = channel, channelId = channelId)

    @Test
    fun messagesWithoutAChannelUseTheOriginalChannels() {
        assertEquals(ChannelRoute("default", null, null), ChannelRoute.of(payload()))
        assertEquals(ChannelRoute("high", null, null), ChannelRoute.of(payload("high")))
    }

    @Test
    fun theDefaultChannelUsesTheOriginalChannels() {
        val p = payload("high", "default", "ch_default_usr_abc")
        assertEquals(ChannelRoute("high", null, null), ChannelRoute.of(p))
    }

    @Test
    fun otherChannelsGetAGroupKeyedByID() {
        assertEquals(
            ChannelRoute("ch_x", "ch:ch_x", "alice/homelab"),
            ChannelRoute.of(payload(channel = "alice/homelab", channelId = "ch_x")),
        )
        assertEquals("ch_x.high", ChannelRoute.of(payload("high", "alice/homelab", "ch_x")).androidChannelId)
    }

    @Test
    fun decodesPayloadsWithAndWithoutAChannel() {
        val old = json.decodeFromString<Payload>("""{"id":"msg_1","title":"Hi","priority":"default","sent_at":1}""")
        assertNull(old.channel)
        assertNull(old.channelId)
        val new = json.decodeFromString<Payload>(
            """{"id":"msg_1","title":"Hi","priority":"high","sent_at":1,"channel":"homelab","channel_id":"ch_x"}""",
        )
        assertEquals(payload("high", "homelab", "ch_x"), new)
    }
}
