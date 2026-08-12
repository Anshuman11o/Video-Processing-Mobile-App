package com.dayreel.upload

import java.io.File
import java.io.IOException
import java.io.RandomAccessFile
import java.util.concurrent.TimeUnit
import okhttp3.OkHttpClient
import okhttp3.Request
import okhttp3.RequestBody
import okhttp3.RequestBody.Companion.toRequestBody
import okhttp3.Response
import okio.BufferedSink
import org.json.JSONObject

/**
 * The three upload-lifecycle endpoints, plus the presigned PUT.
 *
 * OkHttp rather than the JS transport because this runs in a WorkManager worker
 * with no React instance attached — the whole point of stage 8A is that bytes
 * keep moving when there is no JavaScript alive to move them.
 *
 * Every call is blocking. Callers are coroutines on Dispatchers.IO.
 */
class UploadApi(baseUrl: String) {

    private val base = baseUrl.trimEnd('/')

    // POST with no body. Both endpoints accept an empty body by design — the
    // server reconciles against S3 rather than reading anything we send.
    private val noBody = ByteArray(0).toRequestBody(null)

    // No call timeout: a single part PUT is 5 MiB and a slow mobile connection
    // is a legitimate state, not a failure. Read/write timeouts still bound a
    // genuinely dead socket, which is the case that actually needs bounding.
    private val client =
        OkHttpClient.Builder()
            .connectTimeout(30, TimeUnit.SECONDS)
            .readTimeout(120, TimeUnit.SECONDS)
            .writeTimeout(120, TimeUnit.SECONDS)
            .retryOnConnectionFailure(true)
            .build()

    /** A part the server says is still missing, with a freshly signed URL. */
    data class MissingPart(val partNumber: Int, val url: String)

    /**
     * The server's answer to "what is left?".
     *
     * `uploadedCount` comes from S3's ListParts, not from anything this client
     * remembered. There is deliberately no ETag here: stage 8A [DECIDE 2] made
     * resume server-authoritative, and the client never holds one.
     */
    data class ResumePlan(
        val uploadId: String,
        val partSize: Long,
        val totalParts: Int,
        val uploadedCount: Int,
        val missing: List<MissingPart>,
    )

    /**
     * The request cannot succeed no matter how often it is repeated — an
     * aborted upload, an unknown job, a part list S3 rejects. Retrying these is
     * how an app ends up looping forever against an upload that no longer
     * exists, so they are a distinct type from the transient ones.
     */
    class TerminalException(val code: String, message: String) : IOException(message)

    /** Worth trying again later: a 5xx, a dropped socket, no network. */
    class TransientException(message: String, cause: Throwable? = null) :
        IOException(message, cause)

    /**
     * A presigned URL is no longer acceptable (HTTP 403).
     *
     * Recovered from by re-presigning rather than by failing, which is the
     * behaviour the re-presign endpoint exists to make possible. Every URL in a
     * batch was signed at the same instant, so one expiry means all of them.
     */
    class PresignExpiredException(val partNumber: Int) :
        IOException("the presigned URL for part $partNumber is no longer valid")

    /** `POST /jobs/{id}/upload-urls`. Changes no state; safe to call on every run. */
    fun fetchResumePlan(jobId: String): ResumePlan {
        val request =
            Request.Builder().url("$base/jobs/$jobId/upload-urls").post(noBody).build()

        execute(request).use { response ->
            val body = response.body?.string().orEmpty()
            if (!response.isSuccessful) {
                throw classify(response.code, body)
            }
            val json = JSONObject(body)
            val urls = json.optJSONArray("upload_urls")
            val missing = ArrayList<MissingPart>(urls?.length() ?: 0)
            for (i in 0 until (urls?.length() ?: 0)) {
                val entry = urls!!.getJSONObject(i)
                missing.add(MissingPart(entry.getInt("part_number"), entry.getString("url")))
            }
            return ResumePlan(
                uploadId = json.optString("upload_id"),
                partSize = json.getLong("part_size"),
                totalParts = json.getInt("total_parts"),
                uploadedCount = json.optJSONArray("uploaded_parts")?.length() ?: 0,
                missing = missing,
            )
        }
    }

    /**
     * `POST /jobs/{id}/complete` with an empty body.
     *
     * The empty body is the request, not a shortcut: the server derives the
     * part list from ListParts, so the completion is assembled from what S3
     * actually holds rather than from what this process believes it sent. That
     * is what lets a worker that was killed mid-upload finish an upload it
     * never saw the start of.
     */
    fun completeUpload(jobId: String) {
        val request =
            Request.Builder().url("$base/jobs/$jobId/complete").post(noBody).build()
        execute(request).use { response ->
            if (!response.isSuccessful) {
                throw classify(response.code, response.body?.string().orEmpty())
            }
        }
    }

    /** `DELETE /jobs/{id}/upload`. Idempotent server-side; failures are swallowed by callers. */
    fun abortUpload(jobId: String) {
        val request = Request.Builder().url("$base/jobs/$jobId/upload").delete().build()
        execute(request).use { response ->
            if (!response.isSuccessful) {
                throw classify(response.code, response.body?.string().orEmpty())
            }
        }
    }

    /**
     * PUT one byte range of the source file at a presigned URL.
     *
     * The bytes are streamed straight off disk. Nothing reads a 5 MiB part into
     * memory, which matters because this worker runs in a background process
     * that the OS is happy to kill for using any.
     */
    fun putPart(url: String, file: File, partNumber: Int, start: Long, length: Long): String {
        val request = Request.Builder().url(url).put(FileRangeBody(file, start, length)).build()

        val response =
            try {
                client.newCall(request).execute()
            } catch (e: IOException) {
                throw TransientException("part $partNumber: ${e.message}", e)
            }

        response.use {
            if (response.code == 403) {
                throw PresignExpiredException(partNumber)
            }
            if (!response.isSuccessful) {
                val message = "S3 returned ${response.code} for part $partNumber"
                // 4xx other than 403 is the request itself being wrong; repeating
                // it produces the same answer. 5xx is S3 having a moment.
                throw if (response.code in 400..499) {
                    TerminalException("S3_REJECTED", message)
                } else {
                    TransientException(message)
                }
            }
            // Not bookkeeping we keep: the ETag is read only to check S3
            // acknowledged the part. The completion list is derived server-side
            // from ListParts, so this value is deliberately never persisted.
            return response.header("ETag")
                ?: throw TransientException("S3 accepted part $partNumber with no ETag")
        }
    }

    private fun execute(request: Request): Response =
        try {
            client.newCall(request).execute()
        } catch (e: IOException) {
            throw TransientException("${request.url}: ${e.message}", e)
        }

    /**
     * Turn an API error body into the two categories the worker can act on.
     *
     * The backend gives every failure its own `code` precisely so the client's
     * recovery can differ, and the recovery that matters is binary: is there any
     * point trying again?
     */
    private fun classify(status: Int, body: String): IOException {
        val code =
            try {
                JSONObject(body).optString("code")
            } catch (_: Exception) {
                ""
            }
        val message = "HTTP $status ${code.ifEmpty { body.take(200) }}"

        return when {
            // NOT_FOUND, NO_UPLOAD, ALREADY_COMPLETE, UPLOAD_GONE, UPLOAD_MISMATCH,
            // INVALID_PARTS — all of them describe an upload that will never
            // accept another byte from this worker.
            status in 400..499 -> TerminalException(code.ifEmpty { "HTTP_$status" }, message)
            else -> TransientException(message)
        }
    }
}

/**
 * A request body that is one range of a file, streamed.
 *
 * Content-Type is deliberately null so OkHttp sends no header at all. It is not
 * part of what these URLs were signed over, and a Content-Type that disagrees
 * with the signature is a classic SignatureDoesNotMatch. It also keeps
 * LocalStack from trying to form-parse a 5 MiB body, which it answers with a
 * 413 wrapped in a 500.
 */
private class FileRangeBody(
    private val file: File,
    private val offset: Long,
    private val length: Long,
) : RequestBody() {

    override fun contentType() = null

    override fun contentLength() = length

    override fun writeTo(sink: BufferedSink) {
        RandomAccessFile(file, "r").use { raf ->
            raf.seek(offset)
            val buffer = ByteArray(64 * 1024)
            var remaining = length
            while (remaining > 0) {
                val want = minOf(buffer.size.toLong(), remaining).toInt()
                val read = raf.read(buffer, 0, want)
                if (read <= 0) {
                    // The file shrank under us. Failing here is the honest
                    // outcome: a short part is rejected by S3 anyway, and a
                    // silently truncated one would corrupt the assembled object.
                    throw IOException("source file ended early at offset ${offset + (length - remaining)}")
                }
                sink.write(buffer, 0, read)
                remaining -= read
            }
        }
    }
}
