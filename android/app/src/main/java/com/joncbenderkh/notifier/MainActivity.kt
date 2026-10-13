// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier

import android.content.Intent
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.viewModels
import com.joncbenderkh.notifier.push.Notifications
import com.joncbenderkh.notifier.ui.MainViewModel
import com.joncbenderkh.notifier.ui.NotifierScreen
import com.joncbenderkh.notifier.ui.NotifierTheme

class MainActivity : ComponentActivity() {
    private val viewModel: MainViewModel by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        // Only on a fresh launch: after a configuration change the link has
        // already been handled.
        if (savedInstanceState == null) handleIntent(intent)
        setContent {
            NotifierTheme {
                NotifierScreen(viewModel)
            }
        }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        handleIntent(intent)
    }

    private fun handleIntent(intent: Intent) {
        if (intent.action == Intent.ACTION_VIEW) intent.dataString?.let(viewModel::openLink)
        if (intent.getBooleanExtra(Notifications.EXTRA_OPEN_HISTORY, false)) viewModel.openHistory()
    }
}
