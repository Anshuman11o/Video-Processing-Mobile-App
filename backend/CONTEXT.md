# Backend (Go API)

Go HTTP API for the DayReel video processing pipeline. Handles job creation,
presigned URL generation for direct-to-S3 uploads, upload completion, and job
status retrieval. Never touches video bytes — the phone uploads straight to S3
and the workers read from it.

## Structure

```
backend/
├── cmd/api/main.go              # Entry point, wires dependencies, starts server
├── internal/
│   ├── api/
│   │   ├── handlers.go          # HTTP handlers (CreateJob, CompleteUpload, GetJobStatus, GetReel)
│   │   ├── router.go            # gorilla/mux route setup
│   │   └── middleware.go        # Logging + CORS middleware
│   ├── cache/memory.go          # In-process TTL cache for job status (10s)
│   ├── config/config.go         # Environment-based configuration
│   ├── db/dynamodb.go           # DynamoDB CRUD for jobs (single-table design)
│   ├── events/messages.go       # Stage message types and queue/bucket constants
│   ├── models/job.go            # Job, StageState, UploadInfo data models
│   ├── queue/                   # Self-hosted SQLite queue (see its CONTEXT.md)
│   └── storage/s3.go            # S3 multipart upload + presigned URL generation
├── Dockerfile                   # Multi-stage Go build (CGO_ENABLED=0)
├── go.mod / go.sum
```

## What is and isn't AWS

S3 and DynamoDB are the only remote dependencies, and they are real AWS — there
is no emulator. The queue is a local SQLite file (`QUEUE_DB_PATH`), and the
status cache lives in the API's own memory. That means the AWS SDK's default
credential chain must resolve: environment variables, `~/.aws/credentials`, or
an instance/task role.

Because the build sets `CGO_ENABLED=0`, the SQLite driver must be the pure-Go
`modernc.org/sqlite`. A cgo binding such as `mattn/go-sqlite3` will not link.

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | /health | Health check |
| POST | /jobs | Create job, get presigned upload URLs |
| POST | /jobs/{id}/complete | Signal upload completion; enqueues the validate stage |
| GET | /jobs/{id} | Get job status (10s in-process cache, DynamoDB on miss) |
| GET | /jobs/{id}/reel | Get HLS playback URL (completed jobs only) |

## Data Flow

1. Client POSTs `/jobs` with filename + size
2. API creates the S3 multipart upload, presigns one URL per part, writes the job to DynamoDB
3. Client uploads parts **directly to S3** using those URLs — the API is not in the data path
4. Client POSTs `/jobs/{id}/complete` with the part ETags
5. API completes the S3 multipart upload and flips the job to `processing`
6. API publishes a validate `StageMessage` to the local queue — **this is what starts the pipeline**

Step 6 used to be an S3 event notification wired up by the LocalStack init
script. Real S3 cannot notify a SQLite file, so the API is now the sole trigger.
That also makes step 4 retry-safe by necessity: `CompleteMultipartUpload` is
idempotent, so a client retrying after a failed enqueue does not strand a video
that is already assembled in the bucket.

## Running

```bash
cd backend && go run ./cmd/api      # or: make api
```

Requires a `.env` with working AWS credentials (see `.env.example`). The queue
database is created on first start; `make queue-peek` shows what is in it.
