// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.util.Base64
import javax.crypto.Cipher
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/** The plaintext the server seals into every push; mirrors `Payload` in server/src/app.ts. */
@Serializable
data class Payload(
    val id: String,
    val title: String,
    val body: String = "",
    val priority: String = "default",
    @SerialName("sent_at") val sentAt: Long,
    /** The channel it came through, as this user names it ("homelab", "alice/homelab"). */
    val channel: String? = null,
    @SerialName("channel_id") val channelId: String? = null,
    /** Set when this device may answer the message's question. */
    val reply: Reply? = null,
    /** Set when the sender asked to know when this device shows and opens it. */
    val receipts: Boolean = false,
    /** Opened by a tap on the notification. */
    val url: String? = null,
    /** Buttons, each opening its own link; never sent with a question. */
    val actions: List<LinkAction> = emptyList(),
    /** Replaces the notification last shown with the same key from the same channel. */
    val key: String? = null,
)

/** A button that opens [url]; one another server sends incomplete is skipped, not the message. */
@Serializable
data class LinkAction(val label: String = "", val url: String = "")

/** A question's answers, the one-time key to seal one under, and when it expires (Unix seconds). */
@Serializable
data class Reply(
    val options: List<String>,
    val key: String,
    @SerialName("expires_at") val expiresAt: Long,
)

/**
 * Opens the AES-256-GCM envelope carried in an FCM data message. The device ID
 * is the associated data, so a payload sealed for another device is rejected,
 * as is anything FCM (or anyone else) altered or forged.
 */
object SealedMessage {
    private const val VERSION = "1"
    private const val TAG_BITS = 128
    private val json = Json { ignoreUnknownKeys = true }

    class InvalidMessage(message: String, cause: Throwable? = null) : Exception(message, cause)

    fun open(sealKey: ByteArray, deviceId: String, data: Map<String, String>): Payload {
        if (data["v"] != VERSION) throw InvalidMessage("unsupported envelope version ${data["v"]}")
        val decoder = Base64.getDecoder()
        val nonce = decoder.decode(data["n"] ?: throw InvalidMessage("missing nonce"))
        val ciphertext = decoder.decode(data["c"] ?: throw InvalidMessage("missing ciphertext"))
        val plaintext = try {
            Cipher.getInstance("AES/GCM/NoPadding").run {
                init(Cipher.DECRYPT_MODE, SecretKeySpec(sealKey, "AES"), GCMParameterSpec(TAG_BITS, nonce))
                updateAAD(deviceId.toByteArray())
                doFinal(ciphertext)
            }
        } catch (e: java.security.GeneralSecurityException) {
            throw InvalidMessage("authentication failed", e)
        }
        return json.decodeFromString(plaintext.decodeToString())
    }
}
