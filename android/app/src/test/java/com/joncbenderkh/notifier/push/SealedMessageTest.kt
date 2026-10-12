// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import org.junit.Assert.assertEquals
import org.junit.Test
import java.util.Base64

/** Pins the wire format against the vector in server/test/seal.test.ts. */
class SealedMessageTest {
    private val key = Base64.getDecoder().decode("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=")
    private val vector = mapOf(
        "v" to "1",
        "n" to "MDEyMzQ1Njc4OWFi",
        "c" to "TDM7F3Qdq2rkWAzPzRj+L2fQcGZjs92ayPLhYHDQE7A3+4ZuMxigPp5ZB5Zwv1bQKNFhPX3Zl57NQ/14A8U3" +
            "Afl212jFtCR48XIXfkAPsHWEZmARTqUMCVVkb1gc7WGe8Sjshg42esye9bbN9nysV+9Pw6UNZw==",
    )

    @Test
    fun opensServerVector() {
        val payload = SealedMessage.open(key, "dev_test", vector)
        assertEquals(Payload("msg_test", "Hello", "From the test vector", "high", 1791590400), payload)
    }

    @Test(expected = SealedMessage.InvalidMessage::class)
    fun rejectsOtherDevice() {
        SealedMessage.open(key, "dev_other", vector)
    }

    @Test(expected = SealedMessage.InvalidMessage::class)
    fun rejectsTamperedCiphertext() {
        val tampered = vector + ("c" to vector.getValue("c").replaceFirst('T', 'U'))
        SealedMessage.open(key, "dev_test", tampered)
    }

    @Test(expected = SealedMessage.InvalidMessage::class)
    fun rejectsUnknownVersion() {
        SealedMessage.open(key, "dev_test", vector + ("v" to "2"))
    }
}
