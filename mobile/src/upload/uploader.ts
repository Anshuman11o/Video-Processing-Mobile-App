// Multipart upload orchestration.
//
// The transport is injected rather than imported. That is not ceremony: the
// only transport that works on a device needs react-native-blob-util and a
// running emulator, and there is no Android SDK on this machine yet. Keeping
// the retry/ETag/progress logic behind an interface means the part of this file
// most likely to contain a bug is testable today with a fake.

import {CompletedPart, UploadPartInfo} from '../types/api';
import {PartPlan, planParts, retryDelayMs, uploadProgress} from './parts';

/** Uploads one byte range and returns the ETag S3 responded with. */
export interface PartTransport {
  putPart(args: {
    url: string;
    fileUri: string;
    start: number;
    end: number;
    onProgress?: (bytesSent: number) => void;
    signal?: AbortSignal;
  }): Promise<string>;
}

export interface UploadOptions {
  fileUri: string;
  fileSize: number;
  partSize: number;
  urls: UploadPartInfo[];
  transport: PartTransport;
  /** Fraction in [0, 1]. Called often; callers should throttle their own state. */
  onProgress?: (fraction: number) => void;
  signal?: AbortSignal;
  maxAttempts?: number;
  /** Injected so tests do not actually wait out the backoff. */
  sleep?: (ms: number) => Promise<void>;
}

/** A part's presigned URL expired (HTTP 403) — the upload must be restarted. */
export class UploadExpiredError extends Error {
  constructor(partNumber: number) {
    super(
      `the upload URL for part ${partNumber} has expired; start the upload again`,
    );
    this.name = 'UploadExpiredError';
  }
}

/** A part failed every attempt. Carries the last underlying error. */
export class PartUploadError extends Error {
  readonly partNumber: number;
  readonly cause: unknown;

  constructor(partNumber: number, attempts: number, cause: unknown) {
    super(`part ${partNumber} failed after ${attempts} attempts: ${cause}`);
    this.name = 'PartUploadError';
    this.partNumber = partNumber;
    this.cause = cause;
  }
}

const defaultSleep = (ms: number) =>
  new Promise<void>(resolve => setTimeout(resolve, ms));

/**
 * Upload every part in order and return the ETags, ready for
 * `POST /jobs/{id}/complete`.
 *
 * Parts go up sequentially. That is deliberate for now: a phone on a mobile
 * connection gains little from parallel PUTs, and sequential order means the
 * ETags collected so far are always a contiguous prefix — which is the shape
 * Stage 8A needs in order to resume.
 *
 * The returned array is the complete record of what landed. Callers should
 * persist it as they go rather than keeping it in a closure, for the same
 * reason.
 */
export async function uploadParts(opts: UploadOptions): Promise<CompletedPart[]> {
  const {
    fileUri,
    fileSize,
    partSize,
    urls,
    transport,
    onProgress,
    signal,
    maxAttempts = 3,
    sleep = defaultSleep,
  } = opts;

  const plans = planParts(fileSize, partSize);

  // A mismatch here means the client and the API disagree about the file, and
  // every subsequent PUT would sign the wrong bytes. Fail loudly and early
  // rather than uploading a corrupt object that only fails at complete time.
  if (plans.length !== urls.length) {
    throw new Error(
      `part count mismatch: planned ${plans.length} from ${fileSize} bytes at ` +
        `${partSize}/part, but the API issued ${urls.length} URLs`,
    );
  }

  const byNumber = new Map(urls.map(u => [u.part_number, u.url]));
  const completed: CompletedPart[] = [];
  let completedBytes = 0;

  for (const plan of plans) {
    const url = byNumber.get(plan.partNumber);
    if (!url) {
      throw new Error(`no upload URL for part ${plan.partNumber}`);
    }

    const etag = await putWithRetry({
      plan,
      url,
      fileUri,
      transport,
      maxAttempts,
      sleep,
      signal,
      onPartProgress: sent =>
        onProgress?.(uploadProgress(fileSize, completedBytes, sent)),
    });

    completed.push({part_number: plan.partNumber, etag});
    completedBytes += plan.length;
    onProgress?.(uploadProgress(fileSize, completedBytes));
  }

  return completed;
}

async function putWithRetry(args: {
  plan: PartPlan;
  url: string;
  fileUri: string;
  transport: PartTransport;
  maxAttempts: number;
  sleep: (ms: number) => Promise<void>;
  signal?: AbortSignal;
  onPartProgress: (bytesSent: number) => void;
}): Promise<string> {
  const {plan, url, fileUri, transport, maxAttempts, sleep, signal} = args;
  let lastError: unknown;

  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    if (signal?.aborted) {
      throw new Error('upload cancelled');
    }
    try {
      const etag = await transport.putPart({
        url,
        fileUri,
        start: plan.start,
        end: plan.end,
        onProgress: args.onPartProgress,
        signal,
      });
      if (!etag) {
        throw new Error('S3 returned no ETag');
      }
      return etag;
    } catch (err) {
      lastError = err;

      // An expired presign is terminal: every remaining URL was signed at the
      // same moment with the same expiry, so retrying this one and then
      // marching on through the rest just burns the same failure N more times.
      if (isExpired(err)) {
        throw new UploadExpiredError(plan.partNumber);
      }
      if (attempt < maxAttempts) {
        await sleep(retryDelayMs(attempt));
      }
    }
  }

  throw new PartUploadError(plan.partNumber, maxAttempts, lastError);
}

/**
 * A 403 from S3 on a presigned PUT means the signature is no longer acceptable,
 * and in practice on this path that means expiry. It is surfaced as its own
 * error so the UI can say "start over" rather than showing a raw 403.
 */
function isExpired(err: unknown): boolean {
  const status = (err as {status?: number; statusCode?: number} | null)?.status ??
    (err as {statusCode?: number} | null)?.statusCode;
  return status === 403;
}
