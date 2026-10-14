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
import android.os.Bundle
import android.provider.Settings
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import com.joncbenderkh.notifier.MainActivity
import com.joncbenderkh.notifier.R

object Notifications {
    /** Set on the intent a tapped notification opens: show the history. */
    const val EXTRA_OPEN_HISTORY = "com.joncbenderkh.notifier.OPEN_HISTORY"

    /** The question's title, kept on its notification while the title shows an outcome. */
    private const val EXTRA_QUESTION = "com.joncbenderkh.notifier.QUESTION"

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
     * quietly, as [showOutcome] left it.
     */
    fun show(context: Context, payload: Payload, deviceId: String, outcome: String?) {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) return

        val high = payload.priority == "high"
        val route = ChannelRoute.of(payload)
        if (route.groupId != null) createGroup(context, route.groupId, route.groupName ?: route.groupId)
        val text = NotificationText.of(payload, outcome)
        val builder = NotificationCompat.Builder(context, channelFor(context, route))
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(text.title)
            .setContentText(text.text)
            .setSubText(payload.channel?.takeIf { route.groupId != null })
            .setStyle(NotificationCompat.BigTextStyle().bigText(text.text))
            .setSilent(text.settled)
            .setPriority(if (high) NotificationCompat.PRIORITY_HIGH else NotificationCompat.PRIORITY_DEFAULT)
            .setWhen(payload.sentAt * 1000)
            .setShowWhen(true)
            .setContentIntent(openHistory(context))
            .setAutoCancel(true)
            .addExtras(Bundle().apply { putString(EXTRA_QUESTION, payload.title) })
        val reply = payload.reply?.takeUnless { text.settled }
        if (reply != null) addAnswers(context, builder, payload.id, reply, deviceId)
        NotificationManagerCompat.from(context).notify(payload.id, 0, builder.build())
    }

    /** Adds a button per answer, and has them removed when the question expires. */
    private fun addAnswers(context: Context, builder: NotificationCompat.Builder, id: String, reply: Reply, deviceId: String) {
        if (System.currentTimeMillis() / 1000 >= reply.expiresAt) return
        reply.options.forEachIndexed { i, option ->
            builder.addAction(0, option, ReplyReceiver.intent(context, id, reply, i, deviceId))
        }
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
        val shown = manager.activeNotifications.firstOrNull { it.tag == id && it.id == 0 } ?: return
        if (shown.notification.actions.isNullOrEmpty()) return
        val notification = Notification.Builder.recoverBuilder(context, shown.notification)
            .setActions()
            .setOnlyAlertOnce(true)
            .build()
        manager.notify(id, 0, notification)
    }

    /**
     * Shows [outcome] in place of message [id]'s notification, with its
     * question below, if it is still showing; quietly, since it answers a
     * tap. [retry], for an outcome that may still change, keeps the buttons.
     */
    fun showOutcome(context: Context, id: String, outcome: String, retry: Pair<Reply, String>? = null) {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) return
        val manager = NotificationManagerCompat.from(context)
        val shown = manager.activeNotifications.firstOrNull { it.tag == id && it.id == 0 } ?: return
        val extras = shown.notification.extras
        val question = extras.getString(EXTRA_QUESTION) ?: extras.getCharSequence(NotificationCompat.EXTRA_TITLE)?.toString()
        val builder = NotificationCompat.Builder(context, shown.notification.channelId)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(outcome)
            .setContentText(question)
            .addExtras(Bundle().apply { putString(EXTRA_QUESTION, question) })
            .setSubText(shown.notification.extras.getCharSequence(NotificationCompat.EXTRA_SUB_TEXT))
            .setWhen(shown.notification.`when`)
            .setShowWhen(true)
            .setSilent(true)
            .setContentIntent(openHistory(context))
            .setAutoCancel(true)
        if (retry != null) addAnswers(context, builder, id, retry.first, retry.second)
        manager.notify(id, 0, builder.build())
    }

    private fun openHistory(context: Context) = PendingIntent.getActivity(
        context,
        0,
        Intent(context, MainActivity::class.java).putExtra(EXTRA_OPEN_HISTORY, true),
        // UPDATE_CURRENT: an intent cached without the extra must not be reused.
        PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT,
    )

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
