// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

/**
 * The tag a message's notification is posted under: its ID, or for a
 * message with a key, the key within its channel and pairing, so the next
 * message with that key replaces it. Only a channel's owner publishes to
 * it, and only this user sends to the device directly.
 */
object NotificationTag {
    fun of(payload: Payload, deviceId: String): String {
        // What the server accepts; anything else, which only another server
        // could send, could make two scopes' tags alike.
        val key = payload.key?.takeIf(::validKey) ?: return payload.id
        return "key:$deviceId/${payload.channelId ?: "direct"}/$key"
    }

    private val KEY = Regex("[A-Za-z0-9._:-]{1,64}")

    /** Whether [key] is one the server accepts, so a message is posted under it. */
    fun validKey(key: String) = KEY.matches(key)

    /**
     * Whether a notification shown with time [shownAt] (ms) came from a
     * message sent after [sentAt] (s), so that one must not replace it.
     * Send times are whole seconds: of two sent within one, the later
     * arrival wins, which is the later one unless FCM reorders them.
     */
    fun newer(shownAt: Long, sentAt: Long) = shownAt > sentAt * 1000
}
