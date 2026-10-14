// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

/**
 * Where a message is shown: which Android notification channel, inside which
 * channel group, with which of notifier's sounds. Each Notifier channel other
 * than the user's default gets its own group with a channel per [Kind], so it
 * can be muted or given its own sound in system settings. Keyed by channel ID,
 * because labels are not unique (two owners can both be called "alice").
 */
data class ChannelRoute(val androidChannelId: String, val groupId: String?, val groupName: String?, val kind: Kind) {
    /**
     * A kind of message, with its own Android channel and sound. A
     * channel's sound is fixed once it exists, so the channels made before
     * notifier had sounds are replaced; [legacy] is the one whose settings
     * carry over (see [legacyChannelId]). Questions had none of their own:
     * they take the ordinary channel's, so muting it keeps them quiet too.
     */
    enum class Kind(val id: String, val legacy: String) {
        MESSAGE("message", "default"),
        URGENT("urgent", "high"),
        QUESTION("question", "default"),
    }

    companion object {
        const val GROUP_PREFIX = "ch:"

        fun of(payload: Payload): ChannelRoute {
            val kind = when {
                payload.reply != null -> Kind.QUESTION
                payload.priority == "high" -> Kind.URGENT
                else -> Kind.MESSAGE
            }
            val id = payload.channelId
            if (id == null || isDefaultChannel(id)) return ChannelRoute(kind.id, null, null, kind)
            return ChannelRoute(channelId(kind, id), GROUP_PREFIX + id, payload.channel ?: id, kind)
        }

        /** The Android channel for [kind], in Notifier channel [id]'s group if given. */
        fun channelId(kind: Kind, id: String? = null) = if (id == null) kind.id else "$id.${kind.id}"

        /**
         * The channel from before notifier had sounds that [kind] replaces:
         * "default" or "high", or in a group the channel ID or ".high".
         */
        fun legacyChannelId(kind: Kind, id: String? = null): String = when {
            id == null -> kind.legacy
            kind.legacy == Kind.MESSAGE.legacy -> id
            else -> "$id.${kind.legacy}"
        }

        /** The user's own default channel, shown in the app's ungrouped channels. */
        fun isDefaultChannel(channelId: String) = channelId.startsWith("ch_default_")
    }
}
