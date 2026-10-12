// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.util.Log
import com.google.firebase.messaging.FirebaseMessagingService
import com.google.firebase.messaging.RemoteMessage
import com.joncbenderkh.notifier.data.PairingStore
import com.joncbenderkh.notifier.data.ServerClient
import java.io.IOException

/** Receives sealed pushes from FCM and keeps the server's copy of our FCM token current. */
class PushService : FirebaseMessagingService() {
    // Both callbacks run on a Firebase background thread, so blocking I/O is fine here.

    override fun onNewToken(token: String) {
        val pairing = PairingStore(this).load() ?: return
        try {
            ServerClient().updateFcmToken(pairing, token)
        } catch (e: IOException) {
            // MainViewModel re-syncs the token on the next app launch.
            Log.w(TAG, "could not update FCM token", e)
        }
    }

    override fun onMessageReceived(message: RemoteMessage) {
        val pairing = PairingStore(this).load() ?: return
        val payload = try {
            SealedMessage.open(pairing.sealKeyBytes(), pairing.deviceId, message.data)
        } catch (e: Exception) {
            Log.w(TAG, "dropping message that failed to open", e)
            return
        }
        Notifications.show(this, payload)
    }

    private companion object {
        const val TAG = "PushService"
    }
}
