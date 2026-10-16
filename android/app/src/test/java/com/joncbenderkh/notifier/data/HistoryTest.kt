// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import com.joncbenderkh.notifier.push.LinkAction
import com.joncbenderkh.notifier.push.Payload
import com.joncbenderkh.notifier.push.Reply
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import java.time.LocalDateTime
import java.time.ZoneId

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
    fun remembersThatTheSenderAskedForReceipts() {
        val tracked = HistoryEntry.of(Payload("msg_r", "Disk", sentAt = now, receipts = true), receivedAt = now)
        assertEquals(true, tracked.receipts)
        val history = History().add(tracked, now).add(entry("plain"), now)
        assertEquals(listOf(false, true), History.decode(history.encode()).entries.map { it.receipts })
    }

    @Test
    fun keepsTheLinks() {
        val actions = listOf(LinkAction("Logs", "https://ci.example/7/logs"))
        val linked = HistoryEntry.of(
            Payload("msg_l", "Build", sentAt = now, url = "https://ci.example/7", actions = actions),
            receivedAt = now,
        )
        val decoded = History.decode(History().add(linked, now).add(entry("plain"), now).encode()).entries
        assertEquals(listOf(null, "https://ci.example/7"), decoded.map { it.url })
        assertEquals(listOf(emptyList(), actions), decoded.map { it.actions })
    }

    @Test
    fun recordsWhatBecameOfAnAnswer() {
        val reply = Reply(listOf("yes", "no"), "key", now + 600)
        val question = HistoryEntry.of(Payload("msg_q", "Deploy?", sentAt = now, reply = reply), receivedAt = now)
        assertEquals(reply, question.reply)
        val history = History().add(question, now).add(entry("plain"), now)
            .settled("msg_q", "Answered: yes")
            .settled("plain", "Answered: yes")
        assertEquals(listOf(null, "Answered: yes"), history.entries.map { it.outcome })
        // A repeated tap after a restart gets 409; the first outcome stays.
        assertEquals("Answered: yes", history.settled("msg_q", "Already answered").entries.last().outcome)
        assertEquals(history, History.decode(history.encode()))
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
    fun keepsReminders() {
        val history = History().add(entry("a"), now).add(entry("b"), now).remind("a", now + 900)
        assertEquals(listOf("a"), history.reminders().map { it.id })
        assertEquals(now + 900, History.decode(history.encode()).entries.first { it.id == "a" }.remindAt)
        assertEquals(emptyList<HistoryEntry>(), history.remind("a", null).reminders())
        assertEquals(history, history.remind("gone", now))
        // Cleared only if still the time it fired for.
        assertEquals(now + 900, history.remind("a", null) { it.remindAt == now + 60 }.reminders().single().remindAt)
        assertEquals(emptyList<HistoryEntry>(), history.remind("a", null) { it.remindAt == now + 900 }.reminders())
    }

    @Test
    fun keepsASnoozedMessagePastTheEntryLimit() {
        var history = History().add(entry("snoozed", now), now).remind("snoozed", now + 3600)
        for (i in 1..History.MAX_ENTRIES + 5) history = history.add(entry("m$i", now + i), now + i)
        assertEquals(History.MAX_ENTRIES + 1, history.entries.size)
        assertEquals("snoozed", history.entries.last().id)
    }

    @Test
    fun decidesWhatAReminderDoes() {
        val keyed = entry("a").copy(key = "disk", channelId = "ch_1", remindAt = now + 600, arrival = 1)
        var history = History().add(keyed, now)
        assertEquals(Reminder.None, history.reminder("gone", now, 60))
        assertEquals(Reminder.Later(now + 600), history.reminder("a", now, 60))
        assertEquals(Reminder.Show(keyed, now + 600), history.reminder("a", now + 550, 60))
        // Another key or channel does not count; a newer message with its key does.
        history = history.add(entry("b", now + 1).copy(key = "disk", channelId = "ch_2"), now + 1)
            .add(entry("c", now + 1).copy(key = "load", channelId = "ch_1"), now + 1)
        assertEquals(Reminder.Show(keyed, now + 600), history.reminder("a", now + 600, 60))
        // Snoozed after a newer message with its key came.
        history = history.add(entry("d", now + 2).copy(key = "disk", channelId = "ch_1"), now + 2).remind("a", now + 600)
        assertEquals(Reminder.Drop(now + 600), history.reminder("a", now + 600, 60))
        assertEquals(Reminder.None, History().add(entry("e"), now).reminder("e", now, 60))
    }

    @Test
    fun dropsAReminderWhenANewerMessageWithItsKeyArrives() {
        val keyed = entry("a").copy(key = "disk", channelId = "ch_1")
        var history = History().add(keyed, now).remind("a", now + 600)
        history = history.add(entry("b", now + 1).copy(key = "disk", channelId = "ch_1"), now + 1)
        assertEquals(emptyList<HistoryEntry>(), history.reminders())
        // So pruning the newer one cannot bring it back.
        for (i in 2..History.MAX_ENTRIES + 5) history = history.add(entry("m$i", now + i), now + i)
        assertEquals(Reminder.None, history.reminder("a", now + 600, 60))
    }

    @Test
    fun countsALaterArrivalInTheSameSecondAsReplacing() {
        val a = entry("a").copy(key = "disk", channelId = "ch_1")
        val b = entry("b").copy(key = "disk", channelId = "ch_1")
        var history = History().add(a, now).remind("a", now + 600).add(b, now)
        assertEquals(emptyList<HistoryEntry>(), history.reminders())
        assertEquals(setOf("a"), history.replacedIds())
        // One arriving after a newer message with its key was replaced from the start.
        history = History().add(b, now).add(a.copy(sentAt = now - 1), now).remind("a", now + 600)
        assertEquals(setOf("a"), history.replacedIds())
        assertEquals(Reminder.Drop(now + 600), history.reminder("a", now + 600, 60))
    }

    @Test
    fun listsTheDueReminders() {
        val history = History().add(entry("a"), now).add(entry("b"), now).add(entry("c"), now)
            .remind("a", now + 30).remind("b", now + 120)
        assertEquals(listOf("a"), history.due(now, 60).map { it.id })
        assertEquals(setOf("a", "b"), history.due(now + 120, 60).map { it.id }.toSet())
    }

    @Test
    fun aShownReminderCountsAsReceivedThen() {
        val old = now - History.MAX_AGE_SECONDS + 10
        val later = now + 3600
        var history = History().add(entry("a", old), old).add(entry("b", now), now).remind("a", later)
        // Still for that time only.
        assertEquals(history, history.reminded("a", later + 1, later))
        history = history.reminded("a", later, later).pruned(later)
        assertEquals(listOf("a", "b"), history.entries.map { it.id })
        assertEquals(later, history.entries.first().receivedAt)
        assertTrue(history.entries.first().reminded)
        assertEquals(emptyList<HistoryEntry>(), history.reminders())
    }

    @Test
    fun ordersSameSecondReplacementsByArrivalNotTheClock() {
        val a = entry("a").copy(key = "disk", channelId = "ch_1")
        // The clock was set back between the two.
        val b = entry("b", now - 100).copy(sentAt = now, key = "disk", channelId = "ch_1")
        val history = History().add(a, now).add(b, now - 100)
        assertEquals(listOf(1L, 2L), listOf("a", "b").map { id -> history.entries.first { it.id == id }.arrival })
        assertEquals(setOf("a"), history.replacedIds())
    }

    @Test
    fun ignoresAKeyTheServerWouldRefuse() {
        val a = entry("a").copy(key = "bad key", channelId = "ch_1")
        val history = History().add(a, now).remind("a", now + 600).add(entry("b", now + 1).copy(key = "bad key", channelId = "ch_1"), now + 1)
        assertEquals(listOf("a"), history.reminders().map { it.id })
        assertEquals(emptySet<String>(), history.replacedIds())
    }

    @Test
    fun keepsASnoozedMessagePastTheAgeLimit() {
        val old = now - History.MAX_AGE_SECONDS + 10
        val history = History().add(entry("a", old), old).add(entry("b", old), old).remind("a", now)
        assertEquals(listOf("a"), history.pruned(now + 60).entries.map { it.id })
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

    @Test
    fun anOlderMessageWaitsBehindASnoozedNewerOne() {
        val b = entry("b").copy(key = "disk", channelId = "ch_1", remindAt = now + 600)
        val history = History().add(b, now).add(entry("a", now - 60).copy(sentAt = now - 60, key = "disk", channelId = "ch_1"), now + 1)
        assertTrue(history.behindReminder(history.entries.first { it.id == "a" }))
        assertEquals(false, history.behindReminder(history.entries.first { it.id == "b" }))
        val shown = history.reminded("b", now + 600, now + 600)
        assertEquals(false, shown.behindReminder(shown.entries.first { it.id == "a" }))
    }

    @Test
    fun movesTheMorningWithTheTimeZone() {
        val berlin = ZoneId.of("Europe/Berlin")
        val nine = LocalDateTime.of(2027, 1, 15, 9, 0)
        val history = History().add(entry("a"), now).add(entry("b"), now)
            .remind("a", nine.atZone(berlin).toEpochSecond(), nine)
            .remind("b", now + 900)
        val moved = history.inZone(ZoneId.of("America/New_York"))
        assertEquals(nine.atZone(ZoneId.of("America/New_York")).toEpochSecond(), moved.entries.first { it.id == "a" }.remindAt)
        assertEquals(now + 900, moved.entries.first { it.id == "b" }.remindAt)
        // Cleared with its time.
        assertEquals(null, moved.remind("a", null).entries.first { it.id == "a" }.remindLocal)
    }
}
