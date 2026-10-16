// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.util.Log
import kotlin.concurrent.thread

/** Shows a snoozed message again when its alarm fires. Not exported: only our own alarms reach it. */
class ReminderReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        val id = intent.data?.schemeSpecificPart ?: return
        val result = goAsync()
        thread(name = "reminder") {
            try {
                Reminders.fire(context.applicationContext, id)
            } catch (e: Exception) {
                Log.w("ReminderReceiver", "could not show the reminder for $id", e)
            } finally {
                result.finish()
            }
        }
    }
}
