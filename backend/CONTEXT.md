# Backend (Go)

Two binaries share one module. `cmd/api` is the HTTP API — job creation,
presigned URL generation for direct-to-S3 uploads, upload completion and
resumption, job status. It never touches video bytes: the phone uploads straight
to S3 and the workers read from it. `cmd/worker` is the pipeline; one binary
serves every stage and `WORKER_STAGE` selects which.

## Structure

```
backend/
├── cmd/
│   ├── api/main.go              # API entry point, wires dependencies, starts server
│   └── worker/main.go           # Worker entry point, WORKER_STAGE picks the stage
├── internal/
│   ├── api/
│   │   ├── handlers.go          # HTTP handlers (CreateJob, ResumeUpload, CompleteUpload, AbortUpload, GetJobStatus, GetReel)
│   │   ├── router.go            # gorilla/mux route setup
│   │   └── middleware.go        # Logging + CORS middleware
│   ├── cache/memory.go          # In-process TTL cache for job status (10s)
│   ├── config/config.go         # Environment-based configuration
│   ├── db/                      # DynamoDB CRUD for jobs and stage state
│   ├── events/                  # Stage message types, queue/bucket constants, extract manifest
│   ├── media/                   # ffmpeg/ffprobe wrappers, HLS ladder, playlists
│   ├── models/job.go            # Job, StageState, UploadInfo data models
│   ├── queue/                   # Self-hosted SQLite queue (see its CONTEXT.md)
│   ├── storage/                 # S3 multipart upload, presigned URLs, whole-object I/O
│   ├── transcribe/              # whisper.cpp and the mock transcriber
│   └── worker/                  # Shared consume loop plus the four stages
├── go.mod / go.sum
```

## What is and isn't AWS

S3 and DynamoDB are the only remote dependencies, and they are real AWS — there
is no emulator. The queue is a local SQLite file (`QUEUE_DB_PATH`), and the
status cache lives in the API's own memory. That means the AWS SDK's default
credential chain must resolve: environment variables, `~/.aws/credentials`, or
an instance/task role. Nothing injects static test credentials any more.

The SQLite driver is the pure-Go `modernc.org/sqlite`, which registers itself as
`sqlite` and not `sqlite3`. A cgo binding such as `mattn/go-sqlite3` would not
link in a `CGO_ENABLED=0` build.

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | /health | Health check |
| POST | /jobs | Create job, get presigned upload URLs |
| POST | /jobs/{id}/upload-urls | Re-issue presigned URLs for the parts S3 does not hold |
| POST | /jobs/{id}/complete | Complete the upload; enqueues the validate stage |
| DELETE | /jobs/{id}/upload | Abort the upload and release the parts S3 is holding |
| GET | /jobs/{id} | Get job status (10s in-process cache, DynamoDB on miss) |
| GET | /jobs/{id}/reel | Get HLS playback URL (completed jobs only) |

## Data Flow

1. Client POSTs `/jobs` with filename + size
2. API creates the S3 multipart upload, presigns one URL per part, writes the job to DynamoDB
3. Client uploads parts **directly to S3** using those URLs — the API is not in the data path
4. Client POSTs `/jobs/{id}/complete`; the part list is optional, and is derived from `ListParts` when absent
5. API completes the S3 multipart upload and records the upload as finished
6. API publishes a validate `StageMessage` to the local queue — **this is what starts the pipeline**
7. Each worker stage consumes its queue, writes its output to S3, records stage state, and publishes the next stage

Step 6 used to be an S3 event notification wired up by the LocalStack init
script. Real S3 cannot notify a SQLite file, so the API is now the sole trigger.
That is also why a failed enqueue is a 500 with code `QUEUE_ERROR` rather than a
logged warning: nothing else will ever start that job. `CompleteMultipartUpload`
is idempotent so the retry works, and an already-complete upload arriving at
`/complete` again resumes at step 6 instead of being rejected.

## Pipeline

`validate → extract → transcribe → package`, one queue per stage. The shared
consume loop is `internal/worker/runner.go`; a stage only implements `Process`
and says where its output goes.

Delivery is at-least-once, so **every stage must be idempotent**. The runner
checks for the stage's output object before doing any work, and consults the
recorded stage state to tell a duplicate delivery apart from a crash between
uploading the output and recording it.

Retries are the runner's decision, not the broker's — there is no redrive policy
to lean on:

| Failure | What happens |
|---------|--------------|
| Permanent (`worker.Permanent`) — bad codec, corrupt file | Stage recorded failed, message dead-lettered immediately |
| Transient, budget left | `Nack` with a doubling backoff; stage stays `running` |
| Transient, `QUEUE_MAX_DELIVERIES` reached | Stage recorded failed, message dead-lettered |
| Worker crash / lost lease | Lease expires, message redelivered; the budget check dead-letters it once spent |

Stages slower than `QUEUE_VISIBILITY_TIMEOUT` heartbeat their lease every 30s.
If one runs past its lease anyway, another worker claims the message and the
first worker's `Ack` fails with `queue.ErrLeaseLost` — logged, not treated as a
stage failure.

## Running

```bash
cd backend && go run ./cmd/api
WORKER_STAGE=validate go run ./cmd/worker
```

Requires a `.env` with working AWS credentials (see `.env.example`). The queue
database and its parent directory are created on first start by whichever binary
starts first.

## Tests

```bash
cd backend && go test ./...
```

No test needs AWS, LocalStack, or a running queue: the SQLite queue tests use a
temporary file, and the presigning tests sign with credentials they put in the
environment themselves. The media tests skip when `ffmpeg` is absent.
