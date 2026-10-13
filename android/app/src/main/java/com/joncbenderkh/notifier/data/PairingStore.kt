// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import android.content.Context
import androidx.core.content.edit
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.util.Base64

/** Everything the phone needs after pairing; the two secrets never leave the device. */
@Serializable
data class Pairing(
    val serverUrl: String,
    val deviceId: String,
    val deviceName: String,
    val deviceToken: String,
    val sealKey: String,
) {
    fun sealKeyBytes(): ByteArray = Base64.getDecoder().decode(sealKey)
}

/**
 * Persists the [Pairing] in SharedPreferences, encrypted with an AES-GCM key
 * held in the Android Keystore so the secrets are never stored in the clear.
 */
class PairingStore(context: Context) {
    private val prefs = context.getSharedPreferences("pairing", Context.MODE_PRIVATE)
    private val cipher = KeystoreCipher(KEY_ALIAS)

    fun load(): Pairing? {
        val stored = prefs.getString(KEY_BLOB, null) ?: return null
        val plaintext = cipher.decrypt(Base64.getDecoder().decode(stored))
        return Json.decodeFromString<Pairing>(plaintext.decodeToString())
    }

    fun save(pairing: Pairing) {
        val blob = cipher.encrypt(Json.encodeToString(pairing).encodeToByteArray())
        prefs.edit { putString(KEY_BLOB, Base64.getEncoder().encodeToString(blob)) }
    }

    fun clear() {
        prefs.edit { remove(KEY_BLOB) }
    }

    private companion object {
        const val KEY_ALIAS = "notifier-pairing"
        const val KEY_BLOB = "blob"
    }
}
