// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import com.joncbenderkh.notifier.push.ChannelRoute.Kind
import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class ChannelRouteTest {
    private val json = Json { ignoreUnknownKeys = true }

    private fun payload(priority: String = "default", channel: String? = null, channelId: String? = null) =
        Payload("msg_1", "Hi", priority = priority, sentAt = 1, channel = channel, channelId = channelId)

    @Test
    fun messagesWithoutAChannelAreUngrouped() {
        assertEquals(ChannelRoute("message", null, null, Kind.MESSAGE), ChannelRoute.of(payload()))
        assertEquals(ChannelRoute("urgent", null, null, Kind.URGENT), ChannelRoute.of(payload("high")))
    }

    @Test
    fun theDefaultChannelIsUngrouped() {
        val p = payload("high", "default", "ch_default_usr_abc")
        assertEquals(ChannelRoute("urgent", null, null, Kind.URGENT), ChannelRoute.of(p))
    }

    @Test
    fun otherChannelsGetAGroupKeyedByID() {
        assertEquals(
            ChannelRoute("ch_x.message", "ch:ch_x", "alice/homelab", Kind.MESSAGE),
            ChannelRoute.of(payload(channel = "alice/homelab", channelId = "ch_x")),
        )
        assertEquals("ch_x.urgent", ChannelRoute.of(payload("high", "alice/homelab", "ch_x")).androidChannelId)
    }

    @Test
    fun questionsHaveTheirOwnChannelWhateverThePriority() {
        val reply = Reply(listOf("yes"), "k", 1)
        assertEquals(ChannelRoute("question", null, null, Kind.QUESTION), ChannelRoute.of(payload("high").copy(reply = reply)))
        assertEquals("ch_x.question", ChannelRoute.of(payload(channelId = "ch_x").copy(reply = reply)).androidChannelId)
    }

    @Test
    fun namesTheChannelsFromBeforeSounds() {
        assertEquals("default", ChannelRoute.legacyChannelId(Kind.MESSAGE))
        assertEquals("high", ChannelRoute.legacyChannelId(Kind.URGENT))
        assertEquals("ch_x", ChannelRoute.legacyChannelId(Kind.MESSAGE, "ch_x"))
        assertEquals("ch_x.high", ChannelRoute.legacyChannelId(Kind.URGENT, "ch_x"))
        assertEquals("default", ChannelRoute.legacyChannelId(Kind.QUESTION))
        assertEquals("ch_x", ChannelRoute.legacyChannelId(Kind.QUESTION, "ch_x"))
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
