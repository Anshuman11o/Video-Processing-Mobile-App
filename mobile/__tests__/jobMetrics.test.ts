// The three metrics, derived from a real job body.
//
// These are pure functions taking `now` as an argument precisely so this file
// can exist: every ticking number on the detail screen is checked here at a
// fixed instant, rather than being something you can only judge by staring at
// an emulator.

import fixture from './fixtures/job-completed.json';

import type {Job, StageState} from '../src/types/api';
import {
  completedStageDurationMs,
  formatClock,
  formatDuration,
  formatMB,
  formatSeconds,
  jobElapsedMs,
  parseServerTime,
  pipelineEndMs,
  stagePills,
  uploadProgressMetrics,
  uploadSummary,
} from '../src/metrics/jobMetrics';

const completedJob = fixture as unknown as Job;

/** A job with the stages you name and nothing else — `stages` is a Go map. */
function jobWith(
  stages: Partial<Record<string, Partial<StageState>>>,
  overrides: Partial<Job> = {},
): Job {
  return {
    ...completedJob,
    status: 'processing',
    metrics: undefined,
    stages: stages as Job['stages'],
    ...overrides,
  };
}

describe('parseServerTime', () => {
  it('parses the nanosecond precision Go actually sends', () => {
    // Nine fractional digits. ECMA-262 specifies three, so this is the value
    // that would come back NaN on an engine that refuses the extras — and a
    // NaN here freezes every counter on the screen.
    expect(parseServerTime('2026-08-12T19:35:08.271671471Z')).toBe(
      Date.UTC(2026, 7, 12, 19, 35, 8, 271),
    );
  });

  it('parses ordinary millisecond and second precision unchanged', () => {
    expect(parseServerTime('2026-08-12T19:35:08.271Z')).toBe(
      Date.UTC(2026, 7, 12, 19, 35, 8, 271),
    );
    expect(parseServerTime('2026-08-12T19:35:08Z')).toBe(
      Date.UTC(2026, 7, 12, 19, 35, 8, 0),
    );
  });

  it('returns undefined rather than NaN for absent or unparseable input', () => {
    expect(parseServerTime(undefined)).toBeUndefined();
    expect(parseServerTime('')).toBeUndefined();
    expect(parseServerTime('not a date')).toBeUndefined();
  });
});

describe('M1 — the stage tracker', () => {
  it('renders four pills even when the map has no keys at all', () => {
    // `stages` is a Go map: a key is simply absent until a worker writes it.
    // Iterating the payload's own keys would grow the tracker from nothing to
    // four pills as the job ran.
    const pills = stagePills(jobWith({}), 0);
    expect(pills.map(p => p.name)).toEqual([
      'validate',
      'extract',
      'transcribe',
      'package',
    ]);
    expect(pills.every(p => p.state === 'pending')).toBe(true);
  });

  it('marks the running stage current and ticks it from started_at', () => {
    const startedAt = '2026-08-12T19:35:00.000Z';
    const pills = stagePills(
      jobWith({
        validate: {status: 'completed', attempts: 1},
        extract: {status: 'running', started_at: startedAt, attempts: 1},
      }),
      Date.parse(startedAt) + 4500,
    );

    expect(pills[0].state).toBe('completed');
    expect(pills[1]).toMatchObject({state: 'current', elapsedMs: 4500});
    // Not "the rest are current too" — only one pill carries the counter.
    expect(pills[2].state).toBe('pending');
    expect(pills[3].state).toBe('pending');
    expect(pills[2].elapsedMs).toBeUndefined();
  });

  it('does not tick a running stage that has no started_at', () => {
    const pills = stagePills(
      jobWith({validate: {status: 'running', attempts: 1}}),
      1_000_000,
    );
    expect(pills[0].state).toBe('current');
    // A million seconds is what a missing timestamp would have produced if
    // this subtracted from zero.
    expect(pills[0].elapsedMs).toBeUndefined();
  });

  it('never reports a negative elapsed time when the clocks disagree', () => {
    const startedAt = '2026-08-12T19:35:00.000Z';
    const pills = stagePills(
      jobWith({validate: {status: 'running', started_at: startedAt, attempts: 1}}),
      Date.parse(startedAt) - 30_000,
    );
    expect(pills[0].elapsedMs).toBe(0);
  });

  it('marks a failed stage failed rather than pending', () => {
    const pills = stagePills(
      jobWith({
        validate: {status: 'completed', attempts: 1},
        extract: {status: 'failed', attempts: 3, error: 'no video stream'},
      }),
      0,
    );
    expect(pills[1].state).toBe('failed');
  });

  it('uses the recorded duration for a completed stage', () => {
    const pills = stagePills(completedJob, 0);
    expect(pills.map(p => p.state)).toEqual([
      'completed',
      'completed',
      'completed',
      'completed',
    ]);
    // metrics.validate_duration_ms on the captured body.
    expect(pills[0].durationMs).toBe(449);
    expect(pills[3].durationMs).toBe(1883);
  });

  it('falls back to the stage timestamps when metrics are absent', () => {
    // Every field of Metrics is omitempty, so a completed stage can arrive
    // with no duration recorded; showing nothing would look like a pipeline
    // bug rather than a gap in the payload.
    const job = jobWith({
      validate: {
        status: 'completed',
        started_at: '2026-08-12T19:35:07.749559429Z',
        completed_at: '2026-08-12T19:35:08.246763471Z',
        attempts: 1,
      },
    });
    expect(completedStageDurationMs(job, 'validate')).toBe(497);
  });

  it('reports no duration when there is nothing to compute one from', () => {
    const job = jobWith({validate: {status: 'completed', attempts: 1}});
    expect(completedStageDurationMs(job, 'validate')).toBeUndefined();
  });
});

describe('M3 — elapsed time', () => {
  const createdAt = '2026-08-12T19:35:05.686422720Z';

  it('ticks against now while the job is live', () => {
    const job = jobWith({}, {created_at: createdAt, status: 'processing'});
    expect(jobElapsedMs(job, Date.parse('2026-08-12T19:35:35.686Z'))).toBe(30_000);
  });

  it('freezes at the total once the job is terminal', () => {
    // The captured job: created at :05.686, last stage completed at :10.957.
    const long_after = Date.parse('2026-08-12T20:00:00.000Z');
    expect(jobElapsedMs(completedJob, long_after)).toBe(5271);
    // And it is the same answer an hour later — that is what "frozen" means.
    expect(jobElapsedMs(completedJob, long_after + 3_600_000)).toBe(5271);
  });

  it('reconstructs the end from the latest completed_at, not the first', () => {
    expect(pipelineEndMs(completedJob)).toBe(
      Date.parse('2026-08-12T19:35:10.957Z'),
    );
  });

  it('freezes on the observed end when no stage ever completed', () => {
    // A validate failure: terminal, but with no completed_at anywhere to
    // freeze on. Without the fallback this would count up forever.
    const job = jobWith(
      {validate: {status: 'failed', attempts: 1, error: 'bad file'}},
      {created_at: createdAt, status: 'failed'},
    );
    const observedEnd = Date.parse('2026-08-12T19:35:08.686Z');
    expect(jobElapsedMs(job, observedEnd + 60_000, observedEnd)).toBe(3000);
  });

  it('clamps to zero rather than counting backwards', () => {
    const job = jobWith({}, {created_at: createdAt, status: 'processing'});
    expect(jobElapsedMs(job, Date.parse('2026-08-12T19:30:00.000Z'))).toBe(0);
  });
});

describe('M6 — upload progress', () => {
  const totalBytes = 14_947_952; // the captured job's size

  it('reports megabytes uploaded against the total', () => {
    const m = uploadProgressMetrics({fraction: 0.5, totalBytes});
    expect(m.bytesLabel).toBe('7.1 MB / 14.3 MB');
    expect(m.uploadedBytes).toBe(7_473_976);
  });

  it('reports the part count when the uploader knows it', () => {
    expect(
      uploadProgressMetrics({
        fraction: 0.4,
        totalBytes,
        uploadedParts: 1,
        totalParts: 3,
      }).partsLabel,
    ).toBe('1/3');
  });

  it('omits the part count rather than inventing one', () => {
    // The JS fallback uploader cannot see parts. "0/0" would be a lie.
    expect(uploadProgressMetrics({fraction: 0.4, totalBytes}).partsLabel)
      .toBeUndefined();
  });

  it('never shows more parts than exist', () => {
    expect(
      uploadProgressMetrics({
        fraction: 1,
        totalBytes,
        uploadedParts: 5,
        totalParts: 3,
      }).partsLabel,
    ).toBe('3/3');
  });

  it('withholds throughput until the window is long enough to mean something', () => {
    expect(
      uploadProgressMetrics({fraction: 0.1, totalBytes, elapsedMs: 40})
        .throughputLabel,
    ).toBeUndefined();
    expect(
      uploadProgressMetrics({fraction: 0.5, totalBytes, elapsedMs: 5000})
        .throughputLabel,
    ).toBe('1.4 MB/s');
  });

  it('clamps a fraction outside [0, 1] instead of overshooting the total', () => {
    expect(uploadProgressMetrics({fraction: 1.4, totalBytes}).uploadedBytes)
      .toBe(totalBytes);
    expect(uploadProgressMetrics({fraction: -1, totalBytes}).uploadedBytes).toBe(0);
    expect(uploadProgressMetrics({fraction: NaN, totalBytes}).uploadedBytes).toBe(0);
  });

  it('summarises the upload after the fact from the job itself', () => {
    // M6 has to stay on screen for the whole job, so once the live numbers are
    // gone the job's own record takes over.
    const job = jobWith(
      {},
      {metrics: {upload_duration_ms: 2000}, status: 'completed'},
    );
    const summary = uploadSummary(job);
    expect(summary.sizeLabel).toBe('14.3 MB');
    expect(summary.partsLabel).toBe('3/3');
    expect(summary.durationLabel).toBe('2.0s');
    expect(summary.throughputLabel).toBe('7.1 MB/s');
  });

  it('omits the timing summary when no upload duration was recorded', () => {
    const summary = uploadSummary(jobWith({}, {metrics: undefined}));
    expect(summary.sizeLabel).toBe('14.3 MB');
    expect(summary.durationLabel).toBeUndefined();
    expect(summary.throughputLabel).toBeUndefined();
  });
});

describe('formatting', () => {
  it('formats the big clock as MM:SS', () => {
    expect(formatClock(0)).toBe('00:00');
    expect(formatClock(5271)).toBe('00:05');
    expect(formatClock(65_000)).toBe('01:05');
    expect(formatClock(599_000)).toBe('09:59');
  });

  it('grows to H:MM:SS rather than letting minutes roll past 59', () => {
    // 01:00 for an hour would read as one minute.
    expect(formatClock(3_600_000)).toBe('1:00:00');
    expect(formatClock(3_661_000)).toBe('1:01:01');
  });

  it('keeps sub-second precision on short stages', () => {
    // transcribe took 8ms on the captured job; "0s" beside it looks like a
    // stage that never ran.
    expect(formatSeconds(8)).toBe('0.0s');
    expect(formatSeconds(449)).toBe('0.4s');
    expect(formatSeconds(1883)).toBe('1.9s');
    expect(formatSeconds(45_000)).toBe('45s');
  });

  it('formats video length, and says so when it is unknown', () => {
    expect(formatDuration(32)).toBe('0:32');
    expect(formatDuration(95)).toBe('1:35');
    expect(formatDuration(undefined)).toBe('—');
    expect(formatDuration(NaN)).toBe('—');
  });

  it('formats megabytes', () => {
    expect(formatMB(14_947_952)).toBe('14.3 MB');
    expect(formatMB(0)).toBe('0.0 MB');
  });
});
