import {
  cueOffsetMs,
  measureCueBoundary,
  videoRefProbe,
  type CueProbe,
} from '../src/player/captionProbe';

/** A player whose cue changes at exactly `boundary`, answered instantly. */
function idealProbe(boundary: number, before: string, after: string): CueProbe {
  return {
    cueAt: async (seconds: number) =>
      Promise.resolve(seconds < boundary ? before : after),
  };
}

describe('measureCueBoundary', () => {
  it('locates the transition to within the requested tolerance', async () => {
    const found = await measureCueBoundary(idealProbe(3.021, 'one', 'two'), {
      lo: 2.85,
      hi: 3.15,
      toleranceSeconds: 0.004,
    });

    expect(Math.abs(found.seconds - 3.021)).toBeLessThanOrEqual(
      found.toleranceSeconds,
    );
    expect(found.toleranceSeconds).toBeLessThanOrEqual(0.004);
    expect(found.before).toBe('one');
    expect(found.after).toBe('two');
  });

  it('throws rather than reporting a boundary when the bracket has none', async () => {
    // The silent-failure case this whole probe exists to avoid: an unselected
    // text track looks exactly like this, and reporting it as offset 0 would
    // read as "the captions are perfect".
    await expect(
      measureCueBoundary(idealProbe(9, 'one', 'two'), {lo: 2.85, hi: 3.15}),
    ).rejects.toThrow(/no cue change/);
  });

  it('gives up after maxProbes when the probe answers inconsistently', async () => {
    let flip = false;
    const erratic: CueProbe = {
      cueAt: async () => {
        flip = !flip;
        return flip ? 'one' : 'two';
      },
    };

    const found = await measureCueBoundary(erratic, {
      lo: 0,
      hi: 10,
      toleranceSeconds: 0.001,
      maxProbes: 6,
    });
    expect(found.probeCount).toBe(6);
  });
});

describe('cueOffsetMs', () => {
  it('is positive when the player shows the cue late', () => {
    const boundary = {
      seconds: 3.0213,
      toleranceSeconds: 0.001,
      before: 'one',
      after: 'two',
      probeCount: 9,
    };
    expect(cueOffsetMs(boundary, 3)).toBeCloseTo(21.3, 1);
    expect(cueOffsetMs({...boundary, seconds: 2.9787}, 3)).toBeCloseTo(-21.3, 1);
  });
});

describe('videoRefProbe', () => {
  it('returns as soon as the cue changes, without waiting out the cap', async () => {
    // The regression this covers cost a real measurement: reading the cue once
    // after a fixed 400 ms returned the PREVIOUS position's cue, and the
    // bisection then treated that stale value as the answer and converged on
    // the bracket edge.
    let reads = 0;
    const seeks: number[] = [];
    const probe = videoRefProbe(
      seconds => seeks.push(seconds),
      () => {
        reads++;
        return reads > 4 ? 'two' : 'one';
      },
      1000,
      1,
    );

    expect(await probe.cueAt(3.05)).toBe('two');
    expect(seeks).toEqual([3.05]);
    // Far fewer than the 1000 polls the cap allows.
    expect(reads).toBeLessThan(10);
  });

  it('returns the standing cue when the new position carries the same one', async () => {
    // Not a fallback: two probes inside one cue legitimately read alike, and
    // the value already in hand is the correct answer.
    let reads = 0;
    const probe = videoRefProbe(
      () => {},
      () => {
        reads++;
        return 'one';
      },
      10,
      1,
    );

    expect(await probe.cueAt(3.05)).toBe('one');
    expect(reads).toBeGreaterThan(1);
  });

  it('seeks before polling, so the first read cannot precede the seek', async () => {
    const order: string[] = [];
    const probe = videoRefProbe(
      () => order.push('seek'),
      () => {
        order.push('read');
        return 'one';
      },
      2,
      1,
    );

    await probe.cueAt(1);
    // One read to capture the standing cue, then the seek, then polling.
    expect(order.slice(0, 2)).toEqual(['read', 'seek']);
  });
});
