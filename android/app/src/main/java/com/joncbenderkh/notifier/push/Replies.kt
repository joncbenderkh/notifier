// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.security.SecureRandom
import java.util.Base64
import javax.crypto.Cipher
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/** An answer sealed for the asker: base64 nonce and ciphertext (with its tag). */
@Serializable
data class SealedReply(val n: String, val c: String)

/**
 * Seals answers to questions. Only the asker, who chose the one-time key,
 * can open one; the server just stores it.
 */
object Replies {
    private const val TAG_BITS = 128
    private const val NONCE_BYTES = 12
    private val random = SecureRandom()

    @Serializable
    private data class Answer(val answer: String)

    /**
     * Seals [answer] to the question [id] under [key] (base64, 32 bytes):
     * AES-256-GCM with "reply:" + the ID as associated data.
     */
    fun seal(key: String, id: String, answer: String, nonce: ByteArray = ByteArray(NONCE_BYTES).also(random::nextBytes)): SealedReply {
        val ciphertext = Cipher.getInstance("AES/GCM/NoPadding").run {
            init(Cipher.ENCRYPT_MODE, SecretKeySpec(Base64.getDecoder().decode(key), "AES"), GCMParameterSpec(TAG_BITS, nonce))
            updateAAD("reply:$id".toByteArray())
            doFinal(Json.encodeToString(Answer(answer)).toByteArray())
        }
        val encoder = Base64.getEncoder()
        return SealedReply(encoder.encodeToString(nonce), encoder.encodeToString(ciphertext))
    }
}

/** What became of an answer, for the user. A [final] one will not change if tried again. */
data class ReplyOutcome(val text: String, val final: Boolean) {
    companion object {
        fun answered(option: String) = ReplyOutcome("Answered: $option", true)
        val ALREADY_ANSWERED = ReplyOutcome("Already answered", true)
        val EXPIRED = ReplyOutcome("The question has expired", true)
        val NOT_ALLOWED = ReplyOutcome("This phone may no longer answer", true)
        fun failed(reason: String?) = ReplyOutcome("Could not send the answer: ${reason ?: "unknown error"}", false)

        /** The outcome of the server answering [status] (not 2xx) with [message]. */
        fun ofStatus(status: Int, message: String?) = when (status) {
            409 -> ALREADY_ANSWERED
            410 -> EXPIRED
            404 -> NOT_ALLOWED
            else -> failed(message)
        }
    }
}

/**
 * What a message's notification says. A question this phone has [settled]
 * shows the outcome above its title, even if the message arrives again.
 */
data class NotificationText(val title: String, val text: String, val settled: Boolean) {
    companion object {
        fun of(payload: Payload, outcome: String?) =
            if (payload.reply != null && outcome != null) NotificationText(outcome, payload.title, true)
            else NotificationText(payload.title, payload.body, false)
    }
}
