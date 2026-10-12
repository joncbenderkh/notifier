// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

/**
 * Where a message is shown: which Android notification channel, inside which
 * channel group. Each Notifier channel other than the user's default gets its
 * own group with a normal and a high-priority channel, so it can be muted or
 * given its own sound in system settings. Keyed by channel ID, because labels
 * are not unique (two owners can both be called "alice").
 */
data class ChannelRoute(val androidChannelId: String, val groupId: String?, val groupName: String?) {
    companion object {
        const val DEFAULT = "default"
        const val HIGH = "high"
        const val GROUP_PREFIX = "ch:"

        fun of(payload: Payload): ChannelRoute {
            val high = payload.priority == "high"
            val id = payload.channelId
            if (id == null || isDefaultChannel(id)) return ChannelRoute(if (high) HIGH else DEFAULT, null, null)
            return ChannelRoute(if (high) "$id.high" else id, GROUP_PREFIX + id, payload.channel ?: id)
        }

        /** The user's own default channel, shown in the app's original channels. */
        fun isDefaultChannel(channelId: String) = channelId.startsWith("ch_default_")
    }
}
