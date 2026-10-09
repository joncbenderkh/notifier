// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier

import android.app.Application
import com.google.firebase.FirebaseApp
import com.google.firebase.FirebaseOptions
import com.joncbenderkh.notifier.push.Notifications

class NotifierApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Notifications.createChannels(this)
        if (firebaseConfigured) {
            FirebaseApp.initializeApp(
                this,
                FirebaseOptions.Builder()
                    .setProjectId(BuildConfig.FIREBASE_PROJECTID)
                    .setApplicationId(BuildConfig.FIREBASE_APPID)
                    .setApiKey(BuildConfig.FIREBASE_APIKEY)
                    .setGcmSenderId(BuildConfig.FIREBASE_SENDERID)
                    .build(),
            )
        }
    }

    companion object {
        /** False when the build was made without Firebase config (see app/build.gradle.kts). */
        val firebaseConfigured: Boolean
            get() = BuildConfig.FIREBASE_APPID.isNotEmpty()
    }
}
