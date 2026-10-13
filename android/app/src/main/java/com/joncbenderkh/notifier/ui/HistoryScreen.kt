// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import android.text.format.DateUtils
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.joncbenderkh.notifier.data.HistoryEntry
import com.joncbenderkh.notifier.push.ChannelRoute

/** Notifications this phone received, newest first; only this phone has them. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HistoryScreen(
    entries: List<HistoryEntry>,
    snackbar: SnackbarHostState,
    onBack: () -> Unit,
    onDelete: (String) -> Unit,
    onClear: () -> Unit,
) {
    BackHandler(onBack = onBack)
    var confirmClear by rememberSaveable { mutableStateOf(false) }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("History") },
                navigationIcon = { TextButton(onClick = onBack) { Text("Back") } },
                actions = {
                    if (entries.isNotEmpty()) TextButton(onClick = { confirmClear = true }) { Text("Clear all") }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbar) },
    ) { padding ->
        if (entries.isEmpty()) {
            Box(Modifier.padding(padding).fillMaxSize(), contentAlignment = Alignment.Center) {
                Text("Nothing received yet.")
            }
        } else {
            LazyColumn(
                modifier = Modifier.padding(padding).fillMaxSize(),
                contentPadding = PaddingValues(16.dp),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                items(entries, key = { it.id }) { HistoryCard(it, onDelete) }
            }
        }
    }

    if (confirmClear) {
        AlertDialog(
            onDismissRequest = { confirmClear = false },
            title = { Text("Clear the history?") },
            text = { Text("This removes every notification kept on this phone. The server keeps no copy.") },
            confirmButton = {
                TextButton(onClick = {
                    confirmClear = false
                    onClear()
                }) { Text("Clear") }
            },
            dismissButton = { TextButton(onClick = { confirmClear = false }) { Text("Cancel") } },
        )
    }
}

/** One notification: tap to show the whole body, and the delete button. */
@Composable
private fun HistoryCard(entry: HistoryEntry, onDelete: (String) -> Unit) {
    var expanded by rememberSaveable(entry.id) { mutableStateOf(false) }
    val context = LocalContext.current
    val details = buildList {
        if (entry.channel != null && entry.channelId?.let(ChannelRoute::isDefaultChannel) == false) add(entry.channel)
        val now = System.currentTimeMillis()
        add(
            if (now - entry.receivedAt * 1000 < DateUtils.MINUTE_IN_MILLIS) "just now"
            else DateUtils.getRelativeTimeSpanString(entry.receivedAt * 1000, now, DateUtils.MINUTE_IN_MILLIS).toString()
        )
        // Queued or delayed on the way: say when it was actually sent.
        if (entry.receivedAt - entry.sentAt > 60) {
            val flags = DateUtils.FORMAT_SHOW_TIME or DateUtils.FORMAT_SHOW_DATE or DateUtils.FORMAT_ABBREV_MONTH
            add("sent ${DateUtils.formatDateTime(context, entry.sentAt * 1000, flags)}")
        }
        if (entry.priority == "high") add("high priority")
    }.joinToString(" · ")

    Card(Modifier.fillMaxWidth().clickable { expanded = !expanded }) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text(
                entry.title,
                style = MaterialTheme.typography.titleMedium,
                fontWeight = if (entry.priority == "high") FontWeight.Bold else null,
            )
            Text(details, style = MaterialTheme.typography.bodySmall)
            if (entry.body.isNotEmpty()) {
                Text(
                    entry.body,
                    maxLines = if (expanded) Int.MAX_VALUE else 2,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            if (expanded) {
                TextButton(onClick = { onDelete(entry.id) }, modifier = Modifier.align(Alignment.End)) {
                    Text("Delete")
                }
            }
        }
    }
}
