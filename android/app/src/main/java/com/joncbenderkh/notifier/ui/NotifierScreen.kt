// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
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
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Switch
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

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NotifierScreen(viewModel: MainViewModel = viewModel()) {
    val state by viewModel.state.collectAsStateWithLifecycle()
    val snackbar = remember { SnackbarHostState() }

    LaunchedEffect(state.message) {
        state.message?.let {
            snackbar.showSnackbar(it)
            viewModel.dismissMessage()
        }
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
            )
        }
        return
    }

    screens.SaveableStateProvider("main") {
        Scaffold(
            topBar = { TopAppBar(title = { Text("Notifier") }) },
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
                    PairedPanel(
                        pairing,
                        enabled = !state.busy,
                        historyCount = history.size,
                        onHistory = viewModel::openHistory,
                        onTest = viewModel::sendTest,
                        onUnpair = viewModel::unpair,
                    )
                    ChannelsCard(
                        channels = state.channels,
                        enabled = !state.busy,
                        onToggle = viewModel::setSubscribed,
                        onRefresh = viewModel::refreshChannels,
                    )
                    JoinCard(
                        enabled = !state.busy,
                        onJoin = viewModel::join,
                        onScanned = viewModel::openLink,
                        onScanFailed = viewModel::showMessage,
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

@Composable
private fun PairedPanel(
    pairing: Pairing,
    enabled: Boolean,
    historyCount: Int,
    onHistory: () -> Unit,
    onTest: () -> Unit,
    onUnpair: () -> Unit,
) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text("Paired", style = MaterialTheme.typography.titleLarge)
            Text("Server: ${pairing.serverUrl}")
            Text("Device: ${pairing.deviceName}")
            Text("Device ID: ${pairing.deviceId}", style = MaterialTheme.typography.bodySmall)
        }
    }
    Button(onClick = onHistory, modifier = Modifier.fillMaxWidth()) {
        Text("History ($historyCount)")
    }
    OutlinedButton(onClick = onTest, enabled = enabled, modifier = Modifier.fillMaxWidth()) {
        Text("Send test notification")
    }
    var confirmUnpair by rememberSaveable { mutableStateOf(false) }
    TextButton(
        onClick = { confirmUnpair = true },
        enabled = enabled,
        colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Text("Unpair")
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

@Composable
private fun ChannelsCard(
    channels: List<DeviceChannel>?,
    enabled: Boolean,
    onToggle: (String, Boolean) -> Unit,
    onRefresh: () -> Unit,
) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("Channels", style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
                TextButton(onClick = onRefresh, enabled = enabled) { Text("Refresh") }
            }
            if (channels == null) {
                Text("Not loaded yet.")
                return@Column
            }
            for (channel in channels) {
                // A send without a channel goes to default, so it stays on.
                val isDefault = ChannelRoute.isDefaultChannel(channel.id)
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f)) {
                        Text(channel.name)
                        if (isDefault) Text("Always on", style = MaterialTheme.typography.bodySmall)
                    }
                    Switch(
                        checked = channel.subscribed || isDefault,
                        onCheckedChange = { onToggle(channel.id, it) },
                        enabled = enabled && !isDefault,
                    )
                }
            }
        }
    }
}

@Composable
private fun JoinCard(
    enabled: Boolean,
    onJoin: (String) -> Unit,
    onScanned: (String) -> Unit,
    onScanFailed: (String) -> Unit,
) {
    var code by rememberSaveable { mutableStateOf("") }
    val scan = rememberQrScanner(onScanned, onScanFailed)
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text("Join a channel", style = MaterialTheme.typography.titleLarge)
            Text("Scan or enter an invite from the channel's owner (`notify channel invite`).")
            OutlinedButton(onClick = scan, enabled = enabled, modifier = Modifier.fillMaxWidth()) {
                Text("Scan invite QR code")
            }
            OutlinedTextField(
                value = code,
                onValueChange = { code = it },
                label = { Text("Invite code") },
                placeholder = { Text("XXXX-XXXX-XXXX") },
                singleLine = true,
                keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Characters),
                modifier = Modifier.fillMaxWidth(),
            )
            Button(
                onClick = {
                    onJoin(code)
                    code = ""
                },
                enabled = enabled && code.isNotBlank(),
                modifier = Modifier.fillMaxWidth(),
            ) {
                Text("Join")
            }
        }
    }
}
