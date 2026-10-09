// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import android.app.Application
import android.os.Build
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.google.firebase.messaging.FirebaseMessaging
import com.joncbenderkh.notifier.BuildConfig
import com.joncbenderkh.notifier.NotifierApp
import com.joncbenderkh.notifier.data.Pairing
import com.joncbenderkh.notifier.data.PairingStore
import com.joncbenderkh.notifier.data.ServerClient
import com.joncbenderkh.notifier.data.ServerException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.tasks.await
import kotlinx.coroutines.withContext
import java.io.IOException

data class UiState(
    val pairing: Pairing? = null,
    val busy: Boolean = false,
    val message: String? = null,
)

class MainViewModel(application: Application) : AndroidViewModel(application) {
    private val store = PairingStore(application)
    private val client = ServerClient()

    private val _state = MutableStateFlow(UiState(pairing = store.load()))
    val state: StateFlow<UiState> = _state.asStateFlow()

    val defaultDeviceName: String = Build.MODEL

    init {
        _state.value.pairing?.let(::syncFcmToken)
    }

    fun pair(serverUrl: String, code: String, deviceName: String) {
        val url = serverUrl.trim().trimEnd('/')
        val validationError = when {
            !NotifierApp.firebaseConfigured -> "This build has no Firebase configuration."
            url.startsWith("https://") -> null
            url.startsWith("http://") && BuildConfig.DEBUG -> null
            else -> "The server URL must start with https://"
        }
        if (validationError != null) {
            _state.update { it.copy(message = validationError) }
            return
        }
        run("Paired.") {
            val fcmToken = FirebaseMessaging.getInstance().token.await()
            val name = deviceName.trim().ifEmpty { defaultDeviceName }
            val response = withContext(Dispatchers.IO) { client.register(url, code.trim(), name, fcmToken) }
            val pairing = Pairing(url, response.deviceId, name, response.deviceToken, response.sealKey)
            store.save(pairing)
            _state.update { it.copy(pairing = pairing) }
        }
    }

    fun sendTest() {
        val pairing = _state.value.pairing ?: return
        run("Test notification sent.") { withContext(Dispatchers.IO) { client.sendTest(pairing) } }
    }

    fun unpair() {
        val pairing = _state.value.pairing ?: return
        viewModelScope.launch {
            _state.update { it.copy(busy = true, message = null) }
            val message = try {
                withContext(Dispatchers.IO) { client.unpair(pairing) }
                "Unpaired."
            } catch (e: ServerException) {
                if (e.status == 401) "Unpaired (the server had already removed this device)."
                else "Unpaired locally; the server reported: ${e.message}"
            } catch (e: IOException) {
                "Unpaired locally; the server could not be reached, so remove this device there too."
            }
            store.clear()
            _state.update { UiState(message = message) }
        }
    }

    fun dismissMessage() {
        _state.update { it.copy(message = null) }
    }

    private fun syncFcmToken(pairing: Pairing) {
        if (!NotifierApp.firebaseConfigured) return
        viewModelScope.launch {
            runCatching {
                val token = FirebaseMessaging.getInstance().token.await()
                withContext(Dispatchers.IO) { client.updateFcmToken(pairing, token) }
            }
        }
    }

    private fun run(success: String, block: suspend () -> Unit) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true, message = null) }
            val message = try {
                block()
                success
            } catch (e: ServerException) {
                e.message
            } catch (e: IOException) {
                "Could not reach the server: ${e.message}"
            } catch (e: Exception) {
                "Failed: ${e.message}"
            }
            _state.update { it.copy(busy = false, message = message) }
        }
    }
}
