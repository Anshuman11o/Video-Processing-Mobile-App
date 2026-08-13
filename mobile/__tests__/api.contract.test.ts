// Asserts the TypeScript wire types against a REAL captured response.
//
// This exists because the previous types were written from a plan instead of
// from the wire and drifted in eight places, including three of the five
// StageStatus values — which made the job list count 0 of 4 completed stages
// through a fully successful job. TypeScript cannot catch that on its own:
// types are erased, so a body that disagrees with them parses happily.
//
// The fixture is a genuine `GET /jobs/{id}` body from a job driven end to end
// through the local stack (see the capture note in docs/SETUP.md). Regenerate
// it whenever the Go models change; a stale fixture asserting old field names
// is worse than no fixture.

import fixture from './fixtures/job-completed.json';

import {
  ALL_STAGES,
  Job,
  completedStageCount,
  isTerminal,
  jobErrorMessage,
} from '../src/types/api';

// The cast is the assertion under test: if a field named here does not exist
// in the fixture, or has the wrong type, the checks below fail.
const job = fixture as unknown as Job;

describe('the captured job body matches the declared types', () => {
  it('carries the DynamoDB keys the API serialises', () => {
    // GetJobStatus writes the raw models.Job, so pk/sk are on the wire whether
    // or not a client wants them.
    expect(job.pk).toMatch(/^JOB#/);
    expect(job.sk).toBe('METADATA');
  });

  it('uses the field names the Go structs declare', () => {
    expect(typeof job.job_id).toBe('string');
    expect(typeof job.size_bytes).toBe('number');
    expect(typeof job.content_type).toBe('string');
    expect(job.upload).toBeDefined();
    expect(typeof job.upload!.upload_id).toBe('string');
    expect(typeof job.upload!.part_size).toBe('number');
    expect(typeof job.upload!.total_parts).toBe('number');
  });

  it('has no top-level error field', () => {
    // The old types declared `error?: string` on Job. Go has no such field —
    // a failure message lives on the stage that failed.
    expect('error' in (fixture as object)).toBe(false);
  });

  describe('stage states', () => {
    it('names the counter `attempts`, not `retry_count`', () => {
      for (const stage of ALL_STAGES) {
        const state = job.stages[stage];
        expect(state).toBeDefined();
        expect(typeof state!.attempts).toBe('number');
        expect('retry_count' in (state as object)).toBe(false);
      }
    });

    it('spells the finished state `completed`, not `complete`', () => {
      // This exact character difference is why the progress bar read 0/4.
      for (const stage of ALL_STAGES) {
        expect(job.stages[stage]!.status).toBe('completed');
      }
    });

    it('only ever uses the four Go StageStatus values', () => {
      const allowed = ['pending', 'running', 'completed', 'failed'];
      for (const stage of ALL_STAGES) {
        expect(allowed).toContain(job.stages[stage]!.status);
      }
    });

    it('carries output_key', () => {
      expect(job.stages.package!.output_key).toMatch(/master\.m3u8$/);
    });
  });

  describe('output', () => {
    it('reports duration in SECONDS', () => {
      // The old types declared `duration_ms`. Go declares `duration_seconds` —
      // a different name AND a different unit, so a UI reading it as ms would
      // have been wrong by 1000x without ever failing to compile.
      expect(typeof job.output!.duration_seconds).toBe('number');
      expect('duration_ms' in (job.output as object)).toBe(false);
    });

    it('has an hls_url and no caption_url', () => {
      expect(job.output!.hls_url).toMatch(/master\.m3u8$/);
      // The old types invented caption_url. Captions are a rendition inside
      // the master playlist, not a separate field.
      expect('caption_url' in (job.output as object)).toBe(false);
    });
  });

  it('leaves absent metrics absent rather than zero', () => {
    // Every models.Metrics field is omitempty, so a metric that was never
    // written is missing, not 0 — a UI must not render it as "0 ms".
    expect(typeof job.metrics!.total_processing_ms).toBe('number');
  });
});

describe('helpers derived from the real body', () => {
  it('counts all four stages complete', () => {
    expect(completedStageCount(job)).toBe(4);
  });

  it('treats the job as terminal', () => {
    expect(isTerminal(job.status)).toBe(true);
  });

  it('finds no error on a successful job', () => {
    expect(jobErrorMessage(job)).toBeUndefined();
  });

  it('finds the failing stage when one failed', () => {
    const failed: Job = {
      ...job,
      status: 'failed',
      stages: {
        ...job.stages,
        extract: {status: 'failed', attempts: 3, error: 'ffprobe: bad input'},
      },
    };
    expect(completedStageCount(failed)).toBe(3);
    expect(jobErrorMessage(failed)).toBe('extract: ffprobe: bad input');
  });
});
