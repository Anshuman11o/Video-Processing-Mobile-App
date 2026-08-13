// Fetching the finished reel, with 409 treated as "not yet" rather than as a
// failure.
//
// `GET /jobs/{id}/reel` reads DynamoDB directly, while `GET /jobs/{id}` is
// served from a 10-second Redis cache that no worker invalidates. The two
// therefore disagree in both directions for up to the TTL: the cached status
// can say `completed` while the reel endpoint still 409s, and vice versa.
// Surfacing that 409 as an error would turn a routine race into a broken
// screen, so it retries instead.

import {useCallback, useEffect, useState} from 'react';

import {getReel} from '../api/client';
import type {ReelResponse} from '../types/api';

/**
 * How long to wait before re-asking after a 409 NOT_READY.
 *
 * Shorter than the 10s cache TTL on purpose: the disagreement window is
 * bounded by that TTL, so polling inside it is what closes the gap quickly.
 */
export const REEL_RETRY_MS = 1500;

export interface ReelState {
  reel: ReelResponse | null;
  /**
   * The API answered 409 NOT_READY and a retry is scheduled. Distinct from
   * `error`: this is the expected cache-disagreement path, not a failure.
   */
  notReady: boolean;
  /** A real failure — unreachable API, 404, 5xx. */
  error: string | null;
  /** Re-fetch from scratch, discarding any reel already held. */
  refresh: () => void;
}

/**
 * Fetch a job's reel once `enabled` goes true, retrying while the API says
 * NOT_READY.
 *
 * `enabled` exists so the caller can hold off until the job status says
 * `completed`; asking earlier is harmless but guarantees a 409.
 */
export function useReel(jobId: string | null, enabled: boolean): ReelState {
  const [reel, setReel] = useState<ReelResponse | null>(null);
  const [notReady, setNotReady] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Bumping this is what re-runs the fetch effect; it is the retry clock.
  const [attempt, setAttempt] = useState(0);

  // Without this, navigating from one job to another shows the previous job's
  // reel under the new job's id until the new fetch lands.
  useEffect(() => {
    setReel(null);
    setNotReady(false);
    setError(null);
    setAttempt(0);
  }, [jobId]);

  useEffect(() => {
    if (!jobId || !enabled || reel) {
      return;
    }

    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | null = null;

    (async () => {
      try {
        // getReel maps 409 to null; anything else throws.
        const fetched = await getReel(jobId);
        if (cancelled) {
          return;
        }
        if (fetched) {
          setReel(fetched);
          setNotReady(false);
          setError(null);
          return;
        }
        setNotReady(true);
        timer = setTimeout(() => {
          if (!cancelled) {
            setAttempt(a => a + 1);
          }
        }, REEL_RETRY_MS);
      } catch (err) {
        if (!cancelled) {
          setError(err instanceof Error ? err.message : String(err));
          setNotReady(false);
        }
      }
    })();

    return () => {
      cancelled = true;
      if (timer) {
        clearTimeout(timer);
      }
    };
  }, [jobId, enabled, reel, attempt]);

  const refresh = useCallback(() => {
    // Clearing the reel is deliberate: a refresh after a playback failure has
    // to re-mount the player, and it only re-mounts if the source goes away.
    setReel(null);
    setError(null);
    setAttempt(a => a + 1);
  }, []);

  return {reel, notReady, error, refresh};
}
