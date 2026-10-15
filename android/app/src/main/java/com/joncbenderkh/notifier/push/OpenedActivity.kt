// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.app.Activity
import android.content.ActivityNotFoundException
import android.content.Context
import android.content.Intent
import android.os.Bundle
import androidx.core.app.NotificationManagerCompat
import androidx.core.net.toUri
import com.joncbenderkh.notifier.MainActivity
import com.joncbenderkh.notifier.data.Links

/**
 * Where a tap on the notification of a message with a link, or sent with
 * receipts, lands, and its link buttons: it reports the message opened if
 * the sender asked, then opens the link, or the history when there is
 * none or no app opens it. Not exported, so only this app's notifications
 * can report an opening; [MainActivity] is exported, and any app could
 * fill in its extras.
 */
class OpenedActivity : Activity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        val id = intent.getStringExtra(EXTRA_MESSAGE_ID)
        val deviceId = intent.getStringExtra(EXTRA_DEVICE_ID)
        if (id != null && deviceId != null) Receipts.reportLater(this, id, Receipts.OPENED, deviceId)
        // A button leaves its notification showing; the tap on it does not.
        intent.getStringExtra(EXTRA_DISMISS)?.let { NotificationManagerCompat.from(this).cancel(it, 0) }
        val link = intent.getStringExtra(EXTRA_URL)
        if (link == null || !view(this, link)) {
            startActivity(
                Intent(this, MainActivity::class.java)
                    .putExtra(Notifications.EXTRA_OPEN_HISTORY, true)
                    // It runs in a task of its own, kept out of Recents; the app's is
                    // another. Otherwise as an untracked notification's tap opens it.
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK),
            )
        }
        finish()
    }

    companion object {
        const val EXTRA_MESSAGE_ID = "com.joncbenderkh.notifier.MESSAGE_ID"
        const val EXTRA_DEVICE_ID = "com.joncbenderkh.notifier.DEVICE_ID"
        const val EXTRA_URL = "com.joncbenderkh.notifier.URL"
        const val EXTRA_DISMISS = "com.joncbenderkh.notifier.DISMISS"

        /** Opens [url] in the app that handles it (a browser), if it is a web link; false if none could. */
        fun view(context: Context, url: String): Boolean {
            val link = Links.web(url) ?: return false
            // Schemes match case-sensitively: "HTTPS:" would find no browser.
            val view = Intent(Intent.ACTION_VIEW, link.toUri().normalizeScheme())
                .addCategory(Intent.CATEGORY_BROWSABLE)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
            return try {
                context.startActivity(view)
                true
            } catch (_: ActivityNotFoundException) {
                false
            }
        }
    }
}
