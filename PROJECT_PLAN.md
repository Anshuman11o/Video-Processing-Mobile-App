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
                          Presigned │                           │+ CloudFront URL
                          URLs      │                           │
                                    ▼                           │
┌─────────────────────────────────────────────────────────────────────────────┐
│                              BACKEND                                         │
│  Go API + Workers, Docker Compose locally, ECS Fargate on AWS               │
│                                                                              │
│  ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐    ┌─────────┐   │
│  │   API   │───▶│Validate │───▶│ Extract │───▶│Transcribe───▶│ Package │   │
│  │ (Go)    │    │ Worker  │    │ Worker  │    │ Worker  │    │ Worker  │   │
│  └─────────┘    └─────────┘    └─────────┘    └─────────┘    └─────────┘   │
│       │              │              │              │              │         │
│       └──────────────┴──────────────┴──────────────┴──────────────┘         │
│                                    │                                         │
│                    ┌───────────────┼───────────────┐                        │
│                    ▼               ▼               ▼                        │
│              ┌─────────┐    ┌─────────┐    ┌─────────┐                     │
│              │   S3    │    │DynamoDB │    │   SQS   │                     │
│              │ (video, │    │ (jobs)  │    │ (stages)│                     │
│              │  HLS)   │    └─────────┘    └─────────┘                     │
│              └─────────┘          │                                         │
│                    │              │                                         │
│                    └──────┬───────┘                                         │
│                           ▼                                                  │
│                     ┌─────────┐                                             │
│                     │  Redis  │ (status cache)                              │
│                     └─────────┘                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Design invariants:**
- Messages carry S3 pointers, never payloads
- Stages are stateless; DynamoDB is the only truth
- Idempotency by checking if output S3 key exists before processing
- Transient errors retry with backoff; permanent errors go to DLQ
- One DynamoDB item per job with a `stages` map

**Local-first:** LocalStack emulates S3/SQS/DynamoDB. Same Go binaries run locally
and on AWS via configurable endpoints. Docker Compose replaces Fargate locally.

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
│   │   └── worker/              # Worker entry point (all stages)
│   ├── internal/
│   │   ├── api/                 # HTTP handlers, routes
│   │   ├── models/              # DynamoDB schemas, job types
│   │   ├── worker/              # Stage implementations
│   │   │   ├── validate/
│   │   │   ├── extract/
│   │   │   ├── transcribe/
│   │   │   └── package/
│   │   ├── storage/             # S3 client wrapper
│   │   ├── queue/               # SQS client wrapper
│   │   └── cache/               # Redis client wrapper
│   ├── go.mod
│   ├── go.sum
│   ├── Dockerfile
│   └── CONTEXT.md
│
├── infra/
│   ├── docker-compose.yml       # Local stack: LocalStack, Redis, API, workers
│   ├── docker-compose.override.yml  # Local dev overrides
│   ├── localstack/
│   │   └── init-aws.sh          # Creates buckets, queues, tables on startup
│   ├── terraform/               # AWS deployment (later stage)
│   │   ├── main.tf
│   │   ├── variables.tf
│   │   └── outputs.tf
│   └── CONTEXT.md
│
├── config/
│   ├── aws-limits.md            # S3, SQS, DynamoDB constraints
│   ├── free-tier.md             # Free tier quotas and cost traps
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
    ├── test-upload.sh           # CLI test: upload video, poll status
    └── CONTEXT.md
```

---

## Development Sequence (Parallelized for Speed)

**Rationale:** DynamoDB access patterns must be settled before the API. SQS schemas
must be defined before workers. We maximize speed by running independent tracks
concurrently.

### Parallel Execution Map

```
PHASE 1: Foundation (run in parallel)
┌─────────────────────────────┐     ┌─────────────────────────────┐
│ 1A. DynamoDB schema         │     │ 1B. Docker Compose +        │
│     + SQS message schemas   │     │     LocalStack + Redis      │
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
| DynamoDB + SQS schemas | 1A | — | 1B | 30 min |
| Docker/LocalStack/Redis | 1B | — | 1A | 20 min |
| Go API skeleton | 2A | 1A, 1B | 2B | 30 min |
| RN app shell + picker | 2B | 1B | 2A, 3A-6A | 30 min |
| Validate worker | 3A | 2A | 2B | 20 min |
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
**Aim:** Lock DynamoDB and SQS contracts before any code.

**Deliverables:**
- DynamoDB table schema in `backend/internal/models/schema.go`
- SQS message types in `backend/internal/events/messages.go`
- Access pattern documentation

**Verification:**
- Go types compile
- JSON examples validate against schema

**Observable outcome:** Data contracts documented, team aligned.

#### Stage 1B: Local Infrastructure
**Aim:** Docker Compose brings up all services.

**Deliverables:**
- `infra/docker-compose.yml` with LocalStack, Redis
- `infra/localstack/init-aws.sh` creates S3 buckets, SQS queues, DynamoDB table

**Verification:**
```bash
docker-compose up -d
aws --endpoint-url=http://localhost:4566 s3 ls
aws --endpoint-url=http://localhost:4566 dynamodb list-tables
aws --endpoint-url=http://localhost:4566 sqs list-queues
```

**Observable outcome:** All AWS resources exist locally.

**SYNC POINT:** Both 1A and 1B complete before Phase 2 starts.

---

### Phase 2: Core Development (Parallel Tracks)

#### Track A: Backend

##### Stage 2A: Go API
**Aim:** HTTP endpoints for job lifecycle.

**Deliverables:**
- `POST /jobs` — creates job, returns presigned URLs
- `POST /jobs/{id}/complete` — signals upload done, triggers SQS
- `GET /jobs/{id}` — returns job status

**Verification:**
```bash
curl -X POST localhost:8080/jobs -d '{"filename":"test.mp4","size":1000000}'
```

**Observable outcome:** API responds, job in DynamoDB.

##### Stage 3A: Validate Worker
**Aim:** First pipeline stage processes videos.

**Deliverables:**
- SQS consumer polls validate queue
- ffprobe checks codec/duration
- Remux to faststart MP4
- Update job status in DynamoDB

**Verification:**
```bash
aws --endpoint-url=http://localhost:4566 s3 cp test.mp4 s3://raw-videos/job-123/input.mp4
# Send SQS message or let S3 event trigger
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
ffplay http://localhost:4566/hls-output/job-123/master.m3u8
```

**Observable outcome:** **Backend E2E works.** Video in → HLS out.

---

#### Track B: Mobile (runs parallel to Track A)

##### Stage 2B: Mobile Shell
**Aim:** RN app with video picker and job list.

**Deliverables:**
- React Native project scaffolded
- Video picker (react-native-image-picker)
- Job list screen (mock data initially)
- API client stub

**Verification:**
- App runs in Android emulator
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

---

### Phase 4: Polish (Parallel)

#### Stage 8A: Background Upload
**Aim:** Upload survives app kill.

**Deliverables:**
- Kotlin WorkManager native module
- Chunked upload with ETag persistence
- Resume from last successful part

**Verification:**
- Start upload, kill app, reopen
- Upload continues from where it left off

#### Stage 8B: HLS Playback
**Aim:** Play reels in app.

**Deliverables:**
- react-native-video with ExoPlayer
- Play completed reels from CloudFront/LocalStack

**Verification:**
- Completed job shows play button
- Tap plays HLS stream with captions

**FINAL:** Both 8A and 8B complete = **Full E2E demo.**

---

### Phase 5: Deployment (Optional)

#### Stage 10: Terraform
**Aim:** Deploy to real AWS.

**Deliverables:**
- VPC with public subnets (no NAT Gateway)
- VPC endpoints for S3, DynamoDB
- ECS Fargate tasks for API + workers
- CloudFront distribution

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
- SQS: 256KB message limit, 12hr max visibility timeout
- DynamoDB: 400KB item limit, 1KB RCU, 1KB WCU

**config/free-tier.md:**
- S3: 5GB storage, 20,000 GET, 2,000 PUT
- DynamoDB: 25 RCU, 25 WCU, 25GB storage
- Lambda: 1M requests, 400,000 GB-seconds
- SQS: 1M requests
- **COST TRAP:** NAT Gateway ~$32/month. Use public subnets + VPC endpoints.

---

## Open Questions

1. **Test videos:** Do you have sample videos, or should we generate/download them?
   Ideally 3-5 clips: various codecs (H.264, HEVC), durations (10s, 60s), resolutions.

2. **HLS local playback:** We'll try LocalStack S3 first. If CORS or other issues
   block ExoPlayer, we'll flag and discuss (per your preference).

3. **Mock transcription:** Confirmed: implement both real faster-whisper and
   `MOCK_TRANSCRIBE=true` mode for fast iteration.

## Execution Strategy for 3-Hour Budget

With parallelization, here's what's achievable:

| Time | Track A (Backend) | Track B (Mobile) |
|------|-------------------|------------------|
| 0:00-0:30 | 1A: DynamoDB + SQS schemas | 1B: Docker/LocalStack |
| 0:30-1:00 | 2A: Go API skeleton | 2B: RN app shell |
| 1:00-1:20 | 3A: Validate worker | (continue 2B) |
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
