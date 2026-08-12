# Backend (Go API)

Go HTTP API for DayReel video processing pipeline. Handles job creation, presigned URL generation for direct-to-S3 uploads, upload completion, and job status retrieval. Never touches video bytes.

## Structure

```
backend/
├── cmd/api/main.go              # Entry point, wires dependencies, starts server
├── internal/
│   ├── api/
│   │   ├── handlers.go          # HTTP handlers (CreateJob, CompleteUpload, GetJobStatus, GetReel)
│   │   ├── router.go            # gorilla/mux route setup
│   │   └── middleware.go        # Logging + CORS middleware
│   ├── cache/redis.go           # Redis client for job status caching (10s TTL)
│   ├── config/config.go         # Environment-based configuration
│   ├── db/dynamodb.go           # DynamoDB CRUD for jobs (single-table design)
│   ├── events/messages.go       # SQS message types and queue/bucket constants
│   ├── models/job.go            # Job, StageState, UploadInfo data models
│   └── storage/s3.go            # S3 multipart upload + presigned URL generation
├── Dockerfile                   # Multi-stage Go build
├── go.mod / go.sum
```

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | /health | Health check |
| POST | /jobs | Create job, get presigned upload URLs |
| POST | /jobs/{id}/complete | Signal upload completion |
| GET | /jobs/{id} | Get job status (cached via Redis) |
| GET | /jobs/{id}/reel | Get HLS playback URL (completed jobs only) |

## Data Flow

1. Client POSTs /jobs with filename + size
2. API creates S3 multipart upload, generates presigned URLs, saves job to DynamoDB
3. Client uploads parts directly to S3 using presigned URLs
4. Client POSTs /jobs/{id}/complete with ETags
5. API completes S3 multipart upload, updates job status to "processing"
6. S3 event notification triggers validate queue (configured in LocalStack init)

## Running

```bash
# Via Docker Compose (from infra/)
docker-compose up -d --build api

# Locally
cd backend && go run ./cmd/api
```
