package com.dayreel.upload

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.Context
import android.content.pm.ApplicationInfo
import android.content.pm.ServiceInfo
import android.os.Build
import android.util.Log
import androidx.work.CoroutineWorker
import androidx.work.Data
import androidx.work.ForegroundInfo
import androidx.work.WorkerParameters
import androidx.work.workDataOf
import java.io.File
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext

/**
 * Uploads one video to S3 in the background, and keeps doing it across process
 * death.
 *
 * This is stage 8A [DECIDE 1] settled as true background transfer. The reason it
 * is a WorkManager worker and not a JavaScript loop is narrow and worth stating:
 * WorkManager persists the work request in its own database and re-registers it
 * with JobScheduler, so when the OS kills this app's process the system starts
 * the process again — with no Activity, no React instance and no JavaScript — for
 * the sole purpose of running `doWork` again. A JS uploader dies with the process
 * and cannot come back until a human opens the app.
 *
 * It holds no upload progress of its own. Every run begins by asking
 * `POST /jobs/{id}/upload-urls` what S3 is already holding, which is stage 8A
 * [DECIDE 2]: the only copy of "what landed" lives in the system that holds the
 * bytes. That is also what keeps this worker safe to restart at any instant —
 * there is no partially-written local state for a kill to corrupt, and it never
 * re-uploads a part number S3 already has. The last point is not just tidiness:
 * LocalStack sets the ETag of a re-uploaded part to the literal string "None"
 * and the completion then fails with InvalidPart.
 */
class UploadWorker(context: Context, params: WorkerParameters) :
    CoroutineWorker(context, params) {

    companion object {
        const val TAG = "DayReelUpload"

        const val KEY_JOB_ID = "job_id"
        const val KEY_API_BASE_URL = "api_base_url"
        const val KEY_FILE_PATH = "file_path"
        const val KEY_DEBUG_PART_DELAY_MS = "debug_part_delay_ms"

        const val KEY_PROGRESS_UPLOADED = "uploaded_parts"
        const val KEY_PROGRESS_TOTAL = "total_parts"
        const val KEY_PROGRESS_FRACTION = "fraction"
        const val KEY_FAILURE_REASON = "failure_reason"

        /** Unique work name for a job. One upload per job, ever. */
        fun workName(jobId: String) = "dayreel-upload-$jobId"

        private const val NOTIFICATION_ID = 4201
        private const val CHANNEL_ID = "dayreel-uploads"

        /**
         * Attempts per part, and the backoff between them.
         *
         * Mirrors `mobile/src/upload/parts.ts:retryDelayMs` exactly — 1s, 2s, 4s
         * over three attempts — rather than inventing a second policy. Two retry
         * schedules for the same operation on the same service is how a system
         * ends up with a behaviour nobody can state.
         */
        private const val MAX_PART_ATTEMPTS = 3

        fun retryDelayMs(attempt: Int): Long =
            1000L shl (attempt - 1).coerceAtLeast(0)

        /**
         * How many times one run will re-ask the server what is missing.
         *
         * A round ends early on an expired presign, so this bounds the
         * re-presign loop rather than the upload. Hitting it means something is
         * wrong that another round will not fix, and the run yields to
         * WorkManager's own backoff instead of spinning.
         */
        private const val MAX_ROUNDS = 20
    }

    override suspend fun getForegroundInfo(): ForegroundInfo = foregroundInfo(0, 0)

    override suspend fun doWork(): Result = withContext(Dispatchers.IO) {
        val jobId = inputData.getString(KEY_JOB_ID) ?: return@withContext fail("MISSING_JOB_ID")
        val apiBaseUrl =
            inputData.getString(KEY_API_BASE_URL) ?: return@withContext fail("MISSING_API_URL")
        val filePath = inputData.getString(KEY_FILE_PATH) ?: return@withContext fail("MISSING_FILE")

        // Best effort. A foreground service is what keeps a long upload alive
        // against Doze and background limits, but Android 12+ can refuse to
        // start one from the background. Refusal downgrades this to ordinary
        // deferrable work; it does not stop the upload, and pretending the
        // upload failed because the notification did would be a lie.
        try {
            setForeground(foregroundInfo(0, 0))
        } catch (e: Throwable) {
            Log.w(TAG, "job $jobId: continuing without a foreground service: ${e.message}")
        }

        val api = UploadApi(apiBaseUrl)
        val file = File(filePath)

        // Stage 8A [DECIDE 5]'s backstop. DocumentDir survives cache reclamation,
        // but not an uninstall, not a user deleting it, and not a copy that was
        // interrupted. Every part of the resume mechanism can work perfectly and
        // there can still be no bytes to send, so this is checked before the
        // server is asked anything.
        if (!file.isFile || file.length() == 0L) {
            Log.e(TAG, "job $jobId: source file is gone ($filePath); aborting the upload")
            runCatching { api.abortUpload(jobId) }
                .onFailure { Log.w(TAG, "job $jobId: abort after missing source failed: $it") }
            return@withContext fail("SOURCE_MISSING")
        }

        val debugDelayMs = debugPartDelayMs()

        try {
            for (round in 1..MAX_ROUNDS) {
                val plan = api.fetchResumePlan(jobId)

                // The server sizes the upload from what the client declared at
                // POST /jobs. If that disagrees with the file on disk now, every
                // byte range below would be signed for the wrong bytes and the
                // object would assemble into garbage. Same guard, and the same
                // reasoning, as uploader.ts's part-count check.
                val expectedParts =
                    ((file.length() + plan.partSize - 1) / plan.partSize).toInt()
                if (expectedParts != plan.totalParts) {
                    Log.e(
                        TAG,
                        "job $jobId: file is ${file.length()} bytes = $expectedParts parts at " +
                            "${plan.partSize}, but the job was created for ${plan.totalParts}",
                    )
                    return@withContext fail("FILE_MISMATCH")
                }

                // Presence is not correctness. See partSizeFaults.
                val faults = partSizeFaults(plan, file.length())
                if (faults.isNotEmpty()) {
                    Log.e(TAG, "job $jobId: S3 holds wrong-sized parts: ${faults.joinToString()}")
                    // No recovery is available from here: /upload-urls presigns
                    // only the parts it considers missing, and it considers a
                    // short part present, so this client can never obtain a URL
                    // to overwrite it. Abort rather than complete a corrupt
                    // object or retry forever against one.
                    runCatching { api.abortUpload(jobId) }
                        .onFailure { Log.w(TAG, "job $jobId: abort after size fault failed: $it") }
                    return@withContext fail("PART_SIZE_MISMATCH")
                }

                reportProgress(plan.uploadedCount, plan.totalParts)

                // Everything S3 holds for this upload, kept in step as parts go
                // up so completion can assert the whole file is there.
                var bytesOnS3 = plan.bytesOnS3

                if (plan.missing.isEmpty()) {
                    // Every part is on S3 and the upload was never finalised.
                    // This is the case a client-held ETag array gets wrong and
                    // ListParts gets right: the process that uploaded the last
                    // part may never have existed in this run at all.
                    Log.i(TAG, "job $jobId: all ${plan.totalParts} parts present; completing")
                    assertWholeFileOnS3(jobId, bytesOnS3, file.length())
                    api.completeUpload(jobId)
                    deleteSourceCopy(file)
                    reportProgress(plan.totalParts, plan.totalParts)
                    return@withContext Result.success(
                        workDataOf(
                            KEY_PROGRESS_UPLOADED to plan.totalParts,
                            KEY_PROGRESS_TOTAL to plan.totalParts,
                            KEY_PROGRESS_FRACTION to 1.0,
                        )
                    )
                }

                Log.i(
                    TAG,
                    "job $jobId round $round: ${plan.uploadedCount}/${plan.totalParts} on S3, " +
                        "uploading ${plan.missing.size} missing",
                )

                var done = plan.uploadedCount
                var expired = false

                for (part in plan.missing) {
                    if (isStopped) {
                        // WorkManager is taking the worker away — network lost,
                        // cancelled, or the OS reclaiming it. Yield rather than
                        // fight; the next run re-reads S3 and picks up here.
                        Log.i(TAG, "job $jobId: stopped after $done/${plan.totalParts} parts")
                        return@withContext Result.retry()
                    }

                    // Stage 8A [DECIDE 6]. Over loopback a whole upload finishes
                    // in under a second, which leaves no wall-clock window in
                    // which to kill the app — a kill test that cannot reach the
                    // state it tests passes vacuously. Zero unless a debug build
                    // was explicitly given a delay.
                    if (debugDelayMs > 0) {
                        delay(debugDelayMs)
                    }

                    val start = (part.partNumber - 1).toLong() * plan.partSize
                    val length = minOf(plan.partSize, file.length() - start)

                    try {
                        putPartWithRetry(api, part, file, start, length)
                    } catch (e: UploadApi.PresignExpiredException) {
                        // Every URL in this batch was signed at the same moment,
                        // so they are all expired. Re-presigning is a round, not
                        // a failure — which is the whole reason the re-presign
                        // endpoint exists.
                        Log.i(TAG, "job $jobId: presigned URLs expired; re-presigning")
                        expired = true
                        break
                    }

                    done++
                    bytesOnS3 += length
                    reportProgress(done, plan.totalParts)
                }

                if (!expired && done >= plan.totalParts) {
                    Log.i(TAG, "job $jobId: uploaded ${plan.totalParts} parts; completing")
                    assertWholeFileOnS3(jobId, bytesOnS3, file.length())
                    api.completeUpload(jobId)
                    deleteSourceCopy(file)
                    return@withContext Result.success(
                        workDataOf(
                            KEY_PROGRESS_UPLOADED to plan.totalParts,
                            KEY_PROGRESS_TOTAL to plan.totalParts,
                            KEY_PROGRESS_FRACTION to 1.0,
                        )
                    )
                }
            }

            Log.w(TAG, "job $jobId: $MAX_ROUNDS rounds without finishing; backing off")
            Result.retry()
        } catch (e: UploadApi.TerminalException) {
            if (e.code == "ALREADY_COMPLETE") {
                // Another run of this worker finished it. Not an error — the
                // upload is done, which is all anybody asked for.
                Log.i(TAG, "job $jobId: already complete server-side")
                deleteSourceCopy(file)
                return@withContext Result.success()
            }
            Log.e(TAG, "job $jobId: terminal failure ${e.code}: ${e.message}")
            fail(e.code)
        } catch (e: UploadApi.TransientException) {
            // Result.retry() re-runs under the request's backoff AND its network
            // constraint, so an upload that lost signal waits for signal rather
            // than hammering a dead radio.
            Log.w(TAG, "job $jobId: transient failure, will retry: ${e.message}")
            Result.retry()
        } catch (e: Exception) {
            Log.e(TAG, "job $jobId: unexpected failure", e)
            Result.retry()
        }
    }

    /**
     * Parts S3 is holding at the wrong size, described for a log line.
     *
     * Stage 8A [DECIDE 2] made resume server-authoritative through ListParts,
     * and ListParts answers "which part numbers exist" — not "are they intact".
     * Killing this worker mid-PUT (which is 8A's own test method) can leave a
     * part that was cut off in transit but is still listed, and the next run
     * then skips it as already done.
     *
     * S3 catches that only by accident: a non-final part under 5 MiB fails the
     * completion with EntityTooSmall. A truncation that happens to land above
     * 5 MiB is accepted, and the assembled object has a hole in the middle that
     * nothing downstream can see. Observed here as a 524288-byte part 2 of
     * 5242880 after a force-stop, so this is not a hypothetical.
     */
    private fun partSizeFaults(plan: UploadApi.ResumePlan, fileLength: Long): List<String> =
        plan.uploaded.mapNotNull { part ->
            val start = (part.partNumber - 1).toLong() * plan.partSize
            val expected = minOf(plan.partSize, fileLength - start)
            if (part.size == expected) {
                null
            } else {
                "part ${part.partNumber} is ${part.size}B, expected ${expected}B"
            }
        }

    /**
     * Refuse to finalise an upload whose bytes do not add up to the file.
     *
     * The per-part check above localises a fault; this one states the invariant
     * that actually matters, and covers arithmetic that agrees with itself but
     * not with the file. A successful CompleteMultipartUpload is not evidence
     * the object is intact — it is only evidence S3 was willing to assemble it.
     */
    private fun assertWholeFileOnS3(jobId: String, bytesOnS3: Long, fileLength: Long) {
        if (bytesOnS3 != fileLength) {
            throw UploadApi.TerminalException(
                "INCOMPLETE_OBJECT",
                "job $jobId: S3 holds $bytesOnS3 bytes but the source is $fileLength; " +
                    "refusing to complete an object with a hole in it",
            )
        }
    }

    /** Per-part retry, 1s/2s/4s. See MAX_PART_ATTEMPTS for why those numbers. */
    private suspend fun putPartWithRetry(
        api: UploadApi,
        part: UploadApi.MissingPart,
        file: File,
        start: Long,
        length: Long,
    ) {
        var last: Exception? = null
        for (attempt in 1..MAX_PART_ATTEMPTS) {
            if (isStopped) {
                throw UploadApi.TransientException("worker stopped")
            }
            try {
                api.putPart(part.url, file, part.partNumber, start, length)
                return
            } catch (e: UploadApi.PresignExpiredException) {
                throw e
            } catch (e: UploadApi.TerminalException) {
                throw e
            } catch (e: Exception) {
                last = e
                Log.w(TAG, "part ${part.partNumber} attempt $attempt failed: ${e.message}")
                if (attempt < MAX_PART_ATTEMPTS) {
                    delay(retryDelayMs(attempt))
                }
            }
        }
        throw UploadApi.TransientException(
            "part ${part.partNumber} failed after $MAX_PART_ATTEMPTS attempts: ${last?.message}",
            last,
        )
    }

    private suspend fun reportProgress(done: Int, total: Int) {
        val fraction = if (total > 0) done.toDouble() / total else 0.0
        setProgress(
            workDataOf(
                KEY_PROGRESS_UPLOADED to done,
                KEY_PROGRESS_TOTAL to total,
                KEY_PROGRESS_FRACTION to fraction,
            )
        )
        runCatching { setForeground(foregroundInfo(done, total)) }
    }

    /**
     * The app keeps a private copy of every video with an unfinished upload
     * (stage 8A [DECIDE 5]), so the copy has to be released the moment it stops
     * being needed — and the worker is the only thing that reliably knows when
     * that is, since it may finish while nothing else in the app is running.
     */
    private fun deleteSourceCopy(file: File) {
        val filesDir = applicationContext.filesDir?.canonicalPath ?: return
        // Only ever delete inside app-private storage. The path arrives as
        // worker input and a bug upstream must not turn into deleting a user's
        // file somewhere else.
        if (!file.canonicalPath.startsWith(filesDir)) {
            return
        }
        if (file.delete()) {
            Log.i(TAG, "removed the local copy at ${file.name}")
        }
    }

    private fun fail(reason: String): Result =
        Result.failure(workDataOf(KEY_FAILURE_REASON to reason))

    /**
     * Zero unless this is a debuggable build AND a delay was passed in.
     *
     * Two gates rather than one because this is test-only code shipped inside
     * the product. `UPLOAD_PART_SIZE` and `S3_PUBLIC_ENDPOINT` are already
     * settings whose correct value depends on where the stack points; this one
     * must be incapable of taking a wrong value in a release build at all.
     */
    private fun debugPartDelayMs(): Long {
        val debuggable =
            (applicationContext.applicationInfo.flags and ApplicationInfo.FLAG_DEBUGGABLE) != 0
        if (!debuggable) {
            return 0
        }
        val requested = inputData.getLong(KEY_DEBUG_PART_DELAY_MS, 0L)
        return requested.coerceIn(0L, 10_000L)
    }

    private fun foregroundInfo(done: Int, total: Int): ForegroundInfo {
        val manager = applicationContext.getSystemService(NotificationManager::class.java)
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            manager?.createNotificationChannel(
                NotificationChannel(
                    CHANNEL_ID,
                    "Video uploads",
                    NotificationManager.IMPORTANCE_LOW,
                )
            )
        }

        @Suppress("DEPRECATION")
        val builder =
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                Notification.Builder(applicationContext, CHANNEL_ID)
            } else {
                Notification.Builder(applicationContext)
            }

        val notification =
            builder
                .setContentTitle("Uploading video")
                .setContentText(if (total > 0) "$done of $total parts" else "Starting…")
                .setSmallIcon(android.R.drawable.stat_sys_upload)
                .setOngoing(true)
                .apply { if (total > 0) setProgress(total, done, false) }
                .build()

        return if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            ForegroundInfo(
                NOTIFICATION_ID,
                notification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC,
            )
        } else {
            ForegroundInfo(NOTIFICATION_ID, notification)
        }
    }
}

/** Kept out of the class so the module can build input data without a worker. */
fun uploadInputData(
    jobId: String,
    apiBaseUrl: String,
    filePath: String,
    debugPartDelayMs: Long,
): Data =
    workDataOf(
        UploadWorker.KEY_JOB_ID to jobId,
        UploadWorker.KEY_API_BASE_URL to apiBaseUrl,
        UploadWorker.KEY_FILE_PATH to filePath,
        UploadWorker.KEY_DEBUG_PART_DELAY_MS to debugPartDelayMs,
    )
