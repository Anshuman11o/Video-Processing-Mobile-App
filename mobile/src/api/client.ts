import axios, {AxiosInstance} from 'axios';
import {
  CreateJobRequest,
  CreateJobResponse,
  CompleteUploadRequest,
  CompleteUploadResponse,
  Job,
  ReelResponse,
} from '../types/api';

const API_BASE_URL = __DEV__
  ? 'http://localhost:8080'
  : 'http://localhost:8080'; // TODO: production URL

const api: AxiosInstance = axios.create({
  baseURL: API_BASE_URL,
  timeout: 30000,
  headers: {
    'Content-Type': 'application/json',
  },
});

export async function createJob(
  request: CreateJobRequest,
): Promise<CreateJobResponse> {
  const response = await api.post<CreateJobResponse>('/jobs', request);
  return response.data;
}

export async function completeUpload(
  jobId: string,
  request: CompleteUploadRequest,
): Promise<CompleteUploadResponse> {
  const response = await api.post<CompleteUploadResponse>(
    `/jobs/${jobId}/complete`,
    request,
  );
  return response.data;
}

export async function getJobStatus(jobId: string): Promise<Job> {
  const response = await api.get<Job>(`/jobs/${jobId}`);
  return response.data;
}

export async function getReel(jobId: string): Promise<ReelResponse> {
  const response = await api.get<ReelResponse>(`/jobs/${jobId}/reel`);
  return response.data;
}

// Mock data for development before API is ready

const MOCK_JOBS: Job[] = [
  {
    job_id: '550e8400-e29b-41d4-a716-446655440000',
    filename: 'beach-sunset.mp4',
    status: 'completed',
    created_at: '2024-01-15T10:30:00Z',
    updated_at: '2024-01-15T10:35:00Z',
    stages: {
      validate: {status: 'complete', retry_count: 0, completed_at: '2024-01-15T10:31:00Z'},
      extract: {status: 'complete', retry_count: 0, completed_at: '2024-01-15T10:32:00Z'},
      transcribe: {status: 'complete', retry_count: 0, completed_at: '2024-01-15T10:33:00Z'},
      package: {status: 'complete', retry_count: 0, completed_at: '2024-01-15T10:34:00Z'},
    },
    output: {
      hls_url: 'https://dayreel-hls-output.s3.us-east-1.amazonaws.com/550e8400/master.m3u8',
      thumbnail_url: 'https://dayreel-processed.s3.us-east-1.amazonaws.com/550e8400/frames/frame_001.jpg',
      duration_ms: 15000,
    },
  },
  {
    job_id: '660e8400-e29b-41d4-a716-446655440001',
    filename: 'morning-coffee.mp4',
    status: 'processing',
    created_at: '2024-01-15T11:00:00Z',
    updated_at: '2024-01-15T11:02:00Z',
    stages: {
      validate: {status: 'complete', retry_count: 0, completed_at: '2024-01-15T11:01:00Z'},
      extract: {status: 'processing', retry_count: 0, started_at: '2024-01-15T11:01:05Z'},
      transcribe: {status: 'pending', retry_count: 0},
      package: {status: 'pending', retry_count: 0},
    },
  },
];

export function getMockJobs(): Job[] {
  return MOCK_JOBS;
}
