// The whole upload, end to end: create the job, PUT every part, finalize.
//
// Kept separate from the screens so the sequence — and specifically the fact
// that `completeUpload` is what starts the pipeline — is stated in one place
// rather than buried in a button handler.

import {API_BASE_URL, completeUpload, createJob} from '../api/client';
import {JobIndexStore, recordJob} from '../storage/jobIndex';
import {CompletedPart} from '../types/api';
import {startBackgroundUpload} from './nativeUploader';
import {PartTransport, uploadParts} from './uploader';

export interface SelectedVideo {
  /** A readable `file://` path — the picker's `fileCopyUri`, not its `uri`. */
  fileUri: string;
  filename: string;
  sizeBytes: number;
  contentType: string;
  /**
   * Video length in seconds, if the decoder has reported it. Not sent to the
   * API — `POST /jobs` has no field for it — but recorded in the local index so
   * the job list can show it before the pipeline has produced any output.
   */
  durationSeconds?: number;
}

/** The local-index row for a video, kept in one place so both paths agree. */
function indexEntry(jobId: string, video: SelectedVideo) {
  return {
    job_id: jobId,
    filename: video.filename,
    created_at: new Date().toISOString(),
    size_bytes: video.sizeBytes,
    duration_seconds: video.durationSeconds,
  };
}

export interface UploadVideoOptions {
  video: SelectedVideo;
  transport: PartTransport;
  store?: JobIndexStore;
  onProgress?: (fraction: number) => void;
  signal?: AbortSignal;
}

export interface UploadVideoResult {
  jobId: string;
  parts: CompletedPart[];
}

/**
 * Upload a video and start its pipeline job.
 *
 * The job is recorded in the local index as soon as it has an id — before the
 * bytes go up, deliberately. If the upload then fails, the job still appears in
 * the list where it can be inspected; recording it only on success would leave
 * a half-uploaded job invisible and unreachable, since the API has no
 * `GET /jobs` to rediscover it with.
 */
export async function uploadVideo(
  opts: UploadVideoOptions,
): Promise<UploadVideoResult> {
  const {video, transport, store, onProgress, signal} = opts;

  const created = await createJob({
    filename: video.filename,
    size_bytes: video.sizeBytes,
    content_type: video.contentType,
  });

  if (store) {
    await recordJob(store, indexEntry(created.job_id, video));
  }

  const parts = await uploadParts({
    fileUri: video.fileUri,
    fileSize: video.sizeBytes,
    partSize: created.part_size,
    urls: created.upload_urls,
    transport,
    onProgress,
    signal,
  });

  // Not bookkeeping: CompleteMultipartUpload emits the S3 event that starts
  // the pipeline. Without this call the parts sit in S3 and nothing runs.
  await completeUpload(created.job_id, {
    upload_id: created.upload_id,
    parts,
  });

  return {jobId: created.job_id, parts};
}

/**
 * Create the job, then hand the transfer to WorkManager and return immediately.
 *
 * This is the [DECIDE 1] path: the bytes must keep going up after the app is
 * killed, which JavaScript cannot do because the runtime dies with the process.
 * So JS does the one thing that must happen while a user is present — creating
 * the job — and native does the part that has to outlive them.
 *
 * It returns as soon as the work is enqueued, NOT when the upload finishes.
 * There is no completion callback on purpose: the caller may not exist by then.
 * Progress is read back with `getBackgroundUploadStatus`, and the job's real
 * state is always available from `GET /jobs/{id}` regardless of what this
 * device remembers.
 *
 * Note it does not call `completeUpload` — the worker does that itself once the
 * last part lands, for the same reason.
 */
export async function startBackgroundVideoUpload(opts: {
  video: SelectedVideo;
  store?: JobIndexStore;
  /** [DECIDE 6]: >0 slows the worker so an upload can be interrupted on purpose. */
  debugPartDelayMs?: number;
}): Promise<{jobId: string}> {
  const {video, store, debugPartDelayMs} = opts;

  const created = await createJob({
    filename: video.filename,
    size_bytes: video.sizeBytes,
    content_type: video.contentType,
  });

  // Recorded before the transfer starts, deliberately. If the app dies during
  // the upload — which is the case this whole path exists to survive — the job
  // must still be in the list when it comes back, or the device has no way to
  // rediscover it: there is no GET /jobs.
  if (store) {
    await recordJob(store, indexEntry(created.job_id, video));
  }

  await startBackgroundUpload({
    jobId: created.job_id,
    apiBaseUrl: API_BASE_URL,
    filePath: video.fileUri,
    debugPartDelayMs,
  });

  return {jobId: created.job_id};
}
