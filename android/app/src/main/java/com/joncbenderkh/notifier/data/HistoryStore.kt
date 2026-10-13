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
    fun record(payload: Payload, stillPaired: () -> Boolean) {
        val now = now()
        synchronized(lock) {
            if (stillPaired()) update { it.add(HistoryEntry.of(payload, now), now) }
        }
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

        @Volatile private var instance: HistoryStore? = null

        fun get(context: Context): HistoryStore =
            instance ?: synchronized(this) { instance ?: HistoryStore(context.applicationContext).also { instance = it } }

        private fun now() = System.currentTimeMillis() / 1000
    }
}
