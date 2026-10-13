// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import com.joncbenderkh.notifier.push.Payload
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class HistoryTest {
    private val now = 1_800_000_000L

    private fun entry(id: String, receivedAt: Long = now) =
        HistoryEntry(id, "title $id", sentAt = receivedAt, receivedAt = receivedAt)

    @Test
    fun keepsTheNewestFirstAndEachMessageOnce() {
        val history = History()
            .add(entry("a", now - 20), now)
            .add(entry("b", now - 10), now)
            .add(entry("a", now), now)
        assertEquals(listOf("b", "a"), history.entries.map { it.id })
    }

    @Test
    fun putsTheLatestFirstWithinTheSameSecond() {
        val history = History().add(entry("first"), now).add(entry("second"), now)
        assertEquals(listOf("second", "first"), history.entries.map { it.id })
    }

    @Test
    fun copiesThePayload() {
        val payload = Payload("msg_1", "Disk", "full", "high", 100, "alice/homelab", "ch_x")
        assertEquals(
            HistoryEntry("msg_1", "Disk", "full", "high", 100, 160, "alice/homelab", "ch_x"),
            HistoryEntry.of(payload, receivedAt = 160),
        )
    }

    @Test
    fun dropsWhatIsTooOldOrBeyondTheLimit() {
        val old = History().add(entry("old", now - History.MAX_AGE_SECONDS), now - History.MAX_AGE_SECONDS)
        assertEquals(listOf("new"), old.add(entry("new"), now).entries.map { it.id })

        var full = History()
        repeat(History.MAX_ENTRIES + 3) { full = full.add(entry("m$it", now - 1000 + it), now) }
        assertEquals(History.MAX_ENTRIES, full.entries.size)
        assertEquals("m${History.MAX_ENTRIES + 2}", full.entries.first().id)
        assertEquals("m3", full.entries.last().id)
    }

    @Test
    fun removesOneEntry() {
        val history = History().add(entry("a"), now).add(entry("b"), now)
        assertEquals(listOf("b"), history.remove("a").entries.map { it.id })
    }

    @Test
    fun roundTripsAndToleratesNewFields() {
        val history = History().add(entry("a").copy(channel = "homelab", channelId = "ch_x"), now)
        assertEquals(history, History.decode(history.encode()))

        val future = """[{"id":"x","title":"t","sentAt":1,"receivedAt":2,"pinned":true}]"""
        assertEquals(listOf("x"), History.decode(future.encodeToByteArray()).entries.map { it.id })
    }

    @Test
    fun treatsUnreadableDataAsEmpty() {
        for (bad in listOf("", "not json", "{}", """[{"id":1}]""")) {
            assertTrue(bad, History.decode(bad.encodeToByteArray()).entries.isEmpty())
        }
    }
}
