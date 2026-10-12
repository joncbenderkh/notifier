// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.ui

import android.app.Application
import android.os.Build
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.google.firebase.messaging.FirebaseMessaging
import com.joncbenderkh.notifier.BuildConfig
import com.joncbenderkh.notifier.NotifierApp
import com.joncbenderkh.notifier.data.DeviceChannel
import com.joncbenderkh.notifier.data.NotifierLink
import com.joncbenderkh.notifier.data.Pairing
import com.joncbenderkh.notifier.data.PairingStore
import com.joncbenderkh.notifier.data.ServerClient
import com.joncbenderkh.notifier.data.ServerException
import com.joncbenderkh.notifier.push.ChannelRoute
import com.joncbenderkh.notifier.push.Notifications
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
    /** A scanned or opened pairing link waiting to fill the form. */
    val prefill: NotifierLink.Pair? = null,
    /** The channels this device could subscribe to; null until loaded. */
    val channels: List<DeviceChannel>? = null,
    /** A scanned or opened channel invite waiting for the user to confirm. */
    val pendingJoin: NotifierLink.Join? = null,
)

class MainViewModel(application: Application) : AndroidViewModel(application) {
    private val store = PairingStore(application)
    private val client = ServerClient()

    private val _state = MutableStateFlow(UiState(pairing = store.load()))
    val state: StateFlow<UiState> = _state.asStateFlow()

    val defaultDeviceName: String = Build.MODEL

    init {
        _state.value.pairing?.let {
            syncFcmToken(it)
            refreshChannels()
        }
    }

    fun pair(serverUrl: String, code: String, deviceName: String) {
        // Stored as the server spells its own origin, so its invite links match.
        val url = NotifierLink.origin(serverUrl, allowHttp = BuildConfig.DEBUG)
        val validationError = when {
            !NotifierApp.firebaseConfigured -> "This build has no Firebase configuration."
            url == null -> "The server URL must be https:// and the server's address, with no path."
            else -> null
        }
        if (validationError != null || url == null) {
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
            loadChannels(pairing)
        }
    }

    /**
     * Handles a scanned QR code or an opened `notifier://` link. Neither kind
     * acts by itself: a pairing link fills the form for the user to review and
     * tap Pair, and a channel invite asks for confirmation, so a hostile QR
     * code cannot silently redirect the phone or subscribe it to anything.
     */
    fun openLink(raw: String) {
        val link = NotifierLink.parse(raw, allowHttp = BuildConfig.DEBUG)
        _state.update {
            val pairing = it.pairing
            when (link) {
                null -> it.copy(message = "That is not a Notifier code.")
                is NotifierLink.Pair ->
                    if (pairing != null) it.copy(message = "Already paired. Unpair first to pair with another server.")
                    else it.copy(prefill = link, message = "Check the server URL, then tap Pair.")
                is NotifierLink.Join -> when {
                    pairing == null -> it.copy(message = "Pair this phone first, then open the invite again.")
                    // Pairings saved before origins were normalized may be spelled differently.
                    link.serverUrl != (NotifierLink.origin(pairing.serverUrl, allowHttp = true) ?: pairing.serverUrl) ->
                        it.copy(message = "That invite is for another server (${link.serverUrl}).")
                    else -> it.copy(pendingJoin = link)
                }
            }
        }
    }

    fun cancelJoin() {
        _state.update { it.copy(pendingJoin = null) }
    }

    /** Joins the channel of a confirmed invite link. */
    fun confirmJoin() {
        val link = _state.value.pendingJoin ?: return
        _state.update { it.copy(pendingJoin = null) }
        join(link.code)
    }

    /** Joins a channel with an invite code and subscribes this phone to it. */
    fun join(code: String) {
        val pairing = _state.value.pairing ?: return
        if (code.isBlank()) return
        runWithMessage {
            val channel = withContext(Dispatchers.IO) { client.join(pairing, code.trim()) }
            loadChannels(pairing)
            "Joined ${channel.name}."
        }
    }

    fun refreshChannels() {
        val pairing = _state.value.pairing ?: return
        run(null) { loadChannels(pairing) }
    }

    /** Subscribes this phone to a channel, or unsubscribes it. */
    fun setSubscribed(channelId: String, subscribed: Boolean) {
        val pairing = _state.value.pairing ?: return
        val channels = _state.value.channels ?: return
        val ids = channels.filter { if (it.id == channelId) subscribed else it.subscribed }.map { it.id }
        run(null) {
            val generation = ++channelsGeneration
            val updated = try {
                withContext(Dispatchers.IO) { client.setChannels(pairing, ids) }
            } catch (e: ServerException) {
                // Most likely the list changed elsewhere (a channel left or deleted).
                runCatching { loadChannels(pairing) }
                throw e
            }
            if (generation == channelsGeneration) _state.update { it.copy(channels = updated) }
        }
    }

    /** Counts channel-list requests, so a slow, older response never overwrites a newer one. */
    private var channelsGeneration = 0

    private suspend fun loadChannels(pairing: Pairing) {
        val generation = ++channelsGeneration
        val channels = withContext(Dispatchers.IO) { client.channels(pairing) }
        if (generation != channelsGeneration) return
        _state.update { it.copy(channels = channels) }
        // A channel left or deleted elsewhere leaves its notification settings behind.
        Notifications.pruneGroups(getApplication(), channels.map { it.id }.filterNot(ChannelRoute::isDefaultChannel))
    }

    fun consumePrefill() {
        _state.update { it.copy(prefill = null) }
    }

    fun showMessage(message: String) {
        _state.update { it.copy(message = message) }
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

    /** Runs [block] busy, then shows [success] (if any) or the error. */
    private fun run(success: String?, block: suspend () -> Unit) = runWithMessage {
        block()
        success
    }

    private fun runWithMessage(block: suspend () -> String?) {
        viewModelScope.launch {
            _state.update { it.copy(busy = true, message = null) }
            val message = try {
                block()
            } catch (e: ServerException) {
                e.message
            } catch (e: IOException) {
                "Could not reach the server: ${e.message}"
            } catch (e: Exception) {
                "Failed: ${e.message}"
            }
            // No message keeps one set meanwhile, such as a link opened at launch.
            _state.update { it.copy(busy = false, message = message ?: it.message) }
        }
    }
}
