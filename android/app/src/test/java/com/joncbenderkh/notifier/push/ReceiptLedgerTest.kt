// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class ReceiptLedgerTest {
    @Test
    fun sendsEachReceiptOnce() {
        val ledger = ReceiptLedger()
        assertTrue(ledger.claim("msg_a", Receipts.SHOWN))
        assertFalse(ledger.claim("msg_a", Receipts.SHOWN))
        assertTrue(ledger.claim("msg_a", Receipts.OPENED))
        assertTrue(ledger.claim("msg_b", Receipts.SHOWN))
    }

    @Test
    fun triesAgainAfterAFailureThatMayPass() {
        val ledger = ReceiptLedger()
        ledger.claim("msg_a", Receipts.OPENED)
        ledger.failed("msg_a", Receipts.OPENED, null)
        assertTrue(ledger.claim("msg_a", Receipts.OPENED))
        ledger.failed("msg_a", Receipts.OPENED, 503)
        assertTrue(ledger.claim("msg_a", Receipts.OPENED))
    }

    @Test
    fun givesUpOnAMessageWithoutReceipts() {
        val ledger = ReceiptLedger()
        ledger.claim("msg_a", Receipts.OPENED)
        ledger.failed("msg_a", Receipts.OPENED, 404)
        assertFalse(ledger.claim("msg_a", Receipts.OPENED))
    }

    @Test
    fun reportsOnlyForThePairingThatReceivedIt() {
        assertTrue(ReceiptLedger.forPairing("dev_a", "dev_a"))
        assertTrue(ReceiptLedger.forPairing("dev_a", null))
        assertFalse(ReceiptLedger.forPairing("dev_b", "dev_a"))
        assertFalse(ReceiptLedger.forPairing(null, "dev_a"))
        assertFalse(ReceiptLedger.forPairing(null, null))
    }
}
