// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.annotation.SuppressLint
import android.app.AlarmManager
import android.app.NotificationManager
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.os.Build
import android.util.Log
import kotlin.concurrent.thread

/**
 * Sets the reminders' alarms again after a reboot, which clears them,
 * once the user allows exact alarms, so they show on time, and after a
 * change of time zone, which moves the morning, and once notifications
 * are let through again, so a reminder held back meanwhile shows;
 * [com.joncbenderkh.notifier.NotifierApp] does after a force stop.
 */
class BootReceiver : BroadcastReceiver() {
    private companion object {
        // Constants inlined at build time; older Android never sends them.
        @SuppressLint("InlinedApi")
        val SINCE_S = setOf(AlarmManager.ACTION_SCHEDULE_EXACT_ALARM_PERMISSION_STATE_CHANGED)

        @SuppressLint("InlinedApi")
        val SINCE_P = setOf(
            NotificationManager.ACTION_APP_BLOCK_STATE_CHANGED,
            NotificationManager.ACTION_NOTIFICATION_CHANNEL_BLOCK_STATE_CHANGED,
            NotificationManager.ACTION_NOTIFICATION_CHANNEL_GROUP_BLOCK_STATE_CHANGED,
        )

        val ACTIONS = setOf(
            Intent.ACTION_BOOT_COMPLETED,
            Intent.ACTION_MY_PACKAGE_REPLACED,
            Intent.ACTION_TIMEZONE_CHANGED,
        ) + SINCE_S + SINCE_P
    }

    override fun onReceive(context: Context, intent: Intent) {
        // Exported for these system broadcasts, which no other app can send.
        if (intent.action !in ACTIONS) return
        // Unknown to older Android, which therefore lets any app send them.
        if (intent.action in SINCE_S && Build.VERSION.SDK_INT < Build.VERSION_CODES.S) return
        if (intent.action in SINCE_P && Build.VERSION.SDK_INT < Build.VERSION_CODES.P) return
        val result = goAsync()
        thread(name = "reminders") {
            try {
                Reminders.rescheduleAllOrRetry(context.applicationContext)
            } catch (e: Exception) {
                Log.w("BootReceiver", "could not reschedule the reminders", e)
            } finally {
                result.finish()
            }
        }
    }
}
