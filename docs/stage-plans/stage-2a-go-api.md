# Stage 2A: Go API

> **Partly superseded.** The endpoints, data flow and S3 multipart design are
> current. Three things below are not: the Redis cache is now an in-process TTL
> cache, `internal/queue/` is a SQLite queue rather than an SQS wrapper
> (`stage-3b-local-queue.md`), and the Docker Compose section is void — the API
> runs as `make api`. See `infra/CONTEXT.md`.

> **Run in parallel with:** Stage 2B (Mobile Shell)
> **Depends on:** Stage 1A (Data Schemas), Stage 1B (Infrastructure)
> **Estimated time:** 30 minutes
> **Blocks:** Stage 3A (Validate Worker), Stage 7 (Upload Integration)

## Aim

Build the Go HTTP API that handles job creation, presigned URL generation for
direct-to-S3 uploads, upload completion signaling, and job status retrieval.
The API never touches video bytes — it orchestrates the flow.

---

## Components Touched

| Component | Action | Files |
|-----------|--------|-------|
| `backend/cmd/api/` | Create | `main.go` |
| `backend/internal/api/` | Create | `router.go`, `handlers.go`, `middleware.go` |
| `backend/internal/storage/` | Create | `s3.go` |
| `backend/internal/db/` | Create | `dynamodb.go` |
| `backend/internal/queue/` | Create | `sqs.go` |
| `backend/internal/config/` | Create | `config.go` |
| `infra/docker-compose.yml` | Modify | Add API service |

---

## API Endpoints

### POST /jobs
Create a new job and get presigned URLs for multipart upload.

**Request:**
```json
{
  "filename": "beach-sunset.mp4",
  "size_bytes": 15728640,
  "content_type": "video/mp4"
}
```

**Response (201 Created):**
```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "upload_id": "s3-multipart-upload-id",
  "upload_urls": [
    {
      "part_number": 1,
      "url": "https://s3.../presigned-url-part-1"
    },
    {
      "part_number": 2,
      "url": "https://s3.../presigned-url-part-2"
    }
  ],
  "part_size": 5242880,
  "expires_in": 3600
}
```

**Logic:**
1. Generate UUID for job_id
2. Calculate number of parts: `ceil(size_bytes / 5MB)`
3. Call S3 `CreateMultipartUpload`
4. Generate presigned URLs for each part via `UploadPart`
5. Create job in DynamoDB with status=uploading
6. Return response

---

### POST /jobs/{id}/complete
Signal that all parts have been uploaded. Completes the multipart upload.

**Request:**
```json
{
  "upload_id": "s3-multipart-upload-id",
  "parts": [
    {"part_number": 1, "etag": "\"abc123\""},
    {"part_number": 2, "etag": "\"def456\""}
  ]
}
```

**Response (200 OK):**
```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "processing",
  "message": "Upload complete, processing started"
}
```

**Logic:**
1. Call S3 `CompleteMultipartUpload` with ETags
2. Update job status to `processing` in DynamoDB
3. Record `upload.completed_at` timestamp
4. (Optional) Send message to validate queue — or let S3 event notification handle it

---

### GET /jobs/{id}
Get job status including all stage states.

**Response (200 OK):**
```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "filename": "beach-sunset.mp4",
  "status": "processing",
  "created_at": "2024-01-15T10:30:00Z",
  "stages": {
    "validate": {"status": "complete", "completed_at": "2024-01-15T10:31:00Z"},
    "extract": {"status": "processing", "started_at": "2024-01-15T10:31:05Z"},
    "transcribe": {"status": "pending"},
    "package": {"status": "pending"}
  },
  "output": null
}
```

**Logic:**
1. Check Redis cache first (key: `job:{id}`)
2. On cache miss, fetch from DynamoDB
3. Cache result with 10s TTL
4. Return job state

---

### GET /jobs/{id}/reel
Get the HLS playback URL for a completed job.

**Response (200 OK):**
```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "hls_url": "http://localhost:4566/dayreel-hls-output/550e8400.../master.m3u8",
  "thumbnail_url": "http://localhost:4566/dayreel-processed/550e8400.../frames/frame_001.jpg"
}
```

**Response (404 Not Found):** If job doesn't exist
**Response (409 Conflict):** If job not yet complete

---

## Data Flow

```
Mobile App                      Go API                         AWS Services
    │                              │                                │
    │  POST /jobs                  │                                │
    │  {filename, size}            │                                │
    │─────────────────────────────▶│                                │
    │                              │  CreateMultipartUpload         │
    │                              │───────────────────────────────▶│ S3
    │                              │◀───────────────────────────────│
    │                              │  PutItem (job)                 │
    │                              │───────────────────────────────▶│ DynamoDB
    │                              │                                │
    │  {job_id, upload_urls}       │                                │
    │◀─────────────────────────────│                                │
    │                              │                                │
    │  PUT part 1 ─────────────────┼───────────────────────────────▶│ S3
    │  PUT part 2 ─────────────────┼───────────────────────────────▶│ S3
    │  ...                         │                                │
    │                              │                                │
    │  POST /jobs/{id}/complete    │                                │
    │  {parts: [{etag}...]}        │                                │
    │─────────────────────────────▶│                                │
    │                              │  CompleteMultipartUpload       │
    │                              │───────────────────────────────▶│ S3
    │                              │  UpdateItem (status)           │
    │                              │───────────────────────────────▶│ DynamoDB
    │                              │                                │
    │                              │         S3 Event ─────────────▶│ SQS
    │                              │                     (validate) │
    │  {status: processing}        │                                │
    │◀─────────────────────────────│                                │
    │                              │                                │
    │  GET /jobs/{id}              │                                │
    │─────────────────────────────▶│  GET (cache)                   │
    │                              │───────────────────────────────▶│ Redis
    │  {stages: {...}}             │                                │
    │◀─────────────────────────────│                                │
```

---

## Go Package Structure

### `backend/internal/config/config.go`

```go
package config

import "os"

type Config struct {
    Port            string
    AWSEndpoint     string
    AWSRegion       string
    S3RawBucket     string
    S3ProcessedBucket string
    S3HLSBucket     string
    DynamoDBTable   string
    RedisURL        string
    UseLocalStack   bool
}

func Load() *Config {
    return &Config{
        Port:            getEnv("API_PORT", "8080"),
        AWSEndpoint:     getEnv("LOCALSTACK_ENDPOINT", ""),
        AWSRegion:       getEnv("AWS_REGION", "us-east-1"),
        S3RawBucket:     getEnv("S3_RAW_BUCKET", "dayreel-raw-videos"),
        S3ProcessedBucket: getEnv("S3_PROCESSED_BUCKET", "dayreel-processed"),
        S3HLSBucket:     getEnv("S3_HLS_BUCKET", "dayreel-hls-output"),
        DynamoDBTable:   getEnv("DYNAMODB_TABLE", "dayreel-jobs"),
        RedisURL:        getEnv("REDIS_URL", "localhost:6379"),
        UseLocalStack:   getEnv("USE_LOCALSTACK", "true") == "true",
    }
}

func getEnv(key, fallback string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return fallback
}
```

### `backend/internal/storage/s3.go`

```go
package storage

type S3Client struct {
    client *s3.Client
    presignClient *s3.PresignClient
    bucket string
}

// CreateMultipartUpload starts a multipart upload, returns upload ID
func (s *S3Client) CreateMultipartUpload(ctx context.Context, key, contentType string) (string, error)

// GeneratePresignedUploadURL creates a presigned URL for uploading a part
func (s *S3Client) GeneratePresignedUploadURL(ctx context.Context, key, uploadID string, partNumber int, expiry time.Duration) (string, error)

// CompleteMultipartUpload finalizes the upload with the given ETags
func (s *S3Client) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) error

// AbortMultipartUpload cancels an in-progress upload
func (s *S3Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error
```

### `backend/internal/db/dynamodb.go`

```go
package db

type DynamoDBClient struct {
    client *dynamodb.Client
    table  string
}

// CreateJob inserts a new job record
func (d *DynamoDBClient) CreateJob(ctx context.Context, job *models.Job) error

// GetJob retrieves a job by ID
func (d *DynamoDBClient) GetJob(ctx context.Context, jobID string) (*models.Job, error)

// UpdateJobStatus updates the job's overall status
func (d *DynamoDBClient) UpdateJobStatus(ctx context.Context, jobID string, status models.JobStatus) error

// UpdateStageStatus updates a specific stage's status
func (d *DynamoDBClient) UpdateStageStatus(ctx context.Context, jobID string, stage models.StageName, state models.StageState) error
```

### `backend/internal/api/handlers.go`

```go
package api

type Handler struct {
    s3     *storage.S3Client
    db     *db.DynamoDBClient
    cache  *cache.RedisClient
    config *config.Config
}

func (h *Handler) CreateJob(w http.ResponseWriter, r *http.Request)
func (h *Handler) CompleteUpload(w http.ResponseWriter, r *http.Request)
func (h *Handler) GetJobStatus(w http.ResponseWriter, r *http.Request)
func (h *Handler) GetReel(w http.ResponseWriter, r *http.Request)
```

---

## Docker Compose Addition

Add to `infra/docker-compose.yml`:

```yaml
  api:
    build:
      context: ../backend
      dockerfile: Dockerfile
    container_name: dayreel-api
    ports:
      - "8080:8080"
    environment:
      - API_PORT=8080
      - AWS_REGION=us-east-1
      - AWS_ACCESS_KEY_ID=test
      - AWS_SECRET_ACCESS_KEY=test
      - LOCALSTACK_ENDPOINT=http://localstack:4566
      - USE_LOCALSTACK=true
      - S3_RAW_BUCKET=dayreel-raw-videos
      - S3_PROCESSED_BUCKET=dayreel-processed
      - S3_HLS_BUCKET=dayreel-hls-output
      - DYNAMODB_TABLE=dayreel-jobs
      - REDIS_URL=redis:6379
    depends_on:
      localstack:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      - dayreel-network
```

### `backend/Dockerfile`

```dockerfile
FROM golang:1.22-alpine AS builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /api ./cmd/api

FROM alpine:3.19
RUN apk --no-cache add ca-certificates
COPY --from=builder /api /api
EXPOSE 8080
CMD ["/api"]
```

---

## Tasks

1. [ ] Create `backend/internal/config/config.go`
2. [ ] Create `backend/internal/storage/s3.go` with multipart upload methods
3. [ ] Create `backend/internal/db/dynamodb.go` with CRUD methods
4. [ ] Create `backend/internal/cache/redis.go` with get/set methods
5. [ ] Create `backend/internal/api/handlers.go` with all endpoints
6. [ ] Create `backend/internal/api/router.go` with route setup
7. [ ] Create `backend/internal/api/middleware.go` (logging, CORS)
8. [ ] Create `backend/cmd/api/main.go` entry point
9. [ ] Create `backend/Dockerfile`
10. [ ] Update `infra/docker-compose.yml` with API service
11. [ ] Add dependencies: `go get github.com/gorilla/mux github.com/redis/go-redis/v9`
12. [ ] Write integration test for create → complete → get flow
13. [ ] Create `backend/CONTEXT.md`

---

## Test

```bash
# Build and start API
cd infra && docker-compose up -d --build api

# Create a job
JOB=$(curl -s -X POST http://localhost:8080/jobs \
  -H "Content-Type: application/json" \
  -d '{"filename":"test.mp4","size_bytes":10485760,"content_type":"video/mp4"}')
echo $JOB | jq .

# Extract job_id and upload_id
JOB_ID=$(echo $JOB | jq -r '.job_id')
UPLOAD_ID=$(echo $JOB | jq -r '.upload_id')

# Simulate upload completion (without actually uploading)
curl -s -X POST "http://localhost:8080/jobs/${JOB_ID}/complete" \
  -H "Content-Type: application/json" \
  -d "{\"upload_id\":\"${UPLOAD_ID}\",\"parts\":[{\"part_number\":1,\"etag\":\"\\\"abc\\\"\"}]}"

# Check status
curl -s "http://localhost:8080/jobs/${JOB_ID}" | jq .
```

---

## Verification Checklist

- [ ] `POST /jobs` returns 201 with job_id, upload_urls, upload_id
- [ ] Presigned URLs are valid (can PUT to them)
- [ ] `POST /jobs/{id}/complete` triggers S3 CompleteMultipartUpload
- [ ] Job status updates to `processing` after completion
- [ ] `GET /jobs/{id}` returns full job with stages
- [ ] Redis caching works (second GET is faster)
- [ ] `GET /jobs/{id}/reel` returns 409 for incomplete jobs
- [ ] API handles invalid job IDs with 404
- [ ] CORS headers present for mobile access

---

## Claude Code Implementation Plan

### Recommended Approach: Sequential with Parallel File Writes

This stage has interdependent Go packages but file writes can be parallelized.
Use direct implementation, no subagents needed.

### Execution Steps

```
Phase 1: Dependencies (sequential)
1. cd backend && go get github.com/gorilla/mux github.com/redis/go-redis/v9 github.com/google/uuid

Phase 2: Core packages (parallel writes)
2a. Write backend/internal/config/config.go
2b. Write backend/internal/storage/s3.go
2c. Write backend/internal/db/dynamodb.go
2d. Write backend/internal/cache/redis.go

Phase 3: API layer (parallel writes)
3a. Write backend/internal/api/handlers.go
3b. Write backend/internal/api/router.go
3c. Write backend/internal/api/middleware.go

Phase 4: Entry point and Docker
4a. Write backend/cmd/api/main.go
4b. Write backend/Dockerfile
4c. Update infra/docker-compose.yml

Phase 5: Build and test
5. docker-compose up -d --build api
6. Run curl tests
7. Write CONTEXT.md
```

### Parallel Opportunities

| Phase | Parallel Files |
|-------|----------------|
| 2 | config.go, s3.go, dynamodb.go, redis.go |
| 3 | handlers.go, router.go, middleware.go |
| 4 | main.go, Dockerfile, docker-compose.yml |

### Why Not Subagents?

- Go packages have import dependencies (handlers imports storage, db, cache)
- Need to ensure consistent interfaces across packages
- Sequential compilation catches errors early

### Potential Blockers

| Blocker | Resolution |
|---------|------------|
| Go not installed | `brew install go` |
| Port 8080 in use | Change API_PORT or kill conflicting process |
| LocalStack not running | `make dev-up` first |
| S3 presigning fails | Check AWS credentials and endpoint config |

### Time Estimate

- Package writes: ~15 minutes
- Docker build: ~2 minutes (first time with layer caching)
- Testing: ~5 minutes
- **Total:** ~25 minutes

---

## Notes

- **No video bytes through API.** Clients upload directly to S3 via presigned URLs.
  API only handles metadata and orchestration.

- **Presigned URL expiry:** 1 hour default. Client should refresh if upload takes
  longer (large files on slow connections).

- **Redis cache TTL:** 10 seconds. Balances freshness with DynamoDB read reduction.
  Status polling at 2-second intervals gets 80% cache hits.

- **CORS:** Required for mobile app to call API. Allow all origins for local dev,
  restrict in production.

- **Error responses:** Use consistent JSON format:
  ```json
  {"error": "job not found", "code": "NOT_FOUND"}
  ```
