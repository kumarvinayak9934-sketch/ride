package com.safarshare.app

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.SerializationStrategy
import kotlinx.serialization.json.Json
import java.io.IOException
import java.net.HttpURLConnection
import java.net.URL

/**
 * Thin HttpURLConnection client — no extra HTTP dependency, works with the
 * same /v1 surface documented in docs/openapi.yaml. All calls suspend.
 */
object Api {
    private const val BASE = BuildConfig.API_BASE

    val json = Json {
        ignoreUnknownKeys = true
        isLenient = true
    }

    @Volatile
    var token: String? = null

    class ApiException(val problem: Problem) : Exception(problem.detail ?: problem.title)

    sealed class Result<out T> {
        data class Ok<T>(val value: T) : Result<T>()
        data class Err(val message: String) : Result<Nothing>()
    }

    fun <T> encode(serializer: SerializationStrategy<T>, value: T): String =
        json.encodeToString(serializer, value)

    suspend fun <T> request(
        method: String,
        path: String,
        serializer: kotlinx.serialization.KSerializer<T>,
        bodyJson: String? = null,
        idempotencyKey: String? = null,
    ): Result<T> = withContext(Dispatchers.IO) {
        try {
            val conn = URL("$BASE$path").openConnection() as HttpURLConnection
            conn.requestMethod = method
            conn.connectTimeout = 15_000
            conn.readTimeout = 20_000
            conn.setRequestProperty("Content-Type", "application/json")
            conn.setRequestProperty("Accept", "application/json")
            token?.let { conn.setRequestProperty("Authorization", "Bearer $it") }
            idempotencyKey?.let { conn.setRequestProperty("Idempotency-Key", it) }

            if (bodyJson != null) {
                conn.doOutput = true
                conn.outputStream.use { it.write(bodyJson.toByteArray()) }
            }

            val code = conn.responseCode
            val stream = if (code in 200..299) conn.inputStream else conn.errorStream
            val text = stream?.bufferedReader()?.use { it.readText() } ?: ""
            conn.disconnect()

            if (code in 200..299) {
                Result.Ok(json.decodeFromString(serializer, text))
            } else {
                val problem = runCatching {
                    json.decodeFromString(Problem.serializer(), text)
                }.getOrElse { Problem(title = "HTTP $code", status = code) }
                Result.Err(problem.detail ?: problem.title)
            }
        } catch (e: IOException) {
            Result.Err(e.message ?: "network error")
        }
    }
}
