# Stage 1A: Data Schemas (DynamoDB + SQS)

> **Run in parallel with:** Stage 1B (Docker/LocalStack)
> **Estimated time:** 30 minutes
> **Blocks:** Stage 2A (Go API), all workers

## Aim

Lock down DynamoDB table schema and SQS message contracts before writing any
application code. These are the hardest things to change later.

---

## Components Touched

| Component | Action | Files |
|-----------|--------|-------|
| `backend/internal/models/` | Create | `job.go`, `stage.go` |
| `backend/internal/events/` | Create | `messages.go` |
| `backend/` | Create | `go.mod`, `go.sum` |

---

## DynamoDB Schema Design

### Table: `dayreel-jobs`

**Design choice:** Single-table design. One item per job. All stage states embedded
in a `stages` map. This gives us full job status in one `GetItem` call.

#### Primary Key

| Attribute | Type | Value |
|-----------|------|-------|
| `pk` | String | `JOB#{job_id}` |
| `sk` | String | `JOB#{job_id}` |

**Why composite key if pk=sk?** Future-proofs for adding related items (e.g.,
`sk=STAGE#validate` for stage-specific history) without table redesign.

#### Attributes

```json
{
  "pk": "JOB#550e8400-e29b-41d4-a716-446655440000",
  "sk": "JOB#550e8400-e29b-41d4-a716-446655440000",
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "filename": "beach-sunset.mp4",
  "size_bytes": 15728640,
  "content_type": "video/mp4",
  "status": "processing",
  "created_at": "2024-01-15T10:30:00Z",
  "updated_at": "2024-01-15T10:35:00Z",

  "upload": {
    "upload_id": "s3-multipart-upload-id",
    "bucket": "dayreel-raw-videos",
    "key": "550e8400-e29b-41d4-a716-446655440000/input.mp4",
    "part_size": 5242880,
    "total_parts": 3,
    "completed_at": null
  },

  "stages": {
    "validate": {
      "status": "pending",
      "started_at": null,
      "completed_at": null,
      "attempts": 0,
      "error": null,
      "output_key": null
    },
    "extract": {
      "status": "pending",
      "started_at": null,
      "completed_at": null,
      "attempts": 0,
      "error": null,
      "output_key": null
    },
    "transcribe": {
      "status": "pending",
      "started_at": null,
      "completed_at": null,
      "attempts": 0,
      "error": null,
      "output_key": null
    },
    "package": {
      "status": "pending",
      "started_at": null,
      "completed_at": null,
      "attempts": 0,
      "error": null,
      "output_key": null
    }
  },

  "output": {
    "hls_url": null,
    "duration_seconds": null,
    "thumbnail_url": null
  },

  "metrics": {
    "upload_duration_ms": null,
    "total_processing_ms": null
  }
}
```

#### Access Patterns

| Pattern | Operation | Key Condition |
|---------|-----------|---------------|
| Get job by ID | `GetItem` | `pk = JOB#{id}, sk = JOB#{id}` |
| Update stage status | `UpdateItem` | Same, SET `stages.{stage}.status` |
| Mark job complete | `UpdateItem` | Same, SET `status`, `output` |
| List recent jobs (optional) | GSI on `created_at` | Defer until needed |

#### Item Size Estimate

- Base attributes: ~500 bytes
- Stages map (4 stages): ~400 bytes
- Total: ~1KB per job (well under 400KB limit)

---

## SQS Message Schemas

### Queue Architecture

| Queue | Purpose | DLQ |
|-------|---------|-----|
| `dayreel-validate` | Trigger validate worker | `dayreel-dlq` |
| `dayreel-extract` | Trigger extract worker | `dayreel-dlq` |
| `dayreel-transcribe` | Trigger transcribe worker | `dayreel-dlq` |
| `dayreel-package` | Trigger package worker | `dayreel-dlq` |
| `dayreel-dlq` | Failed messages | — |

### Message Format (All Stages)

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "stage": "validate",
  "input": {
    "bucket": "dayreel-raw-videos",
    "key": "550e8400-e29b-41d4-a716-446655440000/input.mp4"
  },
  "attempt": 1,
  "timestamp": "2024-01-15T10:30:00Z",
  "trace_id": "abc123"
}
```

### Stage-Specific Input/Output

#### Validate
- **Input:** `raw-videos/{job_id}/input.mp4`
- **Output:** `processed/{job_id}/validated.mp4` (faststart remux)
- **Next message:** Sends to `dayreel-extract` queue

#### Extract
- **Input:** `processed/{job_id}/validated.mp4`
- **Output:**
  - `processed/{job_id}/frames/frame_001.jpg` ... `frame_N.jpg`
  - `processed/{job_id}/audio.wav`
- **Next message:** Sends to `dayreel-transcribe` queue

#### Transcribe
- **Input:** `processed/{job_id}/extract.json` (the extract manifest; the audio
  key is read from it). Corrected 2026-08-12 — stage 4A made the manifest its
  canonical output, so the transcribe message points at that rather than
  directly at the audio.
- **Output:** `processed/{job_id}/transcript.vtt`
- **Next message:** Sends to `dayreel-package` queue

#### Package
- **Input:**
  - `processed/{job_id}/validated.mp4`
  - `processed/{job_id}/transcript.vtt`
- **Output:**
  - `hls-output/{job_id}/master.m3u8`
  - `hls-output/{job_id}/720p/playlist.m3u8`
  - `hls-output/{job_id}/720p/segment_000.ts` ...
  - `hls-output/{job_id}/480p/...`
  - `hls-output/{job_id}/360p/...`
- **Next message:** None (terminal stage)

---

## S3 Bucket Structure

| Bucket | Purpose | Lifecycle |
|--------|---------|-----------|
| `dayreel-raw-videos` | Original uploads | Delete after 7 days |
| `dayreel-processed` | Intermediate artifacts | Delete after 7 days |
| `dayreel-hls-output` | Final HLS output | Keep indefinitely |

### Key Patterns

```
dayreel-raw-videos/
  {job_id}/
    input.mp4

dayreel-processed/
  {job_id}/
    validated.mp4
    frames/
      frame_001.jpg
      frame_002.jpg
      ...
    audio.wav
    transcript.vtt

dayreel-hls-output/
  {job_id}/
    master.m3u8
    720p/
      playlist.m3u8
      segment_000.ts
      segment_001.ts
      ...
    480p/
      ...
    360p/
      ...
    subtitles.vtt
```

---

## Go Type Definitions

### `backend/internal/models/job.go`

```go
package models

import "time"

type JobStatus string

const (
    JobStatusPending    JobStatus = "pending"
    JobStatusUploading  JobStatus = "uploading"
    JobStatusProcessing JobStatus = "processing"
    JobStatusComplete   JobStatus = "complete"
    JobStatusFailed     JobStatus = "failed"
)

type StageStatus string

const (
    StageStatusPending    StageStatus = "pending"
    StageStatusProcessing StageStatus = "processing"
    StageStatusComplete   StageStatus = "complete"
    StageStatusFailed     StageStatus = "failed"
)

type StageName string

const (
    StageValidate   StageName = "validate"
    StageExtract    StageName = "extract"
    StageTranscribe StageName = "transcribe"
    StagePackage    StageName = "package"
)

type StageState struct {
    Status      StageStatus `dynamodbav:"status" json:"status"`
    StartedAt   *time.Time  `dynamodbav:"started_at,omitempty" json:"started_at,omitempty"`
    CompletedAt *time.Time  `dynamodbav:"completed_at,omitempty" json:"completed_at,omitempty"`
    Attempts    int         `dynamodbav:"attempts" json:"attempts"`
    Error       *string     `dynamodbav:"error,omitempty" json:"error,omitempty"`
    OutputKey   *string     `dynamodbav:"output_key,omitempty" json:"output_key,omitempty"`
}

type UploadInfo struct {
    UploadID    string     `dynamodbav:"upload_id" json:"upload_id"`
    Bucket      string     `dynamodbav:"bucket" json:"bucket"`
    Key         string     `dynamodbav:"key" json:"key"`
    PartSize    int64      `dynamodbav:"part_size" json:"part_size"`
    TotalParts  int        `dynamodbav:"total_parts" json:"total_parts"`
    CompletedAt *time.Time `dynamodbav:"completed_at,omitempty" json:"completed_at,omitempty"`
}

type OutputInfo struct {
    HLSURL          *string `dynamodbav:"hls_url,omitempty" json:"hls_url,omitempty"`
    DurationSeconds *int    `dynamodbav:"duration_seconds,omitempty" json:"duration_seconds,omitempty"`
    ThumbnailURL    *string `dynamodbav:"thumbnail_url,omitempty" json:"thumbnail_url,omitempty"`
}

type Metrics struct {
    UploadDurationMs    *int64 `dynamodbav:"upload_duration_ms,omitempty" json:"upload_duration_ms,omitempty"`
    TotalProcessingMs   *int64 `dynamodbav:"total_processing_ms,omitempty" json:"total_processing_ms,omitempty"`
}

type Job struct {
    PK          string                  `dynamodbav:"pk"`
    SK          string                  `dynamodbav:"sk"`
    JobID       string                  `dynamodbav:"job_id" json:"job_id"`
    Filename    string                  `dynamodbav:"filename" json:"filename"`
    SizeBytes   int64                   `dynamodbav:"size_bytes" json:"size_bytes"`
    ContentType string                  `dynamodbav:"content_type" json:"content_type"`
    Status      JobStatus               `dynamodbav:"status" json:"status"`
    CreatedAt   time.Time               `dynamodbav:"created_at" json:"created_at"`
    UpdatedAt   time.Time               `dynamodbav:"updated_at" json:"updated_at"`
    Upload      UploadInfo              `dynamodbav:"upload" json:"upload"`
    Stages      map[StageName]StageState `dynamodbav:"stages" json:"stages"`
    Output      OutputInfo              `dynamodbav:"output" json:"output"`
    Metrics     Metrics                 `dynamodbav:"metrics" json:"metrics"`
}

// NewJob creates a job with initialized stage states
func NewJob(jobID, filename string, sizeBytes int64) *Job {
    now := time.Now().UTC()
    return &Job{
        PK:          "JOB#" + jobID,
        SK:          "JOB#" + jobID,
        JobID:       jobID,
        Filename:    filename,
        SizeBytes:   sizeBytes,
        ContentType: "video/mp4",
        Status:      JobStatusPending,
        CreatedAt:   now,
        UpdatedAt:   now,
        Stages: map[StageName]StageState{
            StageValidate:   {Status: StageStatusPending},
            StageExtract:    {Status: StageStatusPending},
            StageTranscribe: {Status: StageStatusPending},
            StagePackage:    {Status: StageStatusPending},
        },
    }
}
```

### `backend/internal/events/messages.go`

```go
package events

import "time"

type StageMessage struct {
    JobID     string    `json:"job_id"`
    Stage     string    `json:"stage"`
    Input     S3Ref     `json:"input"`
    Attempt   int       `json:"attempt"`
    Timestamp time.Time `json:"timestamp"`
    TraceID   string    `json:"trace_id,omitempty"`
}

type S3Ref struct {
    Bucket string `json:"bucket"`
    Key    string `json:"key"`
}

// Queue names
const (
    QueueValidate   = "dayreel-validate"
    QueueExtract    = "dayreel-extract"
    QueueTranscribe = "dayreel-transcribe"
    QueuePackage    = "dayreel-package"
    QueueDLQ        = "dayreel-dlq"
)

// Bucket names
const (
    BucketRawVideos = "dayreel-raw-videos"
    BucketProcessed = "dayreel-processed"
    BucketHLSOutput = "dayreel-hls-output"
)

// NextQueue returns the queue for the next stage, or empty if terminal
func NextQueue(currentStage string) string {
    switch currentStage {
    case "validate":
        return QueueExtract
    case "extract":
        return QueueTranscribe
    case "transcribe":
        return QueuePackage
    default:
        return "" // package is terminal
    }
}
```

---

## Tasks

1. [ ] Initialize Go module: `go mod init github.com/user/dayreel/backend`
2. [ ] Create `backend/internal/models/job.go` with types above
3. [ ] Create `backend/internal/events/messages.go` with types above
4. [ ] Add AWS SDK dependency: `go get github.com/aws/aws-sdk-go-v2`
5. [ ] Create `backend/internal/models/repository.go` with DynamoDB operations stub
6. [ ] Write unit tests for `NewJob()` and JSON marshaling
7. [ ] Create `CONTEXT.md` for `backend/internal/models/` and `backend/internal/events/`

---

## Test

```bash
cd backend
go mod tidy
go test ./internal/models/... ./internal/events/...
```

All tests pass, types compile, JSON marshaling works correctly.

---

## Verification Checklist

- [ ] `go build ./...` succeeds
- [ ] Job struct marshals to DynamoDB-compatible format
- [ ] Job struct marshals to JSON for API responses
- [ ] Stage message marshals to JSON for SQS
- [ ] `NewJob()` initializes all four stages with `pending` status
- [ ] Constants defined for all queue names, bucket names, stage names

---

## Claude Code Implementation Plan

### Recommended Approach: Single Agent, Direct Implementation

This stage is **schema definition only** — no external dependencies, no Docker, no
running services. Best done with focused, sequential tool calls.

### Execution Steps

```
1. Create directory structure
   Tool: Bash
   Command: mkdir -p backend/internal/{models,events}

2. Initialize Go module
   Tool: Bash
   Command: cd backend && go mod init github.com/anshumanagarwal/dayreel

3. Write job.go
   Tool: Write
   File: backend/internal/models/job.go
   Content: [Go types from above]

4. Write messages.go
   Tool: Write
   File: backend/internal/events/messages.go
   Content: [Go types from above]

5. Add AWS SDK
   Tool: Bash
   Command: cd backend && go get github.com/aws/aws-sdk-go-v2/...

6. Write basic tests
   Tool: Write
   File: backend/internal/models/job_test.go

7. Run tests
   Tool: Bash
   Command: cd backend && go test ./...

8. Create CONTEXT.md files
   Tool: Write
   Files: backend/internal/models/CONTEXT.md, backend/internal/events/CONTEXT.md
```

### Why Not Subagents?

- Small, focused scope (2 Go files + tests)
- No exploration needed — schema is defined above
- Sequential dependencies (go mod init must happen before go get)
- Fast iteration with direct tool calls

### Slash Commands to Use

- None needed — this is pure code generation from spec

### Potential Blockers

| Blocker | Resolution |
|---------|------------|
| Go not installed | `brew install go` or error and stop |
| Module name conflict | Use full GitHub path |

### Time Estimate

- With Claude Code: ~10-15 minutes
- Mostly waiting on `go get` for AWS SDK download

---

## Notes

- **No GSI for now.** If we need "list recent jobs," we'll add a GSI on `created_at`.
  Premature until we know the query pattern.

- **Single-table, single-item per job.** Simpler than separate stage items. One
  `GetItem` returns everything. Atomic updates via `UpdateItem` expressions.

- **Trace ID optional.** For OpenTelemetry correlation later. Include in messages
  but don't require it initially.
