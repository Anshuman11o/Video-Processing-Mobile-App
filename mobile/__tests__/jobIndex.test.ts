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
});
