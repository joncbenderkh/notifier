// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.Manifest
import android.app.NotificationChannel
import android.app.NotificationChannelGroup
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import androidx.core.app.NotificationCompat
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import com.joncbenderkh.notifier.MainActivity
import com.joncbenderkh.notifier.R

object Notifications {
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

    fun show(context: Context, payload: Payload) {
        if (ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) !=
            PackageManager.PERMISSION_GRANTED
        ) return

        val high = payload.priority == "high"
        val route = ChannelRoute.of(payload)
        if (route.groupId != null) createGroup(context, route.groupId, route.groupName ?: route.groupId)
        val openApp = PendingIntent.getActivity(
            context,
            0,
            Intent(context, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE,
        )
        val notification = NotificationCompat.Builder(context, route.androidChannelId)
            .setSmallIcon(R.drawable.ic_notification)
            .setContentTitle(payload.title)
            .setContentText(payload.body)
            .setSubText(payload.channel?.takeIf { route.groupId != null })
            .setStyle(NotificationCompat.BigTextStyle().bigText(payload.body))
            .setPriority(if (high) NotificationCompat.PRIORITY_HIGH else NotificationCompat.PRIORITY_DEFAULT)
            .setWhen(payload.sentAt * 1000)
            .setShowWhen(true)
            .setContentIntent(openApp)
            .setAutoCancel(true)
            .build()
        NotificationManagerCompat.from(context).notify(payload.id, 0, notification)
    }

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
