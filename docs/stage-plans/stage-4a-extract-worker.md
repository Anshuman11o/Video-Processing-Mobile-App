# Stage 4A: Extract Worker

> Status: **approved — ready to implement.** Decisions settled 2026-08-12; the
> four **[DECIDE]** items below now record the choice that was made and why.
> Every recommendation was accepted as written.

## Aim

Consume `dayreel-extract`, pull the transcription audio and a set of keyframes
out of `{job_id}/validated.mp4`, write them to `dayreel-processed`, and hand off
to `dayreel-transcribe`.

**This is a small stage and the plan should not pretend otherwise.** Stage 3A
built the machinery — SQS wrapper, whole-object S3 ops, per-stage DynamoDB
updates, message normalization, error classification, the consume loop, the
worker image. 4A reuses every bit of it unchanged. What is genuinely new is:

- one `worker.Stage` implementation (`internal/worker/extract`),
- two ffmpeg invocations in `internal/media`,
- one `case` in `cmd/worker/main.go`,
- one compose service.

The interesting part is not the code. It is that extract is the first stage
producing **more than one artifact**, and the `Stage` interface the runner
depends on returns exactly one key. That tension is **[DECIDE 1]** and it is the
reason this plan exists.

## Components

| Component | Action |
|-----------|--------|
| `backend/internal/worker/extract/` | Create — the only substantial new code |
| `backend/internal/media/` | Modify — add audio demux + frame extraction |
| `backend/internal/events/` | Modify — home for the extract manifest type (**[DECIDE 1]**) |
| `backend/cmd/worker/main.go` | Modify — one `case models.StageExtract` in `buildStage` |
| `infra/docker-compose.yml` | Modify — add `worker-extract` |
| `backend/internal/worker/runner.go` | **No change** — deliberately (**[DECIDE 1]**) |
| `backend/internal/queue/`, `storage/`, `db/` | **No change** — 3A left these bucket- and stage-parameterized |
| `backend/Dockerfile.worker` | **No change** — already carries ffmpeg/ffprobe |
| `infra/localstack/init-aws.sh` | **No change** — `dayreel-extract` and `dayreel-transcribe` already exist with redrive |

## Boundaries

### Inbound: one shape, unlike 3A

3A had to reconcile an S3 event envelope with a `StageMessage`
(`stage-3a-validate-worker.md` **[DECIDE 1]**). 4A does not: nothing writes S3
notifications into `dayreel-extract`, so the only producer is the validate
runner's `publishNext` (`runner.go:214-232`).

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

`events.NormalizeMessage` handles this via its `StageMessage` branch
(`normalize.go:50-63`) and overrides `attempt` with the SQS
`ApproximateReceiveCount`. Nothing to build.

Two consequences worth stating up front:

- **`trace_id` is inherited here, not synthesized.** Validate mints a fresh UUID
  for S3-triggered work; extract receives validate's. So 4A is the first place a
  trace can actually be followed across a stage hop, and the verification below
  checks it explicitly rather than assuming it.
- The runner never asserts `msg.Stage == r.stage.Name()`; it uses
  `r.stage.Name()` for every DynamoDB write and reads only `JobID`, `Input` and
  `TraceID` off the message. A misrouted message would be processed as if it
  belonged here. Not a 4A problem to fix, but noted under Risks.

### Outbound: `StageMessage` to `dayreel-transcribe`

```json
{
  "job_id": "550e8400-...",
  "stage": "transcribe",
  "input": {"bucket": "dayreel-processed", "key": "550e8400-.../extract.json"},
  "attempt": 1,
  "timestamp": "2026-08-12T10:32:11Z",
  "trace_id": "<inherited from the extract message>"
}
```

The `input.key` here is the crux of **[DECIDE 1]**. Under the recommended option
it is the manifest, not the audio.

### S3 Objects

| Bucket | Key Pattern | Content |
|--------|-------------|---------|
| `dayreel-processed` | `{job_id}/validated.mp4` | Faststart MP4 from 3A (input) |
| `dayreel-processed` | `{job_id}/audio.wav` | 16 kHz mono `pcm_s16le` (output) |
| `dayreel-processed` | `{job_id}/frames/frame_001.jpg` … `frame_NNN.jpg` | Keyframes (output) |
| `dayreel-processed` | `{job_id}/extract.json` | Manifest — the canonical single output (**[DECIDE 1]**) |

Key layout follows `stage-1a-data-schemas.md` exactly, which already specifies
`processed/{job_id}/frames/frame_001.jpg` and `processed/{job_id}/audio.wav`.
`extract.json` is the one addition.

### The manifest

```json
{
  "job_id": "550e8400-...",
  "created_at": "2026-08-12T10:32:11Z",
  "source": {"bucket": "dayreel-processed", "key": "550e8400-.../validated.mp4"},
  "duration_seconds": 42.5,
  "width": 1920,
  "height": 1080,
  "audio": {
    "present": true,
    "key": "550e8400-.../audio.wav",
    "sample_rate": 16000,
    "channels": 1,
    "codec": "pcm_s16le"
  },
  "frames": [
    {"key": "550e8400-.../frames/frame_001.jpg", "timestamp_seconds": 0.0},
    {"key": "550e8400-.../frames/frame_002.jpg", "timestamp_seconds": 7.25}
  ]
}
```

`duration_seconds`, `width` and `height` come from `media.Probe` on the
downloaded file. They are in the manifest so 5A and 6A do not each re-probe the
same object; `models.OutputInfo.DurationSeconds` (`job.go:69`) wants this value
anyway.

### DynamoDB

Updates `stages.extract` on the existing `JOB#{id}` / `METADATA` item, through
the same `SetStageRunning` / `SetStageCompleted` / `SetStageFailed` the runner
already calls. No new attributes, no schema change.

On success: `status: "completed"`, `output_key: "{job_id}/extract.json"`.

`output_key` is a single string (`job.go:53`), which is the same constraint as
the `Stage` interface expressed in the data model. It is a second argument for
the manifest: `"{job_id}/extract.json"` is a meaningful thing to record there,
`"{job_id}/audio.wav"` quietly omits half the stage's output.

---

### [DECIDE 1] — extract produces several artifacts; the interface returns one key

**RESOLVED: option A — the manifest.** `extract.json` is the canonical single
output, uploaded last. The `Stage` interface is unchanged.

**This is the decision the stage turns on.** Everything else in 4A is
mechanical.

#### What the repo already settles

I checked before proposing anything. The intent is stated in three places and
they agree:

| Source | Says |
|---|---|
| `PROJECT_PLAN.md:329-336` | "Extract keyframes and audio." FFmpeg scene-detect keyframes → JPEG; audio demux → 16 kHz mono WAV. Verification: S3 contains `frames/*.jpg` and `audio.wav` |
| `docs/stage-plans/stage-1a-data-schemas.md:164-169` | Input `validated.mp4`; output `frames/frame_001.jpg…` **and** `audio.wav`; next → transcribe |
| `config/aws-limits.md:97` | Extract sized 1 vCPU / 2 GB, "FFmpeg keyframe extraction" |

So *what* to produce is not actually open: **audio + keyframes**, and the audio
format the plan names (16 kHz mono WAV) is exactly what a local Whisper wants,
which is fortunate given LocalStack Community has no AWS Transcribe and 5A is
heading toward faster-whisper (`PROJECT_PLAN.md:338-346`). Scene-change
detection appears only as the *mechanism* for choosing frames, not as an output
in its own right; no scene list is written anywhere. Metadata appears only as
`models.OutputInfo` fields that the package stage fills.

What is **not** settled anywhere is how a multi-artifact stage fits the runner.

#### The tension, precisely

`worker.Stage` (`runner.go:18-31`) is:

```go
Name() models.StageName
OutputKey(jobID string) string   // singular
OutputBucket() string            // singular
Process(ctx, msg) (string, error)
```

and the runner uses that single key for two distinct jobs:

1. **The idempotency guard** (`runner.go:104-136`): `ObjectExists(bucket, key)`
   before `Process`, plus the recorded stage status to tell a pure duplicate
   from a crash-after-upload. One key has to stand for "all of this stage's
   output is present".
2. **The downstream pointer** (`runner.go:225-231`): the next stage's
   `input.key` is that same key.

Extract writes 1 audio file + N JPEGs, and **N is not known in advance** if
frames are scene-detected. There is no fixed key set to check, and no single
existing artifact that honestly means "extract finished".

#### Options

**A. Manifest object as the canonical single output.** `extract.json` lists
every artifact. Uploaded **last**, after audio and all frames. Its presence
therefore implies everything else landed. `OutputKey` returns
`"{job_id}/extract.json"`; transcribe reads the manifest to find `audio.wav`.

**B. Declare `audio.wav` canonical; frames are a side effect.** Upload frames
first, audio last, so audio's presence implies frames landed. The downstream
message points straight at what transcribe needs. No new concepts at all.

**C. Widen the `Stage` interface** — return `[]string`, or a struct with a
canonical key plus extras.

**D. Split extract into two stages** (extract-audio, extract-frames) with their
own queues.

#### Recommendation: **A, the manifest.**

Reasons, in order of weight:

1. **It does not touch the runner.** 3A's verification found a real idempotency
   bug in that shared loop (`stage-3a-validate-worker.md`, "Bug found in
   verification"), and three of its checks — transient retry, DLQ redrive,
   SIGTERM shutdown — are **being verified right now, separately**. Changing the
   interface every stage depends on while its correctness is still being
   established is the wrong order of operations. Option C also does not remove
   the choice it appears to remove: the runner would still need to pick one key
   for the guard and one for the downstream pointer. It relocates the decision
   into shared code instead of settling it.
2. **It is the only option that survives a variable artifact count honestly.**
   Option B's ordering trick works, but it encodes "audio implies frames" in
   upload order and nowhere else — an invariant with no representation in the
   data, which the next person to reorder two lines will break silently. The
   manifest states the same invariant as a fact you can read.
3. **Downstream needs the frame list anyway.** The alternative is an S3 `LIST`
   on `{job_id}/frames/`, which is paginated, gives no timestamps, and cannot
   distinguish a complete set from a partial one. `models.OutputInfo.ThumbnailURL`
   (`job.go:70`) has to be filled from *something*.
4. **`output_key` becomes truthful.** See the DynamoDB note above.

**Costs accepted, stated plainly:**

- Transcribe (5A) gains one GET and a JSON unmarshal before it can start, rather
  than using `msg.Input.Key` directly. That is roughly five lines, and it makes
  5A's input contract explicit instead of positional.
- The manifest is a second source of truth about S3. If an artifact is deleted
  out from under it, the manifest lies. Acceptable: `dayreel-processed` has a
  7-day lifecycle and holds only intermediates.
- Upload ordering (artifacts first, manifest last) is now load-bearing. It gets
  a comment at the write site and a verification check that every key the
  manifest lists actually exists.

**Sub-decision — where the manifest type lives.** Proposed:
`backend/internal/events/manifest.go`. `events` is already the home of
cross-stage contracts (`StageMessage`, bucket and queue constants) and every
stage imports it, so 5A and 6A get the type without a new dependency and without
a stage package importing a sibling stage package. The package doc comment
widens from "SQS message types" to "cross-stage contracts". The alternative,
`internal/models`, is currently DynamoDB-shaped and this is not a DynamoDB
thing.

**If reviewed and rejected:** B is the honest fallback and costs about an hour
less. It should be chosen deliberately, with the ordering invariant commented,
not by default.

---

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/events/manifest.go` | Create | `ExtractManifest` type + key helpers (**[DECIDE 1]**) |
| `backend/internal/media/audio.go` | Create | `ExtractAudio` — demux to 16 kHz mono WAV |
| `backend/internal/media/frames.go` | Create | `DetectSceneChanges` + `ExtractFrameAt` |
| `backend/internal/media/media_test.go` | Modify | Cases for the two new helpers |
| `backend/internal/worker/extract/extract.go` | Create | The `Stage`: download → probe → audio → frames → manifest |
| `backend/internal/worker/extract/extract_test.go` | Create | Frame selection/capping and manifest assembly, table-driven like `validate_test.go` |
| `backend/cmd/worker/main.go` | Modify | `case models.StageExtract` in `buildStage` (`main.go:70` already reserves it) |
| `infra/docker-compose.yml` | Modify | `worker-extract` service |
| `backend/internal/worker/extract/CONTEXT.md` | Create | Per repo convention |

## Tasks

Each step compiles and is testable on its own; `go build ./...` and `go test
./...` stay green throughout.

1. [ ] `internal/events/manifest.go`: `ExtractManifest` type, `ManifestKey`,
       `AudioKey`, `FrameKey(n)` helpers. Pure data — no I/O, no dependencies.
2. [ ] `internal/media/audio.go`: `ExtractAudio(ctx, inPath, outPath) error`.
       Unit-testable against the synthesized fixture `media_test.go` already
       generates.
3. [ ] `internal/media/frames.go`: `DetectSceneChanges(ctx, path, threshold)
       ([]float64, error)` and `ExtractFrameAt(ctx, inPath, ts, outPath) error`.
4. [ ] `internal/worker/extract/extract.go`: `Name`/`OutputBucket`/`OutputKey`
       + `Process`. Mirrors `validate.go` structurally — temp dir, download,
       probe, work, upload, return key.
5. [ ] Frame-selection unit tests: cap enforcement, the always-include-t=0 rule,
       the zero-scene-change case.
6. [ ] `cmd/worker/main.go`: wire `extract.New(...)`.
7. [ ] `docker-compose.yml`: `worker-extract`, `WORKER_STAGE=extract`, same env
       block and `depends_on` as `worker-validate`.
8. [ ] `CONTEXT.md` for the new package.
9. [ ] End-to-end test below.

## Implementation Plan

### Step 1 — `internal/events/manifest.go`

```go
type ExtractManifest struct {
    JobID           string        `json:"job_id"`
    CreatedAt       time.Time     `json:"created_at"`
    Source          S3Ref         `json:"source"`
    DurationSeconds float64       `json:"duration_seconds"`
    Width, Height   int           `json:"width,omitempty" / "height,omitempty"`
    Audio           AudioArtifact `json:"audio"`
    Frames          []FrameRef    `json:"frames"`
}

type AudioArtifact struct {
    Present    bool   `json:"present"`
    Key        string `json:"key,omitempty"`
    SampleRate int    `json:"sample_rate,omitempty"`
    Channels   int    `json:"channels,omitempty"`
    Codec      string `json:"codec,omitempty"`
}

type FrameRef struct {
    Key              string  `json:"key"`
    TimestampSeconds float64 `json:"timestamp_seconds"`
}
```

Plus `ExtractManifestKey(jobID)`, `ExtractAudioKey(jobID)`,
`ExtractFrameKey(jobID string, n int)` so the extract stage and (later) 5A/6A
agree on layout without repeating string concatenation. `frames` serializes as
`[]` rather than `null` when empty — a downstream `range` over `null` is fine in
Go but the JSON is read by humans during verification.

### Step 2 — `internal/media/audio.go`

```
ffmpeg -v error -y -i <in> -vn -acodec pcm_s16le -ar 16000 -ac 1 -f wav <out>
```

16 kHz mono signed 16-bit PCM is what Whisper and faster-whisper resample to
internally, so producing it here means 5A does no conversion, and the artifact
is directly playable and inspectable during debugging. Size is 32 KB/s: the 10
minute ceiling that validate enforces (`validate.go:36`) caps this at ~19 MB.

Same shape as `RemuxFaststart` — `exec.CommandContext`, stderr captured into the
error. A bare "exit status 1" is unactionable, and here it feeds the
permanent-vs-transient classification.

### Step 3 — `internal/media/frames.go`

Two steps rather than one ffmpeg pass, deliberately:

```
# 1. timestamps only — decode once, write nothing
ffprobe -v error -f lavfi -i "movie=<in>,select=gt(scene\,<threshold>)" \
        -show_entries frame=pkt_pts_time -of csv=p=0

# 2. one seek+encode per selected timestamp
ffmpeg -v error -y -ss <ts> -i <in> -frames:v 1 -q:v 3 <out>.jpg
```

Getting timestamps first means the **cap is applied before any encoding
happens** (**[DECIDE 3]**), and each frame carries a real timestamp for the
manifest rather than an index. The single-pass alternative
(`-vf select=...,showinfo`) requires scraping timestamps out of stderr, which is
brittle in a way that will not announce itself.

The cost is a second decode for seeking. `-ss` before `-i` uses the input
demuxer's index and is fast; with the frame count capped this is bounded.

### Step 4 — `internal/worker/extract/extract.go`

Structurally identical to `validate.go`:

```go
type Options struct {
    SceneThreshold float64  // 0.0-1.0; higher = fewer frames
    MaxFrames      int
    JPEGQuality    int
}

func New(s3 *storage.S3Client, outputBucket string, opts Options) *Stage

func (s *Stage) Name() models.StageName  { return models.StageExtract }
func (s *Stage) OutputBucket() string    { return s.bucket }
func (s *Stage) OutputKey(jobID string) string { return events.ExtractManifestKey(jobID) }
```

`Process`:

1. `os.MkdirTemp` + `defer os.RemoveAll` — same reasoning as validate: a crash
   accumulates nothing and concurrent jobs cannot collide.
2. `DownloadToFile(msg.Input.Bucket, msg.Input.Key, …)` — transient on failure.
3. `media.Probe` — the input is 3A's own output, so a probe failure means
   something is badly wrong with it; still `Permanent`, since re-reading the same
   bytes will not help.
4. If `probe.HasAudio`: `ExtractAudio` → upload `audio.wav` as `audio/wav`.
   Otherwise skip (**[DECIDE 4]**).
5. `DetectSceneChanges` → select and cap (**[DECIDE 3]**) → extract each →
   upload as `image/jpeg`.
6. Build the manifest, upload it **last** as `application/json`.
7. Return `events.ExtractManifestKey(msg.JobID)`.

Error classification, matching 3A's split: ffmpeg/ffprobe failures on a file
that already passed validate are `Permanent`; S3 and network failures return a
plain wrapped error and get redelivered.

### Step 5 — `cmd/worker/main.go`

```go
case models.StageExtract:
    return extract.New(s3Client, cfg.S3ProcessedBucket, extract.DefaultOptions), nil
```

Note extract reads *and* writes `dayreel-processed`. That is fine — 3A
parameterized the bucket on every whole-object method (`storage/objects.go`)
precisely so a stage is not bound to one bucket — but it does mean an
input-key bug writes into the same prefix it reads from. `OutputKey` is
derived from `job_id`, never from `msg.Input.Key`, which keeps that contained.

### Step 6 — compose

`worker-extract`, identical to `worker-validate` apart from
`WORKER_STAGE=extract` and the container name. Same image, same env block, same
`depends_on: localstack: service_healthy`, same `restart: unless-stopped`.

No `init-aws.sh` change: `dayreel-extract` and `dayreel-transcribe` were created
in 1B with the shared DLQ redrive policy (`maxReceiveCount: 3`) and a 300s
visibility timeout.

## Test

```bash
cd infra && docker compose up -d --build

# Drive the real path end to end: upload → validate → extract.
# (Run the upload from inside a container — presigned URLs are signed against
#  the Docker-internal host. See 3A's "presigned URLs are unusable outside the
#  Docker network".)
JOB=$(curl -s -X POST localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{"filename":"test.mp4","size_bytes":1048576,"content_type":"video/mp4"}')
JOB_ID=$(echo "$JOB" | jq -r .job_id)
# ... upload + POST /jobs/$JOB_ID/complete exactly as in stage-3a ...

sleep 25   # validate + extract, past the API's 10s Redis cache TTL

AWS="aws --endpoint-url=http://localhost:4566"

curl -s "localhost:8080/jobs/$JOB_ID" | jq '.stages.extract'
$AWS s3 ls --recursive "s3://dayreel-processed/$JOB_ID/"

# The manifest, and proof every key it claims actually exists
$AWS s3 cp "s3://dayreel-processed/$JOB_ID/extract.json" - | tee /tmp/m.json | jq .
for K in $(jq -r '[.audio.key // empty] + [.frames[].key] | .[]' /tmp/m.json); do
  $AWS s3api head-object --bucket dayreel-processed --key "$K" >/dev/null \
    && echo "ok   $K" || echo "MISSING $K"
done

# Audio really is 16 kHz mono PCM
$AWS s3 cp "s3://dayreel-processed/$JOB_ID/audio.wav" /tmp/a.wav
ffprobe -v error -show_entries stream=codec_name,sample_rate,channels \
        -of default=nw=1 /tmp/a.wav

# Handoff
$AWS sqs receive-message \
  --queue-url http://localhost:4566/000000000000/dayreel-transcribe \
  | jq -r '.Messages[0].Body' | jq .
```

Expected: `stages.extract.status == "completed"`, `attempts == 1`, `output_key
== "$JOB_ID/extract.json"`; every manifest key resolves; `pcm_s16le / 16000 / 1`;
exactly one message on `dayreel-transcribe` with `stage == "transcribe"`, input
pointing at `extract.json`, and the **same `trace_id` as the extract message**.

## Verification

_To be run against LocalStack. Nothing here is checked off until it has actually
been observed._

**Happy path**

- [ ] `worker-extract` starts, resolves the queue, and long-polls without
      busy-spinning
- [ ] A completed upload flows upload → validate → extract with **no manual SQS
      publish** at any point
- [ ] `stages.extract` goes `pending → running → completed`, `attempts == 1`
- [ ] `extract.json` exists, parses, and **every key it lists resolves via
      `head-object`** — this is the check that makes **[DECIDE 1]** honest
- [ ] `audio.wav` probes as `pcm_s16le`, `16000` Hz, `1` channel, duration
      within 0.5s of the source
- [ ] `frames/` holds ≥ 1 and ≤ `MaxFrames` valid JPEGs, and each manifest
      `timestamp_seconds` falls inside the clip duration
- [ ] Exactly one message lands on `dayreel-transcribe`, pointing at
      `extract.json`, carrying the **inherited** `trace_id` (first stage where
      this is inheritance rather than synthesis)
- [ ] Message deleted from `dayreel-extract`; queue depth returns to 0
- [ ] `stages.extract.output_key == "{job_id}/extract.json"`

**Failure and edge paths** — these are where a stage is actually proven

- [ ] **Idempotency (duplicate):** re-publish the identical extract
      `StageMessage`. Expect `already completed, dropping duplicate` in the log,
      no new S3 objects, `attempts` unchanged, `dayreel-transcribe` depth
      unchanged.
- [ ] **Idempotency (crash-resume):** for a fresh job, hand-write an
      `extract.json` while `stages.extract` is still `running`, then publish the
      message. Expect `output exists but stage unrecorded, resuming`, `Process`
      skipped, and the transcribe message published anyway. **3A never
      exercised this branch** (`runner.go:135`) — it is reachable for the first
      time here.
- [ ] **Permanent failure:** `aws s3 cp` a text file to
      `{job_id}/validated.mp4` and publish an extract message. Expect
      `stages.extract` `failed` with a readable ffprobe error, the job flipped
      to `failed`, the message deleted, nothing in `dayreel-dlq`, `attempts == 1`
      (proving it was not retried).
- [ ] **Silent clip:** a video with no audio track completes with
      `audio.present == false`, **no** `audio.wav` object, frames still
      produced, and still publishes to transcribe (**[DECIDE 4]**).
- [ ] **No scene changes:** a static single-colour clip still yields ≥ 1 frame
      via the guaranteed `t=0` frame, rather than an empty `frames` array
      (**[DECIDE 3]**).
- [ ] **Frame-count cap:** a high-motion clip produces exactly `MaxFrames`
      frames, not hundreds, and the manifest agrees with what is in S3.
- [ ] **Missing input:** publish an extract message for a `job_id` whose
      `validated.mp4` does not exist. Observe and record what actually happens —
      the expectation is three transient retries into `dayreel-dlq`, which may
      not be the behaviour we want (see Risks).
- [ ] **Transient failure:** stop LocalStack mid-job; the message returns to the
      queue and succeeds on retry once LocalStack is back.
- [ ] **SIGTERM:** `docker compose stop worker-extract` mid-ffmpeg leaves no
      orphaned ffmpeg process and the message becomes visible again.

**Timing** — new in 4A, because extract is no longer a ~1s operation

- [ ] Wall-clock for a 10-minute 1080p clip stays comfortably under the 300s
      visibility timeout. If it does not, the runner needs visibility
      heartbeating and that is a runner change, not an extract change.

## Claude Code Implementation Plan

### Recommended Approach: Sequential Phases with Parallel File Writes

Every dependency this stage needs already exists — the runner, the `Stage`
interface, whole-object S3 helpers, per-stage DynamoDB updates, and the worker
image with ffmpeg. Nothing shared has to change, which is what makes 4A small.

The work is four leaf packages with a strict import order
(`events` ← `media` ← `extract` ← `main`), so phases are sequential while files
*within* a phase are independent and can be written in one batch.

### Pre-Flight Check

Run before writing anything — 3A's session ended with the stack up but disk
under pressure, and a failed build 40 minutes in is worse than a 30-second check.

```
0a. df -h /System/Volumes/Data          # need >5Gi; builds ate ~5Gi last session
0b. docker builder prune -f             # build cache was 1.4GB at end of 3A
0c. docker compose ps                   # all 4 healthy; docker daemon alive
0d. git log --oneline -1                # expect the 3A verification commit
0e. awslocal sqs purge-queue --queue-url .../dayreel-dlq
                                        # 2 inert test messages from 3A's DLQ runs
                                        # would corrupt a "DLQ is empty" assertion
```

### Execution Steps

```
Phase 1: Pure data, no I/O (single file, no dependencies)
1.  Write backend/internal/events/manifest.go
2.  go build ./... && go test ./...

Phase 2: Media helpers (parallel writes — independent of each other)
3a. Write backend/internal/media/audio.go
3b. Write backend/internal/media/frames.go
4.  Extend backend/internal/media/media_test.go
    - ExtractAudio produces 16 kHz mono PCM (assert via ffprobe, not file size)
    - DetectSceneChanges returns [] on the static testsrc fixture
5.  go test ./internal/media/    <-- runs real ffmpeg; slowest feedback loop,
                                     so get it green before building on it

Phase 3: The stage (depends on 1 and 2)
6a. Write backend/internal/worker/extract/extract.go
6b. Write backend/internal/worker/extract/extract_test.go
    - frame capping: 50 timestamps + MaxFrames 20 -> 20, evenly spaced
    - t=0 always present, including when detection returns []
    - manifest lists exactly the frames uploaded
7.  go test ./internal/worker/extract/

Phase 4: Wiring (parallel writes)
8a. Modify backend/cmd/worker/main.go     -- case models.StageExtract
8b. Modify infra/docker-compose.yml       -- worker-extract service
8c. Write backend/internal/worker/extract/CONTEXT.md
9.  go build ./... && go vet ./...

Phase 5: Build and verify
10. docker compose up -d --build worker-extract
    ONE build at a time. Two concurrent `compose up --build` calls on the same
    image deadlocked in 3A and had to be SIGKILLed.
11. Run the end-to-end Test block above
12. Work the Verification checklist; record results in this file
```

### Parallel Opportunities

| Phase | Parallel Files |
|-------|----------------|
| 2 | `media/audio.go`, `media/frames.go` |
| 3 | `extract/extract.go`, `extract/extract_test.go` |
| 4 | `cmd/worker/main.go`, `docker-compose.yml`, `extract/CONTEXT.md` |

### Why Not Subagents?

- The stage is one file mirroring `validate.go`; describing it to a subagent
  costs more than writing it.
- `extract.go` has to match conventions established across `validate.go`,
  `runner.go`, and `storage/objects.go` — error classification, temp-dir
  discipline, bucket parameterization. That consistency is the whole point of
  the stage and is exactly what a cold agent gets subtly wrong.
- Sequential compilation catches interface drift immediately.

Planning is the exception: drafting *this* document in the background while 3A's
runner checks ran was a good use of one, because it was research over existing
files with no code to keep consistent.

### Potential Blockers

| Blocker | Resolution |
|---------|------------|
| Disk full mid-build | The 3A failure mode. `docker builder prune -f`; needs >5Gi free |
| Docker daemon dead after an ENOSPC | Orphaned `com.docker.backend` makes `open -a Docker` a silent no-op. `kill -9` the orphans first, then relaunch |
| `movie=` lavfi filter unavailable | Alpine's `apk add ffmpeg` may lack `--enable-filter=movie`. Verify with `ffmpeg -filters \| grep movie` **inside the image**, not on the host. Fallback: `-vf select='gt(scene,T)',showinfo` and scrape stderr |
| Concurrent `compose up --build` | Deadlocks. One build at a time |
| Extract exceeds 300s visibility | Message redelivers mid-work and a second worker starts the same job. Surfaces as duplicate frame uploads. Needs runner heartbeating — a runner change, out of 4A's scope |
| `-ss` lands on the wrong frame | `-ss` before `-i` seeks to the nearest preceding keyframe; recorded timestamps may drift from actual content |
| Frames inflate S3 PUT count | No free tier on this account, but PUTs bill at ~$0.005/1,000 — 20 frames/job is a fraction of a cent. Not a budget risk; `MaxFrames: 20` caps it anyway (`config/free-tier.md`) |

### Time Estimate

- Phase 1–2 (manifest + media helpers, with real-ffmpeg tests): ~20 minutes
- Phase 3 (stage + unit tests): ~15 minutes
- Phase 4 (wiring): ~5 minutes
- Docker build: ~2–10 minutes (cold Go layer rebuild is the variable)
- End-to-end + verification checklist: ~15 minutes
- **Total:** ~60 minutes

This exceeds `PROJECT_PLAN.md`'s 20-minute budget for 4A. The overrun is the
verification checklist, not the code. 3A's two bugs were both silent and both
surfaced only in end-to-end verification, so this is the wrong place to
economize.

---

## Notes

### [DECIDE 2] — are keyframes in scope for 4A at all?

**RESOLVED: yes, keep them.** The user's call was to keep building and revisit
when it actually becomes a problem or a failing test. Frames still have no
downstream consumer beyond `OutputInfo.ThumbnailURL`; that is accepted knowingly
rather than overlooked.

Worth asking, because **nothing in the pipeline consumes them.**
`stage-1a-data-schemas.md` has transcribe reading `audio.wav` and package
reading `validated.mp4` + `transcript.vtt`. Frames appear in no downstream
input. Their only consumer is `models.OutputInfo.ThumbnailURL` (`job.go:70`),
filled by the package stage, and eventually the mobile job list.

So a defensible scoping choice is: **audio only in 4A** — it is the artifact
that blocks 5A — with frames as a follow-up. That would cut the stage to one
ffmpeg call and arguably remove the need for **[DECIDE 1]** entirely, since a
single-artifact stage fits the existing interface without discussion.

**Recommendation: keep both, do them now.** Three reasons:

1. Deferring frames does not delete **[DECIDE 1]**, it postpones it to whichever
   stage first needs two outputs — and package (6A) definitely does, writing a
   master playlist plus per-tier segments. Better to settle the pattern on the
   stage where it is cheap.
2. `PROJECT_PLAN.md` and `stage-1a` both specify frames as extract output. A
   silent scope cut here would leave `ThumbnailURL` with no producer and nobody
   would notice until 6A.
3. The marginal cost is one ffprobe call and a bounded loop of seeks.

The counter-argument is the 20-minute budget in `PROJECT_PLAN.md:234`, which
this stage will exceed. That budget was always optimistic; I would rather note
the overrun than quietly drop a documented deliverable.

### [DECIDE 3] — frame selection strategy and the cap

**RESOLVED: bounded scene detection.** `t=0` always included, `MaxFrames: 20`
with an evenly-spaced subset when detection overruns.

`PROJECT_PLAN.md:334` says "FFmpeg scene-detect keyframes". Scene detection has
two unbounded edges, both real:

- **Zero frames.** A static clip — a locked-off shot, a screen recording, a
  single colour — crosses no scene threshold and produces an empty set. A job
  with no thumbnail at all is worse than a job with an arbitrary one.
- **Hundreds of frames.** Shaky handheld footage or a hard-cut montage crosses
  constantly, and each frame is an S3 `PUT`. The original reasoning here cited a
  2,000 PUT/month free-tier allowance; **there is no free tier on this account**
  (`config/free-tier.md`). The corrected picture is less alarming: PUTs bill at
  ~$0.005/1,000, so even a pathological clip costs fractions of a cent. The cap
  is still worth having — unbounded output is worth bounding on its own terms —
  but it is not the budget that justifies it.

Fixed-interval sampling (`fps=1/N`) avoids both but produces frames that mean
nothing — the thumbnail is whatever happened at t=10s.

**Recommendation: scene detection, bounded on both sides.**

```go
var DefaultOptions = Options{
    SceneThreshold: 0.3,   // ffmpeg's scene score; higher = fewer cuts
    MaxFrames:      20,    // hard cap
    JPEGQuality:    3,     // -q:v, 1 best … 31 worst
}
```

- **Always include `t=0`**, whether or not it scores as a scene change. This
  guarantees a non-empty set and gives `ThumbnailURL` a deterministic
  first-frame candidate.
- **Cap at `MaxFrames`.** When detection returns more, keep `t=0` plus an evenly
  spaced subset across the clip, rather than the first 19 — the first 19 of a
  fast-cut opening are all the same three seconds.
- All three values live in one struct, like `validate.DefaultLimits`, so tuning
  them later is a one-line change and not a restructure.

These are starting guesses, not measured values. `0.3` in particular deserves a
look once there is real footage.

### [DECIDE 4] — clips with no audio track

**RESOLVED: option (c).** No `audio.wav`, `audio.present: false` in the
manifest. This creates a ~6-line obligation in 5A to short-circuit to an empty
`WEBVTT` — carry it into the 5A plan explicitly.

Validate deliberately admits silent clips: `gate` treats audio as optional
(`validate.go:117`, "a silent clip is legitimate"). So extract will meet them.

- **(a) Fail permanently.** Rejected outright — it contradicts a decision 3A
  made on purpose, and would fail jobs the pipeline explicitly accepted.
- **(b) Synthesize a silent WAV** (`anullsrc` for the clip duration) so
  downstream is uniform. Transcribe runs normally and produces an empty VTT.
- **(c) Record `audio.present: false` in the manifest**, write no `audio.wav`,
  and let 5A short-circuit to an empty `WEBVTT` file.

**Recommendation: (c).** It does not fabricate data, it does not spend Whisper
compute transcribing manufactured silence, and the manifest already exists to
carry exactly this kind of fact. The cost is a branch in 5A — roughly six lines
— and **that is a 5A obligation this plan is creating**, so it should be carried
into the 5A plan explicitly rather than discovered there.

(b) is the right answer only if 5A turns out to be unable to produce a valid VTT
without running the model. That is worth confirming when 5A is planned.

### Risks and inherited tensions

**4A inherits whatever correctness the runner has.** 3A's verification found a
real idempotency bug in the *shared* loop: replaying a completed job
double-published downstream and bumped `attempts`. It was fixed by moving the
completion check ahead of `SetStageRunning` (`runner.go:104-136`). The relevant
lesson is the shape of that bug — it compiled, unit tests passed, the happy path
was flawless, and only replaying a real message against real infrastructure
exposed it. 4A gets no free pass from 3A's fix; the replay tests are repeated
here, against extract.

**Three runner behaviours are unverified as of this writing.** Transient retry,
DLQ redrive after three deliveries, and clean SIGTERM shutdown are marked
not-yet-run in 3A and are **being verified separately right now**. All three are
runner-level, so 4A depends on them without being able to prove them. If any
turns out to be broken, the fix lands in `runner.go` and affects every stage —
that is a shared-code risk 4A carries but does not own. It also argues for not
touching `runner.go` in this stage (**[DECIDE 1]**).

**A missing input object is currently classified transient.** `DownloadToFile`
failing on `NoSuchKey` returns a plain wrapped error, so it retries three times
into the DLQ. For extract that is arguably wrong: the input was written by
validate one step earlier and S3 is read-after-write consistent, so "absent"
means something is genuinely broken, not slow. This is inherited from
`validate.go:73-75` and I am not changing it in 4A; the verification step above
records the real behaviour so the decision can be made with evidence.

**The runner does not check that a message belongs to its stage.** `handle` uses
`r.stage.Name()` for DynamoDB writes regardless of `msg.Stage`. A message
misrouted onto `dayreel-extract` would be processed as an extract job. Low
likelihood while the only publisher is the runner itself; worth a one-line guard
eventually, in the runner, not here.

**Runtime is no longer negligible.** Validate's `-c copy` remux is ~1s. Extract
does a full decode pass for scene detection plus an audio pass plus per-frame
seeks. On the 1 vCPU sizing in `config/aws-limits.md:97`, a 10-minute 1080p clip
could plausibly run 60-120s. Still inside the 300s visibility timeout, but the
headroom is now measured rather than obvious — hence the explicit timing check.

**Disk.** Input MP4 + WAV + up to 20 JPEGs in a temp dir. Bounded by validate's
10-minute limit and well inside the 20 GB Fargate ephemeral default, but this is
the first stage where temp usage is worth stating at all.

### Deliberately not in scope

- Any change to `runner.go`, `queue`, `storage`, `db`, `Dockerfile.worker`, or
  `init-aws.sh`
- Structured logging / trace propagation — `trace_id` is carried and now
  verifiably inherited, but still unused
- Metrics (`Metrics.ExtractDurationMs` exists in the model, `job.go:78`, and
  stays unwritten, consistent with validate)
- Worker concurrency — still one message at a time
- Stall detection for jobs with no running stage (carried over from 3A)
- The mobile status-vocabulary mismatch — still deferred to Stage 7 per 3A
  **[DECIDE 4]**

### Uncertain, flagged rather than smoothed over

- **Whether 5A actually wants the manifest or the audio key.** I am designing
  5A's input contract from inside 4A, before 5A is planned. If a local Whisper
  integration turns out to want the raw audio pointer and nothing else, the
  manifest is one extra GET of overhead for the whole rest of the pipeline. I
  think it still pays for itself at 6A, but this is a prediction.
- **Scene threshold `0.3` and cap `20`** are unmeasured guesses.
- **The lavfi `movie=` filter under Alpine's ffmpeg build.** The two-step
  timestamp approach assumes `-f lavfi -i "movie=..."` is available in the
  `apk add ffmpeg` build. It normally is, but it is a build-flag dependency I
  have not verified. If it is missing, fall back to the single-pass
  `select,showinfo` variant and accept the stderr scraping.
- **Whether frame timestamps from `-ss` seeking land exactly where detection
  said.** `-ss` before `-i` seeks to the nearest preceding keyframe, so an
  extracted frame can be slightly earlier than its recorded timestamp. Probably
  irrelevant for thumbnails; worth knowing before anyone builds a UI that
  assumes frame-accuracy.
