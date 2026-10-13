// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import com.joncbenderkh.notifier.push.Payload
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json

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
        )
    }
}

/**
 * What this device received, newest first: at most [MAX_ENTRIES], none older
 * than [MAX_AGE_SECONDS]. Kept only on the device; the server stores no
 * notification content.
 */
data class History(val entries: List<HistoryEntry> = emptyList()) {
    /** Adds [entry] unless its ID is already here: the server may deliver a message twice. */
    fun add(entry: HistoryEntry, now: Long): History =
        if (entries.any { it.id == entry.id }) pruned(now)
        else History((listOf(entry) + entries).sortedByDescending { it.receivedAt }).pruned(now)

    fun remove(id: String) = History(entries.filterNot { it.id == id })

    fun pruned(now: Long) = History(entries.filter { it.receivedAt > now - MAX_AGE_SECONDS }.take(MAX_ENTRIES))

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
