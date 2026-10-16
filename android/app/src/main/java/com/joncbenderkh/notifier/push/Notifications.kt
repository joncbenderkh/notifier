// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.Manifest
import android.app.AlarmManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationChannelGroup
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.ContentResolver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.media.AudioAttributes
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.provider.Settings
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import com.joncbenderkh.notifier.MainActivity
import com.joncbenderkh.notifier.R
import com.joncbenderkh.notifier.data.HistoryEntry
import com.joncbenderkh.notifier.data.Links
import com.joncbenderkh.notifier.data.PairingStore

object Notifications {
    /** Set on the intent a tapped notification opens: show the history. */
    const val EXTRA_OPEN_HISTORY = "com.joncbenderkh.notifier.OPEN_HISTORY"

    /** The question's title, kept on its notification while the title shows an outcome. */
    private const val EXTRA_QUESTION = "com.joncbenderkh.notifier.QUESTION"

    /** The message a notification shows, whose ID its tag is not when it has a key (see [NotificationTag]). */
    private const val EXTRA_MESSAGE_ID = "com.joncbenderkh.notifier.SHOWN_MESSAGE_ID"

    /** When its message was sent (ms), which a reminder's shown time is not. */
    private const val EXTRA_SENT_AT = "com.joncbenderkh.notifier.SENT_AT"

    /** Guards each lookup of a notification and the post that follows it, and [showing]. */
    private val lock = Any()

    /**
     * The message and send time each keyed tag last showed in this
     * process: Android posts asynchronously, so its active list can still
     * show the message just replaced.
     */
    private val showing = object : LinkedHashMap<String, Pair<String, Long>>() {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, Pair<String, Long>>) = size > MAX_SHOWING
    }
    private const val MAX_SHOWING = 100

    /** Keyed messages another replaced in this process: a repeated delivery of one must not return. */
    private val replaced = object : LinkedHashMap<String, Unit>() {
        override fun removeEldestEntry(eldest: MutableMap.MutableEntry<String, Unit>) = size > MAX_SHOWING
    }

    /** As many buttons as Android shows on a notification. */
    private const val MAX_BUTTONS = 3

    /** The label of the button that snoozes a message. */
    private const val SNOOZE = "Snooze"

    private const val POSTED_CHECKS = 10
    private const val POSTED_PAUSE_MS = 100L

    private val SOUND = AudioAttributes.Builder()
        .setUsage(AudioAttributes.USAGE_NOTIFICATION)
        .setContentType(AudioAttributes.CONTENT_TYPE_SONIFICATION)
        .build()

    fun createChannels(context: Context) {
        val manager = context.getSystemService(NotificationManager::class.java)
        createKinds(context, manager, null)
        // Groups from before notifier had sounds get them now, not on their next message.
        manager.notificationChannelGroups
            .filter { it.id.startsWith(ChannelRoute.GROUP_PREFIX) }
            .forEach { createKinds(context, manager, it.id) }
    }

    /**
     * Shows [payload], received as [deviceId], with a button for each answer
     * it may give; once [outcome] settles the question, that outcome instead,
     * quietly, as [showOutcome] left it. False when the user does not let
     * it show, or a newer message with its key shows; Android can still
     * drop it (see [posted]).
     */
    fun show(context: Context, payload: Payload, deviceId: String, outcome: String?): Boolean =
        post(context, payload, deviceId, outcome) == Posted.SHOWN

    /** What became of a notification: shown, not shown, or kept out by a newer one with its key. */
    enum class Posted { SHOWN, HIDDEN, REPLACED }

    /**
     * Posts [payload]: dated [shownAt] (ms) rather than when sent, if given;
     * without [regroup], in its channel's group only while that still
     * exists, else as a direct message would be.
     */
    private fun post(
        context: Context,
        payload: Payload,
        deviceId: String,
        outcome: String?,
        shownAt: Long? = null,
        regroup: Boolean = true,
    ): Posted {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) return Posted.HIDDEN

        val high = payload.priority == "high"
        val route = route(context, payload, regroup)
        if (route.groupId != null) {
            // A reminder's label may be out of date: it leaves the name as it is.
            if (regroup) createGroup(context, route.groupId, route.groupName ?: route.groupId)
            else createKinds(context, context.getSystemService(NotificationManager::class.java), route.groupId)
        }
        val text = NotificationText.of(payload, outcome)
        val channelId = channelFor(context, route)
        val builder = NotificationCompat.Builder(context, channelId)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(text.title)
            .setContentText(text.text)
            // Tells the user that opening it tells the sender.
            .setSubText(
                listOfNotNull(payload.channel?.takeIf { route.groupId != null }, "read receipt".takeIf { payload.receipts })
                    .joinToString(" · ")
                    .ifEmpty { null },
            )
            .setStyle(NotificationCompat.BigTextStyle().bigText(text.text))
            .setSilent(text.settled)
            .setPriority(if (high) NotificationCompat.PRIORITY_HIGH else NotificationCompat.PRIORITY_DEFAULT)
            .setWhen(shownAt ?: (payload.sentAt * 1000))
            .setShowWhen(true)
            .setContentIntent(open(context, payload, deviceId, Links.web(payload.url), "notifier-open"))
            .setAutoCancel(true)
            .addExtras(
                Bundle().apply {
                    putString(EXTRA_QUESTION, payload.title)
                    putString(EXTRA_MESSAGE_ID, payload.id)
                    putLong(EXTRA_SENT_AT, payload.sentAt * 1000)
                },
            )
        val reply = payload.reply?.takeUnless { text.settled }
        val answers = reply != null && addAnswers(context, builder, payload.id, reply, deviceId, payload.receipts)
        var buttons = 0
        // The server sends none with a question, whose answers are the buttons.
        if (payload.reply == null) {
            payload.actions.forEachIndexed { i, action ->
                val link = Links.web(action.url) ?: return@forEachIndexed
                if (action.label.isBlank()) return@forEachIndexed
                // Where it leads, as the history shows it: the sender chose both.
                builder.addAction(0, "${action.label} · ${Links.host(link)}", open(context, payload, deviceId, link, "notifier-link", "/$i"))
                buttons++
            }
        }
        // Not for an open question, nor in place of a link: Android shows
        // three buttons, and the history card offers it too.
        if (!answers && buttons < MAX_BUTTONS) builder.addAction(0, SNOOZE, snooze(context, payload.id))
        val manager = NotificationManagerCompat.from(context)
        val shows = allowed(context, channelId)
        var tag = NotificationTag.of(payload, deviceId)
        synchronized(lock) {
            // A reminder posted again, when it was not known to show: once is enough.
            if (shownAt != null && active(manager, payload.id) != null) builder.setOnlyAlertOnce(true)
            if (tag != payload.id) {
                val shown = manager.activeNotifications.firstOrNull { it.tag == tag && it.id == 0 }?.notification
                val current = showing[tag]?.first ?: shown?.extras?.getString(EXTRA_MESSAGE_ID)
                val sentAt = showing[tag]?.second ?: shown?.let { it.extras.getLong(EXTRA_SENT_AT, it.`when`) }
                // FCM may deliver out of order, or twice: an older message
                // keeps out of a newer one's place.
                if (payload.id in replaced) return Posted.REPLACED
                if (sentAt != null && NotificationTag.newer(sentAt, payload.sentAt)) return Posted.REPLACED
                if (current != null && current != payload.id && Answers.isAnswering(current)) {
                    // Its outcome is about to take its place: this one shows apart.
                    tag = payload.id
                } else if (shows) {
                    // Android drops a post to a channel the user turned off,
                    // and what shows under the tag stays the one to update.
                    if (current != null && current != payload.id) replaced[current] = Unit
                    showing[tag] = payload.id to payload.sentAt * 1000
                }
            }
            manager.notify(tag, 0, builder.build())
        }
        if (answers && reply != null) expireAt(context, payload.id, reply)
        return if (shows) Posted.SHOWN else Posted.HIDDEN
    }

    /**
     * Whether message [id]'s notification is showing: Android drops one when
     * too many show or the app posts too often. It posts asynchronously, so
     * this blocks for up to a second.
     */
    fun posted(context: Context, id: String): Boolean {
        val manager = NotificationManagerCompat.from(context)
        repeat(POSTED_CHECKS) {
            if (synchronized(lock) { active(manager, id) } != null) return true
            Thread.sleep(POSTED_PAUSE_MS)
        }
        return false
    }

    /**
     * Message [id]'s notification, if showing: not once a message with the
     * same key replaced it, even before Android lists the replacement.
     * Called holding [lock].
     */
    private fun active(manager: NotificationManagerCompat, id: String) =
        manager.activeNotifications.firstOrNull {
            it.id == 0 && (it.notification.extras.getString(EXTRA_MESSAGE_ID) ?: it.tag) == id
        }?.takeIf { showing[it.tag]?.first.let { shown -> shown == null || shown == id } }

    /** Removes message [id]'s notification, if it is still showing. */
    fun cancel(context: Context, id: String) {
        val manager = NotificationManagerCompat.from(context)
        synchronized(lock) { active(manager, id)?.let { manager.cancel(it.tag, 0) } }
    }

    /** Whether notifications in [channelId] reach the user: not turned off for the app, the channel or its group. */
    private fun allowed(context: Context, channelId: String): Boolean {
        val manager = context.getSystemService(NotificationManager::class.java)
        if (!manager.areNotificationsEnabled()) return false
        val channel = manager.getNotificationChannel(channelId) ?: return false
        if (channel.importance == NotificationManager.IMPORTANCE_NONE) return false
        val group = channel.group ?: return true
        return Build.VERSION.SDK_INT < Build.VERSION_CODES.P || manager.getNotificationChannelGroup(group)?.isBlocked != true
    }

    /** Adds a button per answer, unless the question has expired; true if it did. */
    private fun addAnswers(context: Context, builder: NotificationCompat.Builder, id: String, reply: Reply, deviceId: String, receipts: Boolean): Boolean {
        if (System.currentTimeMillis() / 1000 >= reply.expiresAt) return false
        reply.options.forEachIndexed { i, option ->
            builder.addAction(0, option, ReplyReceiver.intent(context, id, reply, i, deviceId, receipts))
        }
        return true
    }

    /** Has the answer buttons of question [id], once posted, removed when it expires. */
    private fun expireAt(context: Context, id: String, reply: Reply) {
        // Inexact: the buttons may outlive the question a little, and a tap
        // then says it expired.
        context.getSystemService(AlarmManager::class.java)
            .set(AlarmManager.RTC, reply.expiresAt * 1000, ReplyReceiver.expiry(context, id))
    }

    /** Removes the answer buttons from message [id]'s notification, if it is still showing. */
    fun expire(context: Context, id: String) {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) return
        val manager = NotificationManagerCompat.from(context)
        synchronized(lock) {
            val shown = active(manager, id) ?: return
            // Answered, or posted expired: only Snooze, nothing to remove.
            if (shown.notification.actions.orEmpty().all { it.title == SNOOZE }) return
            val notification = Notification.Builder.recoverBuilder(context, shown.notification)
                // An expired question can be put off, as one that arrived expired.
                .setActions(Notification.Action.Builder(null, SNOOZE, snooze(context, id)).build())
                .setOnlyAlertOnce(true)
                .build()
            manager.notify(shown.tag, 0, notification)
        }
    }

    /**
     * Shows [outcome] in place of message [id]'s notification, with its
     * question below, if it is still showing; quietly, since it answers a
     * tap. [retry], for an outcome that may still change, keeps the buttons
     * ([receipts] if the sender asked for them).
     */
    fun showOutcome(context: Context, id: String, outcome: String, retry: Pair<Reply, String>? = null, receipts: Boolean = false) {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) return
        val manager = NotificationManagerCompat.from(context)
        synchronized(lock) {
            val shown = active(manager, id) ?: return
            val extras = shown.notification.extras
            val question = extras.getString(EXTRA_QUESTION) ?: extras.getCharSequence(NotificationCompat.EXTRA_TITLE)?.toString()
            val builder = NotificationCompat.Builder(context, shown.notification.channelId)
                .setSmallIcon(R.drawable.ic_notification)
                .setContentTitle(outcome)
                .setContentText(question)
                .addExtras(
                    Bundle().apply {
                        putString(EXTRA_QUESTION, question)
                        putString(EXTRA_MESSAGE_ID, id)
                        putLong(EXTRA_SENT_AT, extras.getLong(EXTRA_SENT_AT, shown.notification.`when`))
                    },
                )
                .setSubText(shown.notification.extras.getCharSequence(NotificationCompat.EXTRA_SUB_TEXT))
                .setWhen(shown.notification.`when`)
                .setShowWhen(true)
                .setSilent(true)
                // The tap still reports a message sent with receipts opened.
                .setContentIntent(shown.notification.contentIntent ?: openHistory(context))
                .setAutoCancel(true)
            val answers = retry != null && addAnswers(context, builder, id, retry.first, retry.second, receipts)
            // Settled, it can be put off, as one that arrives settled.
            if (!answers) builder.addAction(0, SNOOZE, snooze(context, id))
            manager.notify(shown.tag, 0, builder.build())
            if (answers && retry != null) expireAt(context, id, retry.first)
        }
    }

    /**
     * Opens [link], or the history when it is null, through [OpenedActivity],
     * which also reports the message opened if its sender asked; for a
     * message with neither, just the history. Each message and button gets
     * its own intent, by its data ([scheme]:<id>[part]), since Android
     * matches cached intents ignoring extras.
     */
    private fun open(context: Context, payload: Payload, deviceId: String, link: String?, scheme: String, part: String = ""): PendingIntent {
        if (link == null) return openHistory(context, (payload.id to deviceId).takeIf { payload.receipts })
        val intent = Intent(context, OpenedActivity::class.java)
            .setData(Uri.fromParts(scheme, payload.id + part, null))
            .putExtra(OpenedActivity.EXTRA_URL, link)
        if (payload.receipts) {
            intent.putExtra(OpenedActivity.EXTRA_MESSAGE_ID, payload.id).putExtra(OpenedActivity.EXTRA_DEVICE_ID, deviceId)
        }
        if (part.isNotEmpty()) intent.putExtra(OpenedActivity.EXTRA_DISMISS, payload.id)
        return PendingIntent.getActivity(context, 0, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }

    /**
     * Shows [entry] again, as its reminder: without answers or receipts,
     * dated now, with what became of its question, and under its key, so
     * a newer message with the key replaces it. [posted] tells whether
     * Android shows it.
     */
    fun remind(context: Context, entry: HistoryEntry): Posted {
        val deviceId = PairingStore(context).load()?.deviceId ?: return Posted.HIDDEN
        // A channel left since keeps its group gone from the system settings.
        return post(context, reminder(entry), deviceId, outcome = null, shownAt = System.currentTimeMillis(), regroup = false)
    }

    /**
     * Whether a reminder of [entry] could show: the user lets the app, and
     * the channel and group it would show in, post notifications.
     */
    fun canRemind(context: Context, entry: HistoryEntry): Boolean {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) return false
        val manager = context.getSystemService(NotificationManager::class.java)
        if (!manager.areNotificationsEnabled()) return false
        val channelId = channelFor(context, route(context, reminder(entry), regroup = false))
        // One not created yet is, when it shows.
        return manager.getNotificationChannel(channelId) == null || allowed(context, channelId)
    }

    /**
     * Whether a newer message showing with [entry]'s key would keep its
     * reminder out, as [post] decides; the history may no longer hold it.
     */
    fun keptOut(context: Context, entry: HistoryEntry): Boolean {
        val deviceId = PairingStore(context).load()?.deviceId ?: return false
        val payload = reminder(entry)
        val tag = NotificationTag.of(payload, deviceId)
        if (tag == payload.id) return false
        val manager = NotificationManagerCompat.from(context)
        synchronized(lock) {
            if (payload.id in replaced) return true
            val shown = manager.activeNotifications.firstOrNull { it.tag == tag && it.id == 0 }?.notification
            val sentAt = showing[tag]?.second ?: shown?.let { it.extras.getLong(EXTRA_SENT_AT, it.`when`) }
            return sentAt != null && NotificationTag.newer(sentAt, payload.sentAt)
        }
    }

    /** Where [payload] shows: without [regroup], in its channel's group only while that still exists. */
    private fun route(context: Context, payload: Payload, regroup: Boolean): ChannelRoute {
        val route = ChannelRoute.of(payload)
        if (regroup || route.groupId == null) return route
        val groups = context.getSystemService(NotificationManager::class.java).notificationChannelGroups
        return if (groups.any { it.id == route.groupId }) route else ChannelRoute.of(payload.copy(channel = null, channelId = null))
    }

    /** The message [entry] holds, as a reminder shows it. */
    private fun reminder(entry: HistoryEntry) = Payload(
        id = entry.id,
        title = entry.title,
        // Only a settled or expired question is snoozed.
        body = listOfNotNull(
            entry.outcome ?: "The question has expired".takeIf { entry.reply != null },
            entry.body.ifEmpty { null },
        ).joinToString("\n"),
        priority = entry.priority,
        sentAt = entry.sentAt,
        channel = entry.channel,
        channelId = entry.channelId,
        url = entry.url,
        actions = entry.actions,
        key = entry.key,
    )

    /** The "Snooze" button: asks when to show message [id] again. */
    private fun snooze(context: Context, id: String): PendingIntent = PendingIntent.getActivity(
        context,
        0,
        Intent(context, SnoozeActivity::class.java).setData(Uri.fromParts("notifier-snooze", id, null)),
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )

    /**
     * Opens the history; for [tracked], a message ID and the device that
     * received it, through [OpenedActivity], which also reports the message
     * opened. Each tracked message gets its own intent (by its data), since
     * Android matches cached intents ignoring extras.
     */
    private fun openHistory(context: Context, tracked: Pair<String, String>? = null): PendingIntent {
        val intent = if (tracked == null) {
            Intent(context, MainActivity::class.java).putExtra(EXTRA_OPEN_HISTORY, true)
        } else {
            Intent(context, OpenedActivity::class.java)
                .setData(Uri.fromParts("notifier-open", tracked.first, null))
                .putExtra(OpenedActivity.EXTRA_MESSAGE_ID, tracked.first)
                .putExtra(OpenedActivity.EXTRA_DEVICE_ID, tracked.second)
        }
        // UPDATE_CURRENT: an intent cached without the extras must not be reused.
        return PendingIntent.getActivity(context, 0, intent, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
    }

    /** Creates (or renames) a Notifier channel's group and its channels. */
    private fun createGroup(context: Context, groupId: String, name: String) {
        val manager = context.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannelGroup(NotificationChannelGroup(groupId, name))
        createKinds(context, manager, groupId)
    }

    /**
     * Creates a channel per kind of message, in [groupId] or ungrouped.
     * Recreating an existing channel keeps the user's settings. A channel
     * from before notifier had sounds is replaced: its successor takes its
     * settings, and its sound if the user chose one. It is deleted once no
     * notification shows in it, since deleting a channel removes them.
     */
    private fun createKinds(context: Context, manager: NotificationManager, groupId: String?) {
        val id = groupId?.removePrefix(ChannelRoute.GROUP_PREFIX)
        val showing = manager.activeNotifications.map { it.notification.channelId }.toSet()
        val retired = mutableSetOf<String>()
        val channels = ChannelRoute.Kind.entries.mapNotNull { kind ->
            val channelId = ChannelRoute.channelId(kind, id)
            val old = manager.getNotificationChannel(ChannelRoute.legacyChannelId(kind, id))
            val existing = manager.getNotificationChannel(channelId)
            val fresh = existing == null
            // Checked at every start: the user may change an old channel
            // that stayed for a notification still showing in it.
            if (old != null && keeps(old)) {
                if (fresh) return@mapNotNull null
            } else if (old != null && old.id !in showing) {
                retired += old.id
            }
            // An existing channel's importance as it is: recreating it with
            // a lower one would lower it, unless the user set it there.
            NotificationChannel(channelId, context.getString(label(kind)), existing?.importance ?: importance(kind)).apply {
                group = groupId
                setSound(sound(context, kind), SOUND)
                if (old != null && fresh) inherit(old, kind)
            }
        }
        manager.createNotificationChannels(channels)
        retired.forEach(manager::deleteNotificationChannel)
    }

    /**
     * Whether an old channel stays in use: Android gives a new channel the
     * app's lock-screen visibility and Do Not Disturb setting, whatever it
     * is asked for, so one the user made private or secret, or let through
     * Do Not Disturb, would lose that.
     */
    private fun keeps(channel: NotificationChannel) =
        channel.lockscreenVisibility == Notification.VISIBILITY_PRIVATE ||
            channel.lockscreenVisibility == Notification.VISIBILITY_SECRET ||
            channel.canBypassDnd()

    /**
     * The channel [route] posts to: the one from before sounds while it
     * [keeps] the user's settings (see [createKinds]), else its own.
     */
    private fun channelFor(context: Context, route: ChannelRoute): String {
        val manager = context.getSystemService(NotificationManager::class.java)
        val id = route.groupId?.removePrefix(ChannelRoute.GROUP_PREFIX)
        val old = manager.getNotificationChannel(ChannelRoute.legacyChannelId(route.kind, id))
        return when {
            old != null && keeps(old) -> old.id
            manager.getNotificationChannel(route.androidChannelId) != null -> route.androidChannelId
            else -> old?.id ?: route.androidChannelId
        }
    }

    private fun NotificationChannel.inherit(old: NotificationChannel, kind: ChannelRoute.Kind) {
        // Questions stay heads-up unless the ordinary channel was turned down.
        importance = if (kind == ChannelRoute.Kind.QUESTION && old.importance >= NotificationManager.IMPORTANCE_DEFAULT) {
            maxOf(old.importance, importance(kind))
        } else {
            old.importance
        }
        // The pattern first: setting it also sets whether to vibrate.
        vibrationPattern = old.vibrationPattern
        enableVibration(old.shouldVibrate())
        enableLights(old.shouldShowLights())
        lightColor = old.lightColor
        setShowBadge(old.canShowBadge())
        if (old.sound != Settings.System.DEFAULT_NOTIFICATION_URI) setSound(old.sound, old.audioAttributes)
    }

    private fun label(kind: ChannelRoute.Kind) = when (kind) {
        ChannelRoute.Kind.MESSAGE -> R.string.channel_default
        ChannelRoute.Kind.URGENT -> R.string.channel_high
        ChannelRoute.Kind.QUESTION -> R.string.channel_question
    }

    // Questions are heads-up, so their buttons are in view.
    private fun importance(kind: ChannelRoute.Kind) =
        if (kind == ChannelRoute.Kind.MESSAGE) NotificationManager.IMPORTANCE_DEFAULT else NotificationManager.IMPORTANCE_HIGH

    /** The kind's sound, addressed by name: resource IDs can change between builds, and the channel keeps the URI. */
    private fun sound(context: Context, kind: ChannelRoute.Kind): Uri {
        val raw = when (kind) {
            ChannelRoute.Kind.MESSAGE -> R.raw.notifier_message
            ChannelRoute.Kind.URGENT -> R.raw.notifier_urgent
            ChannelRoute.Kind.QUESTION -> R.raw.notifier_question
        }
        return Uri.Builder()
            .scheme(ContentResolver.SCHEME_ANDROID_RESOURCE)
            .authority(context.packageName)
            .appendPath(context.resources.getResourceTypeName(raw))
            .appendPath(context.resources.getResourceEntryName(raw))
            .build()
    }

    /** Removes the groups (and their channels) of Notifier channels not in [current]. */
    fun pruneGroups(context: Context, current: Collection<String>) {
        val manager = context.getSystemService(NotificationManager::class.java)
        val keep = current.map { ChannelRoute.GROUP_PREFIX + it }.toSet()
        manager.notificationChannelGroups
            .filter { it.id.startsWith(ChannelRoute.GROUP_PREFIX) && it.id !in keep }
            .forEach { manager.deleteNotificationChannelGroup(it.id) }
    }
}
