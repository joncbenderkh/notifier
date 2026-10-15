// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.app.Activity
import android.content.Intent
import android.os.Bundle
import com.joncbenderkh.notifier.MainActivity

/**
 * Where a tap on the notification of a message sent with receipts lands:
 * it reports the message opened, then shows the history. Not exported, so
 * only this app's notifications can report an opening; [MainActivity] is
 * exported, and any app could fill in its extras.
 */
class OpenedActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val id = intent.getStringExtra(EXTRA_MESSAGE_ID)
        val deviceId = intent.getStringExtra(EXTRA_DEVICE_ID)
        if (id != null && deviceId != null) Receipts.reportLater(this, id, Receipts.OPENED, deviceId)
        startActivity(
            Intent(this, MainActivity::class.java)
                .putExtra(Notifications.EXTRA_OPEN_HISTORY, true)
                // It runs in a task of its own, kept out of Recents; the app's is
                // another. Otherwise as an untracked notification's tap opens it.
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
        )
        finish()
    }

    companion object {
        const val EXTRA_MESSAGE_ID = "com.joncbenderkh.notifier.MESSAGE_ID"
        const val EXTRA_DEVICE_ID = "com.joncbenderkh.notifier.DEVICE_ID"
    }
}
