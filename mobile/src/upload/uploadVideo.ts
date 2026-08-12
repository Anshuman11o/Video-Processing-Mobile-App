// The whole upload, end to end: create the job, PUT every part, finalize.
//
// Kept separate from the screens so the sequence — and specifically the fact
// that `completeUpload` is what starts the pipeline — is stated in one place
// rather than buried in a button handler.

import {completeUpload, createJob} from '../api/client';
import {JobIndexStore, recordJob} from '../storage/jobIndex';
import {CompletedPart} from '../types/api';
import {PartTransport, uploadParts} from './uploader';

export interface SelectedVideo {
  /** A readable `file://` path — the picker's `fileCopyUri`, not its `uri`. */
  fileUri: string;
  filename: string;
  sizeBytes: number;
  contentType: string;
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
    await recordJob(store, {
      job_id: created.job_id,
      filename: video.filename,
      created_at: new Date().toISOString(),
    });
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
