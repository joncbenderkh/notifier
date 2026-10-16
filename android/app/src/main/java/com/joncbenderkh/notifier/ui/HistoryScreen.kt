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
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.lifecycle.compose.LifecycleResumeEffect
import com.joncbenderkh.notifier.R
import com.joncbenderkh.notifier.data.History
import com.joncbenderkh.notifier.data.HistoryEntry
import com.joncbenderkh.notifier.data.Links
import com.joncbenderkh.notifier.push.ChannelRoute
import com.joncbenderkh.notifier.push.Reminders
import com.joncbenderkh.notifier.push.Snooze
import java.time.ZoneId
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
    onLink: (HistoryEntry, String) -> Unit,
    onSnooze: (HistoryEntry, Snooze, Long) -> Unit,
    onCancelReminder: (HistoryEntry) -> Unit,
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
            // A replaced message would never show again: its reminder is dropped.
            val replaced = remember(entries) { History(entries).replacedIds() }
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
                items(entries, key = { it.id }) {
                    HistoryCard(it, onDelete, it.id in answering, onAnswer, onOpen, onLink, it.id !in replaced, onSnooze, onCancelReminder)
                }
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
 * One notification: its body collapses to two lines, it can be deleted, its
 * links opened (each named by its host, since the sender chose it), and a
 * question it asks can be answered until it expires. When its sender asked
 * for receipts, only "Mark seen", a link (or answering) tells them it was
 * opened, so a tap that misses another control never does.
 */
@OptIn(ExperimentalLayoutApi::class)
@Composable
private fun HistoryCard(
    entry: HistoryEntry,
    onDelete: (String) -> Unit,
    answering: Boolean,
    onAnswer: (HistoryEntry, String) -> Unit,
    onOpen: (HistoryEntry) -> Unit,
    onLink: (HistoryEntry, String) -> Unit,
    snoozable: Boolean,
    onSnooze: (HistoryEntry, Snooze, Long) -> Unit,
    onCancelReminder: (HistoryEntry) -> Unit,
) {
    var expanded by rememberSaveable(entry.id) { mutableStateOf(false) }
    var choosing by rememberSaveable(entry.id) { mutableStateOf(false) }
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
                    entry.remindAt?.let { at ->
                        // Past due when it could not show: it shows on the next start that can.
                        val time = Reminders.time(LocalContext.current, at)
                        Text(
                            if (at * 1000 < now - DateUtils.MINUTE_IN_MILLIS) "Reminder overdue since $time" else "Reminder $time",
                            style = MaterialTheme.typography.labelSmall,
                            color = MaterialTheme.colorScheme.primary,
                        )
                    }
                    // Queued or delayed on the way: say how much earlier it was sent.
                    val delay = entry.receivedAt - entry.sentAt
                    if (delay > 60 && !entry.reminded) {
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
            }
            val url = Links.web(entry.url)
            val actions = entry.actions.filter { Links.web(it.url) != null && it.label.isNotBlank() }
            val readMore = entry.body.isNotEmpty() && (expanded || overflows)
            val reply = entry.reply
            // Turns to "expired" on time, even while the screen stays open.
            var expired by remember(entry.id) { mutableStateOf(reply != null && now / 1000 >= reply.expiresAt) }
            if (reply != null) {
                LaunchedEffect(entry.id, expired) {
                    if (!expired) {
                        delay(reply.expiresAt * 1000 - System.currentTimeMillis())
                        expired = true
                    }
                }
            }
            // An open question is answered, not put off.
            val asking = reply != null && entry.outcome == null && !expired
            FlowRow(Modifier.padding(end = 12.dp), horizontalArrangement = Arrangement.spacedBy(24.dp)) {
                if (readMore) Link(if (expanded) "Collapse" else "Read message") { expanded = !expanded }
                if (url != null) Link("Open ${Links.host(url)}") { onLink(entry, url) }
                actions.forEach { a -> Link("${a.label} · ${Links.host(a.url)}") { onLink(entry, a.url) } }
                if (entry.receipts) Link("Mark seen") { onOpen(entry) }
                if (entry.remindAt != null) Link("Cancel reminder") { onCancelReminder(entry) }
                else if (!asking && snoozable) Link("Remind me") { choosing = true }
            }
            if (choosing) {
                // The time the labels name: "This morning" stays this morning, past 9:00 too.
                val offeredAt = remember { System.currentTimeMillis() / 1000 }
                AlertDialog(
                    onDismissRequest = { choosing = false },
                    title = { Text("Remind me") },
                    text = {
                        Column {
                            Snooze.choices().forEach { choice ->
                                Text(
                                    choice.label(offeredAt, ZoneId.systemDefault()),
                                    style = MaterialTheme.typography.bodyLarge,
                                    modifier = Modifier
                                        .fillMaxWidth()
                                        .clickable {
                                            choosing = false
                                            onSnooze(entry, choice, offeredAt)
                                        }
                                        .padding(vertical = 12.dp),
                                )
                            }
                        }
                    },
                    confirmButton = {},
                    dismissButton = {
                        val context = LocalContext.current
                        // Asked again on return from the settings.
                        var exact by remember { mutableStateOf(Reminders.exact(context)) }
                        LifecycleResumeEffect(Unit) {
                            exact = Reminders.exact(context)
                            onPauseOrDispose {}
                        }
                        Row {
                            // Inexact, a reminder may come late.
                            if (!exact) {
                                // Still open on return, to choose a time.
                                TextButton(onClick = { context.startActivity(Reminders.settings(context)) }) { Text("On time…") }
                            }
                            TextButton(onClick = { choosing = false }) { Text("Cancel") }
                        }
                    },
                )
            }
            if (reply != null) {
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
