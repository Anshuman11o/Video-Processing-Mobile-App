// TypeScript types matching the Go backend models

export type JobStatus =
  | 'uploading'
  | 'processing'
  | 'completed'
  | 'failed';

export type StageName =
  | 'validate'
  | 'extract'
  | 'transcribe'
  | 'package';

export type StageStatus =
  | 'pending'
  | 'processing'
  | 'complete'
  | 'failed'
  | 'skipped';

export interface StageState {
  status: StageStatus;
  started_at?: string;
  completed_at?: string;
  error?: string;
  retry_count: number;
}

export interface JobOutput {
  hls_url: string;
  thumbnail_url: string;
  duration_ms: number;
  caption_url?: string;
}

export interface Job {
  job_id: string;
  filename: string;
  status: JobStatus;
  created_at: string;
  updated_at: string;
  stages: Record<StageName, StageState>;
  output?: JobOutput;
  error?: string;
}

// Request/Response types

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
