// Wire types for the DayReel API.
//
// These are transcribed from the Go structs, which are the only authority:
//   backend/internal/models/job.go     — Job, StageState, OutputInfo, UploadInfo, Metrics
//   backend/internal/api/handlers.go   — the request/response DTOs
//
// The previous version of this file was written from a plan rather than from
// the wire, and disagreed with the backend in eight places — including three of
// the five StageStatus values, which is why the job list's progress bar counted
// 0 of 4 completed stages through a fully successful job. Anything added here
// must exist in Go. See api.contract.test.ts, which asserts these against a
// captured response.

/** models.JobStatus. `pending` is the state NewJob starts in — do not drop it. */
export type JobStatus =
  | 'pending'
  | 'uploading'
  | 'processing'
  | 'completed'
  | 'failed';

/** models.StageName. */
export type StageName = 'validate' | 'extract' | 'transcribe' | 'package';

/**
 * models.StageStatus — exactly four values.
 *
 * Note `running` (not `processing`) and `completed` (not `complete`); Go uses a
 * different word at each of those points than JobStatus does, and there is no
 * `skipped`.
 */
export type StageStatus = 'pending' | 'running' | 'completed' | 'failed';

/** models.StageState. The count field is `attempts`, not `retry_count`. */
export interface StageState {
  status: StageStatus;
  started_at?: string;
  completed_at?: string;
  attempts: number;
  error?: string;
  output_key?: string;
}

/** models.OutputInfo. Duration is SECONDS here; milliseconds are in Metrics. */
export interface OutputInfo {
  hls_url: string;
  duration_seconds: number;
  thumbnail_url?: string;
}

/** models.UploadInfo. */
export interface UploadInfo {
  upload_id: string;
  bucket: string;
  key: string;
  part_size: number;
  total_parts: number;
  completed_at?: string;
}

/** models.Metrics — every field is omitempty, so every field is optional. */
export interface Metrics {
  upload_duration_ms?: number;
  total_processing_ms?: number;
  validate_duration_ms?: number;
  extract_duration_ms?: number;
  transcribe_duration_ms?: number;
  package_duration_ms?: number;
}

/**
 * models.Job, as `GET /jobs/{id}` serialises it — the raw model, DynamoDB keys
 * included (handlers.go GetJobStatus writes the struct directly).
 *
 * `stages` is a Go map, so a key can be absent; it is NOT safe to index without
 * checking. There is deliberately no top-level `error` field — the previous
 * types invented one. A failure message lives on the stage that failed.
 */
export interface Job {
  pk: string;
  sk: string;
  job_id: string;
  filename: string;
  size_bytes: number;
  content_type: string;
  status: JobStatus;
  created_at: string;
  updated_at: string;
  upload?: UploadInfo;
  stages: Partial<Record<StageName, StageState>>;
  output?: OutputInfo;
  metrics?: Metrics;
}

// --- Request/response DTOs (handlers.go) ---

export interface CreateJobRequest {
  filename: string;
  size_bytes: number;
  content_type: string;
}

export interface UploadPartInfo {
  part_number: number;
  url: string;
}

export interface CreateJobResponse {
  job_id: string;
  upload_id: string;
  upload_urls: UploadPartInfo[];
  part_size: number;
  expires_in: number;
}

/** storage.CompletedPart. The ETag arrives from S3 quoted; send it back as given. */
export interface CompletedPart {
  part_number: number;
  etag: string;
}

export interface CompleteUploadRequest {
  upload_id: string;
  parts: CompletedPart[];
}

export interface CompleteUploadResponse {
  job_id: string;
  status: JobStatus;
  message: string;
}

export interface ReelResponse {
  job_id: string;
  hls_url: string;
  thumbnail_url: string;
}

export interface ErrorResponse {
  error: string;
  code: string;
}

// --- Derived helpers ---

/** Processing order, mirroring models.AllStages(). */
export const ALL_STAGES: StageName[] = [
  'validate',
  'extract',
  'transcribe',
  'package',
];

/** A job stops changing once it reaches one of these — polling should stop too. */
export function isTerminal(status: JobStatus): boolean {
  return status === 'completed' || status === 'failed';
}

/** How many stages have finished, for progress display. */
export function completedStageCount(job: Job): number {
  return ALL_STAGES.filter(s => job.stages[s]?.status === 'completed').length;
}

/**
 * The error off whichever stage failed. The API has no top-level error field,
 * so this is the only place a failure reason can be found.
 */
export function jobErrorMessage(job: Job): string | undefined {
  for (const stage of ALL_STAGES) {
    const state = job.stages[stage];
    if (state?.status === 'failed' && state.error) {
      return `${stage}: ${state.error}`;
    }
  }
  return undefined;
}
