// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.push

import android.app.Activity
import android.app.AlertDialog
import android.content.res.Configuration
import android.os.Bundle
import android.util.Log
import android.widget.Toast
import java.time.ZoneId
import kotlin.concurrent.thread

/**
 * Where a notification's "Snooze" button lands: asks when to show the
 * message again. Not exported, so only this app's notifications open it.
 */
class SnoozeActivity : Activity() {
    /** Whether the user went to allow exact alarms: the choice is offered again on return. */
    private var allowing = false

    /** [opened] when the user went to the settings. */
    private var openedBefore = 0

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        opened++
        intent.data?.schemeSpecificPart ?: return finish()
        offer()
    }

    override fun onRestart() {
        super.onRestart()
        if (!allowing) return
        allowing = false
        // Another snooze since means the user left the settings elsewhere: this one is stale.
        if (opened != openedBefore) finish() else offer()
    }

    private fun offer() {
        val id = intent.data?.schemeSpecificPart ?: return finish()
        val choices = Snooze.choices()
        // The time the labels name; a morning that passed before the tap moves to the next.
        val offeredAt = System.currentTimeMillis() / 1000
        // The activity's own theme is translucent and pre-Material.
        val night = resources.configuration.uiMode and Configuration.UI_MODE_NIGHT_MASK == Configuration.UI_MODE_NIGHT_YES
        val theme =
            if (night) android.R.style.Theme_DeviceDefault_Dialog_Alert
            else android.R.style.Theme_DeviceDefault_Light_Dialog_Alert
        AlertDialog.Builder(this, theme)
            .setTitle("Remind me")
            .setItems(choices.map { it.label(offeredAt, ZoneId.systemDefault()) { at -> Reminders.clock(this, at) } }.toTypedArray()) { _, i ->
                val app = applicationContext
                thread(name = "snooze") {
                    try {
                        val at = Reminders.snooze(app, id, choices[i], offeredAt)
                        val time = Reminders.time(app, at)
                        runOnUiThread { Toast.makeText(app, "Reminder at $time", Toast.LENGTH_SHORT).show() }
                    } catch (e: Exception) {
                        Log.w("SnoozeActivity", "could not snooze $id", e)
                        runOnUiThread { Toast.makeText(app, "Could not snooze it: ${e.message}", Toast.LENGTH_SHORT).show() }
                    }
                }
            }
            .apply {
                // Inexact, a reminder may come late.
                if (!Reminders.exact(this@SnoozeActivity)) {
                    setNeutralButton("On time…") { _, _ ->
                        allowing = true
                        openedBefore = opened
                        // None opened: nothing would bring the chooser back.
                        if (!Reminders.openSettings(this@SnoozeActivity)) finish()
                    }
                }
            }
            .setOnDismissListener { if (!allowing) finish() }
            .show()
    }

    private companion object {
        /** How many choosers this process has opened. */
        var opened = 0
    }
}
