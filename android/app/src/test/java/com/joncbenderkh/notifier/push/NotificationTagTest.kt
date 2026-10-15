// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class NotificationTagTest {
    private val payload = Payload(id = "msg_1", title = "Disk 91%", sentAt = 1)

    @Test
    fun unkeyedMessagesAreTaggedByID() {
        assertEquals("msg_1", NotificationTag.of(payload, "dev_a"))
        assertEquals("msg_1", NotificationTag.of(payload.copy(key = ""), "dev_a"))
        // Only what the server accepts: "b/c" in channel "a" would look like "c" in "a/b".
        assertEquals("msg_1", NotificationTag.of(payload.copy(key = "b/c", channelId = "a"), "dev_a"))
        assertEquals("msg_1", NotificationTag.of(payload.copy(key = "x".repeat(65)), "dev_a"))
    }

    @Test
    fun keysAreScopedByPairingAndChannel() {
        assertEquals("key:dev_a/direct/disk", NotificationTag.of(payload.copy(key = "disk"), "dev_a"))
        val inChannel = payload.copy(key = "disk", channelId = "ch_a")
        assertEquals("key:dev_a/ch_a/disk", NotificationTag.of(inChannel, "dev_a"))
        assertEquals(NotificationTag.of(inChannel, "dev_a"), NotificationTag.of(inChannel.copy(id = "msg_2"), "dev_a"))
        assertNotEquals(NotificationTag.of(inChannel, "dev_a"), NotificationTag.of(inChannel, "dev_b"))
    }

    @Test
    fun anOlderMessageDoesNotReplaceANewerOne() {
        assertTrue(NotificationTag.newer(shownAt = 101_000, sentAt = 100))
        // Within one second, the later arrival wins.
        assertFalse(NotificationTag.newer(shownAt = 100_000, sentAt = 100))
        assertFalse(NotificationTag.newer(shownAt = 99_000, sentAt = 100))
    }
}
