// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import android.Manifest
import android.content.Intent
import android.content.pm.PackageManager
import android.os.Build
import android.provider.Settings
import android.util.Log
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.CardDefaults
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Switch
import androidx.compose.material3.SwitchDefaults
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.saveable.rememberSaveableStateHolder
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.painterResource
import androidx.compose.ui.semantics.LiveRegionMode
import androidx.compose.ui.semantics.liveRegion
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.LifecycleResumeEffect
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import com.google.mlkit.common.MlKitException
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.codescanner.GmsBarcodeScannerOptions
import com.google.mlkit.vision.codescanner.GmsBarcodeScanning
import com.joncbenderkh.notifier.BuildConfig
import com.joncbenderkh.notifier.R
import com.joncbenderkh.notifier.data.DeviceChannel
import com.joncbenderkh.notifier.data.NotifierLink
import com.joncbenderkh.notifier.data.Pairing
import com.joncbenderkh.notifier.push.ChannelRoute
import java.net.URI

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NotifierScreen(viewModel: MainViewModel = viewModel()) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val snackbar = remember { SnackbarHostState() }

    // One at a time, removed only once shown, so none is lost to a
    // rotation or to two arriving together.
    val notice = state.messages.firstOrNull()
    LaunchedEffect(notice?.id) {
        notice ?: return@LaunchedEffect
        snackbar.showSnackbar(notice.text)
        viewModel.messageShown(notice.id)
    }

    val permission = rememberNotificationPermission()
    LaunchedEffect(state.askNotifications, permission.granted) {
        if (state.askNotifications && permission.granted) viewModel.notificationsAsked()
    }

    val history by viewModel.history.collectAsStateWithLifecycle()
    // Keeps each screen's saved state (scroll, typed text) while the other shows.
    val screens = rememberSaveableStateHolder()
    if (state.showHistory) {
        screens.SaveableStateProvider("history") {
            HistoryScreen(
                entries = history,
                snackbar = snackbar,
                onBack = viewModel::closeHistory,
                onDelete = viewModel::deleteHistory,
                onClear = viewModel::clearHistory,
                answering = state.answering,
                onAnswer = viewModel::answer,
                onOpen = viewModel::opened,
            )
        }
        return
    }

    screens.SaveableStateProvider("main") {
        Scaffold(
            topBar = {
                TopAppBar(
                    title = { Text("Notifier") },
                    actions = {
                        state.pairing?.let { pairing ->
                            HomeMenu(pairing, enabled = !state.busy, onRefresh = viewModel::refreshChannels)
                        }
                    },
                )
            },
            snackbarHost = { SnackbarHost(snackbar) },
        ) { padding ->
            Column(
                modifier = Modifier
                    .padding(padding)
                    .fillMaxSize()
                    .verticalScroll(rememberScrollState())
                    .padding(16.dp),
                verticalArrangement = Arrangement.spacedBy(16.dp),
            ) {
                if (state.busy) LinearProgressIndicator(Modifier.fillMaxWidth())
                val pairing = state.pairing
                if (pairing == null) {
                    PairingForm(
                        defaultName = viewModel.defaultDeviceName,
                        busy = state.busy,
                        error = state.pairError,
                        prefill = state.prefill,
                        onPrefillConsumed = viewModel::consumePrefill,
                        onScanned = viewModel::openLink,
                        onScanFailed = viewModel::showMessage,
                        onReset = viewModel::clearPairError,
                        onPair = viewModel::pair,
                    )
                } else {
                    if (!permission.granted) NotificationPermissionCard(permission.request)
                    PairedHeader(pairing)
                    ChannelList(
                        channels = state.channels,
                        enabled = !state.busy,
                        onToggle = viewModel::setSubscribed,
                    )
                    PairedActions(
                        enabled = !state.busy,
                        historyCount = history.size,
                        onJoin = viewModel::join,
                        onScanned = viewModel::openLink,
                        onScanFailed = viewModel::showMessage,
                        onHistory = viewModel::openHistory,
                        onTest = viewModel::sendTest,
                        onUnpair = viewModel::unpair,
                    )
                }
            }
        }
    }

    if (state.askNotifications && !permission.granted) {
        AlertDialog(
            onDismissRequest = viewModel::notificationsAsked,
            title = { Text("Allow notifications?") },
            text = {
                Text(
                    "Notifier needs Android's notification permission to show incoming alerts. " +
                        "Messages are still received and saved to history if you choose not to allow it."
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    viewModel.notificationsAsked()
                    permission.request()
                }) { Text("Continue") }
            },
            dismissButton = { TextButton(onClick = viewModel::notificationsAsked) { Text("Not now") } },
        )
    }

    state.pendingJoin?.let { link ->
        AlertDialog(
            onDismissRequest = viewModel::cancelJoin,
            title = { Text("Join a channel?") },
            text = {
                Text(
                    "Invite ${link.code} adds you to a channel on ${link.serverUrl} and subscribes this " +
                        "phone, which will then show whatever its owner sends. Only join channels from " +
                        "people you trust."
                )
            },
            confirmButton = { TextButton(onClick = viewModel::confirmJoin) { Text("Join") } },
            dismissButton = { TextButton(onClick = viewModel::cancelJoin) { Text("Cancel") } },
        )
    }
}

/** Returns a function that opens Google's QR scanner and reports the result. */
@Composable
private fun rememberQrScanner(onScanned: (String) -> Unit, onScanFailed: (String) -> Unit): () -> Unit {
    val context = LocalContext.current
    val scanner = remember(context) {
        GmsBarcodeScanning.getClient(
            context,
            GmsBarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build(),
        )
    }
    return {
        scanner.startScan()
            .addOnSuccessListener { barcode -> barcode.rawValue?.let(onScanned) }
            .addOnFailureListener { e ->
                Log.w("QrScanner", "QR scan failed", e)
                onScanFailed(
                    if (e is MlKitException && e.errorCode == MlKitException.CODE_SCANNER_UNAVAILABLE) {
                        "The QR scanner is still being installed by Google Play services. " +
                            "Try again shortly, or enter the code by hand."
                    } else {
                        "Could not scan: ${e.message}"
                    }
                )
            }
    }
}

/** Whether notifications may be shown, and a way to ask Android for it. */
private class NotificationPermission(val granted: Boolean, val request: () -> Unit)

@Composable
private fun rememberNotificationPermission(): NotificationPermission {
    val context = LocalContext.current
    fun check() = Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU ||
        ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) ==
        PackageManager.PERMISSION_GRANTED
    var granted by remember { mutableStateOf(check()) }
    // The user may change it in system settings while the app is in the background.
    LifecycleResumeEffect(Unit) {
        granted = check()
        onPauseOrDispose {}
    }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted = it }
    return NotificationPermission(granted) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) launcher.launch(Manifest.permission.POST_NOTIFICATIONS)
    }
}

@Composable
private fun NotificationPermissionCard(onAllow: () -> Unit) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text("Notifications are off", style = MaterialTheme.typography.titleMedium)
            Text("Messages will arrive but cannot be shown until notifications are allowed.")
            Button(onClick = onAllow) {
                Text("Allow notifications")
            }
        }
    }
}

@Composable
private fun PairingForm(
    defaultName: String,
    busy: Boolean,
    error: String?,
    prefill: NotifierLink.Pair?,
    onPrefillConsumed: () -> Unit,
    onScanned: (String) -> Unit,
    onScanFailed: (String) -> Unit,
    onReset: () -> Unit,
    onPair: (String, String, String) -> Unit,
) {
    var serverUrl by rememberSaveable { mutableStateOf("https://") }
    var code by rememberSaveable { mutableStateOf("") }
    var name by rememberSaveable { mutableStateOf(defaultName) }
    // The form shows once a QR code or link filled it in, or on request.
    var filled by rememberSaveable { mutableStateOf(false) }
    var manual by rememberSaveable { mutableStateOf(false) }
    var pasting by rememberSaveable { mutableStateOf(false) }

    fun fill(link: NotifierLink.Pair) {
        serverUrl = link.serverUrl
        code = link.code
        filled = true
    }

    LaunchedEffect(prefill) {
        if (prefill != null) {
            fill(prefill)
            onPrefillConsumed()
        }
    }

    val scan = rememberQrScanner(onScanned, onScanFailed)

    Box(
        Modifier
            .padding(top = 8.dp)
            .size(64.dp)
            .background(MaterialTheme.colorScheme.secondaryContainer, RoundedCornerShape(20.dp)),
        contentAlignment = Alignment.Center,
    ) {
        Icon(
            painterResource(R.drawable.ic_lock),
            contentDescription = null,
            tint = MaterialTheme.colorScheme.onSecondaryContainer,
            modifier = Modifier.size(30.dp),
        )
    }
    Text("Your server. Your notifications.", style = MaterialTheme.typography.headlineSmall)
    Text("Pair this phone with your notifier server. Messages stay encrypted until this phone decrypts them.")

    if (!filled && !manual) {
        Button(onClick = scan, enabled = !busy, modifier = Modifier.fillMaxWidth()) {
            Text("Scan QR code")
        }
        OutlinedButton(onClick = { pasting = true }, enabled = !busy, modifier = Modifier.fillMaxWidth()) {
            Text("Paste link")
        }
        TextButton(onClick = { manual = true }, enabled = !busy, modifier = Modifier.fillMaxWidth()) {
            Text("Enter details by hand")
        }
        Text(
            "Get a code from your server's /pair page or `notify pair`. Codes last ten minutes.",
            style = MaterialTheme.typography.bodySmall,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
        )
    } else {
        if (filled) Text("Details filled in. Review them, then pair.", style = MaterialTheme.typography.titleSmall)
        OutlinedTextField(
            value = serverUrl,
            onValueChange = { serverUrl = it },
            label = { Text("Server URL") },
            singleLine = true,
            enabled = !busy,
            keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = code,
            onValueChange = { code = it },
            label = { Text("Pairing code") },
            placeholder = { Text("XXXX-XXXX") },
            singleLine = true,
            enabled = !busy,
            isError = error != null,
            keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Characters),
            modifier = Modifier.fillMaxWidth(),
        )
        OutlinedTextField(
            value = name,
            onValueChange = { name = it },
            label = { Text("Device name") },
            singleLine = true,
            enabled = !busy,
            modifier = Modifier.fillMaxWidth(),
        )
        if (error != null) {
            Text(
                error,
                color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.semantics { liveRegion = LiveRegionMode.Polite },
            )
        }
        Button(
            onClick = { onPair(serverUrl, code, name) },
            enabled = !busy && code.isNotBlank(),
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text(if (busy) "Pairing…" else "Pair")
        }
        TextButton(
            onClick = {
                serverUrl = "https://"
                code = ""
                filled = false
                manual = false
                onReset()
            },
            enabled = !busy,
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text("Use another QR code or link")
        }
    }

    if (pasting) {
        PasteLinkDialog(
            onDismiss = { pasting = false },
            onLink = {
                pasting = false
                onReset()
                fill(it)
            },
        )
    }
}

/** Takes a pasted `notifier://pair` link; it only fills the form. */
@Composable
private fun PasteLinkDialog(onDismiss: () -> Unit, onLink: (NotifierLink.Pair) -> Unit) {
    var text by rememberSaveable { mutableStateOf("") }
    var error by rememberSaveable { mutableStateOf<String?>(null) }
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Paste pairing link") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Paste the notifier://pair link from your server.")
                OutlinedTextField(
                    value = text,
                    onValueChange = {
                        text = it
                        error = null
                    },
                    label = { Text("Pairing link") },
                    singleLine = true,
                    isError = error != null,
                    supportingText = error?.let { { Text(it) } },
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = {
                    when (val link = NotifierLink.parse(text.trim(), allowHttp = BuildConfig.DEBUG)) {
                        is NotifierLink.Pair -> onLink(link)
                        is NotifierLink.Join -> error = "That is a channel invite. Pair first, then open it."
                        null -> error = "Enter a notifier://pair link."
                    }
                },
                enabled = text.isNotBlank(),
            ) { Text("Fill details") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}

/** The top bar's overflow menu on the paired screen. */
@Composable
private fun HomeMenu(pairing: Pairing, enabled: Boolean, onRefresh: () -> Unit) {
    val context = LocalContext.current
    var open by remember { mutableStateOf(false) }
    var about by rememberSaveable { mutableStateOf(false) }
    IconButton(onClick = { open = true }) {
        Icon(painterResource(R.drawable.ic_more_vert), contentDescription = "More options")
    }
    DropdownMenu(expanded = open, onDismissRequest = { open = false }) {
        DropdownMenuItem(
            text = { Text("Refresh channels") },
            enabled = enabled,
            onClick = {
                open = false
                onRefresh()
            },
        )
        DropdownMenuItem(
            text = { Text("Notification settings") },
            onClick = {
                open = false
                context.startActivity(
                    Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS)
                        .putExtra(Settings.EXTRA_APP_PACKAGE, context.packageName)
                )
            },
        )
        DropdownMenuItem(
            text = { Text("About this device") },
            onClick = {
                open = false
                about = true
            },
        )
    }
    if (about) {
        AlertDialog(
            onDismissRequest = { about = false },
            title = { Text("This device") },
            text = {
                SelectionContainer {
                    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                        Text("Server: ${pairing.serverUrl}")
                        Text("Name: ${pairing.deviceName}")
                        Text("Device ID: ${pairing.deviceId}")
                    }
                }
            },
            confirmButton = { TextButton(onClick = { about = false }) { Text("Close") } },
        )
    }
}

@Composable
private fun PairedHeader(pairing: Pairing) {
    val host = runCatching { URI(pairing.serverUrl).authority }.getOrNull() ?: pairing.serverUrl
    Card(
        Modifier.fillMaxWidth(),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surfaceContainerHigh),
    ) {
        Column(Modifier.padding(20.dp)) {
            Text(
                "Paired with",
                style = MaterialTheme.typography.labelMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Text(host, style = MaterialTheme.typography.bodySmall, fontFamily = FontFamily.Monospace)
            Text(
                pairing.deviceName,
                style = MaterialTheme.typography.titleMedium,
                modifier = Modifier.padding(top = 12.dp),
            )
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(6.dp),
                modifier = Modifier.padding(top = 12.dp),
            ) {
                Icon(painterResource(R.drawable.ic_lock), contentDescription = null, modifier = Modifier.size(14.dp))
                Text("End-to-end encrypted", style = MaterialTheme.typography.labelMedium)
            }
        }
    }
}

@Composable
private fun ChannelList(
    channels: List<DeviceChannel>?,
    enabled: Boolean,
    onToggle: (String, Boolean) -> Unit,
) {
    Column {
        Text("Channels", style = MaterialTheme.typography.labelLarge, modifier = Modifier.padding(bottom = 4.dp))
        if (channels == null) {
            Text("Not loaded yet.", color = MaterialTheme.colorScheme.onSurfaceVariant)
            return
        }
        channels.forEachIndexed { index, channel ->
            // A send without a channel goes to default, so it stays on.
            val isDefault = ChannelRoute.isDefaultChannel(channel.id)
            Row(
                verticalAlignment = Alignment.CenterVertically,
                modifier = Modifier.padding(vertical = 12.dp),
            ) {
                Column(Modifier.weight(1f).padding(end = 16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text(
                        channel.name,
                        style = MaterialTheme.typography.titleSmall,
                        fontFamily = FontFamily.Monospace,
                        fontWeight = FontWeight.Bold,
                    )
                    Text(
                        if (isDefault) "Always subscribed"
                        else "Gets its own notification settings once a message arrives",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                Switch(
                    checked = channel.subscribed || isDefault,
                    onCheckedChange = { onToggle(channel.id, it) },
                    enabled = enabled && !isDefault,
                    // Always on, so it must still read as on.
                    colors = SwitchDefaults.colors(
                        disabledCheckedTrackColor = MaterialTheme.colorScheme.primary.copy(alpha = 0.45f),
                        disabledCheckedThumbColor = MaterialTheme.colorScheme.surface,
                    ),
                )
            }
            if (index < channels.lastIndex) HorizontalDivider()
        }
    }
}

@Composable
private fun PairedActions(
    enabled: Boolean,
    historyCount: Int,
    onJoin: (String) -> Unit,
    onScanned: (String) -> Unit,
    onScanFailed: (String) -> Unit,
    onHistory: () -> Unit,
    onTest: () -> Unit,
    onUnpair: () -> Unit,
) {
    var joining by rememberSaveable { mutableStateOf(false) }
    var confirmUnpair by rememberSaveable { mutableStateOf(false) }
    Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
        OutlinedButton(onClick = { joining = true }, enabled = enabled, modifier = Modifier.fillMaxWidth()) {
            Text("Join channel")
        }
        Button(onClick = onHistory, modifier = Modifier.fillMaxWidth()) {
            Text("History ($historyCount)")
        }
        TextButton(onClick = onTest, enabled = enabled, modifier = Modifier.fillMaxWidth()) {
            Text("Send test notification")
        }
        TextButton(
            onClick = { confirmUnpair = true },
            enabled = enabled,
            colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
            modifier = Modifier.fillMaxWidth(),
        ) {
            Text("Unpair")
        }
    }

    if (joining) {
        JoinDialog(
            onDismiss = { joining = false },
            onScanned = {
                joining = false
                onScanned(it)
            },
            onScanFailed = onScanFailed,
            onCode = {
                joining = false
                onJoin(it)
            },
        )
    }

    if (confirmUnpair) {
        AlertDialog(
            onDismissRequest = { confirmUnpair = false },
            title = { Text("Unpair this phone?") },
            text = {
                Text(
                    "This disconnects this phone from your server and clears all notification history " +
                        "on this phone. You will need a new pairing code to reconnect."
                )
            },
            confirmButton = {
                TextButton(
                    onClick = {
                        confirmUnpair = false
                        onUnpair()
                    },
                    colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                ) { Text("Unpair") }
            },
            dismissButton = { TextButton(onClick = { confirmUnpair = false }) { Text("Cancel") } },
        )
    }
}

/** Twelve letters or digits once dashes and spaces go; the server forgives the rest. */
private fun looksLikeInviteCode(input: String) =
    input.filterNot { it == '-' || it.isWhitespace() }.let { it.length == 12 && it.all(Char::isLetterOrDigit) }

/** Why [input] is not a channel invite link, or null if it is one. */
private fun joinLinkError(input: String): String? = when (NotifierLink.parse(input, allowHttp = BuildConfig.DEBUG)) {
    is NotifierLink.Join -> null
    is NotifierLink.Pair -> "That is a pairing link, not a channel invite."
    null -> "Enter a notifier://join link or an XXXX-XXXX-XXXX code."
}

/**
 * Takes a channel invite: scanned, or typed as a code or `notifier://join`
 * link. A link goes through the same path as a scanned one, which asks
 * before joining.
 */
@Composable
private fun JoinDialog(
    onDismiss: () -> Unit,
    onScanned: (String) -> Unit,
    onScanFailed: (String) -> Unit,
    onCode: (String) -> Unit,
) {
    var text by rememberSaveable { mutableStateOf("") }
    var error by rememberSaveable { mutableStateOf<String?>(null) }
    val scan = rememberQrScanner(
        onScanned = { raw ->
            when (val problem = joinLinkError(raw.trim())) {
                null -> onScanned(raw)
                else -> error = problem
            }
        },
        onScanFailed = onScanFailed,
    )
    AlertDialog(
        onDismissRequest = onDismiss,
        title = { Text("Join channel") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Use an invite from the channel's owner (`notify channel invite`).")
                OutlinedButton(onClick = scan, modifier = Modifier.fillMaxWidth()) {
                    Text("Scan invite QR code")
                }
                OutlinedTextField(
                    value = text,
                    onValueChange = {
                        text = it
                        error = null
                    },
                    label = { Text("Invite link or code") },
                    placeholder = { Text("XXXX-XXXX-XXXX") },
                    singleLine = true,
                    isError = error != null,
                    supportingText = error?.let { { Text(it) } },
                    keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Characters),
                    modifier = Modifier.fillMaxWidth(),
                )
            }
        },
        confirmButton = {
            TextButton(
                onClick = {
                    val input = text.trim()
                    if (looksLikeInviteCode(input)) {
                        onCode(input)
                    } else {
                        when (val problem = joinLinkError(input)) {
                            null -> onScanned(input)
                            else -> error = problem
                        }
                    }
                },
                enabled = text.isNotBlank(),
            ) { Text("Continue") }
        },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } },
    )
}
