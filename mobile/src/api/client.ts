import axios, {AxiosInstance, AxiosError} from 'axios';

import {
  CompleteUploadRequest,
  CompleteUploadResponse,
  CreateJobRequest,
  CreateJobResponse,
  ErrorResponse,
  Job,
  ReelResponse,
} from '../types/api';

/**
 * The backend, as seen from the Android emulator.
 *
 * `10.0.2.2` is the emulator's alias for the host's loopback interface;
 * `localhost` inside the emulator is the emulated device itself.
 *
 * This used to have to name the same client environment as `S3_PUBLIC_ENDPOINT`
 * in the compose file, because uploads went to an emulated S3 reachable under a
 * different name from each client, and a mismatch failed the upload with a
 * signature error while every API call succeeded. That constraint is gone with
 * the emulator: uploads are presigned against real S3's regional endpoint,
 * which resolves identically everywhere. Only the API is host-relative now.
 *
 * See docs/SETUP.md, "Presigned URLs are bound to the host they were signed for".
 */
export const API_BASE_URL = 'http://10.0.2.2:8080';

const api: AxiosInstance = axios.create({
  baseURL: API_BASE_URL,
  timeout: 30000,
  headers: {'Content-Type': 'application/json'},
});

/**
 * An error carrying the API's own `{error, code}` body when it sent one.
 *
 * The API distinguishes failures by `code` (`NOT_READY`, `VALIDATION_ERROR`,
 * `NOT_FOUND`, ...) and the UI needs that distinction — `NOT_READY` on the reel
 * endpoint is an ordinary "not finished yet", not a failure to report.
 */
export class ApiError extends Error {
  readonly status: number;
  readonly code?: string;

  constructor(status: number, message: string, code?: string) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
  }
}

function wrap(err: unknown): never {
  const axiosErr = err as AxiosError<ErrorResponse>;
  if (axiosErr?.isAxiosError && axiosErr.response) {
    const body = axiosErr.response.data;
    throw new ApiError(
      axiosErr.response.status,
      body?.error ?? axiosErr.message,
      body?.code,
    );
  }
  // No response at all: DNS, connection refused, or the 30s timeout. On the
  // emulator this is nearly always the backend not running, or this constant
  // pointing at localhost instead of 10.0.2.2.
  throw new ApiError(0, `cannot reach the API at ${API_BASE_URL}: ${err}`);
}

export async function createJob(
  request: CreateJobRequest,
): Promise<CreateJobResponse> {
  try {
    return (await api.post<CreateJobResponse>('/jobs', request)).data;
  } catch (err) {
    return wrap(err);
  }
}

/**
 * Finalize the multipart upload.
 *
 * This is not bookkeeping. It calls `CompleteMultipartUpload`, whose
 * `s3:ObjectCreated:CompleteMultipartUpload` event is the only thing that
 * starts the pipeline. Skip it and the parts sit in S3 forever and no stage
 * ever runs.
 */
export async function completeUpload(
  jobId: string,
  request: CompleteUploadRequest,
): Promise<CompleteUploadResponse> {
  try {
    return (
      await api.post<CompleteUploadResponse>(`/jobs/${jobId}/complete`, request)
    ).data;
  } catch (err) {
    return wrap(err);
  }
}

export async function getJobStatus(jobId: string): Promise<Job> {
  try {
    return (await api.get<Job>(`/jobs/${jobId}`)).data;
  } catch (err) {
    return wrap(err);
  }
}

/**
 * Fetch the finished reel.
 *
 * Returns `null` when the job is not finished (HTTP 409 `NOT_READY`) so callers
 * can treat "not yet" as a normal outcome. Note this endpoint reads DynamoDB
 * directly while `getJobStatus` is served from a 10-second Redis cache, so the
 * two can disagree briefly — a job can report `completed` here before the
 * cached status catches up, and vice versa.
 */
export async function getReel(jobId: string): Promise<ReelResponse | null> {
  try {
    return (await api.get<ReelResponse>(`/jobs/${jobId}/reel`)).data;
  } catch (err) {
    const axiosErr = err as AxiosError<ErrorResponse>;
    if (axiosErr?.isAxiosError && axiosErr.response?.status === 409) {
      return null;
    }
    return wrap(err);
  }
}
