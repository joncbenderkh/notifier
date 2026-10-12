// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.util.Base64
import javax.crypto.Cipher
import javax.crypto.spec.GCMParameterSpec
import javax.crypto.spec.SecretKeySpec

/** The plaintext the server seals into every push; mirrors `api.Payload`. */
@Serializable
data class Payload(
    val id: String,
    val title: String,
    val body: String = "",
    val priority: String = "default",
    @SerialName("sent_at") val sentAt: Long,
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
