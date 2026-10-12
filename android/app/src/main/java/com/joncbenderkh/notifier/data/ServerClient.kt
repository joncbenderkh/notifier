// SPDX-License-Identifier: GPL-3.0-or-later
package com.joncbenderkh.notifier.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.json.Json
import okhttp3.HttpUrl
import okhttp3.HttpUrl.Companion.toHttpUrl
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import java.io.IOException

/** A non-2xx response from the server, carrying its `error` message. */
class ServerException(val status: Int, message: String) : IOException(message)

@Serializable
data class RegisterResponse(
    @SerialName("device_id") val deviceId: String,
    @SerialName("device_token") val deviceToken: String,
    @SerialName("seal_key") val sealKey: String,
)

@Serializable
private data class RegisterRequest(
    val code: String,
    val name: String,
    @SerialName("fcm_token") val fcmToken: String,
)

@Serializable
private data class UpdateTokenRequest(@SerialName("fcm_token") val fcmToken: String)

@Serializable
private data class ErrorResponse(val error: String)

/** Blocking client for the device-facing half of the notifier API. */
class ServerClient(private val http: OkHttpClient = sharedClient) {
    private val json = Json { ignoreUnknownKeys = true }
    private val jsonType = "application/json".toMediaType()

    fun register(serverUrl: String, code: String, name: String, fcmToken: String): RegisterResponse {
        val body = json.encodeToString(RegisterRequest(code, name, fcmToken)).toRequestBody(jsonType)
        val raw = execute(Request.Builder().url(endpoint(serverUrl, "v1/devices")).post(body))
        return json.decodeFromString(raw)
    }

    fun updateFcmToken(pairing: Pairing, fcmToken: String) {
        val body = json.encodeToString(UpdateTokenRequest(fcmToken)).toRequestBody(jsonType)
        execute(authorized(pairing, "v1/device").put(body))
    }

    fun sendTest(pairing: Pairing) {
        execute(authorized(pairing, "v1/device/test").post(EMPTY_BODY))
    }

    fun unpair(pairing: Pairing) {
        execute(authorized(pairing, "v1/device").delete())
    }

    private fun authorized(pairing: Pairing, path: String): Request.Builder =
        Request.Builder()
            .url(endpoint(pairing.serverUrl, path))
            .header("Authorization", "Bearer ${pairing.deviceToken}")

    private fun endpoint(serverUrl: String, path: String): HttpUrl =
        serverUrl.trimEnd('/').toHttpUrl().newBuilder().addPathSegments(path).build()

    private fun execute(request: Request.Builder): String =
        http.newCall(request.build()).execute().use { response ->
            val raw = response.body.string()
            if (!response.isSuccessful) {
                val message = runCatching { json.decodeFromString<ErrorResponse>(raw).error }
                    .getOrDefault("HTTP ${response.code}")
                throw ServerException(response.code, message)
            }
            raw
        }

    private companion object {
        val sharedClient = OkHttpClient()
        val EMPTY_BODY: RequestBody = ByteArray(0).toRequestBody()
    }
}
