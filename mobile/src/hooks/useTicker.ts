// A local clock, so the counters do not stutter with the polling.
//
// `GET /jobs/{id}` is served from a server-side cache, so poll responses can
// carry the same body several times in a row. Any number derived from when a
// poll *arrived* therefore jumps in cache-sized steps. Every ticking figure on
// the detail screen is instead computed from `Date.now()` and whatever
// timestamps the last poll carried, and this hook is the only thing that makes
// it re-render.

import {useEffect, useState} from 'react';
import {AppState, type AppStateStatus} from 'react-native';

/**
 * Re-render on an interval, returning the current time in milliseconds.
 *
 * Stops while the app is backgrounded and re-reads the clock on return, so a
 * job left running behind a lock screen shows the right elapsed time rather
 * than one missing however long the timers were throttled.
 */
export function useTicker(intervalMs: number = 1000, enabled: boolean = true): number {
  const [now, setNow] = useState(() => Date.now());

  useEffect(() => {
    if (!enabled) {
      // Still take one final reading, so a counter that stops does so on an
      // up-to-date value rather than on whatever the last tick happened to be.
      setNow(Date.now());
      return;
    }

    let timer: ReturnType<typeof setInterval> | null = setInterval(
      () => setNow(Date.now()),
      intervalMs,
    );

    const onAppState = (state: AppStateStatus) => {
      if (state === 'active') {
        setNow(Date.now());
        if (timer === null) {
          timer = setInterval(() => setNow(Date.now()), intervalMs);
        }
      } else if (timer !== null) {
        clearInterval(timer);
        timer = null;
      }
    };
    const sub = AppState.addEventListener('change', onAppState);

    return () => {
      sub.remove();
      if (timer !== null) {
        clearInterval(timer);
      }
    };
  }, [intervalMs, enabled]);

  return now;
}
