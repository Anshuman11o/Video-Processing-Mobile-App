# Stage 3A: Validate Worker

> Status: **draft — needs review before implementation.**
> Four decisions marked **[DECIDE]** below change the shape of the code and
> should be settled first.

## Aim

Stand up the first pipeline worker: consume from `dayreel-validate`, probe the
uploaded video with `ffprobe`, remux it to a faststart MP4 in `dayreel-processed`,
record the stage result in DynamoDB, and hand off to `dayreel-extract`.

This is the stage that proves the whole worker pattern — SQS consume → S3 in →
ffmpeg → S3 out → DynamoDB → SQS next. Stages 4A/5A/6A are variations on it, so
the shared scaffolding built here matters more than the validation logic itself.

## Components

| Component | Action |
|-----------|--------|
| `backend/cmd/worker/` | Create — single entry point, stage chosen by `WORKER_STAGE` |
| `backend/internal/queue/` | Create — SQS wrapper (receive/delete/publish), does not exist yet |
| `backend/internal/worker/` | Create — shared consume loop + stage interface |
| `backend/internal/worker/validate/` | Create — ffprobe + remux logic |
| `backend/internal/media/` | Create — thin `ffprobe`/`ffmpeg` exec wrappers |
| `backend/internal/db/dynamodb.go` | Modify — add per-stage state updates |
| `backend/internal/storage/s3.go` | Modify — add download/upload of whole objects |
| `backend/Dockerfile.worker` | Create — needs ffmpeg, unlike the API image |
| `infra/docker-compose.yml` | Modify — add `worker-validate` service |
| `infra/localstack/init-aws.sh` | Modify — see **[DECIDE 2]** on the `.mp4` filter |
| `mobile/src/types/api.ts` | Modify — status vocabulary is wrong, see **[DECIDE 4]** |

## Boundaries

### Inbound: two message shapes, not one

This is the most important thing to settle before writing code.

`init-aws.sh` wires an S3 event notification from `dayreel-raw-videos` directly
into `dayreel-validate`. So the **first** message this worker sees is an S3 Event
Notification envelope — *not* the `events.StageMessage` that `internal/events`
defines and that every downstream stage will receive:

```json
{
  "Records": [{
    "eventName": "ObjectCreated:CompleteMultipartUpload",
    "s3": {
      "bucket": {"name": "dayreel-raw-videos"},
      "object": {"key": "550e8400-.../clip.mp4", "size": 15000000}
    }
  }]
}
```

versus the canonical internal shape:

```json
{
  "job_id": "550e8400-...",
  "stage": "validate",
  "input": {"bucket": "dayreel-raw-videos", "key": "550e8400-.../clip.mp4"},
  "attempt": 1,
  "timestamp": "2026-08-12T10:30:00Z",
  "trace_id": "..."
}
```

Note the S3 envelope carries no `job_id`, no `trace_id`, no `attempt`. `job_id`
is recoverable as the first path segment of the key, because the API writes
`<job_id>/<filename>` (`backend/internal/api/handlers.go:103`).

**[DECIDE 1] — how to reconcile these.**

- **(a) Worker accepts both, normalizes to `StageMessage` at the edge.**
  *Recommended.* Keeps the S3 notification as the trigger, so there is no
  dual-write: the API completing the multipart upload *is* the event, and a job
  cannot be lost if the API crashes right after `CompleteMultipartUpload`. Cost
  is one `normalizeMessage()` function and a synthesized `trace_id` for
  S3-triggered work.
- **(b) Drop the S3 notification; API publishes `StageMessage` on
  `POST /jobs/{id}/complete`.** Uniform shape everywhere, real `trace_id` from
  the start. But it introduces a dual-write — S3 complete succeeds, SQS publish
  fails, job is stranded in `processing` with no retry path.
- **(c) Both.** Duplicate triggering; needs the idempotency guard below to be
  airtight. Not worth it yet.

I recommend **(a)**, and that `normalizeMessage` lives in `internal/events`
alongside the type it produces, so stages 4A–6A never see the S3 shape.

### Outbound: `StageMessage` to `dayreel-extract`

```json
{
  "job_id": "550e8400-...",
  "stage": "extract",
  "input": {"bucket": "dayreel-processed", "key": "550e8400-.../validated.mp4"},
  "attempt": 1,
  "timestamp": "2026-08-12T10:31:00Z",
  "trace_id": "..."
}
```

Built via the existing `events.NewStageMessage` + `events.NextStage` /
`events.NextQueue` helpers, which already encode validate → extract.

### S3 Objects

| Bucket | Key Pattern | Content |
|--------|-------------|---------|
| `dayreel-raw-videos` | `{job_id}/{original_filename}` | Original upload (input) |
| `dayreel-processed` | `{job_id}/validated.mp4` | Faststart-remuxed MP4 (output) |

### DynamoDB

Updates the `stages.validate` map entry on the existing `JOB#{id}` / `METADATA`
item. No new items, no new attributes at the top level.

```json
{
  "stages": {
    "validate": {
      "status": "running",
      "started_at": "2026-08-12T10:30:05Z",
      "completed_at": null,
      "attempts": 1,
      "error": "",
      "output_key": ""
    }
  }
}
```

On success: `status: "completed"`, `completed_at` set, `output_key:
"{job_id}/validated.mp4"`. On permanent failure: `status: "failed"`, `error` set,
and the parent `status` flips to `failed`.

**Vocabulary is `pending | running | completed | failed`** — from
`backend/internal/models/job.go:25-29`. Not `processing`/`complete`. See
**[DECIDE 4]**.

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/cmd/worker/main.go` | Create | Entry point; reads `WORKER_STAGE`, wires deps, runs loop |
| `backend/internal/queue/sqs.go` | Create | Receive (long-poll), delete, publish, queue-URL resolution |
| `backend/internal/worker/worker.go` | Create | Consume loop, `Stage` interface, error classification |
| `backend/internal/worker/validate/validate.go` | Create | Probe → gate → remux → upload |
| `backend/internal/media/ffprobe.go` | Create | `ffprobe` exec + JSON parse into a typed struct |
| `backend/internal/media/ffmpeg.go` | Create | `ffmpeg` exec wrapper with context cancellation |
| `backend/internal/storage/s3.go` | Modify | `DownloadToFile`, `UploadFile`, `ObjectExists` |
| `backend/internal/db/dynamodb.go` | Modify | `SetStageRunning`, `SetStageCompleted`, `SetStageFailed` |
| `backend/internal/events/messages.go` | Modify | `NormalizeMessage` for the S3 envelope (per **[DECIDE 1a]**) |
| `backend/Dockerfile.worker` | Create | ffmpeg-bearing image |
| `infra/docker-compose.yml` | Modify | `worker-validate` service |
| `infra/localstack/init-aws.sh` | Modify | Notification filter (**[DECIDE 2]**) |
| `mobile/src/types/api.ts` | Modify | Align status strings with Go (**[DECIDE 4]**) |

## Tasks

1. [ ] Settle the four **[DECIDE]** items
2. [ ] `internal/queue`: SQS wrapper — long-poll receive (20s), delete, publish
3. [ ] `internal/storage`: `DownloadToFile`, `UploadFile`, `ObjectExists`
4. [ ] `internal/db`: per-stage update methods with `attempts` increment
5. [ ] `internal/media`: `ffprobe` JSON parse + `ffmpeg` exec, both context-aware
6. [ ] `internal/worker`: `Stage` interface + consume loop with error classification
7. [ ] `internal/worker/validate`: idempotency check → probe → gate → remux → upload
8. [ ] `cmd/worker/main.go`: wire it, `WORKER_STAGE=validate`, graceful shutdown
9. [ ] `Dockerfile.worker` + compose service
10. [ ] Adjust S3 notification filter per **[DECIDE 2]**
11. [ ] Fix mobile status vocabulary per **[DECIDE 4]**
12. [ ] End-to-end test below

## Implementation Plan

Build order follows the dependency graph — each step compiles on its own, so
`go build ./...` stays green throughout rather than only at the end.

### Step 1 — `internal/queue/sqs.go` (no dependencies)

```go
type Message struct {
    Body          string
    ReceiptHandle string
    ReceiveCount  int   // from ApproximateReceiveCount; feeds StageMessage.Attempt
}

func New(ctx context.Context, cfg *config.Config) (*Client, error)
func (c *Client) QueueURL(ctx context.Context, name string) (string, error)  // cached
func (c *Client) Receive(ctx context.Context, queue string) ([]Message, error)
func (c *Client) Delete(ctx context.Context, queue, receiptHandle string) error
func (c *Client) Publish(ctx context.Context, queue string, body any) error
```

`Receive` long-polls at `WaitTimeSeconds: 20`, `MaxNumberOfMessages: 1`, and
requests the `ApproximateReceiveCount` attribute. Long-polling is what keeps the
loop from burning CPU; a bare receive returns instantly and spins.

LocalStack config mirrors the existing S3/DynamoDB clients — `BaseEndpoint` plus
static `test`/`test` credentials when `USE_LOCALSTACK`.

### Step 2 — `internal/storage/s3.go` (refactor + additions)

**Blocking refactor:** `S3Client` binds one bucket at construction
(`s3.go:62`, `bucket: cfg.S3RawBucket`) and every method uses `s.bucket`. The
worker reads from `dayreel-raw-videos` and writes to `dayreel-processed`, so the
bucket has to become a parameter.

Least-disruptive path: keep `s.bucket` as the default for the existing multipart
methods the API calls, and add bucket-explicit methods for the worker.

```go
func (s *S3Client) DownloadToFile(ctx context.Context, bucket, key, destPath string) error
func (s *S3Client) UploadFile(ctx context.Context, bucket, key, srcPath, contentType string) error
func (s *S3Client) ObjectExists(ctx context.Context, bucket, key string) (bool, error)
```

`ObjectExists` uses `HeadObject` and must treat a `*types.NotFound` /
`smithy.APIError` with code `NotFound` as `(false, nil)` — not an error. Getting
this wrong makes the idempotency guard fail open (reprocesses everything) or
closed (skips everything).

ffmpeg needs a real seekable file, so download to a temp path rather than
streaming; `defer os.Remove`.

### Step 3 — `internal/db/dynamodb.go` (additions)

```go
func (d *DynamoDBClient) SetStageRunning(ctx context.Context, jobID string, stage models.StageName) error
func (d *DynamoDBClient) SetStageCompleted(ctx context.Context, jobID string, stage models.StageName, outputKey string) error
func (d *DynamoDBClient) SetStageFailed(ctx context.Context, jobID string, stage models.StageName, errMsg string) error
```

These update a nested map, so the stage name is an expression-attribute *name*,
not a literal:

```
UpdateExpression: "SET stages.#stg.#st = :st, stages.#stg.started_at = :now,
                   updated_at = :now ADD stages.#stg.attempts :one"
ExpressionAttributeNames:  {"#stg": "validate", "#st": "status"}
```

`ADD` for `attempts` rather than a read-modify-write, so concurrent redeliveries
cannot lose an increment.

This works only because `models.NewJob` pre-creates all four stage entries
(`job.go:134-141`) — `SET` on a path under a missing map key fails with
`ValidationException`. If job creation ever stops seeding stages, these break.

`SetStageFailed` also sets the top-level `status` to `failed` in the same call.

### Step 4 — `internal/media/`

```go
type ProbeResult struct {
    DurationSec            float64
    VideoCodec, AudioCodec string
    Width, Height          int
    HasVideo, HasAudio     bool
}

func Probe(ctx context.Context, path string) (*ProbeResult, error)
func RemuxFaststart(ctx context.Context, inPath, outPath string) error
```

- `ffprobe -v error -print_format json -show_format -show_streams <path>`,
  unmarshalled into a private struct and flattened into `ProbeResult`.
- `ffmpeg -v error -y -i <in> -c copy -movflags +faststart -f mp4 <out>`.

Both via `exec.CommandContext` so cancellation kills the child. Capture stderr
and attach it to the error — a bare "exit status 1" is unactionable.

### Step 5 — `internal/worker/worker.go`

```go
type Stage interface {
    Name() models.StageName
    Process(ctx context.Context, msg *events.StageMessage) (outputKey string, err error)
}

type PermanentError struct { Reason string; Err error }
func Permanent(reason string, err error) error   // checked with errors.As
```

The runner loop, once per message:

1. `events.NormalizeMessage(body)` → `*StageMessage` (see **[DECIDE 1]**).
   A parse failure is *permanent* — redelivering unparseable JSON changes nothing.
2. `db.SetStageRunning` — increments `attempts`.
3. Idempotency: `ObjectExists(processed, "<job_id>/validated.mp4")`. If present,
   skip to step 5 — **still publishing downstream**, because a crash between
   upload and publish would otherwise stall the pipeline permanently.
4. `stage.Process(ctx, msg)`.
5. On success: `SetStageCompleted` → publish to `events.NextQueue(stage)` →
   delete message. In that order: publishing before recording risks the next
   stage racing ahead of a job whose state says otherwise.
6. On `PermanentError`: `SetStageFailed`, **delete** the message. Do not let it
   retry 3× and land in the DLQ carrying no new information.
7. On any other error: log and **return without deleting**. The visibility
   timeout expires and SQS redelivers with its own backoff.

Steps 5–7 are the whole point of the stage; 4A–6A reuse this runner unchanged.

### Step 6 — `internal/worker/validate/validate.go`

```go
type Limits struct {
    MaxDuration        time.Duration
    AllowedVideoCodecs []string
    AllowedAudioCodecs []string
}
```

`Process`: download → `Probe` → gate against `Limits` → `RemuxFaststart` →
upload to `dayreel-processed/<job_id>/validated.mp4` → return that key.

Every gate rejection returns `Permanent(...)`: no video stream, duration over
limit, codec outside the allowlist (pending **[DECIDE 3]**). Download and upload
failures stay transient.

### Step 7 — `cmd/worker/main.go`

Reads `WORKER_STAGE`, selects from a `map[models.StageName]worker.Stage`, and
fails fast on an unknown value. Same graceful-shutdown shape as `cmd/api`:
`signal.NotifyContext`, cancel the context, let the in-flight message finish or
be redelivered.

### Step 8 — Docker

`backend/Dockerfile.worker` — same Go builder stage as the API, but the runtime
layer is `alpine` + `apk add --no-cache ffmpeg` (pulls in `ffprobe`). Compose
gets a `worker-validate` service with `WORKER_STAGE=validate`, gated on the same
LocalStack and Redis healthchecks as `api`.

## Test

```bash
cd infra && docker-compose up -d --build

# Upload through the real API path so the S3 notification fires naturally
JOB=$(curl -s -X POST localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{"filename":"test.mp4","size_bytes":1048576,"content_type":"video/mp4"}')
JOB_ID=$(echo "$JOB" | jq -r .job_id)
URL=$(echo "$JOB" | jq -r '.upload_urls[0].url')

ETAG=$(curl -s -X PUT --upload-file test.mp4 "$URL" -D - -o /dev/null \
  | grep -i '^etag:' | tr -d '\r' | cut -d' ' -f2)

curl -s -X POST "localhost:8080/jobs/$JOB_ID/complete" \
  -H 'Content-Type: application/json' \
  -d "{\"upload_id\":\"$(echo "$JOB" | jq -r .upload_id)\",
       \"parts\":[{\"part_number\":1,\"etag\":$ETAG}]}"

sleep 15   # >10s, to outlast the API's Redis cache TTL

curl -s "localhost:8080/jobs/$JOB_ID" | jq '.stages.validate'
aws --endpoint-url=http://localhost:4566 s3 ls "s3://dayreel-processed/$JOB_ID/"
aws --endpoint-url=http://localhost:4566 sqs receive-message \
  --queue-url http://localhost:4566/000000000000/dayreel-extract | jq -r '.Messages[0].Body'
```

Expected: `stages.validate.status == "completed"` with a non-empty `output_key`;
`validated.mp4` present in `dayreel-processed`; one `StageMessage` on
`dayreel-extract` with `stage == "extract"`.

## Verification

- [ ] `worker-validate` starts and long-polls without busy-spinning the CPU
- [ ] A completed upload triggers validate with no manual SQS publish
- [ ] `stages.validate` goes `pending → running → completed`, `attempts == 1`
- [ ] `dayreel-processed/{job_id}/validated.mp4` exists and is faststart
      (`ffprobe -v error -show_entries format=start_time` returns promptly;
      `moov` precedes `mdat`)
- [ ] Exactly one message lands on `dayreel-extract`
- [ ] Message is deleted from `dayreel-validate` (queue depth returns to 0)
- [ ] **Idempotency:** replaying the same message leaves one output object,
      does not double-publish to extract, and does not bump `attempts`
- [ ] **Permanent failure:** a text file renamed `.mp4` marks the stage `failed`
      with a readable error, flips the job to `failed`, deletes the message, and
      does **not** reach the DLQ
- [ ] **Transient failure:** with LocalStack stopped mid-job, the message returns
      to the queue and succeeds on retry once LocalStack is back
- [ ] After 3 genuine transient failures the message lands in `dayreel-dlq`
- [ ] Worker exits cleanly on SIGTERM without orphaning an in-flight ffmpeg

## Notes

### [DECIDE 2] — the `.mp4` suffix filter silently strands jobs

The notification filters on `suffix: ".mp4"`, but the API accepts any filename
and writes the key as `<job_id>/<original_filename>`. Upload a `.mov` — which the
API happily issues presigned URLs for — and the notification never fires. The job
sits in `processing` forever with no error, no DLQ entry, nothing to alert on.
This is the failure mode I'd most expect to lose time to later.

Options: **(a)** drop the suffix filter entirely and let ffprobe be the gate —
validating the file *is* this worker's job, and a non-video simply fails cleanly;
**(b)** normalize the upload key to `<job_id>/input<ext>` in the API and filter on
the `<job_id>/input` prefix instead. I lean **(a)** for this stage — it is one
line and removes a whole class of silent stall — with (b) as a follow-up if we
want key layout to be predictable for the later stages.

Either way, a job stuck in `processing` with no stage running is invisible today.
A staleness check is worth its own small stage.

### [DECIDE 3] — non-h264 input: reject or transcode?

`-c copy` remuxing only works if the codec is already MP4-compatible. For a
VP9/AV1 input the remux fails. Rejecting is cheap and honest for a stage named
"validate"; transcoding is correct but turns a ~1s stage into a minute-plus one
and blows past the 300s visibility timeout for longer clips.

Recommend: **reject** anything outside an h264/hevc + aac/mp3 allowlist as a
permanent failure for 3A, and revisit if real inputs demand it. The gate belongs
in one place so it is easy to loosen later.

### [DECIDE 4] — status vocabulary drift, and it is already wrong in the app

Three vocabularies are in play:

| Source | Stage values |
|---|---|
| `backend/internal/models/job.go` (authoritative) | `pending`, `running`, `completed`, `failed` |
| `mobile/src/types/api.ts` (Stage 2B) | `pending`, `processing`, `complete`, `failed`, `skipped` |
| `PROJECT_PLAN.md` / `TEMPLATE.md` | `pending`, `processing`, `complete`, `failed` |

The mobile types are mine from Stage 2B and they are wrong in one specific,
easy-to-miss place. `JobListScreen.tsx:27` counts completed stages with
`s.status === 'complete'` against a Go value of `"completed"`, so once real data
arrives every job renders **0/4 stages forever** with a progress bar stuck at
zero — while the job-level check on line 106 (`item.status === 'completed'`) is
correct and navigation still works. So the screen looks alive and merely reports
no progress, which reads far more like a broken worker than a broken string.

It typechecks because both sides are string-literal unions that never meet at
compile time. The `skipped` stage status in the mobile union has no Go
counterpart either, and `JobStatus` there is missing `pending`.

Recommend: Go stays authoritative, fix `mobile/src/types/api.ts` to match, and
correct the two docs. Worth doing as part of 3A rather than later — this is
exactly the bug that gets diagnosed as "the worker isn't updating DynamoDB."

### Error classification is the load-bearing decision

The retry policy (`maxReceiveCount: 3` → DLQ) only helps if the worker
distinguishes:

- **Permanent** (corrupt file, disallowed codec, over-duration): record
  `failed`, flip the job to `failed`, **delete the message**. Retrying is
  pointless and burns three visibility timeouts before the DLQ tells us nothing
  new.
- **Transient** (S3 timeout, DynamoDB throttle, LocalStack restart): do **not**
  delete; let the visibility timeout expire so SQS redelivers with backoff.

Getting this backwards in either direction is bad: permanent-as-transient wastes
15 minutes per bad file, transient-as-permanent fails jobs that would have
succeeded. A typed `PermanentError` wrapper checked with `errors.As` is the
cheapest way to keep this honest.

### Idempotency

Per the design invariant, check `dayreel-processed/{job_id}/validated.mp4` before
doing any work and short-circuit if present. SQS is at-least-once and the S3
notification can also double-fire, so this will trigger in practice, not just in
theory. Note the short-circuit path must still publish to `dayreel-extract` —
otherwise a redelivery after a crash between "upload output" and "publish next"
leaves the pipeline stalled.

### Visibility timeout vs. ffmpeg runtime

Queue visibility is 300s. A `-c copy` remux is seconds even for large files, so
there is headroom — but only while we are not transcoding (**[DECIDE 3]**). If
transcoding ever lands, the worker needs to extend visibility on a heartbeat
rather than assume it finishes in time.

### The worker image is not the API image

`backend/Dockerfile` is `alpine` with no ffmpeg. The worker needs its own image;
`jrottenberg/ffmpeg` or `alpine` + `apk add ffmpeg` both work. Keeping them
separate keeps the API image small.

### Redis cache masks worker progress for 10s

`GET /jobs/{id}` serves from a 10s Redis cache and the worker writes straight to
DynamoDB, so a poll right after a stage completes can show stale state. Harmless
in production, actively confusing while testing — hence the `sleep 15` above.
The worker could invalidate `job:<id>` after each write; that couples the worker
to the cache, so I would rather leave it and remember the TTL.

### Deferred

- Structured logging / trace propagation (`trace_id` is carried but unused)
- Metrics (`worker.duration_ms` per PROJECT_PLAN)
- Stall detection for jobs with no running stage
- Concurrency: one message at a time to start; parallelism after it works
