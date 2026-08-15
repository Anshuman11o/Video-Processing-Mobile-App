import {
  JobIndexEntry,
  JobIndexStore,
  MAX_ENTRIES,
  addEntry,
  loadIndex,
  parseEntries,
  recordJob,
} from '../src/storage/jobIndex';

const entry = (id: string, at = '2026-08-13T00:00:00Z'): JobIndexEntry => ({
  job_id: id,
  filename: `${id}.mp4`,
  created_at: at,
});

/** An in-memory store standing in for the device filesystem. */
function memoryStore(initial: string | null = null): JobIndexStore & {
  contents: string | null;
} {
  return {
    contents: initial,
    async read() {
      return this.contents;
    },
    async write(next: string) {
      this.contents = next;
    },
  };
}

/**
 * A store whose read and write actually yield to the event loop.
 *
 * `memoryStore` resolves synchronously enough that a read-modify-write can
 * complete without ever being interleaved, which would make a concurrency test
 * pass against unserialised code. The real store is a filesystem round trip;
 * this one puts a turn of the event loop where that latency is.
 */
function slowStore(): JobIndexStore & {contents: string | null} {
  let contents: string | null = null;
  const tick = () => new Promise<void>(r => setTimeout(r, 0));
  return {
    get contents() {
      return contents;
    },
    set contents(next: string | null) {
      contents = next;
    },
    async read() {
      await tick();
      return contents;
    },
    async write(next: string) {
      await tick();
      contents = next;
    },
  };
}

describe('addEntry', () => {
  it('puts the newest job first', () => {
    const list = addEntry(addEntry([], entry('a')), entry('b'));
    expect(list.map(e => e.job_id)).toEqual(['b', 'a']);
  });

  it('replaces an existing job rather than duplicating it', () => {
    // Two rows for one job would poll twice and render twice.
    const first = addEntry([], entry('a'));
    const again = addEntry(first, {...entry('a'), filename: 'renamed.mp4'});
    expect(again).toHaveLength(1);
    expect(again[0].filename).toBe('renamed.mp4');
  });

  it('caps the list so the file cannot grow without bound', () => {
    let list: JobIndexEntry[] = [];
    for (let i = 0; i < MAX_ENTRIES + 10; i++) {
      list = addEntry(list, entry(`job-${i}`));
    }
    expect(list).toHaveLength(MAX_ENTRIES);
    expect(list[0].job_id).toBe(`job-${MAX_ENTRIES + 9}`);
  });
});

describe('parseEntries', () => {
  it('returns an empty list when nothing is stored', () => {
    expect(parseEntries(null)).toEqual([]);
  });

  it('survives a corrupt file instead of throwing', () => {
    // The job list screen must still open. Losing the list is survivable and
    // documented; crashing on launch is not.
    expect(parseEntries('{not json')).toEqual([]);
    expect(parseEntries('{"jobs": []}')).toEqual([]);
  });

  it('drops entries that do not have the required shape', () => {
    const raw = JSON.stringify([
      entry('good'),
      {job_id: '', filename: 'x', created_at: 'y'},
      {filename: 'no-id.mp4', created_at: 'y'},
      null,
    ]);
    expect(parseEntries(raw).map(e => e.job_id)).toEqual(['good']);
  });

  it('keeps the size and duration when they are stored', () => {
    // These let a row read "14.3 MB · 0:32" before GET /jobs/{id} answers,
    // which is most of the time anyone spends looking at the list.
    const raw = JSON.stringify([
      {...entry('a'), size_bytes: 14_947_952, duration_seconds: 32},
    ]);
    expect(parseEntries(raw)[0]).toMatchObject({
      size_bytes: 14_947_952,
      duration_seconds: 32,
    });
  });

  it('reads entries written before those fields existed', () => {
    const [parsed] = parseEntries(JSON.stringify([entry('a')]));
    expect(parsed.job_id).toBe('a');
    expect(parsed.size_bytes).toBeUndefined();
  });

  it('drops a malformed size rather than the whole job', () => {
    // Losing the entry would lose the only record that the job exists — there
    // is no GET /jobs to rediscover it from. Losing the label costs nothing.
    const raw = JSON.stringify([
      {...entry('a'), size_bytes: 'huge', duration_seconds: null},
    ]);
    const [parsed] = parseEntries(raw);
    expect(parsed.job_id).toBe('a');
    expect(parsed.size_bytes).toBeUndefined();
    expect(parsed.duration_seconds).toBeUndefined();
  });
});

describe('recordJob', () => {
  it('persists across a read', async () => {
    const store = memoryStore();
    await recordJob(store, entry('a'));
    await recordJob(store, entry('b'));
    expect((await loadIndex(store)).map(e => e.job_id)).toEqual(['b', 'a']);
  });

  it('starts clean when the stored file is corrupt', async () => {
    const store = memoryStore('garbage');
    const list = await recordJob(store, entry('a'));
    expect(list.map(e => e.job_id)).toEqual(['a']);
  });

  it('keeps every job when several are recorded at once', async () => {
    // Uploads start in parallel, so N of these are in flight together. Each is
    // a read-modify-write over one file: unserialised, they all read the same
    // empty index and the last write wins, silently losing N-1 jobs from the
    // only record that they exist.
    const store = slowStore();
    await Promise.all(
      ['a', 'b', 'c', 'd', 'e'].map(id => recordJob(store, entry(id))),
    );
    const stored = await loadIndex(store);
    expect(stored.map(e => e.job_id).sort()).toEqual(['a', 'b', 'c', 'd', 'e']);
  });

  it('does not let one failed write poison the ones behind it', async () => {
    const store = slowStore();
    let failNext = true;
    const originalWrite = store.write.bind(store);
    store.write = async (contents: string) => {
      if (failNext) {
        failNext = false;
        throw new Error('disk full');
      }
      return originalWrite(contents);
    };

    await expect(recordJob(store, entry('a'))).rejects.toThrow('disk full');
    await recordJob(store, entry('b'));
    expect((await loadIndex(store)).map(e => e.job_id)).toEqual(['b']);
  });
});
