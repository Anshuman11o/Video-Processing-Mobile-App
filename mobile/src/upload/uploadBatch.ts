// Uploading a selection of clips, all at once.
//
// Every clip is launched up front rather than chained: the progress screen then
// shows N bars moving together, which reads as "the batch is going up" instead
// of "one file is going up and the rest are waiting". Each clip still gets its
// own job and its own WorkManager request — only the launch pattern is
// concurrent.
//
// What that does NOT mean is true simultaneity. WorkManager applies its own
// scheduling constraints and may well run some of these one after another; that
// is its business and this module does not fight it. It reports whatever state
// each request is actually in, so a clip WorkManager is holding shows as queued
// rather than as a stalled upload.
//
// Two properties matter and both are tested:
//   - every clip is started before any of them finishes
//   - a clip that fails neither stalls nor cancels the others
//
// This does not reimplement the uploader. Both implementations below delegate
// to `uploadVideo.ts`; what is new here is only the launch, the isolation of
// failures, and the per-clip progress bookkeeping.

import type {PickedClip} from '../types/clips';
import {blobTransport} from './blobTransport';
import {getBackgroundUploadStatus} from './nativeUploader';
import {startBackgroundVideoUpload, uploadVideo} from './uploadVideo';
import type {JobIndexStore} from '../storage/jobIndex';

/**
 * Where a clip is.
 *
 * `queued` covers both "not launched yet" and "launched, and WorkManager has
 * not started transferring it" — from the screen's point of view those are the
 * same thing: bytes are not moving and that is expected.
 */
export type ClipUploadPhase = 'queued' | 'uploading' | 'done' | 'failed';

/** Progress within one clip's transfer, as the uploader reports it. */
export interface ClipTransferProgress {
  /** 0..1. */
  fraction: number;
  uploadedParts?: number;
  totalParts?: number;
  /** Accepted but not transferring yet — WorkManager is holding it. */
  waiting?: boolean;
}

export interface ClipUploadState extends ClipTransferProgress {
  clipId: string;
  phase: ClipUploadPhase;
  /** Present as soon as `POST /jobs` has answered, before the bytes go up. */
  jobId?: string;
  /** Device clock, for throughput. Set when the clip is launched. */
  startedAtMs?: number;
  /** Device clock, for the final throughput figure. */
  finishedAtMs?: number;
  error?: string;
}

/** Uploads one clip and resolves with its job id once its bytes have landed. */
export interface ClipUploader {
  upload(
    clip: PickedClip,
    onProgress: (progress: ClipTransferProgress) => void,
  ): Promise<string>;
}

export interface RunUploadBatchOptions {
  clips: PickedClip[];
  uploader: ClipUploader;
  /** Called on every change, with the state of every clip. */
  onState: (states: ClipUploadState[]) => void;
  /** Injected so tests can drive the throughput arithmetic. */
  now?: () => number;
}

/**
 * Upload every clip, concurrently.
 *
 * Each clip's failure is caught inside its own task rather than at the
 * `Promise.all`. That is the difference between "one bad file and four
 * uploads" and "one bad file and nothing": `Promise.all` rejects on the first
 * failure, and awaiting it that way would abandon the others' results even
 * though their transfers were already in flight and would carry on regardless.
 * The returned array therefore always has one entry per clip, whatever
 * happened to each.
 */
export async function runUploadBatch(
  opts: RunUploadBatchOptions,
): Promise<ClipUploadState[]> {
  const {clips, uploader, onState, now = Date.now} = opts;

  const states: ClipUploadState[] = clips.map(clip => ({
    clipId: clip.id,
    phase: 'queued',
    fraction: 0,
  }));

  // A fresh array each time: React compares by identity, and mutating the
  // objects in place would leave the progress screen rendering stale rows.
  const publish = () => onState(states.map(s => ({...s})));
  publish();

  await Promise.all(
    clips.map(async (clip, i) => {
      states[i] = {...states[i], phase: 'uploading', startedAtMs: now()};
      publish();

      try {
        const jobId = await uploader.upload(clip, progress => {
          states[i] = {
            ...states[i],
            phase: progress.waiting ? 'queued' : 'uploading',
            // Monotonic: WorkManager reports live progress while running and
            // output data afterwards, and the two can briefly disagree. A bar
            // that goes backwards looks like a failed retry.
            fraction: Math.max(states[i].fraction, progress.fraction),
            uploadedParts: progress.uploadedParts ?? states[i].uploadedParts,
            totalParts: progress.totalParts ?? states[i].totalParts,
            waiting: progress.waiting,
          };
          publish();
        });

        states[i] = {
          ...states[i],
          phase: 'done',
          jobId,
          fraction: 1,
          waiting: false,
          finishedAtMs: now(),
        };
      } catch (err) {
        states[i] = {
          ...states[i],
          phase: 'failed',
          waiting: false,
          finishedAtMs: now(),
          error: err instanceof Error ? err.message : String(err),
        };
      }
      publish();
    }),
  );

  return states.map(s => ({...s}));
}

/** How many clips are fully uploaded — the "x of N" on the progress screen. */
export function uploadedCount(states: ClipUploadState[]): number {
  return states.filter(s => s.phase === 'done').length;
}

/** True once nothing is left queued or in flight, however each clip ended. */
export function batchFinished(states: ClipUploadState[]): boolean {
  return (
    states.length > 0 &&
    states.every(s => s.phase === 'done' || s.phase === 'failed')
  );
}

/**
 * Overall fraction across the batch, weighted by file size.
 *
 * Weighted rather than averaged because the clips are not the same size, and
 * an unweighted mean makes the overall bar jump when a small clip finishes and
 * crawl while a large one goes up.
 */
export function batchFraction(
  clips: PickedClip[],
  states: ClipUploadState[],
): number {
  const total = clips.reduce((sum, c) => sum + c.sizeBytes, 0);
  if (total <= 0) {
    return 0;
  }
  const done = clips.reduce((sum, clip) => {
    const state = states.find(s => s.clipId === clip.id);
    return sum + clip.sizeBytes * (state?.fraction ?? 0);
  }, 0);
  return Math.min(1, done / total);
}

// --- The two real uploaders -------------------------------------------------

/**
 * How often to ask WorkManager how a transfer is going.
 *
 * One timer per clip, all running at once now, so this is N local IPC calls
 * per interval. It is deliberately not faster: these are cheap but they are
 * not free, and the screen cannot show more than one change per frame anyway.
 */
export const WORK_POLL_MS = 500;

/**
 * Give up on a clip that WorkManager never reports as finished.
 *
 * Without a bound, a worker wedged behind a dead network leaves its row
 * spinning forever and the batch never reports itself finished.
 */
export const WORK_TIMEOUT_MS = 10 * 60 * 1000;

/**
 * WorkManager has been asked for a job it does not know about. Expected for a
 * moment right after enqueueing, so it is tolerated briefly and then treated
 * as a failure rather than polled forever.
 */
const UNKNOWN_GRACE_MS = 5000;

const sleep = (ms: number) => new Promise<void>(r => setTimeout(r, ms));

/**
 * The real path: create the job in JS, hand the bytes to WorkManager, then
 * watch that one request until it is done.
 *
 * The watching is what turns a fire-and-forget enqueue into something whose
 * completion the screen can show — `startBackgroundVideoUpload` returns when
 * the work is merely *enqueued*, by design, because the transfer has to
 * outlive this JavaScript context. Polling rather than a callback for the same
 * reason `nativeUploader.ts` gives: asking is always correct, being told is
 * only correct while someone is listening. If this screen goes away mid-batch
 * every transfer carries on; only the progress display is lost.
 */
export function backgroundClipUploader(args: {
  store?: JobIndexStore;
  debugPartDelayMs?: number;
  pollMs?: number;
  timeoutMs?: number;
}): ClipUploader {
  const {
    store,
    debugPartDelayMs,
    pollMs = WORK_POLL_MS,
    timeoutMs = WORK_TIMEOUT_MS,
  } = args;

  return {
    async upload(clip, onProgress) {
      const {jobId} = await startBackgroundVideoUpload({
        video: {
          fileUri: clip.fileUri,
          filename: clip.filename,
          sizeBytes: clip.sizeBytes,
          contentType: clip.contentType,
          durationSeconds: clip.durationSeconds,
        },
        store,
        debugPartDelayMs,
      });

      const deadline = Date.now() + timeoutMs;
      let unknownSince: number | null = null;

      for (;;) {
        const status = await getBackgroundUploadStatus(jobId);
        onProgress({
          fraction: status.fraction,
          uploadedParts: status.uploadedParts,
          totalParts: status.totalParts,
          // WorkManager is entitled to hold a request rather than run it, and
          // with N enqueued at once it often will. Saying so beats showing a
          // bar at 0% that looks stuck.
          waiting: status.state === 'ENQUEUED' || status.state === 'BLOCKED',
        });

        if (status.state === 'SUCCEEDED') {
          onProgress({
            fraction: 1,
            uploadedParts: status.totalParts,
            totalParts: status.totalParts,
          });
          return jobId;
        }
        if (status.state === 'FAILED' || status.state === 'CANCELLED') {
          throw new Error(
            status.failureReason ?? `upload ${status.state.toLowerCase()}`,
          );
        }
        if (status.state === 'NONE') {
          unknownSince = unknownSince ?? Date.now();
          if (Date.now() - unknownSince > UNKNOWN_GRACE_MS) {
            throw new Error('WorkManager never accepted the upload');
          }
        } else {
          unknownSince = null;
        }
        if (Date.now() > deadline) {
          throw new Error('the upload did not finish in time');
        }

        await sleep(pollMs);
      }
    },
  };
}

/**
 * The fallback for any build without the native module — iOS, or an APK built
 * before it existed. Uploads in the foreground and dies with the app.
 *
 * Note that "parallel" here means N JavaScript uploads sharing one thread and
 * one radio, which is genuinely slower in total than doing them in turn. That
 * is accepted: this path is a compatibility fallback, not the demo path, and
 * having it behave visibly like the real one is worth more than the seconds.
 *
 * The part count comes from the API's part size, which the JS uploader does
 * not surface, so parts are left undefined and the screen shows megabytes only.
 */
export function foregroundClipUploader(args: {
  store?: JobIndexStore;
}): ClipUploader {
  return {
    async upload(clip, onProgress) {
      const {jobId} = await uploadVideo({
        video: {
          fileUri: clip.fileUri,
          filename: clip.filename,
          sizeBytes: clip.sizeBytes,
          contentType: clip.contentType,
          durationSeconds: clip.durationSeconds,
        },
        transport: blobTransport,
        store: args.store,
        onProgress: fraction => onProgress({fraction}),
      });
      return jobId;
    },
  };
}
