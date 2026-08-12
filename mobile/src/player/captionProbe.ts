// Measuring where ExoPlayer actually places a caption cue boundary.
//
// Stage 6A found the first cue dropped and the rest landing early, and could
// not evaluate its own fix: ffmpeg ignores X-TIMESTAMP-MAP entirely, so four
// different header values produced byte-identical output. A real player is the
// only oracle, and this stage is the first time one exists.
//
// Watching the screen cannot produce a number. The offsets in question are tens
// of milliseconds; an adb screencap round trip is hundreds, and the emulator
// falls back to software GL under memory pressure, which makes frame timing
// worse still. So this measures a different way, and the difference is what
// makes it exact:
//
//   With the player PAUSED, a seek settles at a definite position and the text
//   renderer reports the cue active there. "Which cue is active at media time
//   T" is therefore a repeatable question with no timing race in it at all, and
//   bisecting T locates the boundary to whatever precision is asked for.
//
// One limitation, read out of the library's source rather than guessed:
// ReactExoplayerView.onCues forwards a cue group only when it is non-empty, so
// the reported cue never clears on its own. Seeking into a gap between cues
// reads the PREVIOUS cue's text, not null. Every bracket must therefore stay
// inside a contiguous run of cues.

/** Stop bisecting once the bracket is this narrow. */
export const DEFAULT_TOLERANCE_SECONDS = 0.004;

/** Hard cap, so a probe that answers inconsistently cannot loop forever. */
export const DEFAULT_MAX_PROBES = 24;

/**
 * Longest to wait after a seek for the text renderer to catch up.
 *
 * Generous because it is a CAP, not a delay: `videoRefProbe` returns as soon as
 * the cue changes, and only waits this long when it does not. Measured on the
 * emulator, a post-seek cue can take well over half a second to arrive — an
 * earlier 400 ms blind wait read the PREVIOUS position's cue and produced a
 * boundary pinned to the bracket edge.
 */
export const DEFAULT_SETTLE_MS = 2500;

/** How often to re-read the cue while waiting for it to change. */
export const DEFAULT_POLL_MS = 50;

export interface CueProbe {
  /**
   * Seek to `seconds` with the player paused and report the caption active
   * there, or null when none is.
   */
  cueAt: (seconds: number) => Promise<string | null>;
}

export interface CueBoundary {
  /** Media time of the transition, in seconds. */
  seconds: number;
  /** Half-width of the final bracket — the uncertainty on `seconds`. */
  toleranceSeconds: number;
  /** The cue below the boundary, and the one at or above it. */
  before: string | null;
  after: string | null;
  probeCount: number;
}

export interface MeasureOptions {
  /** Bracket start. The cue here must differ from the cue at `hi`. */
  lo: number;
  hi: number;
  toleranceSeconds?: number;
  maxProbes?: number;
}

/**
 * Bisect `[lo, hi]` for the media time at which the active caption changes.
 *
 * Throws when the bracket does not straddle a transition. That is a real
 * outcome rather than a defensive check: it is exactly what an unselected or
 * missing text track looks like from in here, and reporting it as an offset of
 * zero would be the silent failure this stage exists to avoid.
 */
export async function measureCueBoundary(
  probe: CueProbe,
  options: MeasureOptions,
): Promise<CueBoundary> {
  const tolerance = options.toleranceSeconds ?? DEFAULT_TOLERANCE_SECONDS;
  const maxProbes = options.maxProbes ?? DEFAULT_MAX_PROBES;
  let lo = options.lo;
  let hi = options.hi;

  if (!(hi > lo)) {
    throw new Error(`caption probe needs a non-empty bracket, got [${lo}, ${hi}]`);
  }

  const before = await probe.cueAt(lo);
  let after = await probe.cueAt(hi);
  let probeCount = 2;

  if (before === after) {
    throw new Error(
      `no cue change between ${lo}s and ${hi}s (both ${describeCue(before)}) — ` +
        'either the bracket misses the boundary, or no text track is selected',
    );
  }

  while (hi - lo > tolerance && probeCount < maxProbes) {
    const mid = (lo + hi) / 2;
    const cue = await probe.cueAt(mid);
    probeCount++;
    if (cue === before) {
      lo = mid;
    } else {
      // Reassigning `after` also narrows correctly when a third cue turns out
      // to sit inside the bracket.
      hi = mid;
      after = cue;
    }
  }

  return {
    seconds: (lo + hi) / 2,
    toleranceSeconds: (hi - lo) / 2,
    before,
    after,
    probeCount,
  };
}

/**
 * How far the player put a boundary from where the VTT authored it.
 *
 * Positive means the player shows the cue LATE relative to the authored time.
 */
export function cueOffsetMs(
  boundary: CueBoundary,
  authoredSeconds: number,
): number {
  return (boundary.seconds - authoredSeconds) * 1000;
}

/**
 * A probe over a mounted player.
 *
 * There is no event meaning "the text renderer has caught up" — onSeek reports
 * the position change, which happens first — so the only handle on settling is
 * the cue value itself. Waiting a fixed delay and reading once is what this
 * used to do, and it silently mismeasured: a 400 ms wait returned the cue from
 * BEFORE the seek, which the bisection then treated as fact.
 *
 * So it polls until the cue differs from the one standing before the seek, and
 * gives up at `settleMs`. Both outcomes are correct rather than one being a
 * fallback: a changed cue is positive evidence the renderer moved, and when the
 * new position genuinely carries the same cue there is nothing to wait for and
 * the value already in hand is the answer. The residual failure — the cue
 * changing later than `settleMs` — is the same one a fixed delay has, but the
 * early return buys a cap large enough to make it unlikely.
 */
export function videoRefProbe(
  seek: (seconds: number) => void,
  readCue: () => string | null,
  settleMs: number = DEFAULT_SETTLE_MS,
  pollMs: number = DEFAULT_POLL_MS,
): CueProbe {
  return {
    cueAt: async (seconds: number) => {
      const before = readCue();
      seek(seconds);
      for (let waited = 0; waited < settleMs; waited += pollMs) {
        await new Promise<void>(resolve => setTimeout(resolve, pollMs));
        const cue = readCue();
        if (cue !== before) {
          return cue;
        }
      }
      return readCue();
    },
  };
}

function describeCue(cue: string | null): string {
  return cue === null ? 'no cue' : `"${cue}"`;
}
