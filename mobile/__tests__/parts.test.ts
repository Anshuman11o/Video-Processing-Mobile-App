import {
  planParts,
  retryDelayMs,
  uploadProgress,
} from '../src/upload/parts';

describe('planParts', () => {
  it('numbers parts from 1 and covers the file exactly once', () => {
    const plans = planParts(12, 5);
    expect(plans.map(p => [p.partNumber, p.start, p.end])).toEqual([
      [1, 0, 5],
      [2, 5, 10],
      [3, 10, 12],
    ]);
    expect(plans.reduce((n, p) => n + p.length, 0)).toBe(12);
  });

  it('never runs the last part past the end of the file', () => {
    // S3 rejects a part longer than the bytes behind it, and a slice past EOF
    // is a silent short read rather than an error on some platforms.
    const plans = planParts(10, 4);
    expect(plans[plans.length - 1].end).toBe(10);
  });

  it('produces one part when the file is smaller than a part', () => {
    expect(planParts(100, 5 * 1024 * 1024)).toHaveLength(1);
  });

  it('agrees with the API on part count', () => {
    // handlers.go: numParts = ceil(size_bytes / part_size). If these ever
    // disagree the presigned URLs stop lining up with the bytes.
    for (const [size, part] of [
      [1, 5],
      [5, 5],
      [6, 5],
      [98520, 5 * 1024 * 1024],
      [20 * 1024 * 1024, 5 * 1024 * 1024],
    ]) {
      expect(planParts(size, part)).toHaveLength(Math.ceil(size / part));
    }
  });

  it('returns nothing for an empty file', () => {
    expect(planParts(0, 5)).toEqual([]);
  });

  it('rejects a non-positive part size rather than looping forever', () => {
    expect(() => planParts(10, 0)).toThrow(/invalid part size/);
  });
});

describe('uploadProgress', () => {
  it('counts in-flight bytes on top of completed ones', () => {
    expect(uploadProgress(100, 50, 25)).toBeCloseTo(0.75);
  });

  it('never exceeds 1 even if a transport over-reports', () => {
    expect(uploadProgress(100, 90, 50)).toBe(1);
  });

  it('is 0 for an empty file rather than NaN', () => {
    expect(uploadProgress(0, 0)).toBe(0);
  });
});

describe('retryDelayMs', () => {
  it('backs off 1s, 2s, 4s', () => {
    expect([1, 2, 3].map(retryDelayMs)).toEqual([1000, 2000, 4000]);
  });
});
