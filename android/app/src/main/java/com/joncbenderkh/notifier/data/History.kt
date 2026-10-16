// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import com.joncbenderkh.notifier.push.LinkAction
import com.joncbenderkh.notifier.push.NotificationTag
import com.joncbenderkh.notifier.push.Payload
import com.joncbenderkh.notifier.push.Reply
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.time.LocalDateTime
import java.time.ZoneId

/** One notification this device received, as kept in its history. Times are Unix seconds. */
@Serializable
data class HistoryEntry(
    val id: String,
    val title: String,
    val body: String = "",
    val priority: String = "default",
    val sentAt: Long,
    val receivedAt: Long,
    /** As this user names it ("homelab", "alice/homelab"); null for a direct send. */
    val channel: String? = null,
    val channelId: String? = null,
    /** The question this phone may answer, if it asked one. */
    val reply: Reply? = null,
    /** What became of this phone's answer ("Answered: yes", "Already answered"), once settled. */
    val outcome: String? = null,
    /** Whether the sender asked to know when it is opened. */
    val receipts: Boolean = false,
    /** The link it opens, and its buttons' links. */
    val url: String? = null,
    val actions: List<LinkAction> = emptyList(),
    /** When a snoozed message is shown again (Unix seconds); null for none. */
    val remindAt: Long? = null,
    /**
     * The local time [remindAt] names ("2026-10-17T09:00"), for a reminder
     * that comes at that time wherever the phone is; null for one that
     * comes after a while.
     */
    val remindLocal: String? = null,
    /** Its key: a later message with the same key, in the same channel, replaces it. */
    val key: String? = null,
    /** Whether it was snoozed and shown again: [receivedAt] is then when. */
    val reminded: Boolean = false,
    /** Its place in the order of arrival, 1 for the first; 0 in histories from before it was kept. */
    val arrival: Long = 0,
) {
    companion object {
        fun of(payload: Payload, receivedAt: Long) = HistoryEntry(
            payload.id,
            payload.title,
            payload.body,
            payload.priority,
            payload.sentAt,
            receivedAt,
            payload.channel,
            payload.channelId,
            payload.reply,
            receipts = payload.receipts,
            url = payload.url,
            actions = payload.actions,
            key = payload.key,
        )
    }
}

/**
 * What recording a message did: whether it was put off, whether a newer
 * message with its key waits for a reminder, and whose reminders it
 * dropped, by replacing them.
 */
data class Recorded(val snoozed: Boolean = false, val behindReminder: Boolean = false, val dropped: List<String> = emptyList())

/**
 * What this device received, newest first: at most [MAX_ENTRIES], none older
 * than [MAX_AGE_SECONDS], besides those waiting for a reminder. Kept only on
 * the device; the server stores no notification content.
 */
data class History(val entries: List<HistoryEntry> = emptyList()) {
    /**
     * This history with [entry], received at [now], and what recording it
     * did. Delivered again after it was put off, it waits for its reminder:
     * nothing changes.
     */
    fun record(entry: HistoryEntry, now: Long): Pair<History, Recorded> {
        if (entries.any { it.id == entry.id && it.remindAt != null }) return this to Recorded(snoozed = true)
        val after = add(entry, now)
        val left = after.reminders().mapTo(HashSet()) { it.id }
        val kept = after.entries.firstOrNull { it.id == entry.id }
        return after to Recorded(
            behindReminder = kept != null && after.behindReminder(kept),
            dropped = reminders().map { it.id }.filter { it !in left },
        )
    }

    /** Adds [entry] unless its ID is already here: the server may deliver a message twice. */
    fun add(entry: HistoryEntry, now: Long): History =
        if (entries.any { it.id == entry.id }) pruned(now)
        else History((listOf(entry.copy(arrival = (entries.maxOfOrNull { it.arrival } ?: 0) + 1)) + entries).sortedByDescending { it.receivedAt })
            .withoutReplacedReminders().pruned(now)

    /**
     * Whether a later message with [entry]'s key, in its channel, took its
     * place: one sent later, or in the same second and received later, as
     * [com.joncbenderkh.notifier.push.NotificationTag] decides.
     */
    fun replaced(entry: HistoryEntry): Boolean = replacements(entry).any()

    /**
     * Whether a later message with [entry]'s key, in its channel, waits for
     * its reminder, which would take its place: [entry] is not shown
     * meanwhile, though no notification may hold the key.
     */
    fun behindReminder(entry: HistoryEntry): Boolean = replacements(entry).any { it.remindAt != null }

    private fun replacements(entry: HistoryEntry): Sequence<HistoryEntry> {
        val key = entry.key?.takeIf(NotificationTag::validKey) ?: return emptySequence()
        val index = entries.indexOfFirst { it.id == entry.id }
        return entries.asSequence().withIndex().filter { (i, it) ->
            it.id != entry.id && it.key == key && it.channelId == entry.channelId &&
                (it.sentAt > entry.sentAt || it.sentAt == entry.sentAt && arrivedLater(it, i, entry, index))
        }.map { it.value }
    }

    // By arrival, not the clock, which can be set back; failing that (from
    // before arrivals were kept), newest first, the later of two received
    // in the same second comes first.
    private fun arrivedLater(a: HistoryEntry, i: Int, b: HistoryEntry, j: Int) =
        if (a.arrival != b.arrival) a.arrival > b.arrival else i < j

    /** The IDs of the messages a later one replaced. */
    fun replacedIds(): Set<String> = entries.filter { it.key != null && replaced(it) }.mapTo(HashSet()) { it.id }

    // Dropped when the message replacing it arrives, not when it fires, as
    // pruning may remove that message first.
    private fun withoutReplacedReminders() =
        History(entries.map { if (it.remindAt != null && replaced(it)) it.copy(remindAt = null, remindLocal = null) else it })

    fun remove(id: String) = History(entries.filterNot { it.id == id })

    /** Records what became of the answer to [id]'s question; the first outcome stays. */
    fun settled(id: String, outcome: String) =
        History(entries.map { if (it.id == id && it.reply != null && it.outcome == null) it.copy(outcome = outcome) else it })

    /**
     * Sets when [id] is shown again, or never for null, at the [local] time
     * wherever the phone is if given; with [only], only if that holds for
     * its entry.
     */
    fun remind(id: String, at: Long?, local: LocalDateTime? = null, only: (HistoryEntry) -> Boolean = { true }) =
        History(entries.map { if (it.id == id && only(it)) it.copy(remindAt = at, remindLocal = local?.toString()) else it })

    /** The reminders set for a local time moved to that time in [zone], after a change of time zone. */
    fun inZone(zone: ZoneId) = History(
        entries.map { e ->
            val local = e.remindLocal?.let { runCatching { LocalDateTime.parse(it) }.getOrNull() }
            if (e.remindAt == null || local == null) e else e.copy(remindAt = local.atZone(zone).toEpochSecond())
        },
    )

    /** The entries waiting to be shown again. */
    fun reminders() = entries.filter { it.remindAt != null }

    /** The entries whose reminders are due at [now] (Unix seconds), [early] seconds early counting. */
    fun due(now: Long, early: Long) = entries.filter { e -> e.remindAt.let { it != null && it <= now + early } }

    /**
     * Records that [id]'s reminder for [at] showed at [now]: it counts as
     * received then, so it stays its 30 days, and its notification's
     * Snooze finds it.
     */
    fun reminded(id: String, at: Long, now: Long) = History(
        entries.map { if (it.id == id && it.remindAt == at) it.copy(remindAt = null, remindLocal = null, receivedAt = now, reminded = true) else it }
            .sortedByDescending { it.receivedAt },
    )

    /** What message [id]'s alarm should do at [now] (Unix seconds); [early] seconds early still count. */
    fun reminder(id: String, now: Long, early: Long): Reminder {
        val entry = entries.firstOrNull { it.id == id } ?: return Reminder.None // deleted, or the history cleared
        val at = entry.remindAt ?: return Reminder.None
        if (at > now + early) return Reminder.Later(at) // a stale alarm, from before it was snoozed again
        // A newer message with its key took its place meanwhile.
        return if (replaced(entry)) Reminder.Drop(at) else Reminder.Show(entry, at)
    }

    // A snoozed message stays until its reminder shows, outside the limits.
    fun pruned(now: Long): History {
        val kept = entries.filter { it.remindAt == null && it.receivedAt > now - MAX_AGE_SECONDS }.take(MAX_ENTRIES).toSet()
        return History(entries.filter { it.remindAt != null || it in kept })
    }

    fun encode(): ByteArray = json.encodeToString(entries).encodeToByteArray()

    companion object {
        const val MAX_ENTRIES = 500
        const val MAX_AGE_SECONDS = 30L * 24 * 60 * 60

        private val json = Json { ignoreUnknownKeys = true }

        /** Unreadable data is an empty history, not an error: it is only a convenience copy. */
        fun decode(bytes: ByteArray): History = try {
            History(json.decodeFromString<List<HistoryEntry>>(bytes.decodeToString()))
        } catch (_: IllegalArgumentException) {
            History()
        }
    }
}

/** What a reminder's alarm should do ([History.reminder]). */
sealed interface Reminder {
    data object None : Reminder

    /** Not yet: set the alarm again for [at]. */
    data class Later(val at: Long) : Reminder

    /** Replaced by a newer message with its key: clear it. */
    data class Drop(val at: Long) : Reminder

    data class Show(val entry: HistoryEntry, val at: Long) : Reminder
}
