# Stage 4A: Extract Worker

> **Depends on:** Stage 3A (Validate Worker — establishes the worker harness)
> **Run in parallel with:** Stage 2B (Mobile Shell)
> **Estimated time:** 20–25 minutes
> **Blocks:** Stage 5A (Transcribe Worker)

## Status of This Plan

**Initial plan — written ahead of Stage 3A.** Stage 3A has not been planned or
implemented yet, and it is the stage that creates the worker runtime (SQS client,
poll loop, retry/DLQ semantics, ffmpeg base image, stage-state DynamoDB writes).
Everything in this document that touches that runtime is written against an
**assumed contract** and is flagged in
[Open Items to Confirm After Stage 3A](#open-items-to-confirm-after-stage-3a).

The stage-specific work — the ffmpeg invocations, the output key layout, the
`extract.json` manifest — does not depend on 3A and should be stable as written.

---

## Aim

Take the normalized `validated.mp4` produced by Stage 3A and extract the two
artifacts downstream stages need: **scene-detected keyframe JPEGs** (for
thumbnails) and a **16 kHz mono WAV** (for transcription), plus a manifest that
records what was produced.

---

## Components Touched

| Component | Action | Files |
|-----------|--------|-------|
| `backend/internal/worker/extract/` | Create | `extract.go`, `extract_test.go`, `CONTEXT.md` |
| `backend/internal/media/` | Modify | `ffmpeg.go` (add frame + audio extraction; created in 3A) |
| `backend/internal/storage/` | Modify | `s3.go` (multi-bucket Get/Put — see Open Items) |
| `backend/cmd/worker/main.go` | Modify | Register the extract stage handler |
| `backend/internal/config/config.go` | Modify | Add extract tunables |
| `infra/docker-compose.yml` | Modify | Add `worker-extract` service |
| `.env.example` | Modify | Document new env vars |

---

## Boundaries

### Input: SQS message on `dayreel-extract`

Emitted by the validate worker (Stage 3A). Shape is `events.StageMessage`
(unchanged from Stage 1A):

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "stage": "extract",
  "input": {
    "bucket": "dayreel-processed",
    "key": "550e8400-e29b-41d4-a716-446655440000/validated.mp4"
  },
  "attempt": 1,
  "timestamp": "2026-01-15T10:31:02Z",
  "trace_id": "abc123"
}
```

**Convention:** the message carries only the *primary* input. Every other key a
stage needs is **derived from `job_id`** (e.g. `{job_id}/extract.json`). This
keeps `StageMessage` fixed for all four stages, as locked in Stage 1A.

### Output: S3 objects

All under bucket `dayreel-processed`:

| Key | Content | Notes |
|-----|---------|-------|
| `{job_id}/frames/frame_001.jpg` … `frame_NNN.jpg` | Keyframe JPEGs, max 640px wide | Capped at 20 frames |
| `{job_id}/audio.wav` | PCM s16le, 16 kHz, mono | Omitted when source has no audio stream |
| `{job_id}/extract.json` | Manifest (below) | **Written last — acts as the completion sentinel** |

### Output: `extract.json` manifest

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "duration_seconds": 42.517,
  "width": 1080,
  "height": 1920,
  "fps": 29.97,
  "has_audio": true,
  "audio_key": "550e8400-e29b-41d4-a716-446655440000/audio.wav",
  "audio_sample_rate": 16000,
  "frame_selection": "scene",
  "frame_count": 7,
  "frames": [
    { "key": "550e8400-.../frames/frame_001.jpg", "timestamp_seconds": 0.0 },
    { "key": "550e8400-.../frames/frame_002.jpg", "timestamp_seconds": 4.837 }
  ],
  "generated_at": "2026-01-15T10:31:24Z"
}
```

`frame_selection` is one of `scene` | `interval` | `single` — records which
strategy actually produced the frames (see [Frame Selection](#frame-selection)).

**This manifest is the contract with Stages 5A and 6A:**
- 5A reads `has_audio` and `duration_seconds`
- 6A reads `frames[0].key` (thumbnail), `width`/`height` (ladder selection), and
  `duration_seconds` (subtitle playlist target duration)

### Output: SQS message to `dayreel-transcribe`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "stage": "transcribe",
  "input": {
    "bucket": "dayreel-processed",
    "key": "550e8400-e29b-41d4-a716-446655440000/audio.wav"
  },
  "attempt": 1,
  "timestamp": "2026-01-15T10:31:24Z",
  "trace_id": "abc123"
}
```

Sent even when `has_audio` is false — the key then points at an object that does
not exist, and 5A short-circuits on the manifest. Keeping the pipeline shape
uniform is worth more than saving one message.

### DynamoDB writes

| When | Update |
|------|--------|
| Handler start | `stages.extract.status = running`, `started_at`, `attempts += 1` |
| Success | `stages.extract.status = completed`, `completed_at`, `output_key = {job_id}/extract.json` |
| Permanent failure | `stages.extract.status = failed`, `error`, and job `status = failed` |
| Success | `metrics.extract_duration_ms` |

---

## Processing Logic

Working directory: `{WORKER_TMP_DIR}/{job_id}/` — created at start, removed by
`defer` on every exit path (including panic recovery in the harness).

### 1. Idempotency check

`HeadObject` on `dayreel-processed/{job_id}/extract.json`.
If present: log `skip=idempotent`, mark the stage completed, forward to
`dayreel-transcribe`, delete the message. **Do not reprocess.**

This is why the manifest is written last — its presence means every other
artifact is already durable.

### 2. Download input

`GetObject` → `{tmp}/validated.mp4`. Reject with a permanent error if the object
is missing (validate stage output should always exist by this point).

### 3. Probe

```bash
ffprobe -v error -print_format json -show_format -show_streams "{tmp}/validated.mp4"
```

Pull `format.duration`, and from the first video stream: `width`, `height`,
`avg_frame_rate`. Detect audio with:

```bash
ffprobe -v error -select_streams a:0 -show_entries stream=codec_type -of csv=p=0 "{tmp}/validated.mp4"
```

Empty output ⇒ `has_audio = false`.

### 4. Frame Selection

**Primary — scene detection:**

```bash
ffmpeg -hide_banner -nostdin -y -i "{tmp}/validated.mp4" \
  -vf "select='gt(scene,0.4)',showinfo,scale='min(640,iw)':-2" \
  -fps_mode vfr -frames:v 20 -q:v 3 \
  "{tmp}/frames/frame_%03d.jpg" 2> "{tmp}/scene.log"
```

Frame timestamps come from `showinfo`, which writes one `pts_time:<float>` per
emitted frame to stderr. Parse `scene.log` with
`regexp.MustCompile(\`pts_time:([0-9.]+)\`)` and zip the results against the
files ffmpeg wrote, in filename order.

**Fallback A — fixed interval** (scene detection produced 0 frames; common for
static/handheld clips with no cuts):

```bash
ffmpeg -hide_banner -nostdin -y -i "{tmp}/validated.mp4" \
  -vf "fps=1/5,showinfo,scale='min(640,iw)':-2" \
  -frames:v 12 -q:v 3 \
  "{tmp}/frames/frame_%03d.jpg" 2> "{tmp}/interval.log"
```

**Fallback B — single frame** (clip shorter than the interval):

```bash
ffmpeg -hide_banner -nostdin -y -ss 0 -i "{tmp}/validated.mp4" \
  -frames:v 1 -q:v 2 "{tmp}/frames/frame_001.jpg"
```

**Invariant: the stage never completes with zero frames.** Stage 6A needs
`frames[0]` for the reel thumbnail. If all three strategies fail, that is a
permanent error.

### 5. Audio extraction

Only when `has_audio`:

```bash
ffmpeg -hide_banner -nostdin -y -i "{tmp}/validated.mp4" \
  -vn -map a:0 -acodec pcm_s16le -ar 16000 -ac 1 "{tmp}/audio.wav"
```

16 kHz mono is what Whisper resamples to internally — doing it here means the
transcriber gets exactly what it wants and the object stays small (~32 KB/s, so
a 60 s clip is ~1.9 MB).

### 6. Upload

Upload frames concurrently (bounded pool, `EXTRACT_UPLOAD_CONCURRENCY`, default
4), then `audio.wav`, then `extract.json` **last**.

Content types matter for the objects the mobile app will later fetch directly:
`image/jpeg` for frames, `audio/wav` for the WAV, `application/json` for the
manifest.

### 7. Finalize

Update DynamoDB, invalidate the Redis key `job:{job_id}`, send the transcribe
message, delete the SQS message.

---

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/worker/extract/extract.go` | Create | Stage handler: probe → frames → audio → manifest → upload |
| `backend/internal/worker/extract/manifest.go` | Create | `ExtractManifest` type + marshal/unmarshal + S3 key helper |
| `backend/internal/worker/extract/extract_test.go` | Create | `showinfo` parsing, fallback selection, manifest round-trip |
| `backend/internal/worker/extract/CONTEXT.md` | Create | Component documentation |
| `backend/internal/media/ffmpeg.go` | Modify | `ExtractScenes`, `ExtractInterval`, `ExtractSingleFrame`, `ExtractAudio`, `Probe` |
| `backend/internal/storage/s3.go` | Modify | `GetObjectToFile`, `PutFile` with explicit bucket + content type |
| `backend/cmd/worker/main.go` | Modify | Register extract handler under `STAGE=extract` |
| `backend/internal/config/config.go` | Modify | `WorkerTmpDir`, `ExtractMaxFrames`, `ExtractSceneThreshold`, `ExtractUploadConcurrency` |
| `infra/docker-compose.yml` | Modify | `worker-extract` service |
| `.env.example` | Modify | New env vars |

---

## Go Sketch

```go
package extract

// Handler implements the stage handler contract from Stage 3A.
type Handler struct {
    s3     *storage.S3Client
    db     *db.DynamoDBClient
    cache  *cache.RedisClient
    media  *media.FFmpeg
    config *config.Config
}

func (h *Handler) Stage() models.StageName { return models.StageExtract }

func (h *Handler) Handle(ctx context.Context, msg *events.StageMessage) (*worker.StageResult, error) {
    // 1. idempotency: HeadObject extract.json -> skip
    // 2. download validated.mp4 to tmp
    // 3. probe -> duration, dims, fps, has_audio
    // 4. frames: scene -> interval -> single
    // 5. audio (if has_audio)
    // 6. upload frames, audio, manifest (manifest last)
    // 7. return next input
    return &worker.StageResult{
        OutputKey: manifestKey,
        NextInput: &events.S3Ref{Bucket: events.BucketProcessed, Key: audioKey},
    }, nil
}
```

### Config additions

```go
WorkerTmpDir             string // WORKER_TMP_DIR, default "/tmp/dayreel"
ExtractMaxFrames         int    // EXTRACT_MAX_FRAMES, default 20
ExtractSceneThreshold    float64 // EXTRACT_SCENE_THRESHOLD, default 0.4
ExtractIntervalSeconds   int    // EXTRACT_INTERVAL_SECONDS, default 5
ExtractUploadConcurrency int    // EXTRACT_UPLOAD_CONCURRENCY, default 4
```

---

## Failure Model

| Condition | Classification | Behavior |
|-----------|----------------|----------|
| Input object missing | Permanent | Fail stage, no retry, → DLQ |
| ffprobe reports no video stream | Permanent | Fail stage |
| ffmpeg exits non-zero on all three frame strategies | Permanent | Fail stage |
| ffmpeg fails on audio but frames succeeded | Degrade | Set `has_audio=false`, log warning, continue |
| S3 5xx / timeout | Transient | Return error, SQS redelivers (maxReceiveCount 3 → DLQ) |
| DynamoDB throttle | Transient | Return error, redeliver |
| Disk full in tmp dir | Transient | Return error; cleanup still runs via defer |

Audio degradation is deliberate: a reel with frames and no captions is still a
usable deliverable; a hard failure gives the user nothing.

---

## Docker Compose Addition

```yaml
  worker-extract:
    build:
      context: ../backend
      dockerfile: Dockerfile.worker
    container_name: dayreel-worker-extract
    environment:
      - STAGE=extract
      - AWS_REGION=us-east-1
      - AWS_ACCESS_KEY_ID=test
      - AWS_SECRET_ACCESS_KEY=test
      - LOCALSTACK_ENDPOINT=http://localstack:4566
      - USE_LOCALSTACK=true
      - S3_PROCESSED_BUCKET=dayreel-processed
      - DYNAMODB_TABLE=dayreel-jobs
      - REDIS_URL=redis:6379
      - WORKER_CONCURRENCY=2
      - WORKER_TMP_DIR=/tmp/dayreel
      - EXTRACT_MAX_FRAMES=20
    depends_on:
      localstack:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      - dayreel-network
```

`Dockerfile.worker` (with ffmpeg/ffprobe installed) comes from Stage 3A.

---

## Tasks

1. [ ] Add `Probe`, `ExtractScenes`, `ExtractInterval`, `ExtractSingleFrame`, `ExtractAudio` to `internal/media/ffmpeg.go`
2. [ ] Add `showinfo` stderr parser (`pts_time` → `[]float64`) with unit tests
3. [ ] Create `internal/worker/extract/manifest.go` with `ExtractManifest` + key helpers
4. [ ] Create `internal/worker/extract/extract.go` implementing the stage handler
5. [ ] Add multi-bucket `GetObjectToFile` / `PutFile` to `internal/storage/s3.go`
6. [ ] Add extract config knobs to `internal/config/config.go` and `.env.example`
7. [ ] Register the handler in `cmd/worker/main.go`
8. [ ] Add `worker-extract` to `infra/docker-compose.yml`
9. [ ] Write `extract_test.go` (parser, fallback ladder, manifest round-trip)
10. [ ] Create `internal/worker/extract/CONTEXT.md`
11. [ ] Run the E2E test below; append anything non-trivial to `TROUBLESHOOTING.md`

---

## Test

```bash
# Bring up infra + workers
cd infra && docker compose up -d --build worker-extract

JOB_ID="test-extract-$(date +%s)"
AWS="aws --endpoint-url=http://localhost:4566"

# Seed a validated.mp4 as if Stage 3A had produced it
$AWS s3 cp ./testdata/sample-720p.mp4 "s3://dayreel-processed/${JOB_ID}/validated.mp4"

# Hand-inject the extract message (bypasses 3A so this stage is testable alone)
$AWS sqs send-message \
  --queue-url http://localhost:4566/000000000000/dayreel-extract \
  --message-body "{\"job_id\":\"${JOB_ID}\",\"stage\":\"extract\",\"input\":{\"bucket\":\"dayreel-processed\",\"key\":\"${JOB_ID}/validated.mp4\"},\"attempt\":1,\"timestamp\":\"$(date -u +%FT%TZ)\",\"trace_id\":\"manual\"}"

sleep 15

# Artifacts
$AWS s3 ls "s3://dayreel-processed/${JOB_ID}/frames/"
$AWS s3 ls "s3://dayreel-processed/${JOB_ID}/"

# Manifest
$AWS s3 cp "s3://dayreel-processed/${JOB_ID}/extract.json" - | jq .

# Audio is 16 kHz mono
$AWS s3 cp "s3://dayreel-processed/${JOB_ID}/audio.wav" /tmp/audio.wav
ffprobe -v error -select_streams a:0 \
  -show_entries stream=sample_rate,channels,codec_name \
  -of default=nw=1 /tmp/audio.wav
# Expect: codec_name=pcm_s16le, sample_rate=16000, channels=1

# Message forwarded to the next stage
$AWS sqs receive-message \
  --queue-url http://localhost:4566/000000000000/dayreel-transcribe | jq -r '.Messages[0].Body'

# Idempotency: replay the same message, confirm no reprocessing
# (logs should show skip=idempotent, frame mtimes unchanged)
```

---

## Verification Checklist

- [ ] `s3://dayreel-processed/{job_id}/frames/` contains at least one JPEG, at most 20
- [ ] `audio.wav` probes as `pcm_s16le`, 16000 Hz, 1 channel
- [ ] `extract.json` parses and `frames[].timestamp_seconds` is strictly increasing
- [ ] `frame_selection` reports the strategy that actually ran
- [ ] A clip with no scene cuts still yields frames (`frame_selection: "interval"`)
- [ ] A video with no audio track completes with `has_audio: false` and no `audio.wav`
- [ ] `GET /jobs/{id}` shows `stages.extract.status = "completed"` with an `output_key`
- [ ] `metrics.extract_duration_ms` is populated
- [ ] A message lands on `dayreel-transcribe`
- [ ] Replaying the message skips reprocessing (idempotent)
- [ ] Temp directory under `WORKER_TMP_DIR` is empty after the run
- [ ] Malformed input sends the message to `dayreel-dlq` after 3 receives

---

## Claude Code Implementation Plan

### Approach: single agent, sequential

Small surface (2 new files + 3 modified), tight compile-test loop. No subagents —
the ffmpeg flag work needs immediate feedback from real runs against a real file,
which is faster in one context than farmed out.

### Execution order

```
1. media/ffmpeg.go extensions        (Edit)   — pure functions, testable first
2. showinfo parser + unit test       (Write)  — go test, no Docker needed
3. manifest.go                       (Write)
4. storage/s3.go multi-bucket ops    (Edit)
5. config.go + .env.example          (Edit)   — parallel with 4
6. extract.go handler                (Write)
7. cmd/worker/main.go registration   (Edit)
8. docker-compose.yml                (Edit)   — parallel with 7
9. docker compose up --build         (Bash)
10. E2E test script                  (Bash)
11. CONTEXT.md                       (Write)
```

Steps 4/5 and 7/8 are independent file writes and can be issued together.

### Potential blockers

| Blocker | Resolution |
|---------|------------|
| `-fps_mode` unrecognized | ffmpeg < 5.1 — fall back to `-vsync vfr` and pin the base image |
| `showinfo` line count ≠ file count | Filter the log to lines from the `Parsed_showinfo` filter only; trust the file list as the source of truth and pad missing timestamps by interpolation |
| Scene threshold yields 20 frames on every clip | Raise `EXTRACT_SCENE_THRESHOLD` toward 0.5; it is env-tunable for this reason |
| No test video available | Generate one: `ffmpeg -f lavfi -i testsrc=duration=30:size=1280x720:rate=30 -f lavfi -i sine=frequency=440:duration=30 -c:v libx264 -c:a aac testdata/sample-720p.mp4` |

### Time estimate

- ffmpeg helpers + parser: ~8 min
- Handler + manifest: ~7 min
- Wiring + compose: ~3 min
- Build + E2E: ~7 min
- **Total: ~25 min**

---

## Open Items to Confirm After Stage 3A

These are assumptions this plan is written against. Reconcile them before
implementing — none should change the shape of the stage, only the names things
are called.

1. **Stage handler interface.** Assumed:
   ```go
   type StageHandler interface {
       Stage() models.StageName
       Handle(ctx context.Context, msg *events.StageMessage) (*StageResult, error)
   }
   type StageResult struct {
       OutputKey string          // → stages.{stage}.output_key
       NextInput *events.S3Ref   // nil ⇒ terminal stage
   }
   ```
   If 3A instead has each handler send its own next message, drop `NextInput`
   and call the queue client directly.

2. **Transient vs permanent error signalling.** Assumed a sentinel wrapper
   (`worker.Permanent(err)`) that the harness uses to decide between
   delete-and-fail versus let-it-redeliver. Confirm the actual mechanism.

3. **`internal/queue/sqs.go` does not exist yet.** Stage 2A's plan listed it but
   it was never written — the API relies on the S3 event notification instead.
   3A must create it (`ReceiveMessages`, `DeleteMessage`,
   `ChangeMessageVisibility`, `SendMessage`, queue-name → URL resolution).

4. **`internal/storage/s3.go` is single-bucket today.** `NewS3Client` hardcodes
   `cfg.S3RawBucket` and only exposes multipart-upload methods. Workers need
   per-call bucket selection plus `GetObjectToFile` / `PutFile` / `HeadObject`.
   Confirm whether 3A refactors `S3Client` to take a bucket per call or
   introduces a separate worker-side client — this plan assumes the former.

5. **`internal/db/dynamodb.go` has no stage-level updates.** Only `CreateJob`,
   `GetJob`, `UpdateJobStatus`, `UpdateUploadInfo` exist. 3A must add
   `UpdateStageState(ctx, jobID, stage, state)` and a metrics setter; confirm
   the exact signature and whether it uses a nested `SET stages.#s = :state`
   update expression (needed for concurrent stage safety).

6. **Redis invalidation from workers.** Workers may not get a Redis client in
   3A. If they don't, stage transitions go stale for up to the 10 s cache TTL in
   `GetJobStatus`, which is acceptable — but decide explicitly rather than by
   omission.

7. **Validate's output key.** This plan assumes `{job_id}/validated.mp4` in
   `dayreel-processed` (per Stage 1A). Note that the API writes raw uploads to
   `{job_id}/{filename}`, **not** `{job_id}/input.mp4` as Stage 1A specified —
   confirm 3A's actual output key before hardcoding.

8. **Visibility timeout headroom.** Queues are created with a 300 s visibility
   timeout. Extraction on a 60 s clip should finish in ~10 s, so this is ample —
   but confirm 3A's heartbeat/extension helper exists for 5A, which needs it.

9. **`Dockerfile.worker` and its ffmpeg version.** Created in 3A. This plan
   assumes ffmpeg ≥ 5.1 for `-fps_mode`.

---

## Notes

- **`extract.json` is the completion sentinel.** Written last, checked first.
  This is the whole idempotency story for the stage — no DynamoDB read required
  to decide whether work is needed.

- **Derived keys over fatter messages.** `StageMessage` stays exactly as locked
  in Stage 1A; anything else a stage needs is computed from `job_id`. Changing
  the message schema would ripple through all four stages and the DLQ replay
  path.

- **Frames are capped at 20** for cost, not quality. 20 JPEGs at ~100 KB is
  ~2 MB per job against a 5 GB free-tier bucket. Only `frames[0]` is used
  downstream today (the 6A thumbnail); the rest are groundwork for a future
  scrubber/filmstrip UI.

- **Frames are 640px on the long edge.** Enough for a thumbnail and a filmstrip,
  small enough that mobile fetches them over a weak connection.

- **Portrait video is the common case.** These clips come from phones —
  1080×1920 is the expectation, not 1920×1080. `scale='min(640,iw)':-2` handles
  both orientations, and the `-2` keeps dimensions even for the JPEG encoder.

- **Deferred:** perceptual dedup of near-identical keyframes, and picking a
  "best" thumbnail by sharpness/face detection. Frame 1 is good enough for the
  demo.
