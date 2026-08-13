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
  return parsed.filter(isEntry);
}

function isEntry(value: unknown): value is JobIndexEntry {
  const e = value as Partial<JobIndexEntry> | null;
  return (
    !!e &&
    typeof e.job_id === 'string' &&
    e.job_id.length > 0 &&
    typeof e.filename === 'string' &&
    typeof e.created_at === 'string'
  );
}

/** Reads the index, tolerating a missing or corrupt file. */
export async function loadIndex(store: JobIndexStore): Promise<JobIndexEntry[]> {
  return parseEntries(await store.read());
}

/** Records a job. Call this immediately after `POST /jobs` returns an id. */
export async function recordJob(
  store: JobIndexStore,
  entry: JobIndexEntry,
): Promise<JobIndexEntry[]> {
  const updated = addEntry(await loadIndex(store), entry);
  await store.write(JSON.stringify(updated));
  return updated;
}
