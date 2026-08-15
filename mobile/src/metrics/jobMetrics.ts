// Metric derivation for the job detail screen.
//
// Pure functions, no React and no clock of their own: every function that needs
// "now" is handed it. That is what makes the three metrics testable, and it is
// also the design constraint that matters most on device — `GET /jobs/{id}` is
// served from a server-side cache, so a number derived from poll *arrival*
// stutters by up to the TTL. Nothing here reads a clock; the screen ticks a
// local one and passes it in, so the seconds counters are smooth regardless of
// how lumpy the polling is.
//
// Three metrics, and deliberately only three:
//   M1  stage tracker   — validate → extract → transcribe → package
//   M3  elapsed time    — MM:SS, ticking, frozen at the total when terminal
//   M6  upload progress — MB uploaded / total MB, parts, throughput

import {
  ALL_STAGES,
  type Job,
  type Metrics,
  type StageName,
  isTerminal,
} from '../types/api';

// --- Time parsing -----------------------------------------------------------

/**
 * Parse a timestamp as Go marshalled it.
 *
 * `time.Time` writes nanosecond precision — `2026-08-12T19:35:08.271671471Z`,
 * nine fractional digits. ECMA-262's Date Time String Format specifies exactly
 * three, and what an engine does with the extras is unspecified: V8 truncates,
 * so this looks fine under Jest, but the app runs on Hermes and a `NaN` here
 * would silently freeze every counter on the screen. Truncating to milliseconds
 * before parsing makes the two engines agree.
 *
 * Returns undefined rather than NaN so callers have to handle "no timestamp",
 * which is a real case: `started_at` is absent on a stage that has not run.
 */
export function parseServerTime(iso: string | undefined): number | undefined {
  if (!iso) {
    return undefined;
  }
  const millis = iso.replace(/(\.\d{3})\d+/, '$1');
  const parsed = Date.parse(millis);
  return Number.isNaN(parsed) ? undefined : parsed;
}

// --- M1: stage tracker ------------------------------------------------------

/** How a pill should render. `current` is the one with the live counter. */
export type PillState = 'completed' | 'current' | 'pending' | 'failed';

export interface StagePill {
  name: StageName;
  state: PillState;
  /** Milliseconds this stage has been running. `current` pills only. */
  elapsedMs?: number;
  /** Milliseconds this stage took. `completed` pills only, when known. */
  durationMs?: number;
}

/**
 * Which `metrics` field holds each stage's duration.
 *
 * These are only written when a stage *completes*, which is why a running
 * stage's counter is ticked locally from `started_at` instead: there is no
 * partially-elapsed duration on the wire to read.
 */
const DURATION_KEY: Record<StageName, keyof Metrics> = {
  validate: 'validate_duration_ms',
  extract: 'extract_duration_ms',
  transcribe: 'transcribe_duration_ms',
  package: 'package_duration_ms',
};

/**
 * How long a finished stage took.
 *
 * Prefers the server's own measurement and falls back to the stage timestamps,
 * because `metrics` is entirely omitempty — a completed stage can arrive with
 * no duration recorded, and showing nothing in that case would look like a bug
 * in the pipeline rather than a gap in the payload.
 */
export function completedStageDurationMs(
  job: Job,
  name: StageName,
): number | undefined {
  const recorded = job.metrics?.[DURATION_KEY[name]];
  if (typeof recorded === 'number') {
    return recorded;
  }
  const state = job.stages[name];
  const started = parseServerTime(state?.started_at);
  const finished = parseServerTime(state?.completed_at);
  if (started === undefined || finished === undefined) {
    return undefined;
  }
  return Math.max(0, finished - started);
}

/**
 * The four pills, in pipeline order, whatever the payload contains.
 *
 * Always four rows: `stages` is a Go map, so a key is simply absent until the
 * worker writes it, and iterating the map's own keys would render a tracker
 * that grows from one pill to four as the job runs. An absent key is `pending`.
 */
export function stagePills(job: Job, nowMs: number): StagePill[] {
  return ALL_STAGES.map((name): StagePill => {
    const state = job.stages[name];
    switch (state?.status) {
      case 'completed':
        return {
          name,
          state: 'completed',
          durationMs: completedStageDurationMs(job, name),
        };
      case 'failed':
        return {name, state: 'failed'};
      case 'running': {
        const started = parseServerTime(state.started_at);
        return {
          name,
          state: 'current',
          elapsedMs: started === undefined ? undefined : Math.max(0, nowMs - started),
        };
      }
      default:
        return {name, state: 'pending'};
    }
  });
}

// --- M3: elapsed time -------------------------------------------------------

/**
 * When the pipeline stopped: the latest `completed_at` across all stages.
 *
 * There is no top-level completion timestamp on the wire — `updated_at` moves
 * for reasons other than finishing — so this is reconstructed from the stages.
 * Undefined when no stage has completed, which is the failed-at-validate case.
 */
export function pipelineEndMs(job: Job): number | undefined {
  let latest: number | undefined;
  for (const name of ALL_STAGES) {
    const finished = parseServerTime(job.stages[name]?.completed_at);
    if (finished !== undefined && (latest === undefined || finished > latest)) {
      latest = finished;
    }
  }
  return latest;
}

/**
 * Total elapsed time for the job, in milliseconds.
 *
 * Ticks against `nowMs` while the job is live and freezes once it is terminal.
 * `observedEndMs` is the fallback for a job that reached a terminal status with
 * no stage timestamp to freeze on — the caller records the moment it first saw
 * that status, so the number stops rather than counting up forever.
 *
 * `created_at` is the server's clock and `nowMs` is the device's. On this
 * setup they are the same machine, but the result is clamped at zero so a
 * skewed device shows `00:00` rather than a negative counter.
 */
export function jobElapsedMs(
  job: Job,
  nowMs: number,
  observedEndMs?: number,
): number {
  const start = parseServerTime(job.created_at);
  if (start === undefined) {
    return 0;
  }
  const end = isTerminal(job.status)
    ? (pipelineEndMs(job) ?? observedEndMs ?? nowMs)
    : nowMs;
  return Math.max(0, end - start);
}

// --- M6: upload progress ----------------------------------------------------

export interface UploadProgressInput {
  /** Fraction in [0, 1], from WorkManager or from the JS uploader. */
  fraction: number;
  /** The file's size — the denominator of "MB uploaded / total MB". */
  totalBytes: number;
  uploadedParts?: number;
  totalParts?: number;
  /** Wall-clock milliseconds since this clip's transfer started. */
  elapsedMs?: number;
}

export interface UploadProgressMetrics {
  uploadedBytes: number;
  totalBytes: number;
  /** "3.2 MB / 14.3 MB" — the required form. */
  bytesLabel: string;
  /** "2/3", or undefined before the part count is known. */
  partsLabel?: string;
  /** "1.4 MB/s", or undefined before there is enough signal to divide by. */
  throughputLabel?: string;
}

const BYTES_PER_MB = 1024 * 1024;

/** Megabytes to one decimal place. Binary MB, matching the part sizes. */
export function formatMB(bytes: number): string {
  return `${(Math.max(0, bytes) / BYTES_PER_MB).toFixed(1)} MB`;
}

/**
 * Too short a window makes throughput meaningless: dividing a few hundred
 * kilobytes by 40 ms reports numbers that swing by an order of magnitude
 * between frames.
 */
const MIN_THROUGHPUT_WINDOW_MS = 750;

export function uploadProgressMetrics(
  input: UploadProgressInput,
): UploadProgressMetrics {
  const {fraction, totalBytes, uploadedParts, totalParts, elapsedMs} = input;
  const clamped = Math.min(1, Math.max(0, Number.isFinite(fraction) ? fraction : 0));
  const uploadedBytes = Math.round(clamped * Math.max(0, totalBytes));

  const partsLabel =
    typeof totalParts === 'number' && totalParts > 0
      ? `${Math.min(uploadedParts ?? 0, totalParts)}/${totalParts}`
      : undefined;

  let throughputLabel: string | undefined;
  if (
    typeof elapsedMs === 'number' &&
    elapsedMs >= MIN_THROUGHPUT_WINDOW_MS &&
    uploadedBytes > 0
  ) {
    const mbPerSecond = uploadedBytes / BYTES_PER_MB / (elapsedMs / 1000);
    throughputLabel = `${mbPerSecond.toFixed(1)} MB/s`;
  }

  return {
    uploadedBytes,
    totalBytes,
    bytesLabel: `${formatMB(uploadedBytes)} / ${formatMB(totalBytes)}`,
    partsLabel,
    throughputLabel,
  };
}

/**
 * The upload, after the fact.
 *
 * M6 has to stay on screen for the whole job, not vanish the moment the bytes
 * land — so once the transfer is over the live counters are replaced by what
 * the job itself records. `upload_duration_ms` is written when the upload
 * completes, so its presence is also the signal that there is a summary to
 * show.
 */
export function uploadSummary(job: Job): {
  sizeLabel: string;
  partsLabel?: string;
  durationLabel?: string;
  throughputLabel?: string;
} {
  const durationMs = job.metrics?.upload_duration_ms;
  const totalParts = job.upload?.total_parts;
  const summary: {
    sizeLabel: string;
    partsLabel?: string;
    durationLabel?: string;
    throughputLabel?: string;
  } = {sizeLabel: formatMB(job.size_bytes)};

  if (typeof totalParts === 'number' && totalParts > 0) {
    summary.partsLabel = `${totalParts}/${totalParts}`;
  }
  if (typeof durationMs === 'number' && durationMs > 0) {
    summary.durationLabel = formatSeconds(durationMs);
    const mbPerSecond = job.size_bytes / BYTES_PER_MB / (durationMs / 1000);
    summary.throughputLabel = `${mbPerSecond.toFixed(1)} MB/s`;
  }
  return summary;
}

// --- Formatting -------------------------------------------------------------

/**
 * `MM:SS`, the big clock. Grows to `H:MM:SS` rather than letting the minutes
 * field roll past 59 and read as a smaller number than it is.
 */
export function formatClock(ms: number): string {
  const total = Math.max(0, Math.floor(ms / 1000));
  const seconds = total % 60;
  const minutes = Math.floor(total / 60) % 60;
  const hours = Math.floor(total / 3600);
  const mmss = `${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}`;
  return hours > 0 ? `${hours}:${mmss}` : mmss;
}

/**
 * The per-stage counter. Sub-second precision on purpose: transcribe finished
 * in 8 ms on the captured job, and "0s" next to it looks like a stage that
 * never ran.
 */
export function formatSeconds(ms: number): string {
  const safe = Math.max(0, ms);
  return safe < 10_000 ? `${(safe / 1000).toFixed(1)}s` : `${Math.round(safe / 1000)}s`;
}

/** Video length, `M:SS`. Undefined until the decoder has reported it. */
export function formatDuration(seconds: number | undefined): string {
  if (typeof seconds !== 'number' || !Number.isFinite(seconds) || seconds < 0) {
    return '—';
  }
  const whole = Math.round(seconds);
  return `${Math.floor(whole / 60)}:${String(whole % 60).padStart(2, '0')}`;
}
