// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.annotation.SuppressLint
import android.app.AlarmManager
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Build
import android.util.Log
import kotlin.concurrent.thread

/**
 * Sets the reminders' alarms again after a reboot, which clears them,
 * once the user allows exact alarms, so they show on time, and after a
 * change of time zone, which moves the morning;
 * [com.joncbenderkh.notifier.NotifierApp] does after a force stop.
 */
class BootReceiver : BroadcastReceiver() {
    private companion object {
        // A constant inlined at build time; older Android never sends it.
        @SuppressLint("InlinedApi")
        val ACTIONS = setOf(
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED,
            Intent.ACTION_TIMEZONE_CHANGED,
            AlarmManager.ACTION_SCHEDULE_EXACT_ALARM_PERMISSION_STATE_CHANGED,
        )
    }

    override fun onReceive(context: Context, intent: Intent) {
        // Exported for these system broadcasts, which no other app can send.
        if (intent.action !in ACTIONS) return
        // Unknown before Android 12, which therefore lets any app send it.
        if (intent.action == AlarmManager.ACTION_SCHEDULE_EXACT_ALARM_PERMISSION_STATE_CHANGED &&
            Build.VERSION.SDK_INT < Build.VERSION_CODES.S
        ) {
            return
        }
        val result = goAsync()
        thread(name = "reminders") {
            try {
                Reminders.rescheduleAll(context.applicationContext)
            } catch (e: Exception) {
                Log.w("BootReceiver", "could not reschedule the reminders", e)
            } finally {
                result.finish()
            }
        }
    }
}
