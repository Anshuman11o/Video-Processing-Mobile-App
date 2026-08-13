// Job status polling.
//
// There is no push, no SSE and no WebSocket — polling is the whole mechanism.
// It must stop on its own, because a poll loop that outlives the screen is the
// client-side version of the hot receive loop `config/free-tier.md` forbids:
// invisible, indefinite, and metered on real AWS.
//
// It stops on three conditions: the job reaches a terminal status, the screen
// loses focus, or the component unmounts.

import {useCallback, useEffect, useRef, useState} from 'react';
import {AppState, AppStateStatus} from 'react-native';

import {getJobStatus} from '../api/client';
import {Job, isTerminal} from '../types/api';

/**
 * How often to ask.
 *
 * `GET /jobs/{id}` is served from Redis with a 10-second TTL that no worker
 * invalidates, so polling faster than that mostly re-reads the same cached
 * body. 2s is kept anyway: the cache is a floor on *freshness*, not a reason to
 * be slow to notice the change when it does land. The pipeline finishes in
 * about four seconds, so a job will often appear to jump straight from
 * `uploading` to `completed` with no visible stages. That is expected and
 * documented in docs/SETUP.md — it is not a bug in this hook.
 */
export const POLL_INTERVAL_MS = 2000;

export interface JobPollingState {
  job: Job | null;
  error: string | null;
  /** True while a poll loop is actually scheduled. */
  polling: boolean;
  /** Fetch immediately, e.g. for pull-to-refresh. */
  refresh: () => Promise<void>;
}

/**
 * Poll one job until it finishes.
 *
 * Pass `null` to poll nothing — the hook still has to be called
 * unconditionally, so callers with no job yet pass null rather than skipping it.
 */
export function useJobPolling(
  jobId: string | null,
  enabled: boolean = true,
): JobPollingState {
  const [job, setJob] = useState<Job | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [polling, setPolling] = useState(false);

  // Held in a ref so the interval callback can read the latest status without
  // being torn down and rebuilt on every tick.
  const doneRef = useRef(false);
  const timerRef = useRef<ReturnType<typeof setInterval> | null>(null);

  const fetchOnce = useCallback(async () => {
    if (!jobId) {
      return;
    }
    try {
      const next = await getJobStatus(jobId);
      setJob(next);
      setError(null);
      if (isTerminal(next.status)) {
        doneRef.current = true;
      }
    } catch (err) {
      // A transient failure must not kill the loop; the next tick retries. The
      // message is surfaced so the UI can show that it is stale, not stuck.
      setError(err instanceof Error ? err.message : String(err));
    }
  }, [jobId]);

  useEffect(() => {
    doneRef.current = false;
    setJob(null);
    setError(null);
  }, [jobId]);

  useEffect(() => {
    const stop = () => {
      if (timerRef.current !== null) {
        clearInterval(timerRef.current);
        timerRef.current = null;
      }
      setPolling(false);
    };

    if (!jobId || !enabled || doneRef.current) {
      stop();
      return stop;
    }

    fetchOnce();
    setPolling(true);
    timerRef.current = setInterval(() => {
      if (doneRef.current) {
        stop();
        return;
      }
      fetchOnce();
    }, POLL_INTERVAL_MS);

    // Backgrounding the app stops the loop. Without this, a job left on screen
    // keeps polling from the background for as long as the OS allows.
    const onAppState = (state: AppStateStatus) => {
      if (state !== 'active') {
        stop();
      }
    };
    const sub = AppState.addEventListener('change', onAppState);

    return () => {
      sub.remove();
      stop();
    };
  }, [jobId, enabled, fetchOnce, job?.status]);

  return {job, error, polling, refresh: fetchOnce};
}
