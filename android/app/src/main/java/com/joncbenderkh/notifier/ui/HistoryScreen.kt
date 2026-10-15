// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import android.text.format.DateUtils
import androidx.activity.compose.BackHandler
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.ExperimentalLayoutApi
import androidx.compose.foundation.layout.FlowRow
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.joncbenderkh.notifier.R
import com.joncbenderkh.notifier.data.HistoryEntry
import com.joncbenderkh.notifier.push.ChannelRoute
import kotlinx.coroutines.delay

/** Notifications this phone received, newest first; only this phone has them. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun HistoryScreen(
    entries: List<HistoryEntry>,
    snackbar: SnackbarHostState,
    onBack: () -> Unit,
    onDelete: (String) -> Unit,
    onClear: () -> Unit,
    answering: Set<String>,
    onAnswer: (HistoryEntry, String) -> Unit,
    onOpen: (HistoryEntry) -> Unit,
) {
    BackHandler(onBack = onBack)
    var confirmClear by rememberSaveable { mutableStateOf(false) }
    var menu by remember { mutableStateOf(false) }

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text("History") },
                navigationIcon = {
                    IconButton(onClick = onBack) {
                        Icon(painterResource(R.drawable.ic_arrow_back), contentDescription = "Back")
                    }
                },
                actions = {
                    if (entries.isNotEmpty()) {
                        IconButton(onClick = { menu = true }) {
                            Icon(painterResource(R.drawable.ic_more_vert), contentDescription = "History options")
                        }
                        DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                            DropdownMenuItem(
                                text = { Text("Clear all") },
                                onClick = {
                                    menu = false
                                    confirmClear = true
                                },
                            )
                        }
                    }
                },
            )
        },
        snackbarHost = { SnackbarHost(snackbar) },
    ) { padding ->
        if (entries.isEmpty()) {
            Column(
                Modifier.padding(padding).fillMaxSize().padding(24.dp),
                verticalArrangement = Arrangement.Center,
                horizontalAlignment = Alignment.CenterHorizontally,
            ) {
                Icon(
                    painterResource(R.drawable.ic_notifications),
                    contentDescription = null,
                    tint = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.size(40.dp),
                )
                Text(
                    "Nothing received yet. New notifications will appear here.",
                    textAlign = TextAlign.Center,
                    modifier = Modifier.padding(top = 20.dp, bottom = 16.dp),
                )
                OutlinedButton(onClick = onBack) { Text("Back to home") }
            }
        } else {
            LazyColumn(
                modifier = Modifier.padding(padding).fillMaxSize(),
                contentPadding = PaddingValues(start = 16.dp, end = 16.dp, bottom = 16.dp),
                verticalArrangement = Arrangement.spacedBy(12.dp),
            ) {
                item {
                    Text(
                        "Decrypted and stored on this phone",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                items(entries, key = { it.id }) { HistoryCard(it, onDelete, it.id in answering, onAnswer, onOpen) }
            }
        }
    }

    if (confirmClear) {
        AlertDialog(
            onDismissRequest = { confirmClear = false },
            title = { Text("Clear the history?") },
            text = { Text("This removes every notification kept on this phone. The server keeps no copy.") },
            confirmButton = {
                TextButton(
                    onClick = {
                        confirmClear = false
                        onClear()
                    },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text("Clear") }
            },
            dismissButton = { TextButton(onClick = { confirmClear = false }) { Text("Cancel") } },
        )
    }
}

/** A coarse length of time: "5 min", "2 h", "3 d". */
private fun roughly(seconds: Long): String = when {
    seconds < 3600 -> "${seconds / 60} min"
    seconds < 86400 -> "${seconds / 3600} h"
    else -> "${seconds / 86400} d"
}

/** A small text button, as a card's "Read message". */
@Composable
private fun Link(text: String, onClick: () -> Unit) {
    Text(
        text,
        style = MaterialTheme.typography.labelMedium,
        color = MaterialTheme.colorScheme.primary,
        modifier = Modifier
            .padding(top = 4.dp)
            .clickable(onClickLabel = text, onClick = onClick)
            .padding(vertical = 8.dp, horizontal = 0.dp),
    )
}

@Composable
private fun Badge(text: String) {
    Text(
        text,
        style = MaterialTheme.typography.labelSmall,
        color = MaterialTheme.colorScheme.onPrimaryContainer,
        maxLines = 1,
        modifier = Modifier
            .background(MaterialTheme.colorScheme.primaryContainer, RoundedCornerShape(4.dp))
            .padding(horizontal = 6.dp, vertical = 2.dp),
    )
}

/**
 * One notification: its body collapses to two lines, it can be deleted, and
 * a question it asks can be answered until it expires. When its sender asked
 * for receipts, only "Mark seen" (or answering) tells them it was opened, so
 * a tap that misses another control never does.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun HistoryCard(
    entry: HistoryEntry,
    onDelete: (String) -> Unit,
    answering: Boolean,
    onAnswer: (HistoryEntry, String) -> Unit,
    onOpen: (HistoryEntry) -> Unit,
) {
    var expanded by rememberSaveable(entry.id) { mutableStateOf(false) }
    var overflows by remember(entry.id) { mutableStateOf(false) }
    val now = System.currentTimeMillis()
    val received =
        if (now - entry.receivedAt * 1000 < DateUtils.MINUTE_IN_MILLIS) "just now"
        else DateUtils.getRelativeTimeSpanString(entry.receivedAt * 1000, now, DateUtils.MINUTE_IN_MILLIS).toString()
    val channel = entry.channel?.takeIf { entry.channelId?.let(ChannelRoute::isDefaultChannel) == false }
    val high = entry.priority == "high"

    // Tapping a long message's card expands it, as "Read message" does.
    Card(
        Modifier
            .fillMaxWidth()
            .clip(CardDefaults.shape)
            .then(
                if (!expanded && !overflows) Modifier
                else Modifier.clickable(onClickLabel = if (expanded) "Collapse" else "Read message") { expanded = !expanded },
            ),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerLowest),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant),
    ) {
        Column(Modifier.padding(start = 16.dp, top = 12.dp, bottom = 12.dp, end = 4.dp)) {
            Row(verticalAlignment = Alignment.Top) {
                Column(Modifier.weight(1f).padding(top = 4.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                        Text(
                            listOfNotNull(channel, received).joinToString(" · "),
                            style = MaterialTheme.typography.labelMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                            maxLines = 1,
                            overflow = TextOverflow.Ellipsis,
                            // Shrinks before the badge does.
                            modifier = Modifier.weight(1f, fill = false),
                        )
                        if (high) Badge("High priority")
                        // Opening it tells the sender.
                        if (entry.receipts) Badge("Read receipt")
                    }
                    Text(entry.title, style = MaterialTheme.typography.titleMedium)
                    // Queued or delayed on the way: say how much earlier it was sent.
                    val delay = entry.receivedAt - entry.sentAt
                    if (delay > 60) {
                        Text(
                            "Sent ${roughly(delay)} earlier",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
                IconButton(onClick = { onDelete(entry.id) }) {
                    Icon(painterResource(R.drawable.ic_delete), contentDescription = "Delete ${entry.title}")
                }
            }
            if (entry.body.isNotEmpty()) {
                Text(
                    entry.body,
                    style = MaterialTheme.typography.bodyMedium,
                    maxLines = if (expanded) Int.MAX_VALUE else 2,
                    overflow = TextOverflow.Ellipsis,
                    onTextLayout = { if (!expanded) overflows = it.hasVisualOverflow },
                    modifier = Modifier.padding(top = 8.dp, end = 12.dp),
                )
                Row(horizontalArrangement = Arrangement.spacedBy(24.dp)) {
                    if (expanded || overflows) {
                        Link(if (expanded) "Collapse" else "Read message") { expanded = !expanded }
                    }
                    if (entry.receipts) Link("Mark seen") { onOpen(entry) }
                }
            }
            val reply = entry.reply
            if (reply != null) {
                // Turns to "expired" on time, even while the screen stays open.
                var expired by remember(entry.id) { mutableStateOf(now / 1000 >= reply.expiresAt) }
                LaunchedEffect(entry.id, expired) {
                    if (!expired) {
                        delay(reply.expiresAt * 1000 - System.currentTimeMillis())
                        expired = true
                    }
                }
                val settled = entry.outcome ?: "The question has expired".takeIf { expired }
                if (settled != null) {
                    Text(
                        settled,
                        style = MaterialTheme.typography.labelMedium,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(top = 8.dp, end = 12.dp),
                    )
                } else {
                    FlowRow(
                        Modifier.padding(top = 8.dp, end = 12.dp),
                        horizontalArrangement = Arrangement.spacedBy(8.dp),
                    ) {
                        reply.options.forEach { option ->
                            OutlinedButton(onClick = { onAnswer(entry, option) }, enabled = !answering) { Text(option) }
                        }
                    }
                }
            }
        }
    }
}
