// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.Manifest
import android.app.AlarmManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationChannelGroup
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Bundle
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

    private const val CHANNEL_DEFAULT = ChannelRoute.DEFAULT
    private const val CHANNEL_HIGH = ChannelRoute.HIGH

    fun createChannels(context: Context) {
        val manager = context.getSystemService(NotificationManager::class.java)
        manager.createNotificationChannels(
            listOf(
                NotificationChannel(
                    CHANNEL_DEFAULT,
                    context.getString(R.string.channel_default),
                    NotificationManager.IMPORTANCE_DEFAULT,
                ),
                NotificationChannel(
                    CHANNEL_HIGH,
                    context.getString(R.string.channel_high),
                    NotificationManager.IMPORTANCE_HIGH,
                ),
            )
        )
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
        val builder = NotificationCompat.Builder(context, route.androidChannelId)
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

    /**
     * Creates (or renames) a Notifier channel's group and its two Android
     * channels. Recreating an existing channel keeps the user's settings.
     */
    private fun createGroup(context: Context, groupId: String, name: String) {
        val manager = context.getSystemService(NotificationManager::class.java)
        val channelId = groupId.removePrefix(ChannelRoute.GROUP_PREFIX)
        manager.createNotificationChannelGroup(NotificationChannelGroup(groupId, name))
        manager.createNotificationChannels(
            listOf(
                NotificationChannel(
                    channelId,
                    context.getString(R.string.channel_default),
                    NotificationManager.IMPORTANCE_DEFAULT,
                ).apply { group = groupId },
                NotificationChannel(
                    "$channelId.high",
                    context.getString(R.string.channel_high),
                    NotificationManager.IMPORTANCE_HIGH,
                ).apply { group = groupId },
            )
        )
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
