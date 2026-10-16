// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import android.content.Context
import android.util.Log
import androidx.core.util.AtomicFile
import com.joncbenderkh.notifier.push.Payload
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import java.io.File
import java.io.IOException
import java.time.LocalDateTime
import java.time.ZoneId

/**
 * The on-device [History], encrypted at rest under its own Keystore key and
 * excluded from backups (data_extraction_rules.xml). One instance per process,
 * since the push service and the UI both write it; every call blocks, so
 * callers pick the thread. Every failure, cryptographic ones included, is an
 * [IOException].
 */
class HistoryStore private constructor(context: Context) {
    private val file = AtomicFile(File(context.filesDir, FILE_NAME))
    private val unreadable = File(context.filesDir, UNREADABLE_FILE_NAME)
    private val cipher = KeystoreCipher(KEY_ALIAS)
    private val lock = Any()
    private var history: History? = null
    private val _entries = MutableStateFlow<List<HistoryEntry>>(emptyList())

    /** Newest first; empty until [load] or the first change. */
    val entries: StateFlow<List<HistoryEntry>> = _entries.asStateFlow()

    fun load() {
        update { it.pruned(now()) }
    }

    /**
     * Records [payload] if [stillPaired] holds. It is checked under the lock
     * that [clear] takes, so a message that arrives while the phone is being
     * unpaired cannot reappear in the next pairing's history.
     */
    fun record(payload: Payload, stillPaired: () -> Boolean): Recorded {
        val now = now()
        synchronized(lock) {
            val before = current()
            // Delivered again after it was put off: it waits for its reminder.
            if (before.entries.any { it.id == payload.id && it.remindAt != null }) return Recorded(snoozed = true)
            if (!stillPaired()) return Recorded()
            update { it.add(HistoryEntry.of(payload, now), now) }
            val after = current()
            val left = after.reminders().mapTo(HashSet()) { it.id }
            val entry = after.entries.firstOrNull { it.id == payload.id }
            return Recorded(
                behindReminder = entry != null && after.behindReminder(entry),
                dropped = before.reminders().map { it.id }.filter { it !in left },
            )
        }
    }

    /**
     * What recording a message did: whether it was put off, whether a newer
     * message with its key waits for a reminder, and whose reminders it
     * dropped, by replacing them.
     */
    data class Recorded(val snoozed: Boolean = false, val behindReminder: Boolean = false, val dropped: List<String> = emptyList())

    /** What became of the answer to [id]'s question, if it is settled. */
    fun outcome(id: String): String? = entry(id)?.outcome

    /** The history as it stands. */
    fun history(): History = current()

    private fun current(): History = synchronized(lock) {
        history ?: read().also {
            history = it
            _entries.value = it.entries
        }
    }

    /** The entry for message [id], if the history still holds it. */
    fun entry(id: String): HistoryEntry? = current().entries.firstOrNull { it.id == id }

    /** The entries waiting to be shown again. */
    fun reminders(): List<HistoryEntry> = current().reminders()

    /**
     * Sets when message [id] is shown again, or never for null, at the
     * [local] time wherever the phone is if given; false if the history no
     * longer holds it.
     */
    fun remind(id: String, at: Long?, local: LocalDateTime? = null): Boolean = synchronized(lock) {
        if (entry(id) == null) return false
        update { it.remind(id, at, local) }
        true
    }

    /** Moves the reminders set for a local time to that time in [zone]; see [History.inZone]. */
    fun inZone(zone: ZoneId) {
        update { it.inZone(zone) }
    }

    /** Records that message [id]'s reminder for [at] showed; see [History.reminded]. */
    fun reminded(id: String, at: Long) {
        update { it.reminded(id, at, now()) }
    }

    /** Clears message [id]'s reminder if it is still [at]: one set again meanwhile stays. */
    fun clearReminder(id: String, at: Long) {
        update { history -> history.remind(id, null) { it.remindAt == at } }
    }

    fun settled(id: String, outcome: String) {
        update { it.settled(id, outcome) }
    }

    fun delete(id: String) {
        update { it.remove(id) }
    }

    fun clear() {
        synchronized(lock) {
            file.delete()
            unreadable.delete()
            history = History()
            _entries.value = emptyList()
        }
    }

    private fun update(change: (History) -> History) {
        synchronized(lock) {
            val before = history ?: read()
            val after = change(before)
            if (after != before) write(after)
            history = after
            _entries.value = after.entries
        }
    }

    private fun read(): History {
        if (!file.baseFile.exists()) return History()
        val sealed = file.readFully()
        return try {
            History.decode(cipher.decrypt(sealed))
        } catch (_: Exception) {
            // The Keystore can fail for a moment: once more before giving up.
            Thread.sleep(RETRY_MS)
            readAgain(sealed)
        }
    }

    private fun readAgain(sealed: ByteArray): History {
        return try {
            History.decode(cipher.decrypt(sealed))
        } catch (e: Exception) {
            // E.g. the Keystore key is gone after a lock-screen reset. Start a
            // new history, but set the old one aside in case the failure passes.
            Log.w(TAG, "history unreadable; starting a new one", e)
            if (!file.baseFile.renameTo(unreadable)) throw IOException("cannot set the unreadable history aside", e)
            History()
        }
    }

    private fun write(history: History) {
        val out = file.startWrite()
        try {
            out.write(cipher.encrypt(history.encode()))
            file.finishWrite(out)
        } catch (e: Exception) {
            file.failWrite(out)
            throw e as? IOException ?: IOException("cannot save the history", e)
        }
    }

    companion object {
        private const val FILE_NAME = "history.bin"
        private const val UNREADABLE_FILE_NAME = "history.unreadable.bin"
        private const val KEY_ALIAS = "notifier-history"
        private const val TAG = "HistoryStore"
        private const val RETRY_MS = 200L

        @Volatile private var instance: HistoryStore? = null

        fun get(context: Context): HistoryStore =
            instance ?: synchronized(this) { instance ?: HistoryStore(context.applicationContext).also { instance = it } }

        private fun now() = System.currentTimeMillis() / 1000
    }
}
