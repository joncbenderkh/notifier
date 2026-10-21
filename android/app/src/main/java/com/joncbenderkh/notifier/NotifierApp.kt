// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier

import android.app.Application
import android.util.Log
import com.google.firebase.FirebaseApp
import com.google.firebase.FirebaseOptions
import com.joncbenderkh.notifier.push.Notifications
import com.joncbenderkh.notifier.push.Reminders
import kotlin.concurrent.thread

class NotifierApp : Application() {
    override fun onCreate() {
        super.onCreate()
        Notifications.createChannels(this)
        // A force stop clears the app's alarms, and any later start comes here.
        thread(name = "reminders") {
            try {
                Reminders.rescheduleAllOrRetry(this)
            } catch (e: Exception) {
                Log.w("NotifierApp", "could not reschedule the reminders", e)
            }
        }
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
