// The device-local job index.
//
// The API has no `GET /jobs`. A job can be fetched by ID but not discovered, so
// the only record of which jobs exist is the one this file keeps. Losing it
// loses the visible history — the jobs themselves survive in DynamoDB. That
// trade is documented in docs/SETUP.md.
//
// The merge logic is pure and the storage is injected, so the ordering and
// de-duplication rules below are testable without a device.

export interface JobIndexEntry {
  job_id: string;
  filename: string;
  /** ISO 8601, client clock. Only used for ordering the list. */
  created_at: string;
  /**
   * The file's size, as the picker reported it.
   *
   * Recorded locally so a row can say "14.3 MB" before — or without —
   * `GET /jobs/{id}` answering. The list fetches N jobs one request each, and
   * during an upload the API may not have much to say about them yet; a row
   * that reads only "loading…" is useless exactly when the user is watching
   * hardest. Optional because entries written by earlier versions do not have
   * it, and losing the index to a stricter parser is worse than a missing size.
   */
  size_bytes?: number;
  /** Video length in seconds, once the decoder reported it. Same reasoning. */
  duration_seconds?: number;
}

/** Persistence for the index. Implemented over the filesystem on a device. */
export interface JobIndexStore {
  read(): Promise<string | null>;
  write(contents: string): Promise<void>;
}

/**
 * How many jobs to keep. The list is a development aid on a project with a
 * handful of runs left; an unbounded file that is rewritten on every job
 * creation is a slow leak with no upside.
 */
export const MAX_ENTRIES = 100;

/**
 * Insert an entry, newest first, replacing any existing entry with the same id.
 *
 * De-duplication is by `job_id` rather than by position because the same job
 * can legitimately be written twice — created, then updated once its filename
 * is known — and two rows for one job would poll twice and render twice.
 */
export function addEntry(
  entries: JobIndexEntry[],
  entry: JobIndexEntry,
): JobIndexEntry[] {
  const without = entries.filter(e => e.job_id !== entry.job_id);
  return [entry, ...without].slice(0, MAX_ENTRIES);
}

/**
 * Parse stored JSON into entries, discarding anything malformed.
 *
 * This is deliberately forgiving. The file is written by an earlier version of
 * an app that is still being built, so a shape change is likely; losing the
 * list is a documented, survivable outcome, whereas throwing here would make
 * the job list screen crash on open with no way for a user to recover.
 */
export function parseEntries(raw: string | null): JobIndexEntry[] {
  if (!raw) {
    return [];
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(raw);
  } catch {
    return [];
  }
  if (!Array.isArray(parsed)) {
    return [];
  }
  return parsed.map(normaliseEntry).filter((e): e is JobIndexEntry => e !== null);
}

/**
 * Validate one stored entry, dropping fields rather than whole entries where
 * it can.
 *
 * The optional fields are handled differently from the required ones on
 * purpose. A missing `job_id` makes the entry useless, so it goes; a
 * `size_bytes` of the wrong type only makes one label wrong, and discarding
 * the job over it would lose the only record that the job exists — the API has
 * no `GET /jobs` to rediscover it from.
 */
function normaliseEntry(value: unknown): JobIndexEntry | null {
  const e = value as Partial<JobIndexEntry> | null;
  if (
    !e ||
    typeof e.job_id !== 'string' ||
    e.job_id.length === 0 ||
    typeof e.filename !== 'string' ||
    typeof e.created_at !== 'string'
  ) {
    return null;
  }
  const entry: JobIndexEntry = {
    job_id: e.job_id,
    filename: e.filename,
    created_at: e.created_at,
  };
  if (typeof e.size_bytes === 'number' && Number.isFinite(e.size_bytes)) {
    entry.size_bytes = e.size_bytes;
  }
  if (
    typeof e.duration_seconds === 'number' &&
    Number.isFinite(e.duration_seconds)
  ) {
    entry.duration_seconds = e.duration_seconds;
  }
  return entry;
}

/** Reads the index, tolerating a missing or corrupt file. */
export async function loadIndex(store: JobIndexStore): Promise<JobIndexEntry[]> {
  return parseEntries(await store.read());
}

/**
 * Serialises writes per store.
 *
 * `recordJob` is read-modify-write over a single file, and uploads now start in
 * parallel — so N calls can be in flight at once, every one of them having read
 * the index before any of them wrote it. Last write wins, and the other N-1
 * jobs are gone from the only record that they exist: there is no `GET /jobs`
 * to rediscover them with, so losing a row here loses the job from the app
 * permanently while it runs happily on the backend.
 *
 * Keyed by store rather than global so a test's in-memory store cannot be
 * serialised behind the device one. A WeakMap so a discarded store does not
 * pin its queue forever.
 */
const writeQueues = new WeakMap<JobIndexStore, Promise<unknown>>();

/** Records a job. Call this immediately after `POST /jobs` returns an id. */
export function recordJob(
  store: JobIndexStore,
  entry: JobIndexEntry,
): Promise<JobIndexEntry[]> {
  const previous = writeQueues.get(store) ?? Promise.resolve();
  // `catch` before chaining, so one failed write does not poison the queue and
  // reject every job recorded after it.
  const next = previous.catch(() => undefined).then(async () => {
    const updated = addEntry(await loadIndex(store), entry);
    await store.write(JSON.stringify(updated));
    return updated;
  });
  writeQueues.set(store, next);
  return next;
}
