// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import java.util.Base64

/** Pins the reply format against the vector in server/test/seal.test.ts, notify and notifyd. */
class RepliesTest {
    private val key = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

    @Test
    fun sealsTheSharedVector() {
        val sealed = Replies.seal(key, "msg_test", "yes", Base64.getDecoder().decode("MDEyMzQ1Njc4OWFi"))
        assertEquals(SealedReply("MDEyMzQ1Njc4OWFi", "TDMzHSVQ7HW1BXHCzRiocGE39XiLrrhG+LEwUrTqrw8="), sealed)
    }

    @Test
    fun usesAFreshNonceEachTime() {
        assertTrue(Replies.seal(key, "msg_test", "yes").n != Replies.seal(key, "msg_test", "yes").n)
    }

    @Test
    fun readsTheReplyFromThePayload() {
        val payload = Json { ignoreUnknownKeys = true }.decodeFromString<Payload>(
            """{"id":"msg_q","title":"Deploy?","sent_at":1,"reply":{"options":["yes","no"],"key":"$key","expires_at":601}}""",
        )
        assertEquals(Reply(listOf("yes", "no"), key, 601), payload.reply)
    }

    @Test
    fun tellsWhatBecameOfTheAnswer() {
        assertEquals(ReplyOutcome.ALREADY_ANSWERED, ReplyOutcome.ofStatus(409, "already answered"))
        assertEquals(ReplyOutcome.EXPIRED, ReplyOutcome.ofStatus(410, "expired"))
        assertEquals(ReplyOutcome.NOT_ALLOWED, ReplyOutcome.ofStatus(404, "not found"))
        val failed = ReplyOutcome.ofStatus(503, "unavailable")
        assertFalse(failed.final)
        assertEquals("Could not send the answer: unavailable", failed.text)
        assertEquals(ReplyOutcome("Answered: yes", true), ReplyOutcome.answered("yes"))
    }

    @Test
    fun keepsTheOutcomeWhenTheQuestionArrivesAgain() {
        val question = Payload(id = "msg_q", title = "Deploy?", body = "prod", sentAt = 1, reply = Reply(listOf("yes"), key, 601))
        assertEquals(NotificationText("Deploy?", "prod", false), NotificationText.of(question, null))
        assertEquals(NotificationText("Answered: yes", "Deploy?", true), NotificationText.of(question, "Answered: yes"))
        val plain = question.copy(reply = null)
        assertEquals(NotificationText("Deploy?", "prod", false), NotificationText.of(plain, "Answered: yes"))
    }
}
