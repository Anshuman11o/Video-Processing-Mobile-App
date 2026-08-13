// Part planning and progress arithmetic.
//
// Kept free of React Native imports on purpose: this is the part of the
// uploader that is easy to get subtly wrong (off-by-one part numbers, a final
// part sized past the end of the file, progress that exceeds 1) and it is the
// only part that can be tested without an emulator.

/** A byte range of the source file, and the S3 part number it belongs to. */
export interface PartPlan {
  /** S3 part numbers are 1-based. */
  partNumber: number;
  /** Inclusive start offset. */
  start: number;
  /** Exclusive end offset. */
  end: number;
  /** `end - start`, precomputed so callers never re-derive it inconsistently. */
  length: number;
}

/**
 * Divide a file into upload parts.
 *
 * The API computes `numParts = ceil(size_bytes / part_size)` and presigns one
 * URL per part, so this must produce exactly that many parts in exactly that
 * order or the URLs will not line up with the bytes.
 *
 * A zero-length file yields no parts: S3 rejects a zero-byte part, and the API
 * rejects `size_bytes <= 0` before it ever gets here.
 */
export function planParts(fileSize: number, partSize: number): PartPlan[] {
  if (!Number.isFinite(fileSize) || fileSize <= 0) {
    return [];
  }
  if (!Number.isFinite(partSize) || partSize <= 0) {
    throw new Error(`invalid part size: ${partSize}`);
  }

  const plans: PartPlan[] = [];
  for (let start = 0, n = 1; start < fileSize; start += partSize, n++) {
    const end = Math.min(start + partSize, fileSize);
    plans.push({partNumber: n, start, end, length: end - start});
  }
  return plans;
}

/**
 * Fraction uploaded, in [0, 1].
 *
 * `bytesInFlight` is progress within the part currently uploading, which is
 * reported separately from completed parts because a part's bytes only become
 * durable when its PUT returns an ETag.
 */
export function uploadProgress(
  fileSize: number,
  completedBytes: number,
  bytesInFlight: number = 0,
): number {
  if (fileSize <= 0) {
    return 0;
  }
  const done = completedBytes + bytesInFlight;
  if (done <= 0) {
    return 0;
  }
  return Math.min(1, done / fileSize);
}

/**
 * Backoff before retry attempt `attempt` (1-based), in milliseconds: 1s, 2s, 4s.
 *
 * Same shape as the SQS receive backoff the workers use, for the same reason —
 * `config/free-tier.md` requires that nothing in this system retries in a tight
 * loop against a metered service.
 */
export function retryDelayMs(attempt: number): number {
  return 1000 * Math.pow(2, Math.max(0, attempt - 1));
}
