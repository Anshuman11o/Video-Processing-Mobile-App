# Stage 5A: Transcribe Worker

> Status: **draft — not reviewed.** Four **[DECIDE]** items below are open.
> **[DECIDE 1]** (how Whisper is invoked from a Go worker) and **[DECIDE 2]**
> (which image carries it) are the ones that matter; they are entangled, and
> both are constrained by an 8 GB Docker disk ceiling. No code until reviewed.

## Aim

Consume `dayreel-transcribe`, turn the extracted audio into timed text with
Whisper, write `{job_id}/transcript.vtt` to `dayreel-processed`, and hand off to
`dayreel-package`.

**This stage is not like 3A or 4A.** Those shell out to a binary that was already
in the image and finish in about a second. This one introduces a **machine
learning runtime, a model file, and a non-Go toolchain** into a project that has
so far been one static Go binary plus ffmpeg. The transcription code itself is
small. Everything expensive is in packaging and runtime shape, which is why the
open decisions are about images and process boundaries rather than about
transcription.

Two things are already settled and are not reopened here:

- **Whisper, not AWS Transcribe.** LocalStack Community does not emulate
  Transcribe, and there is no free tier on this account. Local inference is
  compute on hardware already paid for.
- **`MOCK_TRANSCRIBE=true`** must exist, per `PROJECT_PLAN.md:341`. Given the
  budget rules in `config/free-tier.md` — 1–5 real runs, then the project closes
  — mock mode is not a convenience, it is the **default** way this stage runs
  during development.

## Components

| Component | Action |
|-----------|--------|
| `backend/internal/worker/transcribe/` | Create — the `Stage` implementation |
| `backend/internal/transcribe/` | Create — Whisper invocation + VTT writing, behind an interface |
| `backend/Dockerfile.worker-transcribe` | Create — **probably** (**[DECIDE 2]**) |
| `infra/docker-compose.yml` | Modify — add `worker-transcribe` |
| `backend/cmd/worker/main.go` | Modify — one `case models.StageTranscribe` |
| `backend/internal/worker/runner.go` | **No change** — but see the timeout risk below |
| `backend/internal/events/` | **No change** — the manifest type already carries what is needed |
| `infra/localstack/init-aws.sh` | **No change** — `dayreel-transcribe` and `dayreel-package` already exist |

## Boundaries

### Inbound: the manifest, not the audio

`stage-1a-data-schemas.md:171` says transcribe's input is
`processed/{job_id}/audio.wav`. **That is now out of date.** Stage 4A resolved
its multi-artifact problem by declaring `extract.json` the canonical output, so
what actually arrives is:

```json
{
  "job_id": "550e8400-...",
  "stage": "transcribe",
  "input": {"bucket": "dayreel-processed", "key": "550e8400-.../extract.json"},
  "attempt": 1,
  "timestamp": "2026-08-12T10:32:11Z",
  "trace_id": "<inherited from extract>"
}
```

This drift is mine, introduced in 4A, and this plan is where it gets reconciled
rather than discovered. **Stage 5A must fetch and parse the manifest first**,
then read `audio.key` from it. `stage-1a` should be corrected as part of this
stage's work, not left to contradict the code.

The upside of the indirection: the manifest carries `duration_seconds` and the
audio's sample rate and channel count, so this stage never re-probes the media
to learn things extract already knew.

### The silent-clip obligation, inherited from 4A

4A's **[DECIDE 4]** is a debt this stage now owes. A clip with no audio produces
**no `audio.wav` object at all** and records:

```json
"audio": {"present": false}
```

**If `audio.present` is false, this stage must not invoke the model.** It writes
a valid empty WebVTT and completes normally. Treating a silent clip as a failure
would contradict a decision validate made deliberately (`validate.go:117`, "a
silent clip is legitimate") and fail a job the pipeline explicitly accepted.

This is roughly six lines. It is called out here because 4A predicted it would
otherwise be *discovered* in 5A, most likely as a crash on a missing S3 key.

### Outbound: `StageMessage` to `dayreel-package`

```json
{
  "job_id": "550e8400-...",
  "stage": "package",
  "input": {"bucket": "dayreel-processed", "key": "550e8400-.../transcript.vtt"},
  "attempt": 1,
  "timestamp": "2026-08-12T10:34:02Z",
  "trace_id": "<inherited>"
}
```

Single output, so 4A's manifest problem does not recur — `transcript.vtt` is the
whole product of this stage.

Note 6A needs **both** `validated.mp4` and `transcript.vtt`
(`stage-1a-data-schemas.md:175-178`). It derives the former from `job_id`, the
same way this stage could have derived the audio key. Pointing at the transcript
is the more informative choice and matches the "input.key is what this stage
produced" convention.

### S3 Objects

| Bucket | Key | Content |
|--------|-----|---------|
| `dayreel-processed` | `{job_id}/extract.json` | Manifest from 4A (input) |
| `dayreel-processed` | `{job_id}/audio.wav` | 16 kHz mono PCM (input, via the manifest) |
| `dayreel-processed` | `{job_id}/transcript.vtt` | WebVTT transcript (output) |

### DynamoDB

`stages.transcribe` through the existing runner methods. No schema change.
`Metrics.TranscribeDurationMs` (`job.go:79`) already exists and is currently
never written — worth populating here, since this is the first stage where
duration varies enough to be interesting.

---

### [DECIDE 1] — how a Go worker invokes Whisper

Every prior stage shells out to a binary baked into the image. Whisper does not
fit that shape cleanly: `faster-whisper` is a **Python** library, and the worker
is a static Go binary built with `CGO_ENABLED=0` on Alpine
(`Dockerfile.worker:7`).

#### Options

**(A) `faster-whisper` via a Python CLI script, exec'd like ffmpeg.**
A small `transcribe.py` in the image; Go runs it with `exec.CommandContext` and
reads JSON from stdout. Matches the existing `media` package shape exactly.
- Model loads on **every invocation** — several seconds of startup per job on
  top of inference.
- Alpine is a real problem: CTranslate2 wheels are `manylinux`/glibc, and musl
  builds are not officially published. This likely forces a `python:3.11-slim`
  (Debian) base for this image.

**(B) `whisper.cpp` binary, exec'd like ffmpeg.**
A C++ binary and a GGML model file. No Python at all, and it builds against musl,
so **Alpine survives**.
- Closest possible fit to the existing pattern — it is literally "another
  binary in the image", exactly like ffmpeg.
- Contradicts `PROJECT_PLAN.md:340`, which names faster-whisper.
- Requires compiling it in a builder stage, or vendoring a prebuilt binary.

**(C) A Whisper HTTP sidecar service.**
A separate container holding Python, the model, and a tiny HTTP server; the Go
worker POSTs the audio and gets VTT or JSON back.
- **Model loads once at container start**, not per job. For a stage that may run
  repeatedly this is the difference between seconds and tens of seconds per job.
- The worker image stays exactly as it is — no Python, no model, no bloat, and
  validate/extract are untouched.
- Costs a new service, a network hop, a health check, and a second failure mode
  (sidecar down = transient failure, which is correct but must be classified).

**(D) Managed API (OpenAI Whisper API or similar).**
- Rejected. It needs credentials that do not exist, spends real money against a
  $20 ceiling, and breaks the offline-by-default posture. Listed only to record
  that it was considered and why it lost.

#### Recommendation: **(C), the sidecar — with (B) as the fallback.**

The deciding argument is not elegance, it is **where the model lives**. Options A
and B put a model file inside the image that *every* worker pulls, and reload it
on every single job. C loads it once per container and leaves the Go image
untouched.

It also keeps the ML runtime behind a boundary this project already understands —
an address and a timeout — rather than a Python toolchain wired into a Go build.
And it makes `MOCK_TRANSCRIBE` trivially honest: mock mode simply never dials the
sidecar, so the mock path exercises the same code up to the network call.

**(B) is the better answer if the sidecar's operational weight proves annoying
in practice** — it is the only option that keeps everything in one Alpine image
with no Python anywhere, and "another binary next to ffmpeg" is a genuinely good
fit for this codebase. It is a fallback rather than the recommendation only
because of per-job model reload.

Whichever is chosen, **the Go side is an interface**:

```go
type Transcriber interface {
    Transcribe(ctx context.Context, audioPath string) ([]Segment, error)
}
```

with a `mockTranscriber` and a real one. The stage depends on the interface, so
[DECIDE 1] can be revisited without touching the stage.

---

### [DECIDE 2] — which image carries the model (8 GB Docker ceiling)

Docker's disk allowance on this machine is **8 GB and cannot be raised without
restarting the container**. Current usage is ~2.6 GB of images plus build cache.
This is a hard constraint on model choice, not a theoretical one.

Approximate costs:

| Model | Size | Notes |
|---|---|---|
| `tiny` | ~75 MB | Poor accuracy; fine for pipeline plumbing |
| `base` | ~145 MB | Reasonable for short English clips |
| `small` | ~485 MB | Noticeably better |
| `medium` | ~1.5 GB | Slow on CPU |
| `large-v3` | ~3 GB | **Would not fit** alongside existing images |

Plus the runtime: Python + CTranslate2 + deps is roughly **0.8–1.2 GB** on a
Debian slim base; whisper.cpp is a few MB plus the model.

**Recommendation: a separate image for transcribe, and `base` as the default
model.**

One shared worker image was right while every stage needed exactly ffmpeg. It
stops being right the moment one stage needs a gigabyte the others never touch —
validate and extract would each carry a Whisper runtime they never invoke, and
every rebuild of any worker would push that layer around.

`base` because the pipeline is what is being proven here, not transcription
quality, and because test clips are **≤10 seconds** under the budget rules. Model
choice is one environment variable; it can be raised for a real run if the output
is visibly poor.

**Open sub-question:** bake the model into the image, or download on first run
into a mounted volume? Baking makes the image bigger but keeps runs offline and
repeatable; downloading keeps the image small but adds a network dependency and a
cold-start delay, and would re-download on every fresh container unless
volume-mounted. Leaning **baked**, for the same offline-determinism reason the
rest of the stack runs on LocalStack.

---

### [DECIDE 3] — the 300s visibility timeout becomes a real risk

Validate takes ~1s. Extract took ~1s on a 6s clip. **Transcription does not
behave like this.** On CPU, `base` runs roughly 2–5× faster than realtime, so a
10-minute clip — which `validate.DefaultLimits.MaxDuration` still permits — could
take **2–5 minutes**, against a **300s visibility timeout**.

When that timeout is exceeded, SQS redelivers while the first worker is still
running. Two workers then transcribe the same job concurrently, and the runner's
idempotency guard does not help: it checks whether the *output* exists, and the
output does not exist until the work finishes.

This is the first stage where that gap is reachable. Options:

- **(a) Raise the visibility timeout** for `dayreel-transcribe` specifically —
  one line in `init-aws.sh`. Blunt but effective, and honest about the fact that
  this stage is slower than the others.
- **(b) Heartbeat the visibility timeout** from the runner during long work
  (`ChangeMessageVisibility` on a ticker). Correct, general, and a **shared
  runner change** — which every stage then inherits, right after two silent bugs
  were found in that same runner.
- **(c) Lower `MaxDuration`** so long clips never reach this stage. Cheapest, and
  aligned with the ≤10s test-clip rule, but it changes product behaviour.

**Recommendation: (a) now, (c) alongside it, (b) only if needed.** Raise the
transcribe queue's timeout to 900s, and separately consider dropping
`MaxDuration` — `config/free-tier.md` already flags that ceiling as the only
guard between a mis-selected large file and a long billed run. Heartbeating is
the right long-term answer but is a shared-runner change, and this stage should
not be the reason one is made.

---

### [DECIDE 4] — what `MOCK_TRANSCRIBE=true` actually produces

Mock mode is the default development path, so what it emits matters more than it
sounds.

- **(a) A fixed stub** — one cue, "This is a mock transcript." Trivial, but the
  VTT is structurally unlike a real one, so it exercises nothing about timing.
- **(b) Duration-aware synthetic cues** — read `duration_seconds` from the
  manifest and emit a cue every ~3 seconds with plausible text. The output has
  realistic shape and cue count, so 6A and the mobile client get something
  representative to render.
- **(c) A recorded fixture** — a real Whisper output committed to the repo and
  replayed. Realistic, but tied to one specific clip and stale the moment it
  drifts.

**Recommendation: (b).** The point of mock mode here is to let 6A and Stage 7 be
built and demoed without spending model compute, and a single-cue transcript
would not exercise cue rendering, seeking, or overlap at all. It costs a few more
lines than (a) and is honest about being synthetic.

---

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/transcribe/transcribe.go` | Create | `Transcriber` interface, `Segment` type |
| `backend/internal/transcribe/mock.go` | Create | Duration-aware synthetic cues (**[DECIDE 4]**) |
| `backend/internal/transcribe/whisper.go` | Create | Real implementation (**[DECIDE 1]**) |
| `backend/internal/transcribe/vtt.go` | Create | `Segment` → WebVTT, with timestamp formatting |
| `backend/internal/transcribe/vtt_test.go` | Create | Formatting and edge cases; pure, no model |
| `backend/internal/worker/transcribe/transcribe.go` | Create | The `Stage`: manifest → audio → transcribe → VTT |
| `backend/internal/worker/transcribe/transcribe_test.go` | Create | Silent-clip short-circuit, manifest parsing |
| `backend/internal/worker/transcribe/CONTEXT.md` | Create | Per repo convention |
| `backend/cmd/worker/main.go` | Modify | `case models.StageTranscribe` |
| `backend/Dockerfile.worker-transcribe` | Create | **[DECIDE 2]** |
| `infra/docker-compose.yml` | Modify | `worker-transcribe` (+ sidecar, if **[DECIDE 1]** = C) |
| `infra/localstack/init-aws.sh` | Modify | Raise transcribe visibility timeout (**[DECIDE 3]**) |
| `docs/stage-plans/stage-1a-data-schemas.md` | Modify | Correct transcribe's input to `extract.json` |

## Tasks

1. [ ] `internal/transcribe`: `Transcriber` interface + `Segment`. Pure types.
2. [ ] `internal/transcribe/vtt.go`: `Segment` → WebVTT. Pure, fully unit-testable.
3. [ ] `internal/transcribe/mock.go`: duration-aware cues (**[DECIDE 4]**).
4. [ ] `internal/worker/transcribe`: the `Stage` — fetch manifest, short-circuit
       on `audio.present == false`, download audio, transcribe, write VTT, upload.
5. [ ] Unit tests: VTT formatting, silent-clip short-circuit, manifest parsing.
6. [ ] `cmd/worker/main.go`: wire it, defaulting to mock when `MOCK_TRANSCRIBE=true`.
7. [ ] Compose service + `MOCK_TRANSCRIBE=true` as the **default** in compose.
8. [ ] **End-to-end in mock mode first** — prove the pipeline wiring with zero
       model compute.
9. [ ] Real Whisper implementation (**[DECIDE 1]**) + its image (**[DECIDE 2]**).
10. [ ] One real transcription run, then back to mock.
11. [ ] Correct `stage-1a-data-schemas.md`; `CONTEXT.md` for the new packages.

**The ordering is deliberate:** steps 1–8 need no model, no Python, and no extra
image, and they cover every line of pipeline wiring. If the stage is going to
break the way 3A and 4A broke — silently, in the plumbing — it will break there,
where the feedback loop is seconds rather than minutes.

## Test

```bash
cd infra && docker compose up -d --build

# Mock mode: full pipeline, no model compute.
JOB=$(./upload.sh clip.mp4 clip.mp4)

# Expect validate -> extract -> transcribe, each completed
curl -s localhost:8080/jobs/$JOB | jq '.stages'

# transcript.vtt exists and is valid WebVTT
awslocal s3 cp s3://dayreel-processed/$JOB/transcript.vtt -

# A message reaches dayreel-package pointing at the transcript
awslocal sqs receive-message --queue-url .../dayreel-package
```

## Verification

_To be run against LocalStack. Nothing is checked off until observed._

**Happy path (mock mode)**

- [ ] `worker-transcribe` starts and long-polls without busy-spinning
- [ ] Full chain upload → validate → extract → transcribe with **no manual SQS
      publish**
- [ ] `stages.transcribe` goes `pending → running → completed`, `attempts == 1`
- [ ] `transcript.vtt` exists, begins with `WEBVTT`, and parses as valid WebVTT
- [ ] Cue timestamps are monotonic, non-overlapping, and none exceeds the
      manifest's `duration_seconds`
- [ ] Exactly one message on `dayreel-package`, pointing at `transcript.vtt`,
      carrying the **inherited** `trace_id` (now two hops from validate)
- [ ] `Metrics.TranscribeDurationMs` is populated

**Failure and edge paths**

- [ ] **Silent clip:** a job whose manifest has `audio.present == false`
      completes with a valid empty `WEBVTT`, **never invokes the model**, and
      still publishes to package (the 4A obligation, discharged)
- [ ] **Idempotency (duplicate):** replaying the message logs
      `already completed, dropping duplicate`, writes no new object, and does
      not double-publish
- [ ] **Idempotency (crash-resume):** hand-write `transcript.vtt` while
      `stages.transcribe` is `running`, then publish. Expect
      `output exists but stage unrecorded, resuming`. **Still unexercised after
      3A and 4A** — the last untested branch in shared code
- [ ] **Missing manifest:** publish for a `job_id` with no `extract.json`.
      Record actual behaviour (expected: transient → DLQ after 3)
- [ ] **Malformed manifest:** valid JSON, wrong shape. Expect a *permanent*
      failure — re-reading identical bytes cannot help
- [ ] **Sidecar down** (if **[DECIDE 1]** = C): expect a **transient** failure
      and successful retry once it is back, not a permanent one
- [ ] **SIGTERM mid-transcription** leaves no orphaned process

**Real model (budgeted — run once)**

- [ ] One real transcription of a ≤10s spoken clip produces recognisable text
- [ ] Wall-clock recorded, and compared against the visibility timeout
      (**[DECIDE 3]**)
- [ ] Image size measured and recorded against the **8 GB** ceiling

## Claude Code Implementation Plan

### Recommended Approach: Mock-First, Sequential, with a Parallel Image Track

The pipeline work and the ML packaging work are genuinely independent and have
very different failure modes and feedback loops. Build the pipeline against the
mock transcriber first — it needs no model, no Python and no new image — and
treat the real Whisper runtime as a separate track that plugs into an interface
already proven end to end.

This is also the cheapest ordering under an 8 GB Docker ceiling: nothing large is
built until the wiring is known to work.

### Pre-Flight Check

```
0a. docker system df                    # ~2.6GB used of an 8GB ceiling;
                                        # DECIDE 2 could add ~1GB
0b. docker builder prune -f             # reclaim before any large build
0c. df -h /System/Volumes/Data          # host disk hit 96% during 4A
0d. docker compose ps                   # 5 containers healthy
0e. awslocal sqs purge-queue --queue-url .../dayreel-dlq
                                        # 4A left one transcribe message there,
                                        # redriven by verification probes
0f. Confirm DECIDE 1 and DECIDE 2 are settled. Do NOT start step 9 before
    they are — that is the step that spends disk and build time.
```

### Execution Steps

```
Phase 1: Pure types and formatting (no I/O, no model)
1.  Write backend/internal/transcribe/transcribe.go   (interface + Segment)
2.  Write backend/internal/transcribe/vtt.go
3.  Write backend/internal/transcribe/vtt_test.go
    - HH:MM:SS.mmm formatting, including >1h and sub-second
    - empty segment list -> a valid, cue-less WEBVTT file
    - cue ordering preserved
4.  go test ./internal/transcribe/     <-- fast loop, no containers

Phase 2: Mock transcriber
5.  Write backend/internal/transcribe/mock.go   (duration-aware cues)
6.  go test ./internal/transcribe/

Phase 3: The stage (parallel writes)
7a. Write backend/internal/worker/transcribe/transcribe.go
7b. Write backend/internal/worker/transcribe/transcribe_test.go
    - audio.present == false -> empty VTT, transcriber never called
      (assert with a spy: the silent path must not reach the model)
    - malformed manifest -> worker.Permanent
8.  go test ./internal/worker/transcribe/

Phase 4: Wiring (parallel writes)
9a. Modify backend/cmd/worker/main.go        -- case models.StageTranscribe
9b. Modify infra/docker-compose.yml          -- worker-transcribe,
                                                MOCK_TRANSCRIBE=true default
9c. Modify infra/localstack/init-aws.sh      -- transcribe visibility timeout
9d. Write backend/internal/worker/transcribe/CONTEXT.md
10. go build ./... && go vet ./...

Phase 5: End-to-end in MOCK mode  <-- the whole pipeline, zero model compute
11. docker compose up -d --build worker-transcribe    (ONE build at a time)
12. Recreate localstack if init-aws.sh changed, or set the queue attribute
    directly on the running stack
13. Run the Test block; work the mock-mode verification items
14. COMMIT. The pipeline is provably correct before any ML enters the repo.

Phase 6: Real Whisper  (only after DECIDE 1 + DECIDE 2 are settled)
15. Write backend/internal/transcribe/whisper.go
16. Write backend/Dockerfile.worker-transcribe (+ sidecar service if C)
17. Build. Measure image size against the 8GB ceiling BEFORE building anything
    else. Prune first.
18. One real transcription of a <=10s clip. Record wall-clock and output.
19. Switch compose back to MOCK_TRANSCRIBE=true as the default.
20. Record results in this file; correct stage-1a-data-schemas.md.
```

### Parallel Opportunities

| Phase | Parallel Files |
|-------|----------------|
| 1 | `transcribe.go`, `vtt.go`, `vtt_test.go` |
| 3 | `worker/transcribe/transcribe.go`, `transcribe_test.go` |
| 4 | `main.go`, `docker-compose.yml`, `init-aws.sh`, `CONTEXT.md` |
| 6 | `whisper.go` and `Dockerfile.worker-transcribe` are independent tracks |

### Subagents

Unlike 3A and 4A, this stage has work genuinely worth delegating — but only in
Phase 6, and only the parts that are research rather than authoring:

- **Worth an agent:** determining the working Whisper invocation inside a
  container — exact flags, exact output shape, musl-vs-glibc viability, actual
  on-disk model and image sizes. This is empirical, container-bound, slow, and
  independent of the Go code. It is exactly the shape of work that found the
  `pkt_pts_time` bug in 4A, where the documented command was silently wrong.
- **Not worth an agent:** the Go code. It mirrors `validate.go` and
  `extract.go`, and consistency with those is the point.

Give any such agent an explicit file boundary (as in 4A: it owned
`docker-compose.yml`, and touched nothing under `backend/`), and require it to
report exact commands and exact output rather than conclusions.

### Potential Blockers

| Blocker | Resolution |
|---|---|
| **8 GB Docker ceiling** | Cannot be raised without restarting Docker. Prune before Phase 6, measure image size immediately after, and prefer the `base` model. If it will not fit, say so before building rather than after |
| Alpine + `faster-whisper` | CTranslate2 ships glibc wheels; musl is not officially supported. Either use a Debian-slim base for this image, or choose whisper.cpp (**[DECIDE 1]** B) |
| Model reload per invocation | Seconds of startup on every job under options A/B. The sidecar (C) loads once per container |
| Transcription exceeds 300s visibility | Duplicate concurrent processing, which the idempotency guard cannot catch because the output does not exist yet (**[DECIDE 3]**) |
| Sidecar startup race | The worker may long-poll and pick up a job before the sidecar has loaded its model. Needs a health check in `depends_on`, or transient-failure classification on connection refused |
| First-run model download | Slow, and repeats on every fresh container unless baked or volume-mounted |
| Real runs cost time, not money | Whisper is local, so a run costs no AWS spend. The budget rules still apply to anything provisioned, per `config/free-tier.md` |

### Time Estimate

- Phases 1–2 (types, VTT, mock): ~20 minutes
- Phase 3 (stage + tests): ~20 minutes
- Phase 4 (wiring): ~10 minutes
- Phase 5 (mock end-to-end + verification): ~20 minutes
- **Subtotal to a working, verified pipeline in mock mode: ~70 minutes**
- Phase 6 (real Whisper, image, one real run): **~45–90 minutes, high variance**

The variance is entirely in Phase 6 and is honest: it depends on **[DECIDE 1]**,
on whether the runtime cooperates with the base image, and on how long one real
transcription takes on this machine. Phases 1–5 are predictable because they are
the same shape of work as 3A and 4A.

---

## Notes

### Risks and inherited tensions

**This stage inherits the runner, and one branch of it is still untested.**
`output exists but stage unrecorded, resuming` has not been exercised in 3A or
4A. It is listed again here. If it is not verified in this stage, it should stop
being listed and instead be either deleted or covered by a unit test — a branch
that is never verified is worse than one that does not exist.

**Concurrency becomes real here.** Every earlier stage finished well inside the
visibility timeout, so redelivery-during-work was theoretical. At transcription
speeds it is not (**[DECIDE 3]**).

**Mock mode can hide integration bugs.** Building and verifying entirely in mock
mode proves the *pipeline*, not the *transcription*. The single budgeted real run
is what closes that gap, and it must actually be run — a stage verified only in
mock mode is a stage whose real path has never executed.

### Deliberately not in scope

- **Language detection, translation, diarization, word-level timestamps.** The
  deliverable is timed text.
- **Transcript quality tuning.** `base` is a starting point; model choice is one
  environment variable.
- **Runner heartbeating** (**[DECIDE 3]** option b) — a shared-runner change that
  should not be made as a side effect of this stage.
- **Correcting `MaxDuration`** — flagged in `config/free-tier.md` as a cost lever
  and left as the user's product decision.

### Uncertain, flagged rather than smoothed over

- **Transcription speed on this machine is unmeasured.** The 2–5× realtime figure
  for `base` on CPU is a general expectation, not a measurement, and
  **[DECIDE 3]** rests on it. Measure during the one real run.
- **Model and image sizes above are approximate.** They decide whether this fits
  in 8 GB, so measure before committing to a model.
- **Whether 6A wants the transcript key or the manifest** — this plan predicts
  the transcript, mirroring 4A's reasoning, but 6A is not planned yet. Same class
  of prediction 4A made about this stage, and that one turned out to need
  correcting in `stage-1a`.
- **`faster-whisper` vs `whisper.cpp` is not settled**, and `PROJECT_PLAN.md`
  names faster-whisper. Deviating is defensible on packaging grounds, but it is a
  deviation and should be an explicit decision rather than a silent one.
