# DayReel Project Plan

## Context

DayReel is an offline-first mobile video capture app with a backend processing
pipeline. Users record short clips, the app queues and uploads them via resumable
chunked transfer, and a backend pipeline validates, extracts, transcribes, and
packages clips into adaptive-bitrate HLS reels.

**Time budget: ~3 hours.** This plan prioritizes the narrowest possible E2E demo
path. Expand incrementally once the core flow works.

---

## Architecture Summary

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              MOBILE APP                                      │
│  React Native + TypeScript                                                   │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐     │
│  │ Video Picker │→ │ Local Queue  │→ │ Upload Worker│→ │ HLS Player   │     │
│  │ (Gallery)    │  │ (AsyncStore) │  │ (Kotlin WM)  │  │ (ExoPlayer)  │     │
│  └──────────────┘  └──────────────┘  └──────────────┘  └──────────────┘     │
└─────────────────────────────────────────────────────────────────────────────┘
                                    │                           ▲
                          POST /jobs│                           │GET /jobs/{id}
                          Presigned │                           │+ HLS URL (S3)
                          URLs      │                           │
                                    ▼                           │
┌─────────────────────────────────────────────────────────────────────────────┐
│                              BACKEND                                        │
│  Two Go processes: `go run ./cmd/api` and `go run ./cmd/worker`             │
│                                                                             │
│  ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐    │
│  │   API   │───▶│Validate │───▶│ Extract │───▶│Transcribe───▶│ Package │    │
│  │ (Go)    │    │ Stage   │    │ Stage   │    │ Stage   │    │ Stage   │    │
│  └────┬────┘    └─────────┘    └─────────┘    └─────────┘    └─────────┘    │
│       │              │              │              │              │         │
│       │              └──────────────┴──────────────┴──────────────┘         │
│       │                             │                                       │
│       │  in-process                 │  claim / ack / dead-letter            │
│       │  TTL cache                  ▼                                       │
│       │  (status)         ┌───────────────────┐                             │
│       │                   │  QUEUE (pluggable)│  QUEUE_DRIVER picks one:    │
│       │                   │  (validate,       │   sqlite → data/queue.db    │
│       │                   │   extract,        │     (default, one file)     │
│       │                   │   transcribe,     │   sqs → real Amazon SQS     │
│       │                   │   package, dlq)   │  Same five queue names.     │
│       │                   └───────────────────┘                             │
│       │                                                                     │
│       └──────────────┬──────────────┐                                       │
│                      ▼              ▼                                       │
│                ┌─────────┐    ┌─────────┐        ── real AWS ──             │
│                │   S3    │    │DynamoDB │                                   │
│                │ (video, │    │ (jobs)  │                                   │
│                │  HLS)   │    └─────────┘                                   │
│                └─────────┘                                                  │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Design invariants:**
- Messages carry S3 pointers, never payloads
- Stages are stateless; DynamoDB is the only truth
- Idempotency by checking if output S3 key exists before processing
- Transient errors retry with backoff; permanent errors go to DLQ
- One DynamoDB item per job with a `stages` map

Dropping SQS did not drop any of these. The SQLite queue implements the same
contract — claim with a visibility timeout, ack on success, dead-letter after N
deliveries — which is the part the pipeline actually depends on.

**The queue is pluggable.** `QUEUE_DRIVER` selects it at startup:

| Driver | When |
|--------|------|
| `sqlite` (default) | Everything, until the constraint below bites. One file, no account, no per-request cost. |
| `sqs` | Workers on more than one machine, or the queue has to outlive the box. Billed per request; the queues must be created first with `make sqs-setup`. |

Both implement `queue.Queue`, both dead-letter explicitly rather than trusting a
broker policy, and `queue.FromConfig` is the only code that knows there are two
— the API, the runner and all four stages take the interface. Where the
semantics genuinely differ (SQS's 10-message and 20-second ceilings, its
client-computed lease deadline, its per-request cost) is written down in
`backend/internal/queue/CONTEXT.md`.

**Local-first:** the entire local stack is two Go processes and a file. S3 and
DynamoDB are real AWS from the start, so there is no emulator to install and no
emulator-vs-real behaviour gap to debug. The same binaries run on a VM if we
deploy — and the one piece that has to change for a second host, the queue, is
now a configuration value rather than a port.

**Deferred (deliberate, not an oversight):**
- **No CDN.** At this scale — a handful of short clips — CloudFront is cost and
  complexity with nothing to show for it. HLS is served directly from the HLS
  bucket, which means that bucket has to be readable by the player. A CDN in front
  of a private bucket is the first thing to add if this ever gets real viewers.
- **No managed container hosting.** ECS Fargate buys autoscaling we don't need. The
  Go binaries are identical either way, so this is a hosting choice we can revisit
  without touching application code.
- **No Docker, no LocalStack, no Redis.** The project processes a handful
  of 10–60 second clips. Containers, an AWS emulator, and a separate cache server
  cost 1.5–2.5 GB of RAM and buy nothing at that scale. Redis becomes an
  in-process TTL cache, and Compose has nothing left to orchestrate. See
  `infra/CONTEXT.md`. SQS is the exception: it came back as a selectable driver
  rather than the only option, because "the workers must share a filesystem with
  the API" is a real ceiling and the emulator was never what made SQS expensive.
- **No S3 event notifications.** The API enqueues the validate message on
  `POST /jobs/{id}/complete` rather than S3 doing it. This started as a SQLite
  constraint — real S3 cannot notify a file — and stays that way on SQS too, so
  the pipeline starts the same way on both drivers instead of having two
  entry paths to reason about.

---

## Directory Structure

```
/
├── PROJECT_PLAN.md              # This file
├── TROUBLESHOOTING.md           # Running log of issues and fixes
├── CLAUDE.md                    # Agent instructions (if needed)
│
├── mobile/                      # React Native app
│   ├── android/                 # Android native code + Kotlin WorkManager module
│   ├── ios/                     # iOS native code (stub for now)
│   ├── src/
│   │   ├── components/          # UI components
│   │   ├── screens/             # Screen components
│   │   ├── services/            # Upload queue, API client
│   │   └── native/              # Native module bridge
│   ├── package.json
│   └── CONTEXT.md
│
├── backend/
│   ├── cmd/
│   │   ├── api/                 # HTTP API entry point
│   │   └── worker/              # Worker entry point (WORKER_STAGE picks the stage)
│   ├── internal/
│   │   ├── api/                 # HTTP handlers, routes
│   │   ├── models/              # DynamoDB schemas, job types
│   │   ├── events/              # Stage message contract, queue names, manifests
│   │   ├── media/               # ffmpeg/ffprobe wrappers, HLS ladder, playlists
│   │   ├── transcribe/          # Whisper and mock transcribers, WebVTT
│   │   ├── worker/              # Stage implementations + the runner
│   │   │   ├── validate/
│   │   │   ├── extract/
│   │   │   ├── transcribe/
│   │   │   └── packager/
│   │   ├── storage/             # S3 client wrapper
│   │   ├── queue/               # Broker: SQLite (default) or SQS, one interface
│   │   └── cache/               # In-process TTL cache for job status
│   ├── go.mod
│   ├── go.sum
│   └── CONTEXT.md
│
├── data/
│   └── queue.db                 # SQLite queue (gitignored, created on first run)
│
├── infra/
│   ├── terraform/               # AWS deployment (later stage, not written yet)
│   │   ├── main.tf
│   │   ├── variables.tf
│   │   └── outputs.tf
│   └── CONTEXT.md               # Why there is no compose stack here
│
├── config/
│   ├── aws-limits.md            # S3 and DynamoDB constraints, queue settings
│   ├── free-tier.md             # Cost constraints, $20 budget (no free tier)
│   └── CONTEXT.md
│
├── docs/
│   ├── stage-plans/
│   │   ├── TEMPLATE.md          # Stage plan template
│   │   └── stage-N-*.md         # Individual stage plans (written before each stage)
│   └── CONTEXT.md
│
└── scripts/
    ├── dev-setup.sh             # One-command local setup
    ├── aws-sqs-setup.sh         # Create/inspect/delete the SQS queues (QUEUE_DRIVER=sqs)
    ├── test-upload.sh           # CLI test: upload video, poll status
    └── CONTEXT.md
```

---

## Development Sequence (Parallelized for Speed)

**Rationale:** DynamoDB access patterns must be settled before the API. Stage
message schemas must be defined before workers. We maximize speed by running
independent tracks concurrently.

**Status:** stages 1A through 6A are **built**, not planned — the pipeline runs
end to end from a queued validate message to an HLS master playlist. Stages 7
and 8 are the remaining work.

### Parallel Execution Map

```
PHASE 1: Foundation (run in parallel)
┌─────────────────────────────┐     ┌─────────────────────────────┐
│ 1A. DynamoDB schema         │     │ 1B. AWS account setup:      │
│     + stage message schemas │     │     buckets + jobs table    │
│     (~30 min)               │     │     (~20 min)               │
└─────────────┬───────────────┘     └─────────────┬───────────────┘
              │                                   │
              └─────────────┬─────────────────────┘
                            ▼
                    PHASE 1 SYNC POINT
                            │
         ┌──────────────────┴──────────────────┐
         ▼                                     ▼
PHASE 2: TRACK A (Backend)            TRACK B (Mobile)
┌─────────────────────────────┐     ┌─────────────────────────────┐
│ 2A. Go API skeleton         │     │ 2B. React Native app shell  │
│     (~30 min)               │     │     + video picker          │
└─────────────┬───────────────┘     │     (~30 min)               │
              │                     └─────────────┬───────────────┘
              ▼                                   │
┌─────────────────────────────┐                   │
│ 3A. Validate worker         │                   │
│     (~20 min)               │                   │
└─────────────┬───────────────┘                   │
              │                                   │
              ▼                                   │
┌─────────────────────────────┐                   │
│ 4A. Extract worker          │                   │
│     (~20 min)               │                   │
└─────────────┬───────────────┘                   │
              │                                   │
              ▼                                   │
┌─────────────────────────────┐                   │
│ 5A. Transcribe worker       │                   │
│     (~20 min)               │                   │
└─────────────┬───────────────┘                   │
              │                                   │
              ▼                                   │
┌─────────────────────────────┐                   │
│ 6A. Package worker          │                   │
│     (~20 min)               │                   │
│ *** BACKEND E2E WORKS ***   │                   │
└─────────────┬───────────────┘                   │
              │                                   │
              └─────────────┬─────────────────────┘
                            ▼
                    PHASE 2 SYNC POINT
                            │
                            ▼
              ┌─────────────────────────────┐
              │ 7. Upload integration       │
              │    (connect mobile→backend) │
              │    (~20 min)                │
              └─────────────┬───────────────┘
                            │
         ┌──────────────────┴──────────────────┐
         ▼                                     ▼
┌─────────────────────────────┐     ┌─────────────────────────────┐
│ 8A. Kotlin WorkManager      │     │ 8B. HLS playback in app     │
│     (background upload)     │     │     (ExoPlayer)             │
│     (~30 min)               │     │     (~20 min)               │
└─────────────┬───────────────┘     └─────────────┬───────────────┘
              │                                   │
              └─────────────┬─────────────────────┘
                            ▼
              ┌─────────────────────────────┐
              │ 9. Full E2E demo working    │
              │    *** SHIP IT ***          │
              └─────────────┬───────────────┘
                            │
                            ▼
              ┌─────────────────────────────┐
              │ 10. Terraform (optional)    │
              │     AWS deployment          │
              └─────────────────────────────┘
```

### Dependency Table

| Task | ID | Depends On | Can Parallel With | Est. Time |
|------|-----|------------|-------------------|-----------|
| DynamoDB + message schemas | 1A | — | 1B | 30 min |
| AWS buckets + jobs table | 1B | — | 1A | 20 min |
| Go API skeleton | 2A | 1A, 1B | 2B | 30 min |
| RN app shell + picker | 2B | 1B | 2A, 3A-6A | 30 min |
| SQLite queue | 3B | 1A | 2B, 3A | 30 min |
| Validate worker | 3A | 2A, 3B | 2B | 20 min |
| Extract worker | 4A | 3A | 2B | 20 min |
| Transcribe worker | 5A | 4A | 2B | 20 min |
| Package worker | 6A | 5A | 2B | 20 min |
| Upload integration | 7 | 2A, 2B, 6A | — | 20 min |
| Kotlin WorkManager | 8A | 7 | 8B | 30 min |
| HLS playback | 8B | 6A, 7 | 8A | 20 min |
| Terraform | 10 | 6A | 8A, 8B | 30 min |

### Time Estimates

| Execution Mode | Total Time | Notes |
|----------------|------------|-------|
| **Fully serial** | ~4.5 hours | No parallelization |
| **2 parallel tracks** | ~2.5 hours | Backend + Mobile concurrent |
| **Aggressive parallel** | ~2 hours | Max concurrency at every phase |

---

## Agile Staging (Parallelized)

Each stage ends with something runnable and verifiable. Stages within the same
phase can run concurrently.

### Phase 1: Foundation (Parallel)

#### Stage 1A: Data Schemas
**Aim:** Lock DynamoDB and stage-message contracts before any code.

**Deliverables:**
- DynamoDB table schema in `backend/internal/models/schema.go`
- Stage message types in `backend/internal/events/messages.go`
- Access pattern documentation

**Verification:**
- Go types compile
- JSON examples validate against schema

**Observable outcome:** Data contracts documented, team aligned.

#### Stage 1B: AWS Resources
**Aim:** The three buckets and the jobs table exist in a real AWS account.

**Deliverables:**
- `dayreel-raw-videos`, `dayreel-processed`, `dayreel-hls-output` buckets with CORS
- `dayreel-jobs` DynamoDB table (pk/sk, PAY_PER_REQUEST)
- `scripts/dev-setup.sh` checks all of it, plus toolchain and credentials

**Verification:**
```bash
./scripts/dev-setup.sh
make verify
```

**Observable outcome:** `make verify` prints the caller ARN, three OK buckets,
and an ACTIVE table.

> Superseded: this stage originally stood up Docker Compose with LocalStack and
> Redis. Both are gone; see `infra/CONTEXT.md`.

**SYNC POINT:** Both 1A and 1B complete before Phase 2 starts.

---

### Phase 2: Core Development (Parallel Tracks)

#### Track A: Backend

##### Stage 2A: Go API
**Aim:** HTTP endpoints for job lifecycle.

**Deliverables:**
- `POST /jobs` — creates job, returns presigned URLs
- `POST /jobs/{id}/complete` — signals upload done, enqueues the validate message
- `GET /jobs/{id}` — returns job status (in-process TTL cache in front of DynamoDB)

**Verification:**
```bash
curl -X POST localhost:8080/jobs -d '{"filename":"test.mp4","size":1000000}'
```

**Observable outcome:** API responds, job in DynamoDB.

##### Stage 3B: Local Queue
**Aim:** Replace SQS with a self-hosted SQLite queue.

See `docs/stage-plans/stage-3b-local-queue.md`. Runs in parallel with 2A; blocks
3A.

##### Stage 3A: Validate Worker
**Aim:** First pipeline stage processes videos.

**Deliverables:**
- Worker claims from the validate queue in `data/queue.db`
- ffprobe checks codec/duration
- Remux to faststart MP4
- Update job status in DynamoDB

**Verification:**
```bash
aws s3 cp test.mp4 s3://dayreel-raw-videos/job-123/input.mp4
# Enqueue via POST /jobs/{id}/complete, then watch the row move:
make queue-peek
curl localhost:8080/jobs/job-123  # status: validate:complete
```

##### Stage 4A: Extract Worker
**Aim:** Extract keyframes and audio.

**Deliverables:**
- FFmpeg scene-detect keyframes → JPEG
- Audio demux → 16kHz mono WAV

**Verification:** S3 contains `frames/*.jpg` and `audio.wav`

##### Stage 5A: Transcribe Worker
**Aim:** Speech-to-text with timestamps.

**Deliverables:**
- faster-whisper integration
- `MOCK_TRANSCRIBE=true` mode for fast dev
- Output WebVTT with timings

**Verification:** S3 contains `transcript.vtt`

##### Stage 6A: Package Worker
**Aim:** HLS output with captions.

**Deliverables:**
- FFmpeg HLS ladder (3 bitrate tiers)
- 6-second segments
- Master playlist with VTT subtitle track

**Verification:**
```bash
ffplay "$HLS_BASE_URL/job-123/master.m3u8"
```

**Observable outcome:** **Backend E2E works.** Video in → HLS out.

---

#### Track B: Mobile (runs parallel to Track A)

##### Stage 2B: Mobile Shell
**Aim:** RN app with video picker and job list.

**Deliverables:**
- React Native project scaffolded
- ~~Video picker (react-native-image-picker)~~ — the picker is
  `@react-native-documents/picker`. It went via `react-native-document-picker`,
  which **does not compile against RN 0.87** and is deprecated with no successor.
- Job list screen (mock data initially)
- API client stub

**Verification:**
- App runs in Android emulator *(first achieved 2026-08-13, at Stage 7)*
- Can pick video from gallery
- Job list renders

**Observable outcome:** Mobile app shell functional.

**SYNC POINT:** Track A (through 6A) and Track B (2B) complete before Stage 7.

---

### Phase 3: Integration

#### Stage 7: Upload Integration
**Aim:** Connect mobile to backend.

**Deliverables:**
- API client calls real backend
- Foreground upload using presigned URLs
- Job status polling and display

**Verification:**
- Pick video in app
- See upload progress
- See job move through pipeline stages

**Observable outcome:** Mobile uploads work, jobs process.

> **DONE, 2026-08-13.** Verified on an Android emulator: a 14.9 MB video picked
> in the app, uploaded as 3 real multipart parts, processed to `completed`.
> LocalStack only. See `docs/stage-plans/stage-7-upload-integration.md`.

---

### Phase 4: Polish (Parallel)

#### Stage 8A: Background Upload
**Aim:** Upload survives app kill.

**Deliverables:**
- Kotlin WorkManager native module
- ~~Chunked upload with ETag persistence~~ — **superseded 2026-08-13.** 8A
  **[DECIDE 2]** settled the opposite: resume state is **server-authoritative via
  `ListParts`**, and the client persists identifiers only, never an ETag. A
  client-held ETag array is wrong in the one case that matters — killed after the
  last part but before `POST /complete` — and asking S3 what landed is right in
  all of them.
- Resume from last successful part

**Verification:**
- Start upload, kill app, reopen
- Upload continues from where it left off

> **NOT DONE.** The backend half is built and tested; the WorkManager module
> compiles and ships in the APK but **JavaScript cannot resolve it**, so the app
> silently falls back to the foreground uploader. The verification above has
> never been run.

#### Stage 8B: HLS Playback
**Aim:** Play reels in app.

**Deliverables:**
- react-native-video with ExoPlayer
- Play completed reels from ~~CloudFront/LocalStack~~ **the HLS bucket on S3
  directly, with no CDN.** CloudFront was never testable here — it is LocalStack
  Pro-only (`config/free-tier.md`) — and it has since been dropped from the
  architecture outright as unwarranted at this scale. LocalStack is gone too, so
  what a player actually fetches is a real S3 bucket.

**Verification:**
- Completed job shows play button
- Tap plays HLS stream with captions

> **DONE, 2026-08-13** — a reel plays in-app with captions rendering, on
> LocalStack, on one emulator. The caption *offset* on ExoPlayer is still
> unmeasured; the published figures are AVFoundation's.

**FINAL:** Both 8A and 8B complete = **Full E2E demo.** *8B is complete; 8A is
not, so this is not yet claimable.*

---

### Phase 5: Deployment (Optional)

#### Stage 10: Terraform
**Aim:** Deploy to real AWS.

**Deliverables:**
- VPC with public subnets (no NAT Gateway)
- VPC endpoints for S3, DynamoDB
- A single small VM running the same two Go binaries (API + worker) that run
  locally — identical binaries, different host, no containers
- HLS bucket readable by the player, served directly (no CDN)
- The SQLite queue file lives on the VM's disk. Fine for one host; a second host
  is the point at which it has to be swapped for a real broker.

**Verification:**
- `terraform apply` succeeds
- E2E test passes against production URL

---

## Metrics Plan

Instrumentation hooks for the four metrics. **Add these as you build, don't retrofit.**

| Metric | Where to Instrument | Implementation |
|--------|---------------------|----------------|
| **Cost per 100 clips** | Package worker completion | Log S3 storage, compute time; aggregate externally |
| **p95 E2E latency** | API: timestamp at job creation; Package worker: timestamp at completion | Store both in DynamoDB, compute delta |
| **Sustained throughput** | Worker completion events | Count completions per minute via CloudWatch/logs |
| **Upload success rate** | Mobile upload service | Track attempts, retries, failures in app analytics |

**Instrumentation pattern (non-blocking):**
```go
// Fire-and-forget with bounded buffer
select {
case metrics <- Metric{Name: "worker.duration_ms", Value: elapsed}:
default:
    // Buffer full, drop silently
}
```

---

## Documentation Conventions

### CONTEXT.md in Every Directory

Every directory containing code gets a `CONTEXT.md` explaining:
- What this component is
- What each file does
- How it fits the wider architecture
- Non-obvious decisions

Updated in the same commit as the code. Written so a different agent can pick up
mid-project.

### TROUBLESHOOTING.md at Repo Root

Running log of every non-trivial problem hit:
- **Symptom:** What went wrong
- **Cause:** Why it happened
- **Fix:** How it was resolved

Append as we go. Same problem never debugged twice.

### config/ Limits and Quotas

**config/aws-limits.md:**
- S3: 5MB min part size, 10,000 max parts, 5TB max object
- DynamoDB: 400KB item limit, 1KB RCU, 1KB WCU
- Queue: 5-minute visibility timeout, maxDeliveries=3 — same on both drivers

**config/free-tier.md:**
- **No free tier on this account** (confirmed 2026-08-12). Everything bills from
  the first request; the quota table that used to sit here was removed.
- **Hard budget: $20 total.** Test clips ≤10s. Only two AWS services are used —
  S3 and DynamoDB — and both bill per request, so development traffic is pennies.
- Per-request costs are negligible at this scale (a job is a fraction of a cent).
  **Per-hour costs are the whole risk.**
- **COST TRAPS, each over budget on its own:** NAT Gateway ~$32/mo,
  Fargate 4 workers 24/7 ~$115/mo, ElastiCache ~$12/mo. None of the three is in
  the architecture any more, and none should come back without a decided
  teardown time.

---

## Open Questions

1. **Test videos:** Do you have sample videos, or should we generate/download them?
   Ideally 3-5 clips: various codecs (H.264, HEVC), durations (10s, 60s), resolutions.

2. **HLS playback:** ~~We'll try LocalStack S3 first. If CORS or other issues
   block ExoPlayer, we'll flag and discuss.~~ **ANSWERED 2026-08-13:** it works.
   ExoPlayer plays the master from LocalStack, with the subtitle rendition listed
   and rendering. No CORS problem arose.

   **This said nothing about real AWS, and that is now the live question.**
   LocalStack served unsigned GETs to any bucket, so playback there exercised no
   access model at all. The emulator has since been removed, and reels stream
   straight off the real HLS bucket with no CDN in front — which means that
   bucket has to be player-readable and CORS-configured for range requests, and
   neither has been asserted. Playlists cannot be presigned (they reference
   segments by relative path), so this needs a bucket access model rather than a
   signed URL: `docs/aws-public-hls.md`, opt-in and off by default.

3. **Mock transcription:** Confirmed: implement both real faster-whisper and
   `MOCK_TRANSCRIBE=true` mode for fast iteration.

## Execution Strategy for 3-Hour Budget

With parallelization, here's what's achievable:

| Time | Track A (Backend) | Track B (Mobile) |
|------|-------------------|------------------|
| 0:00-0:30 | 1A: DynamoDB + message schemas | 1B: AWS buckets + table |
| 0:30-1:00 | 2A: Go API skeleton | 2B: RN app shell |
| 1:00-1:20 | 3B: SQLite queue, 3A: Validate worker | (continue 2B) |
| 1:20-1:40 | 4A: Extract worker | — |
| 1:40-2:00 | 5A: Transcribe (mock mode) | — |
| 2:00-2:20 | 6A: Package worker | — |
| 2:20-2:40 | 7: Upload integration | 7: Upload integration |
| 2:40-3:00 | Verify E2E | Verify E2E |

**3-hour deliverable:** Pick video → upload → process (with mock transcription) → play HLS in VLC.

**Deferred to later:** Background upload (8A), in-app playback (8B), real transcription, Terraform.

---

## Per-Stage Plan Convention

Stage plans live in `/docs/stage-plans/`. Write each plan **immediately before**
starting that stage. See `TEMPLATE.md` for the required format.

Each stage plan must specify:
1. **Aim:** What this stage achieves
2. **Components:** Exact services/modules touched
3. **Boundaries:** Input/output at each boundary with data shapes
4. **Files:** Files to create or modify
5. **Tasks:** Ordered implementation steps
6. **Test:** The test that proves the stage works
7. **Verification:** Observable outcome you can verify yourself
