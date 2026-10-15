// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.content.Context
import android.util.Log
import com.joncbenderkh.notifier.data.PairingStore
import com.joncbenderkh.notifier.data.ServerClient
import com.joncbenderkh.notifier.data.ServerException
import java.util.concurrent.ConcurrentHashMap
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit

/**
 * Tells the sender of a message sent with receipts that this phone showed or
 * opened it. Best-effort: a receipt that cannot be sent only costs the
 * sender a receipt.
 */
object Receipts {
    const val SHOWN = "shown"
    const val OPENED = "opened"

    private const val TAG = "Receipts"

    // Short: an expired answer reports within its button's ten seconds.
    private val client = ServerClient(ServerClient.sharedClient.newBuilder().callTimeout(5, TimeUnit.SECONDS).build())
    private val background = Executors.newSingleThreadExecutor()

    // Apart from [background], so a tap need not wait behind a burst of shown reports.
    private val openings = Executors.newSingleThreadExecutor()

    // Apart from [background], so a slow report cannot delay the check
    // past the notification's dismissal.
    private val postedChecks = Executors.newSingleThreadExecutor()

    private val ledger = ReceiptLedger()

    /**
     * Reports [state] for message [id], received as [deviceId] (the current
     * pairing when null), once per process. Blocks.
     */
    fun report(context: Context, id: String, state: String, deviceId: String? = null) {
        if (!ledger.claim(id, state)) return
        try {
            val pairing = PairingStore(context).load() ?: return
            if (!ReceiptLedger.forPairing(pairing.deviceId, deviceId)) return
            client.receipt(pairing, id, state)
        } catch (e: Exception) {
            ledger.failed(id, state, (e as? ServerException)?.status)
            Log.w(TAG, "could not report message $id as $state", e)
        }
    }

    /**
     * Reports message [id] shown once its notification is, on a background
     * thread: FCM delivers messages one at a time, so they must not wait.
     */
    fun reportShownLater(context: Context, id: String, deviceId: String) {
        val app = context.applicationContext
        postedChecks.execute {
            if (Notifications.posted(app, id)) background.execute { report(app, id, SHOWN, deviceId) }
        }
    }

    /** [report], on a background thread. */
    fun reportLater(context: Context, id: String, state: String, deviceId: String? = null) {
        val app = context.applicationContext
        (if (state == OPENED) openings else background).execute { report(app, id, state, deviceId) }
    }
}

/** Which receipts this process has sent, or is sending, so each goes once. Pure JVM, for tests. */
class ReceiptLedger {
    // "<id>/<state>"
    private val sent = ConcurrentHashMap.newKeySet<String>()

    /** Whether [state] for message [id] is still to send; if so, it is claimed. */
    fun claim(id: String, state: String): Boolean = sent.add("$id/$state")

    /**
     * Sending it failed, with HTTP [status] or none: a later open may try
     * again, unless the server has no receipts for it (404), which asking
     * again does not change.
     */
    fun failed(id: String, state: String, status: Int?) {
        if (status != 404) sent.remove("$id/$state")
    }

    companion object {
        /**
         * Whether to report under the pairing as [current] device, for a
         * message received as [receivedAs] (null: the current pairing's).
         * Paired again since, it was not this pairing's message.
         */
        fun forPairing(current: String?, receivedAs: String?): Boolean =
            current != null && (receivedAs == null || receivedAs == current)
    }
}
