// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import com.joncbenderkh.notifier.ui.NotifierScreen
import com.joncbenderkh.notifier.ui.NotifierTheme

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            NotifierTheme {
                NotifierScreen()
            }
        }
    }
}
