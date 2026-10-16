// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.app.AlarmManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.provider.Settings
import android.text.format.DateUtils
import android.util.Log
import com.joncbenderkh.notifier.data.HistoryStore
import com.joncbenderkh.notifier.data.Reminder
import java.time.Instant
import java.time.LocalDateTime
import java.time.ZoneId

/**
 * Snoozed messages: the time each shows again is kept in its history
 * entry, so deleting the entry or clearing the history cancels it, and an
 * alarm, rescheduled after a reboot ([BootReceiver]), shows it then. The
 * alarm is exact when the user allows it ([exact]); else a windowed one
 * comes within ten minutes while the phone is awake, and an inexact one,
 * allowed while idle, can come much later (Android lets it slip by up to
 * three quarters of the wait).
 */
object Reminders {
    private const val TAG = "Reminders"

    /** A reminder may show this early, as an alarm rarely fires on the second. */
    private const val EARLY_SECONDS = 60L

    /** Snoozing, cancelling and showing a reminder each read and then change it. */
    private val lock = Any()

    /** The history no longer holds the message, so nothing could show it again. */
    class NotInHistory : Exception("it is no longer in the history")

    /** A newer message with its key took its place, so it would never show again. */
    class Replaced : Exception("a newer message replaced it")

    /** As many messages as [MAX_REMINDERS] wait already. */
    class TooMany : Exception("$MAX_REMINDERS messages are snoozed already")

    /** At most this many messages wait for their reminders. */
    const val MAX_REMINDERS = 100

    /** How many other due reminders an alarm shows besides its own. */
    private const val MAX_OTHERS = 4

    /** Notifications are off, for the app or its channel, so it could not show again. */
    class NotificationsOff : Exception("notifications are turned off for it")

    /**
     * Records [payload] in the history if [stillPaired], and reports whether
     * it waits for a reminder: a repeated delivery of a snoozed message, or
     * one older than a snoozed message with its key, which FCM delivered
     * late. Cancels the alarms of the reminders it replaced. Blocks.
     */
    fun record(context: Context, payload: Payload, stillPaired: () -> Boolean): Boolean = synchronized(lock) {
        val recorded = HistoryStore.get(context).record(payload, stillPaired)
        recorded.dropped.forEach { unschedule(context, it) }
        recorded.snoozed || recorded.behindReminder
    }

    /**
     * Records [payload] (see [record]) and, unless it waits for a reminder,
     * shows it with [show], under the lock snoozing takes, so a snooze cannot
     * come between them. Reports what [show] did. Blocks.
     */
    fun receive(context: Context, payload: Payload, stillPaired: () -> Boolean, show: () -> Boolean): Boolean = synchronized(lock) {
        // The history is a convenience copy; no failure of it may cost the notification.
        val snoozed = try {
            record(context, payload, stillPaired)
        } catch (e: Exception) {
            Log.w(TAG, "could not record message ${payload.id} in the history", e)
            false
        }
        !snoozed && show()
    }

    /** Deletes message [id] from the history, with its reminder. Blocks. */
    fun delete(context: Context, id: String) = synchronized(lock) {
        unschedule(context, id)
        HistoryStore.get(context).delete(id)
    }

    /** Clears the history, and every reminder. Blocks. */
    fun clearHistory(context: Context) = synchronized(lock) {
        val store = HistoryStore.get(context)
        // Unreadable, it is cleared all the same: an alarm left finds no reminder.
        try {
            store.reminders().forEach { unschedule(context, it.id) }
        } catch (e: Exception) {
            Log.w(TAG, "could not read the reminders to cancel", e)
        }
        store.clear()
    }

    /**
     * Snoozes message [id] by [choice], as offered at [offeredAt] (Unix
     * seconds), and returns when it shows again. Blocks.
     */
    fun snooze(context: Context, id: String, choice: Snooze, offeredAt: Long): Long = synchronized(lock) {
        // The morning offered, unless it has passed since; minutes and hours from now.
        val now = System.currentTimeMillis() / 1000
        val zone = ZoneId.systemDefault()
        val at = choice.at(if (choice == Snooze.MORNING) offeredAt else now, zone)
            .takeIf { it > now } ?: choice.at(now, zone)
        // The morning is 9:00 wherever the phone is when it comes.
        val local = if (choice == Snooze.MORNING) LocalDateTime.ofInstant(Instant.ofEpochSecond(at), zone) else null
        val store = HistoryStore.get(context)
        val history = store.history()
        val entry = history.entries.firstOrNull { it.id == id } ?: throw NotInHistory()
        if (history.replaced(entry) || Notifications.keptOut(context, entry)) throw Replaced()
        // Each takes three of the 500 alarms Android allows an app.
        if (entry.remindAt == null && history.reminders().size >= MAX_REMINDERS) throw TooMany()
        if (!Notifications.canRemind(context, entry)) throw NotificationsOff()
        if (!store.remind(id, at, local)) throw NotInHistory()
        if (!schedule(context, id, at)) {
            store.remind(id, null)
            throw IllegalStateException("Android would not set the alarm")
        }
        Notifications.cancel(context, id)
        at
    }

    /** The time [at] (Unix seconds) names, with its weekday when not today. */
    fun time(context: Context, at: Long): String {
        val day = if (DateUtils.isToday(at * 1000)) 0 else DateUtils.FORMAT_SHOW_WEEKDAY or DateUtils.FORMAT_ABBREV_WEEKDAY
        return DateUtils.formatDateTime(context, at * 1000, DateUtils.FORMAT_SHOW_TIME or day)
    }

    /** Cancels message [id]'s reminder. Blocks. */
    fun cancel(context: Context, id: String) = synchronized(lock) {
        HistoryStore.get(context).remind(id, null)
        unschedule(context, id)
    }

    /**
     * Sets the alarms again: a reboot or a force stop clears them, and a
     * change of time zone moves the morning. One whose time has passed
     * fires at once. Blocks.
     */
    fun rescheduleAll(context: Context) = synchronized(lock) {
        val now = System.currentTimeMillis() / 1000
        val store = HistoryStore.get(context)
        store.inZone(ZoneId.systemDefault())
        store.reminders().forEach { e ->
            val at = e.remindAt ?: return@forEach
            when {
                at > now -> schedule(context, e.id, at)
                // Overdue: one alarm, and none while it still cannot show.
                Notifications.canRemind(context, e) -> schedule(context, e.id, at, overdue = true)
            }
        }
    }

    /**
     * Shows message [id] again if its time has come, and any other whose
     * time has: idle, Android holds back an app's inexact alarms, so the
     * next could come many minutes late. Called by its alarm. Blocks.
     */
    fun fire(context: Context, id: String) {
        val store = HistoryStore.get(context)
        val now = System.currentTimeMillis() / 1000
        // Posted under the lock; whether each shows is known only after up
        // to a second, which messages arriving meanwhile need not wait for.
        val posted = synchronized(lock) {
            // A few others, as each may wait a second to know it shows, and the
            // alarm's broadcast has a deadline; the rest have alarms of their own.
            val others = store.history().due(now, EARLY_SECONDS).map { it.id }.filter { it != id }.take(MAX_OTHERS)
            // One that fails does not hold up the others.
            (listOf(id) + others).mapNotNull {
                try {
                    fire(context, store, it, now)
                } catch (e: Exception) {
                    Log.w(TAG, "could not show the reminder for $it", e)
                    null
                }
            }
        }
        posted.forEach { (entryId, at) ->
            try {
                val shows = Notifications.posted(context, entryId)
                synchronized(lock) {
                    // Snoozed again or deleted meanwhile: that stands.
                    if (store.history().entries.firstOrNull { it.id == entryId }?.remindAt != at) return@synchronized
                    // Shown, it is cleared; not (too many showing), the next
                    // start of the app, or opening the history, tries again.
                    if (shows) store.reminded(entryId, at)
                    unschedule(context, entryId) // its other alarms, or all
                }
            } catch (e: Exception) {
                Log.w(TAG, "could not show the reminder for $entryId", e)
            }
        }
    }

    /** Posts message [id]'s reminder if due, and returns it and its time if Android took it. */
    private fun fire(context: Context, store: HistoryStore, id: String, now: Long): Pair<String, Long>? {
        when (val r = store.history().reminder(id, now, EARLY_SECONDS)) {
            Reminder.None -> {}
            is Reminder.Later -> schedule(context, id, r.at)
            is Reminder.Drop -> {
                store.clearReminder(id, r.at)
                unschedule(context, id)
            }
            is Reminder.Show -> {
                // It waits, without alarms, for the next start of the app or
                // opening of the history.
                if (!Notifications.canRemind(context, r.entry)) {
                    unschedule(context, id)
                    return null
                }
                when (Notifications.remind(context, r.entry)) {
                    Notifications.Posted.SHOWN -> return id to r.at
                    // Its key's notification shows a newer message, which the
                    // history no longer holds.
                    Notifications.Posted.REPLACED -> {
                        store.clearReminder(id, r.at)
                        unschedule(context, id)
                    }
                    // Notifications off: the next start of the app, or
                    // opening the history, tries again.
                    Notifications.Posted.HIDDEN -> unschedule(context, id)
                }
            }
        }
        return null
    }

    /**
     * Whether reminders show on time: Android lets an app set exact alarms
     * only with "Alarms & reminders" ([settings]), off by default since
     * Android 14. Without it they may show late: within ten minutes while
     * the phone is awake, later while it dozes.
     */
    fun exact(context: Context): Boolean =
        Build.VERSION.SDK_INT < Build.VERSION_CODES.S || context.getSystemService(AlarmManager::class.java).canScheduleExactAlarms()

    /** Where the user lets this app set exact alarms. */
    fun settings(context: Context): Intent =
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) {
            Intent(Settings.ACTION_REQUEST_SCHEDULE_EXACT_ALARM, Uri.fromParts("package", context.packageName, null))
        } else {
            Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.fromParts("package", context.packageName, null))
        }

    /**
     * Sets message [id]'s alarms for [at], and reports whether Android took
     * them. An exact alarm comes with the inexact ones: turning "Alarms &
     * reminders" off cancels the exact ones, without telling the app.
     */
    private fun schedule(context: Context, id: String, at: Long, overdue: Boolean = false): Boolean {
        val alarms = context.getSystemService(AlarmManager::class.java)
        return try {
            alarms.setAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at * 1000, alarm(context, id, IDLE))
            if (overdue) return true // fires at once
            alarms.setWindow(AlarmManager.RTC_WAKEUP, at * 1000, WINDOW_MS, alarm(context, id, WINDOW))
            if (exact(context)) alarms.setExactAndAllowWhileIdle(AlarmManager.RTC_WAKEUP, at * 1000, alarm(context, id, EXACT))
            true
        } catch (e: SecurityException) {
            // "Alarms & reminders" turned off since [exact] asked: the
            // inexact one is set.
            Log.w(TAG, "could not set an exact alarm for $id", e)
            true
        } catch (e: IllegalStateException) {
            // Too many alarms set: none, rather than some.
            Log.w(TAG, "could not set the reminder for $id", e)
            unschedule(context, id)
            false
        }
    }

    private fun unschedule(context: Context, id: String) {
        val alarms = context.getSystemService(AlarmManager::class.java)
        for (kind in listOf(IDLE, WINDOW, EXACT)) alarms.cancel(alarm(context, id, kind))
    }

    // A message's alarms, told apart by the fragment of their intents' data.
    private val IDLE: String? = null
    private const val WINDOW = "window"
    private const val EXACT = "exact"

    /** The shortest window Android grants since Android 12. */
    private const val WINDOW_MS = 10 * 60 * 1000L

    private fun alarm(context: Context, id: String, kind: String?): PendingIntent = PendingIntent.getBroadcast(
        context,
        0,
        Intent(context, ReminderReceiver::class.java)
            // Several alarms per message: Android tells intents apart by their data.
            .setData(Uri.fromParts("notifier-remind", id, kind)),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )
}
