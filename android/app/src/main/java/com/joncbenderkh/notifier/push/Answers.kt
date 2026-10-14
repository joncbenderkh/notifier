// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.content.Context
import android.util.Log
import com.joncbenderkh.notifier.data.HistoryStore
import com.joncbenderkh.notifier.data.PairingStore
import com.joncbenderkh.notifier.data.ServerClient
import com.joncbenderkh.notifier.data.ServerException
import okhttp3.OkHttpClient
import java.io.IOException
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.TimeUnit

/** Sends this phone's answers, from a notification's buttons or the history. */
object Answers {
    private const val TAG = "Answers"

    // Within the ten seconds a notification button's broadcast may take.
    private val client = ServerClient(OkHttpClient.Builder().callTimeout(8, TimeUnit.SECONDS).build())

    // Questions being answered: a second tap meanwhile does nothing, rather
    // than wait past its broadcast's deadline.
    private val answering = ConcurrentHashMap.newKeySet<String>()

    // What this process has settled, in case the history could not keep it.
    private val settled = ConcurrentHashMap<String, String>()

    /**
     * What became of this phone's answer to [id], if that is settled: a
     * second tap must not turn "Answered: yes" into "Already answered".
     */
    fun settled(context: Context, id: String): String? = settled[id] ?: try {
        HistoryStore.get(context).outcome(id)
    } catch (e: Exception) {
        Log.w(TAG, "could not read the history", e)
        null
    }

    /**
     * Answers the question [id] with [option], as [deviceId], and shows the
     * outcome in place of its notification if that is still showing. Blocks;
     * null if the question is being answered already.
     */
    fun send(context: Context, id: String, reply: Reply, option: String, deviceId: String): ReplyOutcome? {
        if (!answering.add(id)) return null
        try {
            val known = settled(context, id)
            val outcome = if (known != null) ReplyOutcome(known, true) else attempt(context, id, reply, option, deviceId)
            if (outcome.final && known == null) {
                settled[id] = outcome.text
                try {
                    HistoryStore.get(context).settled(id, outcome.text)
                } catch (e: Exception) {
                    Log.w(TAG, "could not record the answer in the history", e)
                }
            }
            Notifications.showOutcome(context, id, outcome.text, (reply to deviceId).takeUnless { outcome.final })
            return outcome
        } finally {
            answering.remove(id)
        }
    }

    private fun attempt(context: Context, id: String, reply: Reply, option: String, deviceId: String): ReplyOutcome {
        if (option !in reply.options) return ReplyOutcome.failed("\"$option\" is not one of the answers")
        if (System.currentTimeMillis() / 1000 >= reply.expiresAt) return ReplyOutcome.EXPIRED
        val pairing = try {
            PairingStore(context).load()
        } catch (e: Exception) {
            return ReplyOutcome.failed(e.message)
        }
        // Paired again since: the question was not for this pairing.
        if (pairing?.deviceId != deviceId) return ReplyOutcome.NOT_ALLOWED
        val sealed = try {
            Replies.seal(reply.key, id, option)
        } catch (e: Exception) {
            return ReplyOutcome.failed("cannot seal it: ${e.message}")
        }
        return try {
            client.reply(pairing, id, sealed)
            ReplyOutcome.answered(option)
        } catch (e: ServerException) {
            ReplyOutcome.ofStatus(e.status, e.message)
        } catch (e: IOException) {
            ReplyOutcome.failed(e.message)
        }
    }
}
