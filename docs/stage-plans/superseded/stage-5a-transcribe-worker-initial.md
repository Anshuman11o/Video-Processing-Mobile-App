# Stage 5A: Transcribe Worker

> **Depends on:** Stage 4A (Extract Worker), Stage 3A (worker harness)
> **Run in parallel with:** Stage 2B (Mobile Shell)
> **Estimated time:** 25–35 minutes (mock path ~15 min; faster-whisper sidecar adds ~20 min)
> **Blocks:** Stage 6A (Package Worker)

## Status of This Plan

**Initial plan — written ahead of Stages 3A and 4A.** The worker runtime
assumptions carry over from Stage 4A's plan; see
[Open Items](#open-items-to-confirm-after-stages-3a-and-4a) at the bottom for
what must be reconciled once those stages land.

The transcription-specific design here — the `Transcriber` interface, the WebVTT
writer, the mock mode, the sidecar contract — is self-contained and should be
stable.

---

## Aim

Turn `audio.wav` into a **timestamped WebVTT transcript**, with two
interchangeable backends: a deterministic mock (default, fast) and a real
faster-whisper sidecar. Both paths produce byte-identical output *format*, so
Stage 6A never has to care which ran.

---

## Design Decision: Sidecar, Not In-Process

faster-whisper is Python. The Go worker cannot call it in-process. Three options
were considered:

| Option | Verdict |
|--------|---------|
| Python sidecar HTTP service | **Chosen** |
| Bundle Python + model into the Go worker image, shell out per message | Rejected: ~1.5 GB image, and the model reloads (5–10 s) on *every* message |
| whisper.cpp binary shelled out | Rejected: another toolchain to build; revisit if the sidecar proves heavy |

The sidecar loads the model **once** at startup and holds it in memory. It also
mirrors how this deploys on AWS — a separate ECS task that can be sized and
scaled independently of the Go workers, which is the difference between paying
for CPU-heavy tasks across the whole pipeline versus just this stage.

Cost: one more service, one more network hop, one more failure mode. Worth it.

**`MOCK_TRANSCRIBE=true` is the default for local development** and is the path
the 3-hour E2E demo runs on. The sidecar is built in the same stage but is not
on the critical path.

---

## Components Touched

| Component | Action | Files |
|-----------|--------|-------|
| `backend/internal/worker/transcribe/` | Create | `transcribe.go`, `mock.go`, `whisper.go`, `*_test.go`, `CONTEXT.md` |
| `backend/internal/vtt/` | Create | `vtt.go`, `vtt_test.go` — WebVTT writer |
| `backend/internal/config/config.go` | Modify | Transcription config |
| `backend/cmd/worker/main.go` | Modify | Register the transcribe handler |
| `infra/transcriber/` | Create | `Dockerfile`, `app.py`, `requirements.txt` |
| `infra/docker-compose.yml` | Modify | `worker-transcribe` + `transcriber` services |
| `.env.example` | Modify | New env vars |

---

## Boundaries

### Input: SQS message on `dayreel-transcribe`

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

### Derived input: `extract.json`

Read from `dayreel-processed/{job_id}/extract.json` (Stage 4A). Two fields
matter:

- `has_audio` — `false` means skip transcription entirely (see
  [No-Audio Path](#no-audio-path))
- `duration_seconds` — used to clamp segment end times and to size mock output

### Output: S3 objects

| Bucket | Key | Content Type | Content |
|--------|-----|--------------|---------|
| `dayreel-processed` | `{job_id}/transcript.vtt` | `text/vtt` | WebVTT — **the contract with 6A** |
| `dayreel-processed` | `{job_id}/transcript.json` | `application/json` | Raw segments + metadata |

`transcript.vtt` is written **last** and is the idempotency sentinel.

### `transcript.vtt` shape

```
WEBVTT

1
00:00:00.000 --> 00:00:04.120
Alright, so this is the beach at sunset.

2
00:00:04.120 --> 00:00:08.900
You can just about hear the waves behind me.
```

### `transcript.json` shape

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "engine": "mock",
  "model": "mock-v1",
  "language": "en",
  "duration_seconds": 42.517,
  "segment_count": 9,
  "transcribe_duration_ms": 84,
  "realtime_factor": 0.002,
  "segments": [
    { "start": 0.0, "end": 4.12, "text": "Alright, so this is the beach at sunset." }
  ],
  "generated_at": "2026-01-15T10:31:29Z"
}
```

`engine` is `mock` | `faster-whisper`. Kept because it is the only durable
record of which path produced a given transcript — invaluable when a demo
transcript looks suspiciously tidy.

### Output: SQS message to `dayreel-package`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "stage": "package",
  "input": {
    "bucket": "dayreel-processed",
    "key": "550e8400-e29b-41d4-a716-446655440000/validated.mp4"
  },
  "attempt": 1,
  "timestamp": "2026-01-15T10:31:29Z",
  "trace_id": "abc123"
}
```

**Note the input flips back to the video.** Packaging's primary input is
`validated.mp4`; the transcript is a derived key. This is consistent with the
one-primary-input convention from Stage 4A.

### DynamoDB writes

| When | Update |
|------|--------|
| Handler start | `stages.transcribe.status = running`, `started_at`, `attempts += 1` |
| Success | `status = completed`, `completed_at`, `output_key = {job_id}/transcript.vtt` |
| Permanent failure | `status = failed`, `error`, job `status = failed` |
| Success | `metrics.transcribe_duration_ms` |

---

## Go Design

### The interface both backends satisfy

```go
package transcribe

type Segment struct {
    Start float64 `json:"start"`
    End   float64 `json:"end"`
    Text  string  `json:"text"`
}

type Result struct {
    Engine   string    `json:"engine"`
    Model    string    `json:"model"`
    Language string    `json:"language"`
    Segments []Segment `json:"segments"`
}

type Transcriber interface {
    Transcribe(ctx context.Context, audioPath string, durationSeconds float64) (*Result, error)
}
```

Selected at worker startup:

```go
var t Transcriber
if cfg.MockTranscribe {
    t = NewMockTranscriber(cfg)
} else {
    t = NewWhisperTranscriber(cfg) // HTTP client for the sidecar
}
```

One `Transcriber`, one VTT writer, one code path through the handler. The mock
is not a test double bolted on the side — it is a first-class backend, which is
what keeps the mock path honest as a demo path.

### Mock transcriber

Deterministic, seeded by `job_id` so the same job always yields the same text
(makes assertions possible and demos reproducible):

- One cue every `MOCK_SEGMENT_SECONDS` (default 5 s), covering the full duration
- Text drawn round-robin from a fixed phrase pool, prefixed with the cue index
- Final cue is clamped to `duration_seconds`
- Language reported as `en`, model as `mock-v1`
- Sleeps `MOCK_TRANSCRIBE_DELAY_MS` (default 0) so pipeline timing can be
  exercised without real inference

### WebVTT writer — `internal/vtt/vtt.go`

```go
func Write(w io.Writer, segments []Segment, opts Options) error
func FormatTimestamp(seconds float64) string // "HH:MM:SS.mmm"
```

Normalization applied before writing (Whisper output is not always well-formed):

1. Sort by `start`
2. Drop segments with empty text after trimming
3. Clamp `end` to `duration_seconds`
4. Force `end > start` (minimum 0.1 s cue)
5. Clip overlaps: if `segments[i].end > segments[i+1].start`, truncate the
   earlier cue
6. Collapse internal newlines and runs of whitespace to single spaces
7. Escape `&` → `&amp;`, `<` → `&lt;`, `>` → `&gt;`
8. Guard against a literal `-->` inside cue text

`Options` carries `TimestampMap` — see the HLS note in Stage 6A; the copy of the
VTT that lands in the HLS bucket needs an `X-TIMESTAMP-MAP` header, and it is
cheaper to support that here than to string-munge the file in 6A.

### faster-whisper sidecar contract

**`POST /transcribe`** — `multipart/form-data`, field `audio` = the WAV file.

The Go worker already has the file on local disk after downloading it, so
uploading the bytes is simpler than handing the sidecar S3 credentials and an
endpoint. Keeps the Python service completely AWS-unaware.

**Response 200:**

```json
{
  "language": "en",
  "language_probability": 0.98,
  "duration": 42.517,
  "model": "tiny.en",
  "segments": [
    { "start": 0.0, "end": 4.12, "text": " Alright, so this is the beach at sunset." }
  ]
}
```

**`GET /health`** — returns 200 only once the model is loaded; 503 while
loading. Compose healthcheck depends on this.

**Errors:** 400 for undecodable audio (permanent), 500 for inference failure
(transient), 503 while loading (transient).

### `infra/transcriber/app.py` (sketch)

```python
from fastapi import FastAPI, UploadFile, File, HTTPException
from faster_whisper import WhisperModel
import os, tempfile

MODEL_SIZE   = os.getenv("WHISPER_MODEL", "tiny.en")
DEVICE       = os.getenv("WHISPER_DEVICE", "cpu")
COMPUTE_TYPE = os.getenv("WHISPER_COMPUTE_TYPE", "int8")

app = FastAPI()
model = None

@app.on_event("startup")
def load_model():
    global model
    model = WhisperModel(MODEL_SIZE, device=DEVICE, compute_type=COMPUTE_TYPE)

@app.get("/health")
def health():
    if model is None:
        raise HTTPException(status_code=503, detail="model loading")
    return {"status": "ok", "model": MODEL_SIZE}

@app.post("/transcribe")
async def transcribe(audio: UploadFile = File(...)):
    with tempfile.NamedTemporaryFile(suffix=".wav", delete=True) as f:
        f.write(await audio.read())
        f.flush()
        segments, info = model.transcribe(f.name, beam_size=1, vad_filter=True)
        out = [{"start": s.start, "end": s.end, "text": s.text} for s in segments]
    return {
        "language": info.language,
        "language_probability": info.language_probability,
        "duration": info.duration,
        "model": MODEL_SIZE,
        "segments": out,
    }
```

`beam_size=1` and `int8` are chosen for CPU speed over accuracy — this is a demo
pipeline, not a captioning product. `vad_filter=True` suppresses the
hallucinated text Whisper produces over silence, which is the single most
visible quality problem on phone-recorded clips.

---

## Processing Logic

1. **Idempotency:** `HeadObject` on `{job_id}/transcript.vtt` → skip, forward,
   delete message.
2. **Read `extract.json`.** Missing ⇒ permanent error (4A must have run).
3. **No-audio short-circuit** (see below).
4. **Download** `audio.wav` to `{WORKER_TMP_DIR}/{job_id}/`.
5. **Transcribe** via the selected backend, under
   `TRANSCRIBE_TIMEOUT_SECONDS` (default 300).
6. **Normalize + write VTT** and the JSON sidecar file.
7. **Upload** `transcript.json`, then `transcript.vtt` **last**.
8. **Finalize:** DynamoDB, Redis invalidation, send to `dayreel-package`,
   delete the message, remove the temp directory.

### No-Audio Path

When `extract.json` has `has_audio: false`:

- Write a valid header-only VTT: `WEBVTT\n\n`
- Write `transcript.json` with `segments: []` and `engine: "none"`
- Mark the stage **completed** (not failed — a silent video is a legitimate reel)
- Forward to `dayreel-package` as normal

Stage 6A must then handle a cue-less VTT by omitting the subtitle rendition
entirely — a subtitle track advertised in the master playlist that resolves to
nothing is worse than no track at all in most players. **This is a hard
dependency to carry into 6A.**

### Visibility Timeout

Queues are created with a 300 s visibility timeout. `tiny.en` at int8 on CPU
runs roughly 5–10× faster than realtime, so a 60 s clip is ~10 s — comfortable.
But cold start is not: the first request after container start also pays model
download (~75 MB for `tiny.en`) and load.

The handler must use the harness's **visibility heartbeat** (extend by 60 s
every 30 s while the call is in flight) rather than relying on headroom. Confirm
that helper exists in 3A.

---

## Failure Model

| Condition | Classification | Behavior |
|-----------|----------------|----------|
| `extract.json` missing | Permanent | Fail stage |
| `audio.wav` missing but `has_audio: true` | Permanent | Fail stage (4A contract violated) |
| Sidecar returns 400 | Permanent | Fail stage |
| Sidecar returns 503 (model loading) | Transient | Redeliver; backoff |
| Sidecar timeout / connection refused | Transient | Redeliver |
| Zero segments returned from real engine | Success | Write header-only VTT — genuinely silent audio |
| S3 / DynamoDB errors | Transient | Redeliver (3 receives → DLQ) |

---

## Docker Compose Additions

```yaml
  transcriber:
    build:
      context: ./transcriber
      dockerfile: Dockerfile
    container_name: dayreel-transcriber
    ports:
      - "8090:8090"
    environment:
      - WHISPER_MODEL=tiny.en
      - WHISPER_DEVICE=cpu
      - WHISPER_COMPUTE_TYPE=int8
    volumes:
      - whisper-models:/root/.cache/huggingface
    networks:
      - dayreel-network
    healthcheck:
      test: ["CMD", "python", "-c", "import urllib.request;urllib.request.urlopen('http://localhost:8090/health')"]
      interval: 10s
      timeout: 5s
      retries: 10
      start_period: 180s

  worker-transcribe:
    build:
      context: ../backend
      dockerfile: Dockerfile.worker
    container_name: dayreel-worker-transcribe
    environment:
      - STAGE=transcribe
      - AWS_REGION=us-east-1
      - AWS_ACCESS_KEY_ID=test
      - AWS_SECRET_ACCESS_KEY=test
      - LOCALSTACK_ENDPOINT=http://localstack:4566
      - USE_LOCALSTACK=true
      - S3_PROCESSED_BUCKET=dayreel-processed
      - DYNAMODB_TABLE=dayreel-jobs
      - REDIS_URL=redis:6379
      - MOCK_TRANSCRIBE=true
      - TRANSCRIBER_URL=http://transcriber:8090
      - TRANSCRIBE_TIMEOUT_SECONDS=300
      - WORKER_CONCURRENCY=1
    depends_on:
      localstack:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      - dayreel-network

volumes:
  whisper-models:
```

**`transcriber` is deliberately not in `worker-transcribe`'s `depends_on`.** With
`MOCK_TRANSCRIBE=true` the sidecar is unnecessary, and a 180 s model-load start
period must not block the pipeline coming up. Put the sidecar behind a compose
profile (`profiles: ["whisper"]`) so `docker compose up` skips it by default and
`docker compose --profile whisper up` pulls it in.

`WORKER_CONCURRENCY=1` for this stage: transcription is CPU-bound and the
sidecar serializes anyway.

### Config additions

```go
MockTranscribe           bool   // MOCK_TRANSCRIBE, default true locally
MockSegmentSeconds       int    // MOCK_SEGMENT_SECONDS, default 5
MockTranscribeDelayMs    int    // MOCK_TRANSCRIBE_DELAY_MS, default 0
TranscriberURL           string // TRANSCRIBER_URL, default "http://transcriber:8090"
TranscribeTimeoutSeconds int    // TRANSCRIBE_TIMEOUT_SECONDS, default 300
```

---

## Tasks

1. [ ] Create `internal/vtt/vtt.go` — writer, timestamp formatter, normalization
2. [ ] Write `internal/vtt/vtt_test.go` — table tests for overlap clipping, clamping, escaping, timestamp formatting
3. [ ] Create `internal/worker/transcribe/transcribe.go` — `Transcriber` interface, `Segment`, `Result`
4. [ ] Create `mock.go` — deterministic mock backend
5. [ ] Create `whisper.go` — HTTP client for the sidecar (multipart upload, timeout, error classification)
6. [ ] Create the stage handler, including the `has_audio: false` short-circuit
7. [ ] Add transcription config to `config.go` and `.env.example`
8. [ ] Register the handler in `cmd/worker/main.go`
9. [ ] Create `infra/transcriber/{Dockerfile,app.py,requirements.txt}`
10. [ ] Add `transcriber` (profile `whisper`) and `worker-transcribe` to `docker-compose.yml`
11. [ ] Test the mock path E2E
12. [ ] Test the real path E2E (`--profile whisper`, `MOCK_TRANSCRIBE=false`)
13. [ ] Create `internal/worker/transcribe/CONTEXT.md`

---

## Test

### Mock path (the one that must pass)

```bash
cd infra && docker compose up -d --build worker-transcribe

JOB_ID="test-transcribe-$(date +%s)"
AWS="aws --endpoint-url=http://localhost:4566"

# Seed Stage 4A's outputs
ffmpeg -f lavfi -i sine=frequency=440:duration=30 -ar 16000 -ac 1 /tmp/audio.wav -y
$AWS s3 cp /tmp/audio.wav "s3://dayreel-processed/${JOB_ID}/audio.wav"
cat > /tmp/extract.json <<EOF
{"job_id":"${JOB_ID}","duration_seconds":30.0,"width":1280,"height":720,
 "has_audio":true,"audio_key":"${JOB_ID}/audio.wav","frame_count":1,
 "frames":[{"key":"${JOB_ID}/frames/frame_001.jpg","timestamp_seconds":0.0}]}
EOF
$AWS s3 cp /tmp/extract.json "s3://dayreel-processed/${JOB_ID}/extract.json"

$AWS sqs send-message \
  --queue-url http://localhost:4566/000000000000/dayreel-transcribe \
  --message-body "{\"job_id\":\"${JOB_ID}\",\"stage\":\"transcribe\",\"input\":{\"bucket\":\"dayreel-processed\",\"key\":\"${JOB_ID}/audio.wav\"},\"attempt\":1,\"timestamp\":\"$(date -u +%FT%TZ)\",\"trace_id\":\"manual\"}"

sleep 10

$AWS s3 cp "s3://dayreel-processed/${JOB_ID}/transcript.vtt" -
$AWS s3 cp "s3://dayreel-processed/${JOB_ID}/transcript.json" - | jq '.engine, .segment_count'

# Validate the VTT parses (ffmpeg is a good-enough validator)
$AWS s3 cp "s3://dayreel-processed/${JOB_ID}/transcript.vtt" /tmp/t.vtt
ffprobe -v error -show_entries format=format_name -of default=nw=1 /tmp/t.vtt
# Expect: format_name=webvtt

# Next stage was triggered
$AWS sqs receive-message \
  --queue-url http://localhost:4566/000000000000/dayreel-package | jq -r '.Messages[0].Body'
```

### Real path

```bash
cd infra && docker compose --profile whisper up -d --build transcriber
# First run downloads the model — wait for healthy
docker compose ps transcriber

curl -sf http://localhost:8090/health | jq .

# Direct sidecar check, no pipeline involved
curl -s -F "audio=@/tmp/audio.wav" http://localhost:8090/transcribe | jq '.model, .language, (.segments|length)'

# Then flip the worker and replay
docker compose stop worker-transcribe
MOCK_TRANSCRIBE=false docker compose up -d worker-transcribe
```

### No-audio path

Re-run the mock test with `"has_audio": false` in `extract.json` and no
`audio.wav` uploaded. Expect a header-only `transcript.vtt`,
`stages.transcribe.status = completed`, and a message on `dayreel-package`.

---

## Verification Checklist

- [ ] Mock path produces `transcript.vtt` in under 2 s
- [ ] VTT starts with `WEBVTT`, cues are monotonic and non-overlapping
- [ ] `ffprobe` identifies the file as `webvtt`
- [ ] Final cue end time does not exceed `duration_seconds`
- [ ] Mock output is identical across two runs of the same `job_id`
- [ ] `has_audio: false` yields a header-only VTT and a **completed** stage
- [ ] Zero-segment real transcription also yields a header-only VTT
- [ ] `transcript.json` records the correct `engine`
- [ ] `GET /jobs/{id}` shows `stages.transcribe.status = "completed"`
- [ ] `metrics.transcribe_duration_ms` is populated
- [ ] Message lands on `dayreel-package`
- [ ] Replaying the message skips reprocessing
- [ ] `docker compose up` (no profile) does **not** start the transcriber
- [ ] With `--profile whisper`: `/health` goes 503 → 200, and real transcription of a speech clip returns plausible text
- [ ] Sidecar down + `MOCK_TRANSCRIBE=false` ⇒ message redelivers, does not fail permanently

---

## Claude Code Implementation Plan

### Approach: single agent, VTT-writer first

Build `internal/vtt` and its tests before anything else — it is pure, fully
unit-testable without Docker, and it is where the fiddly correctness lives
(timestamp formatting, overlap clipping). Everything downstream is plumbing.

### Execution order

```
1. internal/vtt/vtt.go + vtt_test.go   (Write) — go test, fast loop
2. transcribe.go (types + interface)   (Write)
3. mock.go                             (Write) — parallel with 2
4. whisper.go (HTTP client)            (Write) — parallel with 2
5. handler + no-audio short-circuit    (Write)
6. config.go, .env.example             (Edit)  — parallel with 5
7. cmd/worker/main.go registration     (Edit)
8. infra/transcriber/*                 (Write) — independent, can go anytime
9. docker-compose.yml                  (Edit)
10. Mock E2E                           (Bash)
11. Real E2E (--profile whisper)       (Bash)  — slow; do last
12. CONTEXT.md                         (Write)
```

Steps 2/3/4 and 5/6 are independent writes and can be issued together. Step 8 is
fully independent of the Go work.

### Subagent consideration

The Python sidecar (step 8) is genuinely independent — different language,
different container, contract already fixed above. It is the one piece in this
stage worth handing off in parallel if the Go side is taking a while. Everything
else shares Go types and is faster in one context.

### Potential blockers

| Blocker | Resolution |
|---------|------------|
| faster-whisper model download slow/blocked | Ship the mock path; add the sidecar behind the profile and treat it as optional for the demo |
| `ctranslate2` wheel unavailable for the base image arch (ARM Macs) | Use `python:3.11-slim` on `linux/amd64` via `platform:` in compose, or fall back to `openai-whisper` CPU |
| Sidecar OOM on larger models | `tiny.en` at int8 needs ~200 MB; do not move past `base.en` without checking container limits |
| Whisper hallucinates text over silence | `vad_filter=True` (already specified); if it persists, drop segments with `no_speech_prob > 0.6` |
| Captions appear offset in the player | Not a 5A bug — see the `X-TIMESTAMP-MAP` note in Stage 6A |

### Time estimate

- `internal/vtt` + tests: ~10 min
- Backends + handler: ~10 min
- Sidecar: ~10 min (plus model download wall-time)
- Wiring + E2E: ~8 min
- **Total: ~30 min mock-only, ~40 min including the real path**

---

## Open Items to Confirm After Stages 3A and 4A

Everything in Stage 4A's "Open Items" applies here too (handler interface, error
classification, `internal/queue/sqs.go`, multi-bucket `S3Client`, stage-level
DynamoDB updates, Redis invalidation from workers). Additionally:

1. **`extract.json` field names.** This plan reads `has_audio` and
   `duration_seconds`. If 4A's manifest lands with different names or moves
   these into DynamoDB, update the handler's read path.

2. **Visibility heartbeat helper.** 5A is the first stage where a single message
   can plausibly run for minutes (cold model load + a long clip). Confirm 3A
   provides `ChangeMessageVisibility` extension, or add it in this stage.

3. **Where `has_audio` should live.** Manifest-in-S3 is assumed. If 3A/4A end up
   putting stage outputs into DynamoDB instead, reading it from there saves an
   S3 GET per message. Decide once, apply to 6A as well.

4. **Whether the transcript belongs in the HLS bucket too.** 6A copies it to
   `dayreel-hls-output` with an `X-TIMESTAMP-MAP` header. Alternative: write
   both copies here. Left in 6A because only the packager knows the MPEG-TS
   offset it produced.

5. **Model choice.** `tiny.en` is assumed for speed. If transcript quality
   visibly hurts the demo, `base.en` is ~2× slower and ~145 MB — still viable on
   CPU. English-only models are assumed; drop the `.en` suffix if multilingual
   clips matter.

---

## Notes

- **The mock is a backend, not a stub.** Same interface, same VTT writer, same
  handler path. The only difference is where segments come from — which means
  the mock path exercises everything except inference itself.

- **Deterministic mock output** (seeded by `job_id`) makes the E2E test
  assertable and demos repeatable. Random filler text would make both worse.

- **Zero segments is a success, not a failure.** Silent audio, a music-only
  clip, and a failed transcription all look similar from here; only the first
  two are common, and failing the job for them would break the pipeline for
  perfectly good videos.

- **`transcript.json` is kept** even though nothing reads it today. It is the
  raw material for keyword search, chaptering, or a highlight-picker later, and
  it costs a few KB.

- **Deferred:** speaker diarization, word-level timestamps, translation,
  multi-language detection, and punctuation restoration.
