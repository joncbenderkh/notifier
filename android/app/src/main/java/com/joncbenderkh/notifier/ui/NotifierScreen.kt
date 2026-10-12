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
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.LinearProgressIndicator
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
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
import com.joncbenderkh.notifier.data.Pairing
import com.joncbenderkh.notifier.data.PairingLink

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
                    onScanned = viewModel::openPairingLink,
                    onScanFailed = viewModel::showMessage,
                    onPair = viewModel::pair,
                )
            } else {
                PairedPanel(pairing, enabled = !state.busy, onTest = viewModel::sendTest, onUnpair = viewModel::unpair)
            }
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
    prefill: PairingLink?,
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

    val context = LocalContext.current
    val scanner = remember(context) {
        GmsBarcodeScanning.getClient(
            context,
            GmsBarcodeScannerOptions.Builder().setBarcodeFormats(Barcode.FORMAT_QR_CODE).build(),
        )
    }

    Text("Pair with a server", style = MaterialTheme.typography.titleLarge)
    Text(
        "Open the server's /admin/pair page or run `notify pair`, then scan the QR code " +
            "(or enter the code by hand) within ten minutes."
    )
    Button(
        onClick = {
            scanner.startScan()
                .addOnSuccessListener { barcode -> barcode.rawValue?.let(onScanned) }
                .addOnFailureListener { e ->
                    Log.w("PairingForm", "QR scan failed", e)
                    onScanFailed(
                        if (e is MlKitException && e.errorCode == MlKitException.CODE_SCANNER_UNAVAILABLE) {
                            "The QR scanner is still being installed by Google Play services. " +
                                "Try again shortly, or enter the code by hand."
                        } else {
                            "Could not scan: ${e.message}"
                        }
                    )
                }
        },
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
private fun PairedPanel(pairing: Pairing, enabled: Boolean, onTest: () -> Unit, onUnpair: () -> Unit) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
            Text("Paired", style = MaterialTheme.typography.titleLarge)
            Text("Server: ${pairing.serverUrl}")
            Text("Device: ${pairing.deviceName}")
            Text("Device ID: ${pairing.deviceId}", style = MaterialTheme.typography.bodySmall)
        }
    }
    Button(onClick = onTest, enabled = enabled, modifier = Modifier.fillMaxWidth()) {
        Text("Send test notification")
    }
    OutlinedButton(onClick = onUnpair, enabled = enabled, modifier = Modifier.fillMaxWidth()) {
        Text("Unpair")
    }
}
