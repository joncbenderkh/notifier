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
import com.joncbenderkh.notifier.data.HistoryEntry
import com.joncbenderkh.notifier.data.HistoryStore
import com.joncbenderkh.notifier.data.NotifierLink
import com.joncbenderkh.notifier.data.Pairing
import com.joncbenderkh.notifier.data.PairingStore
import com.joncbenderkh.notifier.data.ServerClient
import com.joncbenderkh.notifier.data.ServerException
import com.joncbenderkh.notifier.push.Answers
import com.joncbenderkh.notifier.push.ChannelRoute
import com.joncbenderkh.notifier.push.Notifications
import com.joncbenderkh.notifier.push.Receipts
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch
import kotlinx.coroutines.tasks.await
import kotlinx.coroutines.withContext
import java.io.IOException
import java.util.concurrent.atomic.AtomicLong

/** A message for the user; the ID tells two with the same text apart. */
data class Notice(val id: Long, val text: String)

private val noticeIds = AtomicLong()

/** Queues [text] for the user, if any. */
private fun UiState.say(text: String?) =
    if (text == null) this else copy(messages = messages + Notice(noticeIds.incrementAndGet(), text))

data class UiState(
    val pairing: Pairing? = null,
    val busy: Boolean = false,
    /** Messages for the user, oldest first; each shows once, then is removed. */
    val messages: List<Notice> = emptyList(),
    /** A scanned or opened pairing link waiting to fill the form. */
    val prefill: NotifierLink.Pair? = null,
    /** The channels this device could subscribe to; null until loaded. */
    val channels: List<DeviceChannel>? = null,
    /** A scanned or opened channel invite waiting for the user to confirm. */
    val pendingJoin: NotifierLink.Join? = null,
    /** The history screen is open in place of the main one. */
    val showHistory: Boolean = false,
    /** Messages whose question this phone is answering from the history. */
    val answering: Set<String> = emptySet(),
    /** Why the last pairing attempt failed, shown under the form. */
    val pairError: String? = null,
    /** Just paired: explain the notification permission before Android asks for it. */
    val askNotifications: Boolean = false,
)

class MainViewModel(application: Application) : AndroidViewModel(application) {
    private val store = PairingStore(application)
    private val client = ServerClient()
    private val historyStore = HistoryStore.get(application)

    private val _state = MutableStateFlow(UiState(pairing = store.load()))
    val state: StateFlow<UiState> = _state.asStateFlow()

    val defaultDeviceName: String = Build.MODEL

    /** Received notifications, newest first; updated as messages arrive. */
    val history: StateFlow<List<HistoryEntry>> = historyStore.entries

    init {
        _state.value.pairing?.let {
            syncFcmToken(it)
            refreshChannels()
        }
        editHistory { historyStore.load() }
    }

    fun openHistory() {
        if (_state.value.pairing == null) return
        _state.update { it.copy(showHistory = true) }
        // Drops what expired while the app stayed open.
        editHistory { historyStore.load() }
    }

    fun closeHistory() {
        _state.update { it.copy(showHistory = false) }
    }

    fun deleteHistory(id: String) = editHistory("Notification deleted.") { historyStore.delete(id) }

    fun clearHistory() = editHistory("History cleared.") { historyStore.clear() }

    /** Tells the sender of [entry], if they asked for receipts, that it was opened. */
    fun opened(entry: HistoryEntry) {
        // The pairing it was received under: the history belongs to it.
        val deviceId = _state.value.pairing?.deviceId ?: return
        if (!entry.receipts) return
        Receipts.reportLater(getApplication(), entry.id, Receipts.OPENED, deviceId)
        _state.update { it.say("The sender can see you opened it.") }
    }

    /** Answers [entry]'s question with [option], and says what became of it. */
    fun answer(entry: HistoryEntry, option: String) {
        val reply = entry.reply ?: return
        val deviceId = _state.value.pairing?.deviceId ?: return
        if (entry.id in _state.value.answering) return
        _state.update { it.copy(answering = it.answering + entry.id) }
        viewModelScope.launch {
            val outcome = withContext(Dispatchers.IO) { Answers.send(getApplication(), entry.id, reply, option, deviceId, entry.receipts) }
            _state.update { it.copy(answering = it.answering - entry.id).say(outcome?.text) }
        }
    }

    private fun editHistory(done: String? = null, block: () -> Unit) {
        viewModelScope.launch {
            try {
                withContext(Dispatchers.IO) { block() }
                _state.update { it.say(done) }
            } catch (e: Exception) {
                if (e is CancellationException) throw e
                _state.update { it.say("Could not update the history: ${e.message}") }
            }
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
            _state.update { it.copy(pairError = validationError) }
            return
        }
        viewModelScope.launch {
            _state.update { it.copy(busy = true, pairError = null) }
            val pairing = try {
                val fcmToken = FirebaseMessaging.getInstance().token.await()
                val name = deviceName.trim().ifEmpty { defaultDeviceName }
                val response = withContext(Dispatchers.IO) { client.register(url, code.trim(), name, fcmToken) }
                Pairing(url, response.deviceId, name, response.deviceToken, response.sealKey).also { store.save(it) }
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                _state.update { it.copy(busy = false, pairError = pairingError(e)) }
                return@launch
            }
            _state.update { it.copy(pairing = pairing, askNotifications = true) }
            // Loaded here rather than through run(), which would clear "Paired." before it shows.
            val loaded = try {
                loadChannels(pairing)
                null
            } catch (e: CancellationException) {
                throw e
            } catch (e: Exception) {
                e
            }
            _state.update {
                it.copy(busy = false).say(if (loaded == null) "Paired." else "Paired, but the channels did not load: ${loaded.message}")
            }
        }
    }

    private fun pairingError(e: Exception): String = when {
        // The server answers one 403 for every unusable code, on purpose.
        e is ServerException && e.status == 403 ->
            "This code is invalid, expired or already used, or your account has reached its device " +
                "limit. Create a new pairing code on your server."
        e is ServerException -> e.message ?: "The server refused the pairing."
        e is IOException -> "Could not reach the server: ${e.message}"
        else -> "Failed: ${e.message}"
    }

    fun clearPairError() {
        _state.update { it.copy(pairError = null) }
    }

    fun notificationsAsked() {
        _state.update { it.copy(askNotifications = false) }
    }

    /**
     * Handles a scanned QR code or an opened `notifier://` link. Neither kind
     * acts by itself: a pairing link fills the form for the user to review and
     * tap Pair, and a channel invite asks for confirmation, so a hostile QR
     * code cannot silently redirect the phone or subscribe it to anything.
     */
    fun openLink(raw: String) {
        val link = NotifierLink.parse(raw, allowHttp = BuildConfig.DEBUG)
        // Back to the main screen, where the form and the join dialog are.
        _state.update { it.copy(showHistory = false) }
        _state.update {
            val pairing = it.pairing
            when (link) {
                null -> it.say("That is not a Notifier code.")
                is NotifierLink.Pair ->
                    if (pairing != null) it.say("Already paired. Unpair first to pair with another server.")
                    else it.copy(prefill = link, pairError = null)
                is NotifierLink.Join -> when {
                    pairing == null -> it.say("Pair this phone first, then open the invite again.")
                    // Pairings saved before origins were normalized may be spelled differently.
                    link.serverUrl != (NotifierLink.origin(pairing.serverUrl, allowHttp = true) ?: pairing.serverUrl) ->
                        it.say("That invite is for another server (${link.serverUrl}).")
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
        val name = channels.firstOrNull { it.id == channelId }?.name ?: return
        run(if (subscribed) "Subscribed to $name." else "Unsubscribed from $name.") {
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

    /** Sends this user's receipts on a channel they joined, where the owner offers them, or stops. */
    fun setSendsReceipts(channelId: String, sends: Boolean) {
        val pairing = _state.value.pairing ?: return
        val name = _state.value.channels?.firstOrNull { it.id == channelId }?.name ?: return
        run(if (sends) "Sending receipts on $name." else "No receipts on $name.") {
            val generation = ++channelsGeneration
            val got = try {
                withContext(Dispatchers.IO) { client.acceptReceipts(pairing, channelId, sends) }
            } catch (e: ServerException) {
                // The owner may have changed the setting meanwhile.
                runCatching { loadChannels(pairing) }
                throw e
            }
            if (generation == channelsGeneration) {
                _state.update { s ->
                    s.copy(channels = s.channels?.map {
                        if (it.id == channelId) it.copy(receipts = got.receipts, receiptsAccepted = got.receiptsAccepted) else it
                    })
                }
            }
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
        _state.update { it.say(message) }
    }

    fun sendTest() {
        val pairing = _state.value.pairing ?: return
        run("Test notification sent.") { withContext(Dispatchers.IO) { client.sendTest(pairing) } }
    }

    fun unpair() {
        val pairing = _state.value.pairing ?: return
        viewModelScope.launch {
            _state.update { it.copy(busy = true) }
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
            // The next pairing may be another account: its history starts empty.
            withContext(Dispatchers.IO) { historyStore.clear() }
            _state.update { UiState(messages = it.messages).say(message) }
        }
    }

    /** The snackbar for [id] has been shown. */
    fun messageShown(id: Long) {
        _state.update { state -> state.copy(messages = state.messages.filterNot { it.id == id }) }
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
            _state.update { it.copy(busy = true) }
            val message = try {
                block()
            } catch (e: ServerException) {
                e.message
            } catch (e: IOException) {
                "Could not reach the server: ${e.message}"
            } catch (e: Exception) {
                "Failed: ${e.message}"
            }
            _state.update { it.copy(busy = false).say(message) }
        }
    }
}
