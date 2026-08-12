import {
  PartTransport,
  PartUploadError,
  UploadExpiredError,
  uploadParts,
} from '../src/upload/uploader';
import {UploadPartInfo} from '../src/types/api';

const noSleep = () => Promise.resolve();

function urlsFor(count: number): UploadPartInfo[] {
  return Array.from({length: count}, (_, i) => ({
    part_number: i + 1,
    url: `https://s3.test/part${i + 1}`,
  }));
}

/** Records every call and returns a predictable ETag per part. */
function fakeTransport(
  behaviour: (call: number, url: string) => Promise<string> = () =>
    Promise.resolve('"etag"'),
): PartTransport & {calls: Array<{url: string; start: number; end: number}>} {
  const calls: Array<{url: string; start: number; end: number}> = [];
  return {
    calls,
    async putPart({url, start, end}) {
      calls.push({url, start, end});
      return behaviour(calls.length, url);
    },
  };
}

describe('uploadParts', () => {
  it('uploads every part in order and returns their ETags', async () => {
    const t = fakeTransport((n: number) => Promise.resolve(`"etag-${n}"`));

    const parts = await uploadParts({
      fileUri: 'file:///clip.mp4',
      fileSize: 12,
      partSize: 5,
      urls: urlsFor(3),
      transport: t,
      sleep: noSleep,
    });

    expect(parts).toEqual([
      {part_number: 1, etag: '"etag-1"'},
      {part_number: 2, etag: '"etag-2"'},
      {part_number: 3, etag: '"etag-3"'},
    ]);
    expect(t.calls.map(c => [c.start, c.end])).toEqual([
      [0, 5],
      [5, 10],
      [10, 12],
    ]);
  });

  it('reports progress that ends at exactly 1', async () => {
    const seen: number[] = [];
    await uploadParts({
      fileUri: 'file:///clip.mp4',
      fileSize: 10,
      partSize: 5,
      urls: urlsFor(2),
      transport: fakeTransport(),
      onProgress: f => seen.push(f),
      sleep: noSleep,
    });
    expect(seen[seen.length - 1]).toBe(1);
    // Monotonic: progress that goes backwards reads as a stall to a user.
    expect([...seen].sort((a, b) => a - b)).toEqual(seen);
  });

  it('retries a failed part and keeps the ETags already collected', async () => {
    let attempts = 0;
    const t = fakeTransport((n, url) => {
      if (url.endsWith('part2')) {
        attempts++;
        if (attempts < 3) {
          return Promise.reject(new Error('connection reset'));
        }
      }
      return Promise.resolve(`"etag-${url.slice(-1)}"`);
    });

    const parts = await uploadParts({
      fileUri: 'file:///clip.mp4',
      fileSize: 10,
      partSize: 5,
      urls: urlsFor(2),
      transport: t,
      sleep: noSleep,
    });

    expect(attempts).toBe(3);
    expect(parts).toHaveLength(2);
    // The retry must re-PUT only the failed part, not restart the upload.
    expect(t.calls.filter(c => c.start === 0)).toHaveLength(1);
  });

  it('gives up after maxAttempts and names the part', async () => {
    const t = fakeTransport(() => Promise.reject(new Error('nope')));
    await expect(
      uploadParts({
        fileUri: 'file:///clip.mp4',
        fileSize: 5,
        partSize: 5,
        urls: urlsFor(1),
        transport: t,
        sleep: noSleep,
      }),
    ).rejects.toBeInstanceOf(PartUploadError);
    expect(t.calls).toHaveLength(3);
  });

  it('treats a 403 as expiry and stops immediately', async () => {
    // Every URL was signed at the same moment with the same expiry, so
    // retrying and then marching on would just repeat the same failure.
    const t = fakeTransport(() =>
      Promise.reject(Object.assign(new Error('403'), {status: 403})),
    );
    await expect(
      uploadParts({
        fileUri: 'file:///clip.mp4',
        fileSize: 10,
        partSize: 5,
        urls: urlsFor(2),
        transport: t,
        sleep: noSleep,
      }),
    ).rejects.toBeInstanceOf(UploadExpiredError);
    expect(t.calls).toHaveLength(1);
  });

  it('refuses to upload when the API issued a different number of URLs', async () => {
    // This means the client and the API disagree about the file. Uploading
    // anyway produces an object that only fails at complete time.
    await expect(
      uploadParts({
        fileUri: 'file:///clip.mp4',
        fileSize: 12,
        partSize: 5,
        urls: urlsFor(2),
        transport: fakeTransport(),
        sleep: noSleep,
      }),
    ).rejects.toThrow(/part count mismatch/);
  });

  it('rejects a 200 with no ETag', async () => {
    // Without an ETag the part cannot be named in CompleteMultipartUpload, so
    // a success with no ETag is a failure.
    const t = fakeTransport(() => Promise.resolve(''));
    await expect(
      uploadParts({
        fileUri: 'file:///clip.mp4',
        fileSize: 5,
        partSize: 5,
        urls: urlsFor(1),
        transport: t,
        sleep: noSleep,
      }),
    ).rejects.toBeInstanceOf(PartUploadError);
  });
});
