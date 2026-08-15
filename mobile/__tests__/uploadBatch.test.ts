// The batch's two load-bearing properties.
//
// 1. Every clip is launched before any of them finishes — the demo shows N
//    bars moving together, and a regression to chaining would look like a
//    slower demo rather than like a bug.
// 2. One clip failing neither stalls nor cancels the others.
//
// Both are tested against a fake uploader whose completion is controlled by the
// test, which is the only way to observe "started but not finished" at all.

import type {PickedClip} from '../src/types/clips';
import {
  type ClipTransferProgress,
  type ClipUploadState,
  batchFinished,
  batchFraction,
  runUploadBatch,
  uploadedCount,
} from '../src/upload/uploadBatch';

const clip = (id: string, sizeBytes = 1000): PickedClip => ({
  id,
  fileUri: `file:///tmp/${id}.mp4`,
  filename: `${id}.mp4`,
  sizeBytes,
  contentType: 'video/mp4',
});

/**
 * An uploader that never finishes on its own.
 *
 * Each call records that it started and hands back a resolver, so a test can
 * assert on the state of the world at a moment when several uploads are open
 * at once — which is precisely the thing a sequential implementation makes
 * impossible.
 */
function controllableUploader() {
  const started: string[] = [];
  const settle = new Map<
    string,
    {resolve: (jobId: string) => void; reject: (err: Error) => void}
  >();
  const report = new Map<string, (p: ClipTransferProgress) => void>();

  return {
    started,
    settle,
    /** Push a progress update as the real uploader's poll loop would. */
    progress(clipId: string, p: ClipTransferProgress) {
      report.get(clipId)?.(p);
    },
    uploader: {
      upload(c: PickedClip, onProgress: (p: ClipTransferProgress) => void) {
        started.push(c.id);
        report.set(c.id, onProgress);
        return new Promise<string>((resolve, reject) => {
          settle.set(c.id, {resolve, reject});
        });
      },
    },
  };
}

/** Let queued microtasks run, so `Promise.all`'s tasks reach their `await`. */
const flush = () => new Promise<void>(r => setImmediate(r));

describe('runUploadBatch', () => {
  it('starts every clip before any of them finishes', async () => {
    const {started, settle, uploader} = controllableUploader();
    const clips = [clip('a'), clip('b'), clip('c')];

    const run = runUploadBatch({clips, uploader, onState: () => {}});
    await flush();

    // The whole point of the parallel launch: all three are open at once, and
    // none has completed. A chained implementation would show exactly one.
    expect(started).toEqual(['a', 'b', 'c']);
    expect(settle.size).toBe(3);

    settle.get('a')!.resolve('job-a');
    settle.get('b')!.resolve('job-b');
    settle.get('c')!.resolve('job-c');
    const states = await run;

    expect(states.map(s => s.jobId)).toEqual(['job-a', 'job-b', 'job-c']);
    expect(states.every(s => s.phase === 'done')).toBe(true);
  });

  it('marks every clip uploading immediately, not one at a time', async () => {
    const {settle, uploader} = controllableUploader();
    const clips = [clip('a'), clip('b')];
    let latest: ClipUploadState[] = [];

    const run = runUploadBatch({clips, uploader, onState: s => (latest = s)});
    await flush();

    expect(latest.map(s => s.phase)).toEqual(['uploading', 'uploading']);

    settle.get('a')!.resolve('job-a');
    settle.get('b')!.resolve('job-b');
    await run;
  });

  it('keeps the other clips going when one fails', async () => {
    const {started, settle, uploader} = controllableUploader();
    const clips = [clip('a'), clip('b'), clip('c')];

    const run = runUploadBatch({clips, uploader, onState: () => {}});
    await flush();

    // The first clip fails while the others are still in flight. Awaiting a
    // bare Promise.all would abandon their results here.
    settle.get('a')!.reject(new Error('part 2 failed'));
    await flush();

    expect(started).toEqual(['a', 'b', 'c']);
    settle.get('b')!.resolve('job-b');
    settle.get('c')!.resolve('job-c');

    const states = await run;
    expect(states.map(s => s.phase)).toEqual(['failed', 'done', 'done']);
    expect(states[0].error).toContain('part 2 failed');
    // A failure is not a completion.
    expect(uploadedCount(states)).toBe(2);
    // But the batch is over: nothing is left to wait for.
    expect(batchFinished(states)).toBe(true);
  });

  it('tracks each clip’s progress independently', async () => {
    const {settle, uploader, progress} = controllableUploader();
    const clips = [clip('a'), clip('b')];
    let latest: ClipUploadState[] = [];

    const run = runUploadBatch({clips, uploader, onState: s => (latest = s)});
    await flush();

    progress('a', {fraction: 0.25, uploadedParts: 1, totalParts: 4});
    progress('b', {fraction: 0.75, uploadedParts: 3, totalParts: 4});

    expect(latest[0]).toMatchObject({fraction: 0.25, uploadedParts: 1});
    expect(latest[1]).toMatchObject({fraction: 0.75, uploadedParts: 3});

    settle.get('a')!.resolve('job-a');
    settle.get('b')!.resolve('job-b');
    await run;
  });

  it('never lets a progress bar go backwards', async () => {
    // WorkManager reports live progress while running and output data after,
    // and the two briefly disagree. A bar that retreats looks like a retry.
    const {settle, uploader, progress} = controllableUploader();
    let latest: ClipUploadState[] = [];

    const run = runUploadBatch({
      clips: [clip('a')],
      uploader,
      onState: s => (latest = s),
    });
    await flush();

    progress('a', {fraction: 0.8});
    progress('a', {fraction: 0.2});
    expect(latest[0].fraction).toBe(0.8);

    settle.get('a')!.resolve('job-a');
    await run;
  });

  it('shows a clip WorkManager is holding as queued, not as stalled', async () => {
    const {settle, uploader, progress} = controllableUploader();
    let latest: ClipUploadState[] = [];

    const run = runUploadBatch({
      clips: [clip('a')],
      uploader,
      onState: s => (latest = s),
    });
    await flush();

    progress('a', {fraction: 0, waiting: true});
    expect(latest[0].phase).toBe('queued');

    progress('a', {fraction: 0.1, waiting: false});
    expect(latest[0].phase).toBe('uploading');

    settle.get('a')!.resolve('job-a');
    await run;
  });

  it('publishes a fresh array each time so React sees the change', async () => {
    const {settle, uploader, progress} = controllableUploader();
    const seen: ClipUploadState[][] = [];

    const run = runUploadBatch({
      clips: [clip('a')],
      uploader,
      onState: s => seen.push(s),
    });
    await flush();
    progress('a', {fraction: 0.5});

    expect(seen.length).toBeGreaterThan(1);
    expect(seen[seen.length - 1]).not.toBe(seen[seen.length - 2]);
    expect(seen[seen.length - 1][0]).not.toBe(seen[seen.length - 2][0]);

    settle.get('a')!.resolve('job-a');
    await run;
  });
});

describe('batch arithmetic', () => {
  const clips = [clip('a', 1000), clip('b', 9000)];

  it('weights the overall bar by file size', () => {
    // An unweighted mean would call this 50%; only a tenth of the bytes are up.
    const states: ClipUploadState[] = [
      {clipId: 'a', phase: 'done', fraction: 1},
      {clipId: 'b', phase: 'uploading', fraction: 0},
    ];
    expect(batchFraction(clips, states)).toBeCloseTo(0.1);
  });

  it('reports zero rather than dividing by zero', () => {
    expect(batchFraction([], [])).toBe(0);
  });

  it('is not finished while anything is still queued', () => {
    expect(
      batchFinished([
        {clipId: 'a', phase: 'done', fraction: 1},
        {clipId: 'b', phase: 'queued', fraction: 0},
      ]),
    ).toBe(false);
  });

  it('is not finished before it has started', () => {
    expect(batchFinished([])).toBe(false);
  });
});
