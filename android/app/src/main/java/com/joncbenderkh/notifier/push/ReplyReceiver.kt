// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.Uri
import kotlin.concurrent.thread

/** Answers a question from one of its notification's buttons. Not exported: only our own intents reach it. */
class ReplyReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val id = intent.getStringExtra(EXTRA_ID) ?: return
        if (intent.action == ACTION_EXPIRE) {
            Notifications.expire(context, id)
            return
        }
        val options = intent.getStringArrayExtra(EXTRA_OPTIONS)?.toList() ?: return
        val option = options.getOrNull(intent.getIntExtra(EXTRA_INDEX, -1)) ?: return
        val key = intent.getStringExtra(EXTRA_KEY) ?: return
        val deviceId = intent.getStringExtra(EXTRA_DEVICE_ID) ?: return
        val reply = Reply(options, key, intent.getLongExtra(EXTRA_EXPIRES_AT, 0))
        val receipts = intent.getBooleanExtra(EXTRA_RECEIPTS, false)
        val result = goAsync()
        thread(name = "reply") {
            try {
                Answers.send(context.applicationContext, id, reply, option, deviceId, receipts)
            } finally {
                result.finish()
            }
        }
    }

    companion object {
        private const val ACTION_EXPIRE = "com.joncbenderkh.notifier.EXPIRE"
        private const val EXTRA_ID = "id"
        private const val EXTRA_OPTIONS = "options"
        private const val EXTRA_INDEX = "index"
        private const val EXTRA_KEY = "key"
        private const val EXTRA_EXPIRES_AT = "expires_at"
        private const val EXTRA_DEVICE_ID = "device_id"
        private const val EXTRA_RECEIPTS = "receipts"

        /**
         * The button for answer [index] to message [id]'s question, received
         * as [deviceId]; [receipts] if its sender asked for them.
         */
        fun intent(context: Context, id: String, reply: Reply, index: Int, deviceId: String, receipts: Boolean): PendingIntent {
            val intent = Intent(context, ReplyReceiver::class.java)
                // Tells each message's buttons apart: Android matches cached
                // intents by their data, not their extras.
                .setData(Uri.fromParts("notifier-reply", "$id/$index", null))
                .putExtra(EXTRA_ID, id)
                .putExtra(EXTRA_OPTIONS, reply.options.toTypedArray())
                .putExtra(EXTRA_INDEX, index)
                .putExtra(EXTRA_KEY, reply.key)
                .putExtra(EXTRA_EXPIRES_AT, reply.expiresAt)
                .putExtra(EXTRA_DEVICE_ID, deviceId)
                .putExtra(EXTRA_RECEIPTS, receipts)
            return PendingIntent.getBroadcast(
                context,
                index,
                intent,
                PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
            )
        }

        /** Removes message [id]'s buttons when its question expires. */
        fun expiry(context: Context, id: String): PendingIntent = PendingIntent.getBroadcast(
            context,
            0,
            Intent(context, ReplyReceiver::class.java)
                .setAction(ACTION_EXPIRE)
                .setData(Uri.fromParts("notifier-reply", "$id/expiry", null))
                .putExtra(EXTRA_ID, id),
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
        )
    }
}
