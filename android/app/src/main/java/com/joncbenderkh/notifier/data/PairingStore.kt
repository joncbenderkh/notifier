// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import androidx.core.content.edit
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import java.security.KeyStore
import java.util.Base64
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

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

    fun load(): Pairing? {
        val stored = prefs.getString(KEY_BLOB, null) ?: return null
        val blob = Base64.getDecoder().decode(stored)
        val cipher = Cipher.getInstance(TRANSFORMATION).apply {
            init(Cipher.DECRYPT_MODE, keystoreKey(), GCMParameterSpec(128, blob, 0, IV_BYTES))
        }
        val plaintext = cipher.doFinal(blob, IV_BYTES, blob.size - IV_BYTES)
        return Json.decodeFromString<Pairing>(plaintext.decodeToString())
    }

    fun save(pairing: Pairing) {
        val cipher = Cipher.getInstance(TRANSFORMATION).apply { init(Cipher.ENCRYPT_MODE, keystoreKey()) }
        val blob = cipher.iv + cipher.doFinal(Json.encodeToString(pairing).encodeToByteArray())
        prefs.edit { putString(KEY_BLOB, Base64.getEncoder().encodeToString(blob)) }
    }

    fun clear() {
        prefs.edit { remove(KEY_BLOB) }
    }

    private fun keystoreKey(): SecretKey {
        val keyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }
        (keyStore.getKey(KEY_ALIAS, null) as SecretKey?)?.let { return it }
        return KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, ANDROID_KEYSTORE).run {
            init(
                KeyGenParameterSpec.Builder(
                    KEY_ALIAS,
                    KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
                )
                    .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                    .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                    .setKeySize(256)
                    .build()
            )
            generateKey()
        }
    }

    private companion object {
        const val ANDROID_KEYSTORE = "AndroidKeyStore"
        const val KEY_ALIAS = "notifier-pairing"
        const val KEY_BLOB = "blob"
        const val TRANSFORMATION = "AES/GCM/NoPadding"
        const val IV_BYTES = 12
    }
}
