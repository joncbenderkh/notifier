// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import android.Manifest
import android.content.pm.PackageManager
import android.os.Build
import android.util.Log
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
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
import androidx.compose.ui.text.input.KeyboardCapitalization
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import androidx.lifecycle.viewmodel.compose.viewModel
import com.google.mlkit.common.MlKitException
import com.google.mlkit.vision.barcode.common.Barcode
import com.google.mlkit.vision.codescanner.GmsBarcodeScannerOptions
import com.google.mlkit.vision.codescanner.GmsBarcodeScanning
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
                NotificationPermissionCard()
                val pairing = state.pairing
                if (pairing == null) {
                    PairingForm(
                        defaultName = viewModel.defaultDeviceName,
                        enabled = !state.busy,
                        prefill = state.prefill,
                        onPrefillConsumed = viewModel::consumePrefill,
                        onScanned = viewModel::openLink,
                        onScanFailed = viewModel::showMessage,
                        onPair = viewModel::pair,
                    )
                } else {
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

@Composable
private fun NotificationPermissionCard() {
    if (Build.VERSION.SDK_INT < Build.VERSION_CODES.TIRAMISU) return
    val context = LocalContext.current
    var granted by remember {
        mutableStateOf(
            ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) ==
                PackageManager.PERMISSION_GRANTED
        )
    }
    val launcher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { granted = it }
    if (granted) return

    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text("Notifications are off", style = MaterialTheme.typography.titleMedium)
            Text("Messages will arrive but cannot be shown until notifications are allowed.")
            Button(onClick = { launcher.launch(Manifest.permission.POST_NOTIFICATIONS) }) {
                Text("Allow notifications")
            }
        }
    }
}

@Composable
private fun PairingForm(
    defaultName: String,
    enabled: Boolean,
    prefill: NotifierLink.Pair?,
    onPrefillConsumed: () -> Unit,
    onScanned: (String) -> Unit,
    onScanFailed: (String) -> Unit,
    onPair: (String, String, String) -> Unit,
) {
    var serverUrl by rememberSaveable { mutableStateOf("https://") }
    var code by rememberSaveable { mutableStateOf("") }
    var name by rememberSaveable { mutableStateOf(defaultName) }

    LaunchedEffect(prefill) {
        if (prefill != null) {
            serverUrl = prefill.serverUrl
            code = prefill.code
            onPrefillConsumed()
        }
    }

    val scan = rememberQrScanner(onScanned, onScanFailed)

    Text("Pair with a server", style = MaterialTheme.typography.titleLarge)
    Text(
        "Open the server's /pair page or run `notify pair`, then scan the QR code " +
            "(or enter the code by hand) within ten minutes."
    )
    Button(
        onClick = scan,
        enabled = enabled,
        modifier = Modifier.fillMaxWidth(),
    ) {
        Text("Scan QR code")
    }
    OutlinedTextField(
        value = serverUrl,
        onValueChange = { serverUrl = it },
        label = { Text("Server URL") },
        singleLine = true,
        keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri),
        modifier = Modifier.fillMaxWidth(),
    )
    OutlinedTextField(
        value = code,
        onValueChange = { code = it },
        label = { Text("Pairing code") },
        placeholder = { Text("XXXX-XXXX") },
        singleLine = true,
        keyboardOptions = KeyboardOptions(capitalization = KeyboardCapitalization.Characters),
        modifier = Modifier.fillMaxWidth(),
    )
    OutlinedTextField(
        value = name,
        onValueChange = { name = it },
        label = { Text("Device name") },
        singleLine = true,
        modifier = Modifier.fillMaxWidth(),
    )
    Button(
        onClick = { onPair(serverUrl, code, name) },
        enabled = enabled && code.isNotBlank(),
        modifier = Modifier.fillMaxWidth(),
    ) {
        Text("Pair")
    }
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
