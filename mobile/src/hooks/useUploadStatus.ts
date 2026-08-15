// What WorkManager believes about a job's transfer.
//
// Separate from `useJobPolling` because it answers a different question from a
// different source. The API knows whether an upload has been *completed*; only
// the device knows how many bytes of it are currently up. During the upload
// phase `GET /jobs/{id}` says `pending` or `uploading` and nothing else, so M6
// cannot come from there.
//
// This is device-local knowledge and it is lossy by design: WorkManager only
// knows about work this installation enqueued, so a job uploaded from another
// device — or before the app's data was cleared — simply reports NONE. The
// caller falls back to the job's own metrics in that case.

import {useEffect, useState} from 'react';

import {
  type UploadStatus,
  getBackgroundUploadStatus,
  isBackgroundUploadAvailable,
} from '../upload/nativeUploader';

/**
 * Faster than the 2s job poll: this is a local IPC call, not a network
 * request, and it is the only source for a number that should look live.
 */
export const UPLOAD_POLL_MS = 500;

/**
 * Poll the native uploader while `enabled`.
 *
 * Returns null when there is nothing to report — no native module, disabled, or
 * WorkManager has never heard of this job.
 */
export function useUploadStatus(
  jobId: string | null,
  enabled: boolean,
): UploadStatus | null {
  const [status, setStatus] = useState<UploadStatus | null>(null);

  useEffect(() => {
    setStatus(null);
  }, [jobId]);

  useEffect(() => {
    if (!jobId || !enabled || !isBackgroundUploadAvailable()) {
      return;
    }

    let cancelled = false;
    const read = async () => {
      try {
        const next = await getBackgroundUploadStatus(jobId);
        if (!cancelled) {
          // NONE means "not enqueued here", which is absence of information
          // rather than zero progress. Reporting it as a status would render a
          // 0.0 MB / 14.3 MB row over a job that finished uploading yesterday.
          setStatus(next.state === 'NONE' ? null : next);
        }
      } catch {
        // The module is Android-only and app-local; a throw here means it went
        // away, which the caller already handles as "no status".
        if (!cancelled) {
          setStatus(null);
        }
      }
    };

    read();
    const timer = setInterval(read, UPLOAD_POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [jobId, enabled]);

  return status;
}
