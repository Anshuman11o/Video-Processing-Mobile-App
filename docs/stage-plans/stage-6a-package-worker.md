# Stage 6A: Package Worker

> Status: **approved — ready to implement.** Decisions settled 2026-08-12.
> Four went as recommended; **[DECIDE 4]** did not — bucket access is applied
> locally only, with the real-AWS access model left as an explicit open
> question rather than decided here.

## Aim

Consume `dayreel-package`, transcode `validated.mp4` into an HLS ladder with a
WebVTT subtitle rendition, write it to `dayreel-hls-output`, and **finish the
job**.

**This is the terminal stage, and that makes it different in kind.** Every stage
so far did media work and handed off. This one has to do media work and then
close the loop: mark the job complete and record where the output lives. The
pipeline has no code that does that today.

It is also the first stage that genuinely **transcodes**. Stage 3A deliberately
refused to (`stage-3a-validate-worker.md` **[DECIDE 3]**), on the grounds that
remuxing takes a second while transcoding runs for minutes. 6A is where that
bill comes due — and where the visibility-timeout machinery built in 5A actually
earns its place. The 5A plan led with that risk and was wrong; here it is real.

## Components

| Component | Action |
|-----------|--------|
| `backend/internal/worker/package/` | Create — the `Stage` implementation |
| `backend/internal/media/hls.go` | Create — the ffmpeg HLS ladder invocation |
| `backend/internal/media/playlist.go` | Create — master playlist assembly (**[DECIDE 2]**) |
| `backend/internal/db/dynamodb.go` | Modify — a method to complete a job (**[DECIDE 1]**) |
| `backend/internal/worker/runner.go` | Modify — an optional finalize hook (**[DECIDE 1]**) |
| `backend/cmd/worker/main.go` | Modify — one `case models.StagePackage` |
| `infra/docker-compose.yml` | Modify — add `worker-package` |
| `infra/localstack/init-aws.sh` | Modify — public read on the HLS bucket (**[DECIDE 4]**) |
| `backend/Dockerfile.worker` | **No change** — ffmpeg is already there |

## Boundaries

### Inbound

```json
{
  "job_id": "550e8400-...",
  "stage": "package",
  "input": {"bucket": "dayreel-processed", "key": "550e8400-.../transcript.vtt"},
  "attempt": 1,
  "trace_id": "<inherited, now three hops from validate>"
}
```

The message points at the transcript, but this stage needs **two** inputs:
`validated.mp4` and `transcript.vtt`. The video is derived from `job_id`, the
same way 5A could have derived the audio key and chose not to.

That asymmetry is worth stating plainly rather than repeating by habit: the
convention this pipeline has settled into is **`input.key` names what the
previous stage produced**, not everything the next stage needs. A stage with
several inputs reconstructs the rest from the job ID.

### Outbound: nothing

`NextStage(package)` returns `""` and `publishNext` already handles that with an
early return (`runner.go`). **No runner change is needed for the absence of a
next queue** — that part works today.

What does *not* work today is finishing the job. See **[DECIDE 1]**.

### S3 Objects

First stage to write to a third bucket.

| Bucket | Key | Content |
|--------|-----|---------|
| `dayreel-processed` | `{job_id}/validated.mp4` | Input, derived from job_id |
| `dayreel-processed` | `{job_id}/transcript.vtt` | Input, from the message |
| `dayreel-hls-output` | `{job_id}/master.m3u8` | **The declared output** |
| `dayreel-hls-output` | `{job_id}/720p/playlist.m3u8` + `segment_NNN.ts` | Rendition |
| `dayreel-hls-output` | `{job_id}/480p/…`, `{job_id}/360p/…` | Renditions |
| `dayreel-hls-output` | `{job_id}/subs/playlist.m3u8` + `subs_000.vtt` | Subtitle rendition (**[DECIDE 2]**) |

**The multi-artifact problem from 4A returns, larger.** A 60s clip at 6s segments
is ~10 segments × 3 renditions, plus 4 playlists and the subtitles — on the order
of 40 objects.

4A solved this with a manifest. Here **no new manifest is needed**: `master.m3u8`
*is* the manifest, by construction. It references every rendition playlist, which
in turn reference every segment. So `OutputKey` is `{job_id}/master.m3u8`, and —
exactly as in 4A — **it is uploaded last**, after every object it points at. The
runner's idempotency guard then means what it says.

### DynamoDB

`stages.package` through the existing runner methods, plus — new to this stage —
the job's own completion:

```json
{
  "status": "completed",
  "output": {
    "hls_url": "http://…/dayreel-hls-output/{job_id}/master.m3u8",
    "duration_seconds": 6.02,
    "thumbnail_url": "http://…/dayreel-processed/{job_id}/frames/frame_001.jpg"
  }
}
```

`models.OutputInfo` (`job.go:67`) already defines these three fields.
`duration_seconds` and the thumbnail both come from 4A's extract manifest, so
this stage reads it rather than re-probing.

---

### [DECIDE 1] — nothing marks a job completed, and this stage must

**RESOLVED: option (B), the optional `Finalizer` interface.** The runner
type-asserts after `SetStageCompleted`, so the job can never claim completion
ahead of its own stage row. Separate commit, unit-tested like the heartbeat.

**This is the decision the stage turns on.** The rest is ffmpeg.

#### The gap, precisely

`JobStatusCompleted` is defined in `models/job.go:17` and **never written by any
code path**. Searching the backend finds exactly two references: the definition,
and `handlers.go:256`, which reads it.

That line is `GET /jobs/{id}/reel`:

```go
if job.Status != models.JobStatusCompleted || job.Output == nil {
    writeError(w, http.StatusConflict, "job not yet complete", "NOT_READY")
```

So **the reel endpoint has returned 409 for every job since Stage 2A**, and will
continue to until this stage writes both fields. `job.Output` is likewise never
populated — there is no `db` method that writes it. `UpdateJobStatus` exists but
is only ever called with `processing`/`failed`.

This is not a 6A bug. It is a gap that has been latent since 2A and that 6A is
the first stage in a position to close.

#### The tension

`worker.Stage` is deliberately narrow:

```go
Process(ctx, msg) (string, error)   // do media work, return the key
```

Everything else — stage state, retries, publishing — belongs to the runner. That
separation is why stages have stayed small and why the runner's bugs were
findable. Completing a job is job-lifecycle work, not media work, so putting it
inside `Process` would be the first violation of that boundary.

#### Options

**(A) Do it inside `Process`.** The package stage calls the db itself.
- Smallest diff, no shared-code change.
- Breaks the separation, and the ordering is wrong: `Process` runs *before*
  `SetStageCompleted`, so the job would be marked `completed` while
  `stages.package` still said `running`. A crash in between leaves a job claiming
  completion with a stage that never finished.

**(B) An optional `Finalizer` interface the runner checks.**
```go
type Finalizer interface {
    Finalize(ctx context.Context, msg *events.StageMessage, outputKey string) error
}
```
The runner type-asserts after `SetStageCompleted` and calls it if present.
- Ordering is correct by construction: stage row first, then job completion.
- Only the terminal stage implements it; the other three are untouched and the
  `Stage` interface does not grow.
- Costs a shared-runner change — the fourth this project has made — though a
  small and additive one.

**(C) Teach the runner that the last stage completes the job.** The runner checks
`NextStage() == ""` and completes the job itself.
- No new interface.
- But the runner would have to build `OutputInfo`, which means knowing about HLS
  URLs and thumbnails. That is stage knowledge leaking into shared code, and the
  runner currently knows nothing about any stage's semantics.

**(D) A separate reconciler** that watches for all-stages-completed and finishes
the job.
- Cleanly decoupled, and would also catch jobs stuck part-way.
- An entire new service for one write, on a project with a $20 budget and a
  handful of runs left. Recorded because it is the "right" answer at scale, and
  clearly wrong here.

#### Recommendation: **(B), the optional interface.**

It puts the write after `SetStageCompleted`, which is the only ordering that
cannot leave a job claiming to be finished ahead of its own stage row. It leaves
the other three stages and the `Stage` interface untouched. And it keeps
HLS-specific knowledge inside the HLS stage, which is the property (C) gives up.

The cost is honest: **a fourth change to the shared runner**, in a file where two
silent bugs have already been found. It should be additive, separately committed,
and unit-tested like the heartbeat was.

---

### [DECIDE 2] — how the transcript becomes an HLS subtitle track

**RESOLVED: option (a), hand-assemble the master playlist**, including the
subtitle rendition.

`PROJECT_PLAN.md` says "Master playlist with VTT subtitle track". Real HLS
subtitles are not just a `.vtt` file next to the video; they need:

- a subtitle **playlist** (`subs/playlist.m3u8`) listing VTT segments,
- an `EXT-X-MEDIA:TYPE=SUBTITLES` entry in the master,
- and a `SUBTITLES="subs"` attribute on **every** `EXT-X-STREAM-INF`.

ffmpeg's `hls` muxer does not produce subtitle renditions.

- **(a) Hand-assemble the master playlist.** Let ffmpeg produce the three video
  renditions and their playlists, then write `master.m3u8` ourselves, including
  the subtitle rendition. Segment the VTT — for a ≤60s clip, a single VTT
  "segment" in a one-entry playlist is legitimate and much simpler.
- **(b) Side-load the transcript.** Copy `transcript.vtt` into the output prefix
  and let the client attach it as an external text track. React Native players
  support this. Simple, but the master playlist then does not describe the
  subtitles, so anything consuming plain HLS (ffplay, Safari) sees no captions —
  and the verification step in `PROJECT_PLAN` is an `ffplay` command.
- **(c) Burn subtitles into the video.** Never toggleable, tripled encode cost,
  and wrong for the same reason.

**Recommendation: (a).** Writing a master playlist is string formatting against a
well-specified format, and it is the only option where `ffplay <master.m3u8>`
shows captions — which is the plan's own stated verification. It also means the
master is genuinely ours to construct, which **[DECIDE 1]**'s "uploaded last"
ordering already assumes.

---

### [DECIDE 3] — the ladder, and what to do when the source is smaller

**RESOLVED as recommended:** skip renditions above the source height, always
keeping at least one.

`stage-1a-data-schemas.md` specifies 720p/480p/360p. Two problems:

1. **Upscaling.** Test clips are 640×480. Encoding a 720p rendition from a 480p
   source produces a larger file that looks no better — pure waste, and on a
   budget where every rendition is transcode time and S3 PUTs.
2. **The `MaxDuration` change.** Validate now caps at 60s (lowered in 5A), so the
   worst case is far smaller than the ladder was originally sized for.

Options: encode all three regardless; or **skip renditions above the source
height**, which is what real encoders do.

**Recommendation: skip renditions above source height, always keeping at least
one.** A 640×480 source yields 480p and 360p; a 1080p source yields all three. If
the source is smaller than the lowest rung, encode that one rung anyway so there
is always a playable rendition.

Proposed rungs, to be tuned once measured:

| Name | Height | Video bitrate | Audio |
|---|---|---|---|
| 720p | 720 | 2800k | 128k AAC |
| 480p | 480 | 1400k | 128k AAC |
| 360p | 360 | 800k | 96k AAC |

6-second segments, per `PROJECT_PLAN.md`.

---

### [DECIDE 4] — what `hls_url` actually points at, and whether the bucket is readable

**RESOLVED: public-read applied to LocalStack only. The real-AWS access model is
deliberately left open.**

Not the recommendation, which was to adopt public-read generally. The chosen
position is narrower and better: development is unblocked without a
public-bucket decision being baked into anything that ships. **This is now an
open question that Stage 7 or any deployment work must answer**, not a settled
default — a public bucket on real AWS is both a cost and an exposure, and it
should be an explicit choice made with that in view rather than inherited from a
local convenience.

Presigning remains ruled out on technical grounds regardless: HLS playlists
reference segments by relative path, so a signed master is followed by 403s on
every segment.

`init-aws.sh` sets **CORS** on `dayreel-hls-output` but **no bucket policy and no
public-read ACL**. So the objects are not currently fetchable by an unauthenticated
player, and CORS alone does not change that.

This is the same class of problem as the **presigned-URL finding from 4A**, and
worse here: HLS playlists reference segments by *relative path*. A presigned
master playlist would work, and then every segment fetch would 403, because the
player has no way to sign them. **Presigning is not a viable option for HLS**
unless every referenced URL is rewritten, which defeats the format.

- **(a) Public-read policy on the HLS bucket**, plain URLs. What CloudFront would
  sit in front of in production. Simple, works with any player, no expiry.
- **(b) Presigned master only.** Broken for the reason above; recorded so it is
  not proposed later.
- **(c) Serve HLS through the API.** A proxy endpoint that streams from S3.
  Avoids public objects but puts video bytes through the API — which
  `stage-2a-go-api.md` explicitly designed against ("No video bytes through
  API").

**Recommendation: (a), with the caveat stated loudly.** A public-read bucket is
correct for local development and for a CloudFront origin; it is **not** something
to carry into a real deployment unnoticed. It should be recorded in
`config/free-tier.md` alongside the teardown rules, since a public bucket left up
is both a cost and an exposure.

The URL itself has the **same docker-internal-host problem** flagged in 4A:
`http://localstack:4566/...` does not resolve outside the compose network. Stage 7
needs a public-facing endpoint setting, and this stage should read that same
setting rather than inventing a second mechanism.

---

### [DECIDE 5] — transcode time is real here, unlike 5A

**RESOLVED as recommended:** measure first, then set the package queue's
visibility timeout to roughly 10× the measured worst case.

Transcription measured ~0.1× realtime, which made the visibility timeout a
non-issue. **Transcoding three renditions is a different order of magnitude**, and
this is the stage the 300s default was actually dangerous for.

Unknowns that must be measured rather than guessed:

- wall-clock for a 60s 1080p source across three renditions on this machine,
- whether `-preset veryfast` vs `medium` matters enough to pin,
- whether three sequential ffmpeg invocations or one multi-output invocation is
  faster (one pass decoding once and encoding three times is usually
  substantially better).

Mitigations already in place, none of which needs redoing: heartbeating (5A),
`MaxDuration` at 60s (5A), and a 900s timeout **on the transcribe queue only** —
note `dayreel-package` is still at **300s**. That should almost certainly be
raised too, but the number should follow the measurement rather than precede it.

**Recommendation: measure first, then set the package queue's timeout to roughly
10× the measured worst case.** With heartbeating in place the timeout is a
backstop rather than the primary defence.

---

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/media/hls.go` | Create | ffmpeg HLS ladder; rendition selection by source height |
| `backend/internal/media/playlist.go` | Create | Master playlist assembly incl. subtitle rendition |
| `backend/internal/media/playlist_test.go` | Create | Playlist formatting; pure, no ffmpeg |
| `backend/internal/worker/package/package.go` | Create | The `Stage` + `Finalize` |
| `backend/internal/worker/package/package_test.go` | Create | Rendition selection, `OutputInfo` assembly |
| `backend/internal/worker/package/CONTEXT.md` | Create | Per repo convention |
| `backend/internal/db/dynamodb.go` | Modify | `CompleteJob(ctx, jobID, output)` |
| `backend/internal/worker/runner.go` | Modify | Optional `Finalizer` hook (**[DECIDE 1]**) |
| `backend/internal/worker/finalizer_test.go` | Create | The hook fires after `SetStageCompleted`, and only when implemented |
| `backend/cmd/worker/main.go` | Modify | `case models.StagePackage` |
| `infra/docker-compose.yml` | Modify | `worker-package` |
| `infra/localstack/init-aws.sh` | Modify | Public-read policy on HLS bucket; package queue timeout |

## Tasks

1. [ ] `internal/media/playlist.go`: master playlist assembly. Pure string
       formatting, no I/O, fully unit-testable.
2. [ ] `internal/media/hls.go`: the ffmpeg ladder + rendition selection.
3. [ ] `internal/db`: `CompleteJob` writing `status` and `output` in one update.
4. [ ] `internal/worker`: the optional `Finalizer` hook + its tests
       (**separate commit** — shared runner change).
5. [ ] `internal/worker/package`: the Stage, `Finalize`, `OutputInfo` assembly.
6. [ ] Unit tests: rendition selection incl. the small-source case, playlist
       formatting, `OutputInfo` assembly.
7. [ ] Wiring: `main.go`, compose, `init-aws.sh` bucket policy.
8. [ ] End-to-end: upload → all four stages → `master.m3u8` plays.
9. [ ] **Force the crash-resume branch** (see below).
10. [ ] Measure transcode wall-clock; set the package queue timeout from it.
11. [ ] `CONTEXT.md`; update `stage-1a-data-schemas.md` if key layout shifts.

## Test

```bash
cd infra && docker compose up -d --build

JOB=$(./upload.sh clip.mp4 clip.mp4)

# All four stages complete, and the job itself is finally "completed"
curl -s localhost:8080/jobs/$JOB | jq '{status, output, stages}'

# The endpoint that has returned 409 since Stage 2A
curl -s localhost:8080/jobs/$JOB/reel | jq

# The plan's own verification
ffplay http://localhost:4566/dayreel-hls-output/$JOB/master.m3u8

# Captions present in the master, not just on disk
awslocal s3 cp s3://dayreel-hls-output/$JOB/master.m3u8 - | grep SUBTITLES
```

## Verification

_To be run against LocalStack. Nothing checked off until observed._

**Happy path**

- [ ] `worker-package` starts and long-polls without busy-spinning
- [ ] Full chain upload → validate → extract → transcribe → package, no manual
      publish at any point
- [ ] `stages.package` goes `pending → running → completed`, `attempts == 1`
- [ ] `master.m3u8` exists and lists every rendition present in S3 — and
      **every URI it references resolves via `head-object`** (the 4A manifest
      check, applied to a playlist)
- [ ] Each rendition playlist lists segments that all exist
- [ ] **`ffplay master.m3u8` plays**, and captions appear
- [ ] `job.status == "completed"` and `job.output` is populated — the first time
      either has ever been true in this project
- [ ] **`GET /jobs/{id}/reel` returns 200**, not the 409 it has returned since 2A
- [ ] `output.duration_seconds` matches the extract manifest
- [ ] `output.thumbnail_url` points at a frame that exists
- [ ] No message is published anywhere — this is the terminal stage

**Failure and edge paths**

- [ ] **Idempotency (duplicate):** replay logs `already completed, dropping
      duplicate`, writes no new objects, and does not re-complete the job
- [ ] **Crash-resume — MUST be run here.** See below.
- [ ] **Missing transcript:** publish for a job with no `transcript.vtt`.
      Record actual behaviour; decide whether a video-only package is acceptable
      or a hard failure
- [ ] **Source smaller than the lowest rung:** still produces one playable
      rendition rather than an empty ladder (**[DECIDE 3]**)
- [ ] **Silent clip's empty VTT:** a cue-less `WEBVTT` still yields a valid
      subtitle rendition, and the player does not error on it. This is 4A's and
      5A's silent-clip decision arriving at its final consumer
- [ ] **SIGTERM mid-transcode** leaves no orphaned ffmpeg and the message returns
- [ ] **Heartbeating observed:** a transcode longer than one interval logs
      heartbeats and the message is *not* redelivered. First real exercise of the
      5A machinery

**Timing**

- [ ] Wall-clock for a 60s source across the ladder, recorded
- [ ] Package queue visibility timeout set from that measurement
- [ ] Object count and total size per job recorded — ~40 objects/job affects the
      S3 PUT cost noted in `config/free-tier.md`

### The crash-resume branch stops here

`output exists but stage unrecorded, resuming` (`runner.go`) has now gone
unexercised through 3A, 4A and 5A. It is real code in shared infrastructure, on
the path every stage takes, and it has never once executed.

**It gets settled in this stage, one of two ways:**

- **Forced end to end:** hand-write `master.m3u8` into the HLS bucket while
  `stages.package` still reads `running`, then publish the message. Expect
  `output exists but stage unrecorded, resuming`, `Process` skipped, and the job
  completed anyway.
- **Or covered by a unit test** against a faked storage layer, if forcing it
  proves awkward.

If neither happens, the branch should be **deleted**. A branch that is never
verified is worse than one that does not exist, because it reads as tested.

## Claude Code Implementation Plan

### Recommended Approach: Pure-Formatting First, Then Encode, Then Finalize

Three separable tracks, in increasing order of feedback-loop cost:

1. **Playlist assembly** — pure string formatting, no ffmpeg, no S3. Instant tests.
2. **The ffmpeg ladder** — real encoding, slow, empirical.
3. **Job completion** — the shared-runner hook and the db write.

Do them in that order. The playlist is where format mistakes hide and where tests
are free; the encode is where time goes; the completion hook is small but touches
shared code and deserves its own commit and its own attention.

### Pre-Flight Check

```
0a. docker system df                 # 8GB ceiling; build cache was ~1.4GB after 5A
0b. docker builder prune -f          # reclaim before building
0c. df -h /System/Volumes/Data       # host disk was tight throughout 4A/5A
0d. docker compose ps                # 6 containers healthy
0e. awslocal sqs purge-queue --queue-url .../dayreel-dlq
0f. Confirm DECIDE 1 is settled — it dictates whether runner.go is touched at
    all, and everything in phase 4 depends on it.
```

### Execution Steps

```
Phase 1: Playlist assembly (pure, no ffmpeg, no I/O)
1.  Write backend/internal/media/playlist.go
2.  Write backend/internal/media/playlist_test.go
    - master lists every rendition passed to it
    - EXT-X-MEDIA subtitle entry present, and SUBTITLES="subs" on EVERY
      EXT-X-STREAM-INF (a player ignores captions if one variant omits it)
    - a cue-less VTT still yields a valid subtitle playlist
3.  go test ./internal/media/          <-- instant loop

Phase 2: The ladder
4.  Write backend/internal/media/hls.go
5.  Extend media_test.go: rendition selection by source height, incl. a source
    smaller than the lowest rung
6.  go test ./internal/media/          <-- now slow; real encoding

Phase 3: The stage
7a. Write backend/internal/worker/package/package.go
7b. Write backend/internal/worker/package/package_test.go
8.  go test ./internal/worker/package/

Phase 4: Job completion  <-- SEPARATE COMMIT, shared runner
9.  Add db.CompleteJob
10. Add the Finalizer hook to runner.go + finalizer_test.go
    - fires after SetStageCompleted, never before
    - a stage that does not implement it is unaffected
    - a Finalize error does not delete the message
11. go test -race ./internal/worker/
12. COMMIT this separately from the package stage.

Phase 5: Wiring
13a. main.go   13b. docker-compose.yml   13c. init-aws.sh (bucket policy)
14. go build ./... && go vet ./...

Phase 6: End to end
15. docker compose up -d --build worker-package   (ONE build at a time)
16. Apply the bucket policy to the running stack (init-aws.sh only runs on a
    fresh localstack) — same pattern as the 5A queue-timeout change
17. Full chain; then ffplay the master playlist
18. Verify GET /jobs/{id}/reel finally returns 200
19. FORCE THE CRASH-RESUME BRANCH — do not skip, do not defer
20. Measure transcode wall-clock; set the package queue timeout from it
21. Record results in this file
```

### Parallel Opportunities

| Phase | Parallel Files |
|-------|----------------|
| 1 | `playlist.go`, `playlist_test.go` |
| 3 | `package.go`, `package_test.go` |
| 4 | `db/dynamodb.go`, `runner.go`, `finalizer_test.go` |
| 5 | `main.go`, `docker-compose.yml`, `init-aws.sh` |

### Subagents

The pattern that has paid off twice — `pkt_pts_time` in 4A, the exit-0-on-corrupt
-input trap in 5A — is delegating **empirical container-bound research**, not
authoring:

- **Worth an agent:** determining the ffmpeg HLS invocation empirically inside
  the worker image. Exact flags for a multi-rendition single-pass ladder, the
  precise on-disk layout it produces, whether `-var_stream_map` names playlists
  the way the docs imply, what it does with a source smaller than a rung, and
  measured wall-clock per preset. Require exact commands, exact directory
  listings, and exact playlist contents — not conclusions.
- **Also worth an agent:** verifying that a hand-written master playlist actually
  plays with captions, in `ffplay` and ideally one more player. Playlist bugs are
  silent — a player showing no captions looks identical to a clip with none.
- **Not worth an agent:** the Go code, the runner hook, the db method.

Give any agent an explicit file boundary and the 8 GB disk warning, as in 4A and
5A.

### Potential Blockers

| Blocker | Resolution |
|---|---|
| **Transcode exceeds the package queue's 300s** | Heartbeating (5A) should hold it, and this is its first real test. Raise the queue timeout from the measurement |
| ffmpeg HLS layout differs from expectations | `-var_stream_map` and `-master_pl_name` behaviour is fiddly; verify the real output tree before writing the uploader |
| Public-read bucket policy in LocalStack | LocalStack's policy enforcement is looser than real S3 — an object may be fetchable locally and 403 in AWS. Do not treat local success as proof |
| Docker-internal host in `hls_url` | Same defect as 4A's presigned URLs. The URL will not resolve outside compose; Stage 7 needs a public-endpoint setting and this stage should read it |
| ~40 objects per job | Multiplies S3 PUTs by an order of magnitude over earlier stages. Still pennies (`config/free-tier.md`), but worth watching across repeated E2E runs |
| Disk | HLS output is several copies of the video. With `MaxDuration` at 60s this is bounded, but repeated E2E runs accumulate in the volume |

### Time Estimate

- Phase 1 (playlist + tests): ~25 minutes
- Phase 2 (ladder, empirical): ~30 minutes, high variance
- Phase 3 (stage + tests): ~20 minutes
- Phase 4 (completion hook, separate commit): ~20 minutes
- Phase 5 (wiring): ~10 minutes
- Phase 6 (E2E, crash-resume, measurement): ~30 minutes
- **Total: ~2¼ hours**, the longest stage so far

It is the longest because it is three things at once: a transcode stage, a
playlist format, and the job-lifecycle closure the project has been missing since
2A. The estimate does not assume the ffmpeg ladder works first try; Phase 2's
variance is where it will move.

---

## Notes

### What this stage closes

**`GET /jobs/{id}/reel` has returned 409 for every job since Stage 2A**, because
nothing ever set `JobStatusCompleted` or populated `job.Output`. That endpoint,
the mobile client's eventual entry point, has never once succeeded. 6A is the
first stage able to change that, and **[DECIDE 1]** is how.

Worth noting how this went unnoticed: every stage verified its *own* outputs, and
every one of them passed. Nothing verified the thing the whole pipeline exists to
produce. That is an argument for making the reel endpoint returning 200 an
explicit checklist item, which it now is.

### Risks and inherited tensions

- **Fourth shared-runner change.** Backoff, heartbeating, the idempotency
  reordering, and now the finalize hook. Each was justified; the accumulation is
  worth naming. This one is additive and optional, which is the least invasive
  shape available.
- **The crash-resume branch is settled here or deleted.** Stated in Verification;
  repeated because it has now slipped three times.
- **`Metrics` — RESOLVED: populate in 6A.** `TotalProcessingMs` is written when
  the job completes, and the runner records each stage's own duration. This
  stops a set of model fields being permanently decorative, at the cost of
  touching the shared runner alongside the `Finalizer` hook.

### Deliberately not in scope

- **CloudFront.** Not available in LocalStack Community, and a per-hour cost
  against a $20 budget.
- **DRM, encryption, multi-audio, per-title encoding.**
- **Adaptive ladder tuning.** Fixed rungs; tune later if ever.
- **Lifecycle rules on the HLS bucket.** `stage-1a` mentions 7-day expiry on the
  other buckets; whether HLS output should expire is a product question, not a
  6A one.

### Uncertain, flagged rather than smoothed over

- **Transcode wall-clock is entirely unmeasured**, and it is the input to the
  queue-timeout decision. Everything in **[DECIDE 5]** is contingent on it.
- **Whether one ffmpeg invocation with `-var_stream_map` beats three sequential
  ones** on this machine. Usually yes; unverified here.
- **Whether LocalStack's bucket-policy enforcement matches real S3.** A local
  200 is not evidence the same object is readable in AWS.
- **Whether a cue-less VTT subtitle rendition is valid to all players.** It is
  valid WebVTT and should be valid HLS, but players disagree about edge cases and
  the silent-clip path produces exactly this.
- **The bitrate rungs are guesses**, unmeasured against any real footage.
