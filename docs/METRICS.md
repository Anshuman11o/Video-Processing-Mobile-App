# DayReel Metrics

The four metrics `PROJECT_PLAN.md` → "Metrics Plan" asked for, measured against
**real AWS** on **2026-08-13**. This is the first time any of them has been
measured on this substrate; every earlier figure in this repository came from
LocalStack.

Bucket names and the account id are redacted here on purpose — this repository
is public, and the only place the real names need to exist is `.env`, which is
git-ignored and is where every script and Makefile target reads them from.

## Read this before quoting any number

**The sample is five clips.** Not five hundred, not fifty. Five 8–14.4 second
clips, processed one at a time on a developer's laptop, over a home broadband
connection, while an Android emulator and a Metro bundler were running.

This project has been burned repeatedly by green results that meant nothing, so
every figure below carries a label and nothing is rounded up into confidence it
has not earned:

- **MEASURED** — observed in this run, from a named artifact.
- **DERIVED** — computed from measured inputs, under assumptions that are stated.
- **NOT MEASURED** — the experiment was never run. No number is offered.

Two of the four metrics are weak and one is absent. That is the honest state, and
saying so is more useful than four confident rows.

| Metric | Verdict | Basis |
|---|---|---|
| Cost per 100 clips | **Solid** | Real object counts and byte counts; only unit prices are external |
| p95 E2E latency | **Weak** | n=5. At that size p95 is the max with a hat on |
| Sustained throughput | **NOT MEASURED** | No load test has ever run. Only a derived ceiling exists |
| Upload success rate | **Weak** | N=7 genuine attempts; the mobile uploader has completed exactly 1 |

### The sample

Five jobs completed the pipeline. The table holds 18 items; the other 13 are
negative-path upload fixtures from `scripts/verify-presign.sh` (29-byte
`neg.mp4`) and `scripts/verify-resume.sh` (`cancelled.mp4`, `partial.mp4`) that
never ran a stage. Counting those as failures gives a 61% failure rate and is
meaningless — they failed because a test told them to.

| Job | Source clip | Origin | Transcription |
|---|---|---|---|
| `a0a28d77…` | 14.95 MB / 10.0 s / 720p | curl from host | mock |
| `5cbc3ee0…` | 14.95 MB / 10.0 s / 720p | **Android app, WorkManager** | mock |
| `6c788a1f…` | 16.79 MB / 14.4 s | curl from host | **real whisper.cpp** |
| `b5062e0c…` | 13.08 MB / 8.0 s | `verify-resume.sh` | mock |
| `e5bd18bb…` | 13.08 MB / 8.0 s | `verify-resume.sh` | mock |

---

## 1. Cost per 100 clips

**MEASURED**, and the answer is that processing is free and storage is nearly
free. The interesting cost is egress, and it may also be zero.

Unit prices were pulled from AWS's public price-list JSON (the IAM user is
denied `pricing:GetProducts`), dated 2026-07-20 to 2026-08-07:
S3 Standard $0.023/GB-mo, S3 Tier-1 $0.005/1k, S3 Tier-2 $0.0004/1k,
DynamoDB on-demand $0.625/M writes and $0.125/M reads, egress $0.090/GB.

### Per-clip footprint — MEASURED

Every completed clip creates **19–22 objects across three buckets**, 33.5 MB for
the representative 10-second job:

| Bucket | Objects | Bytes |
|---|---|---|
| raw | 1 | 14,947,952 |
| processed | 5 — `validated.mp4`, `audio.wav`, `extract.json`, 1 frame, `transcript.vtt` | 15,294,913 |
| hls | 13 — 3 renditions × (playlist + 2 segments), master, subs playlist, subs VTT, thumbnail | 3,286,839 |
| **total** | **19** | **33.5 MB** |

24 Tier-1 and 11 Tier-2 S3 requests per clip. This is **DERIVED** — read from
every S3 call site in `backend/internal/` and cross-checked against the object
inventory, which reconciles exactly. No CloudTrail capture was taken, so a
retried request would not show; all five jobs report `attempts: 1` on every
stage, so retries were zero here.

15 DynamoDB writes per clip: `CreateJob`, `MarkUploadComplete`, three writes per
stage × 4 stages, `CompleteJob`.

### The answer

| | Cost per 100 clips |
|---|---|
| **One-off processing** | **$0.014** |
| **Storage, per month** | **$0.072** (3.12 GiB) |
| **Egress @ 1 view each** | **$0.026** |
| Egress @ 100 views each | $2.59 |
| Egress @ 1,000 views each | $25.90 — **over the $20 budget** |

Processing 100 clips costs **1.4 cents, once** — 0.07% of the project budget.
It is not a number worth optimising.

Storage beats egress until each clip is watched about **7 times a month**. Past
that, egress dominates and keeps growing while storage does not.

### The largest uncertainty, and it is large

**AWS grants every account 100 GB/month of free internet egress, permanently.**
This is *not* the 12-month free tier that `config/free-tier.md` correctly says
this account does not have — it is a separate universal allowance from December
2021, and the price-list description confirms it ("first 10 TB / month …
**beyond** [the allowance]").

If it applies — believed, **NOT VERIFIED** against this account's bill — then
egress is $0 up to roughly 35,000 720p playbacks per month and the dominant term
collapses entirely. Realistic total project spend on S3 + DynamoDB would be
**under $0.20**. Treat the egress rows above as an upper bound.

Cost Explorer would settle it, and was deliberately skipped: it bills ~$0.01 per
request to measure a thing that costs $0.014, its data lags ~24h, and the account
only started billing today.

### Compute is $0, and the trap is idle time

Workers run on the developer's Mac; nothing in the account bills per hour.
Priced against `config/aws-limits.md`'s sizing table on Fargate **per clip**:
$0.30 per 100 clips for four per-stage tasks (dominated by Fargate's 1-minute
minimum — transcribe runs 3.2 s and bills 60), or $0.53 for a single 2 vCPU task
running all four stages.

Against that, a 24/7 four-worker fleet recomputes to **$132.63/month**.
`config/free-tier.md` says ~$115 — right order, about 13% low.
**The entire cost of the Fargate trap is idle time: per-clip execution for 100
clips is 0.2% of one month of the same fleet sitting idle.**

> `config/free-tier.md` quotes DynamoDB at ~$1.25/M writes. That is the
> pre-2024 rate; on-demand halved to $0.625/M. It changes no conclusion — the
> term is $0.000016 per clip — but the file is stale on this point.

### What would move the number, in order

1. **Whether the 100 GB egress allowance applies.** Swings the dominant term
   between $2.59 and $0.00 per 100 clips at 100 views. Nothing else is close.
2. **View count.** Cost scales with views, not clips.
3. **Deleting intermediates.** `validated.mp4` is the same size as the raw
   upload in all five jobs — **89% of stored bytes per clip are two copies of
   the same video**, and only the 3.3 MB HLS tree is needed to serve. Deleting
   raw + processed after packaging cuts storage ~90%.
4. **HLS bitrate.** The 720p rung ranges 1,037–3,090 kbps across three source
   clips for no configured reason — a 3× swing straight onto the dominant term.

---

## 2. p95 end-to-end latency

**MEASURED, but n=5. The p95 is an interpolation between the 4th and 5th of five
values — read every p95 below as "the max was X".** There is no load test here
and nothing in this data supports a tail-latency claim.

### Two intervals, because they differ and only one is the pipeline's fault

| | p50 | max | sorted (s) |
|---|---|---|---|
| **A — job creation → package complete** (includes client upload) | 192.8 | 258.5 | 176.1, 184.7, 192.8, 208.8, 258.5 |
| **B — upload complete → package complete** (server side) | **152.0** | 227.4 | 147.6, 149.3, 152.0, 160.8, 227.4 |

Restricted to the three user-shaped runs (excluding the two script-paced
`verify-resume` jobs, whose upload windows contain deliberate assertion round
trips): **E2E-B = 147.6, 149.3, 160.8 s, median 149.3.**

The upload component ranges 23.9–61.2 s and is the dominant source of A's
spread. Three of five uploads were curl from a laptop in India to `us-east-1`;
one was an emulator on that same laptop. Neither is a phone on a mobile network.
**B is the number the pipeline owns and the one worth quoting.**

**Supportable claim: median server-side pipeline latency ≈ 150 s for an 8–14 s
720p clip**, single worker per stage, on a laptop.

### Per stage

| Stage | min | median | max | spread |
|---|---|---|---|---|
| validate | 9.1 | 35.9 | 130.0 | **14.3×** |
| extract | 47.8 | **53.0** | 57.1 | **1.2×** |
| transcribe | 1.6 | 3.2 | 5.3 | 3.3× |
| package | 16.3 | **57.7** | 73.9 | 4.5× |

**Extract is the only stage this sample can characterise** — five values within
±9% across three different clips. The validate and package spreads are laptop
CPU and bandwidth contention, not pipeline behaviour: `e5bd18bb`'s 130 s validate
ran entirely inside another job's extract→package window, and `5cbc3ee0`'s 67 s
validate coincides with heavy operator ffmpeg work on the same machine. The same
14.95 MB clip validated in 9.1 s on an idle machine.

**Queue wait is not where the time goes.** Real `queue_wait_ms` is 25–245 ms;
the ~1.2 s inter-stage gap is the worker's poll round trip plus the DynamoDB
status write, totalling ~5 s per job (~3% of E2E-B). With one worker per stage
and no concurrency, nothing ever queued.

### Real transcription costs nothing here — an unexpected result

Real whisper.cpp (n=1, **3.2 s**) sits *inside* the mock range (1.6–5.3 s), below
the mock median. That clip carries 7.6 s of audio and whisper ran at ~0.42×
realtime, so the stage's cost is the S3 audio download and VTT upload, not the
model.

The widely-assumed "mock ≈ free, real ≈ expensive" framing **does not hold at
this clip length**. It says nothing about a 5-minute clip: whisper scales with
audio duration, the S3 round trips do not.

### Against the LocalStack era

Same 14.95 MB / 10 s clip under mock. Stage sum went 20.9 s → ~143 s (6.8×).

**Extract regressed most and is the only row the sample supports**: 13.1× and
13.3× on two independent runs, +50 s absolute both times.

> **A recorded explanation does not survive the data.** `docs/SETUP.md` and
> `docs/RUNBOOK.md` attribute the package-stage slowdown to "uploads every
> segment of every rendition individually". Package writes only **13 objects**
> for a 10 s clip, and the *same clip* producing the *same 13 objects* took
> **74.9 s vs 17.3 s** across two runs — 4.3× apart at constant work volume.
> Per-object network cost cannot explain that. The data is consistent with
> package time being dominated by the three-rung ffmpeg encode competing for
> laptop CPU, 17.3 s being the uncontended floor — but that is a hypothesis the
> data permits, **not one it proves**. A controlled idle-machine run would
> settle it.

The transcribe ratio against LocalStack (67×–144×) is arithmetic, not
information: it divides by a 0.04 s baseline to describe a 2.7–5.8 s absolute
change. It should not be quoted.

**No new instrumentation is needed for interval A** — `metrics.total_processing_ms`
is already stored per job and matches it exactly on all five.

---

## 3. Sustained throughput

**NOT MEASURED.** No load test has ever run on this project. Every clip in the
record traversed the pipeline essentially alone, so clips-per-minute under
sustained load has no observation behind it.

What follows is a **DERIVED** capacity ceiling. It is not a measurement and must
not be quoted as one.

### DERIVED ceiling: ~1.0 clips/min (~62 clips/hour)

For 8–14 s clips, single host, one process per stage. Plausible range 28–75
clips/hour depending which end of the observed variance you take.

| Basis | Bottleneck | Service time | clips/hr |
|---|---|---|---|
| per-stage min | extract | 47.8 s | 75.3 |
| **per-stage median** | **package** | **57.7 s** | **62.4** |
| per-stage max | validate | 130.0 s | 27.7 |

**The bottleneck is not cleanly identified.** Package is median-slowest (57.7 s)
but its 4.5× spread overlaps extract's entire range. Extract is the *stable*
stage (1.2× spread) and the honest planning bottleneck.

**Assumptions, all of which must hold:** single host; one process per stage at
concurrency 1; four-stage series where every clip visits every stage; source
clips 8–14.4 s; perfect pipelining with zero queue wait; no contention between
concurrent stages; no retries.

### The ceiling is optimistic, and the real number moves down

- **CPU:** Apple M2, 8 cores, **8 GB RAM**. At steady state all four stages run
  at once, and package alone spawns three concurrent x264 encodes.
- **Network is probably the binding constraint.** Every stage is
  S3-download → local work → S3-upload, and the workers run on the local Mac, so
  all of it crosses home broadband. This is why pure-I/O validate swings 14.3×
  while CPU-bound extract holds to 1.2×. Stage parallelism multiplies bandwidth
  demand, not just CPU.
- **Every measurement is already contaminated.** The emulator started 06:22:10
  and six jest workers at 06:23:28 — both before the first measured clip, both
  still running.

### The one real concurrency observation

Exactly one overlap episode exists, during the `verify-resume` window: three
stages of one job ran under another job's validate.

**The network-bound stage degraded; the CPU-bound stage did not.** That validate
took 130.0 s versus 35.9 s for a byte-identical file — 3.6× — while the other
job's 13-object package upload ran underneath it. The overlapping extract ran
47.8 s, the *fastest* of all five.

Suggestive of bandwidth contention. **n=1 episode, and validate already spans 7×
with no concurrency at all, so it proves nothing.**

### Queue state

`make queue-peek`: zero rows, fully drained. Consistent with five completed
clips and four idle workers.

### The experiment that would measure it

- **50 clips × 10 s**, submitted all at once so the bottleneck never waits for
  work — trickling arrivals measures the trickle, not the pipeline.
- **Record:** `queue ack` timestamps (completions/min, rolling), `held_ms` per
  stage, and `queue_wait_ms` per stage — *the stage whose wait grows
  monotonically is the bottleneck*, which settles package-vs-extract
  empirically. Sample queue depth per minute; capture CPU and interface counters
  to separate CPU saturation from link saturation.
- **Control:** shut down the emulator, Metro and jest first, or the result
  inherits today's contamination.
- **Cost: DERIVED, under $0.50** — ~1,000 PUTs, ~1.65 GB stored for a day, ~750
  DynamoDB writes, and ~2.2 GB of S3 egress to the Mac (~$0.19), which is the
  real term. Comfortably inside the $20 ceiling. Nothing new is provisioned.

**Caveat on scope:** all five clips are 8–14.4 s, and the pipeline spends
140–220 s of stage work on ~10 s of video (15–20× realtime). A 50-clip test at
10 s each measures the only length this pipeline has ever been exercised at.

---

## 4. Upload success rate

**The instrumentation this metric asks for does not exist, and the architecture
deliberately precludes it.** That is the finding.

`PROJECT_PLAN.md` specifies "track attempts, retries, failures in app analytics".
There is no analytics SDK in `mobile/` — zero matches for Firebase, Sentry,
Amplitude, Mixpanel, Segment or Bugsnag. `mobile/src/storage/jobIndex.ts`
persists exactly three fields per job: `job_id`, `filename`, `created_at`. No
counters, no `SharedPreferences`, no Room, no DataStore.

This is consistent with the design rather than an oversight: **the client
persists identifiers, never progress**, because resume is server-authoritative
via S3's `ListParts`. The only attempt counter in the system is WorkManager's own
`runAttemptCount`, which is per-work-request, reset on success and
garbage-collected.

So everything below is reconstructed from ephemeral logcat, DynamoDB, and the API
log — not read from a counter.

### The rate

**N = 7 genuine upload attempts. 5 succeeded, 2 failed.**

- **Overall: 5/7 = 71%**
- **Attributable to the upload path: 5/5 = 100%**
- **App / WorkManager path: 1/1**

15 further failures were **intentional** and are excluded: six tampered PUTs from
`verify-presign.sh` (corrupt signature, rewritten host, tampered key, tampered
part number, expired window, anonymous PUT — all correctly 403), three more
tampered PUTs on re-issued URLs in `verify-resume.sh`, plus its deliberate
404/409/410 assertions and two aborts.

**Say the N out loud: the mobile uploader has completed exactly one upload on
real AWS.** "100%" means "nothing failed in one attempt". At N=1 the confidence
interval is the whole range. Across the five completed uploads, 16 parts all
landed first-attempt with zero retries — again far too small to bound a per-part
failure rate.

The app upload itself: one worker run, 06:35:47.882 → SUCCESS 06:36:13.038
(~25 s). Round 1 only — `0/3 on S3, uploading 3 missing` → `uploaded 3 parts;
completing` → `removed the local copy`. Progress strictly monotonic.

### The two genuine failures were a test script's bug, not the uploader

Both came from a broken `verify-resume.sh` run. The API log is decisive:

```
WARN: job 41d90aaf… completing with 1 of 3 parts   → 409
WARN: job 25a58eea… completing with 0 of 3 parts   → 409
```

That run's `put_part_direct()` helper — which uses `aws s3api upload-part` with
ambient credentials, **not** presigned URLs, and checks no error — failed
silently against a bucket name that had not been sourced from `.env`. The bytes
never left the host, so neither failure exercised the presigned path, the app, or
a worker.

Two positives fall out: the resume endpoint worked correctly throughout,
correctly reporting missing parts; and **the server refused to assemble both
short objects.** The `INCOMPLETE_UPLOAD` guard did exactly its job.

**No hard-failure mode has ever fired.** `PART_SIZE_MISMATCH`, `FILE_MISMATCH`,
`SOURCE_MISSING`, `INCOMPLETE_OBJECT`, backoff — zero occurrences across 23,655
logcat lines. `PART_SIZE_MISMATCH` in particular is untriggered code on this
substrate. The queue and DLQ are empty; no message was ever dead-lettered.

### What is not covered — this matters more than the rate

- **No upload has ever been interrupted by a real network failure**, on any
  substrate.
- **Every measured upload ran over a laboratory-clean link** — logged
  `VALIDATED, NOT_METERED, NOT_CONGESTED, LinkUpBandwidth ≥ 12000 Kbps`.
- **App-kill-and-resume on real AWS is UNVERIFIED.** The resume contract was
  verified on the old LocalStack substrate; today's app run completed in one pass
  and killed nothing.
- **Doze and long uploads: UNKNOWN.** Every measured upload was ≤ 25 s.
- **`ListParts` paging never exercised** — 3–4 part fixtures never reach page two.
- **The presigned-URL expiry / re-presign round has never fired.** The multi-round
  resume loop is untested at runtime on real AWS.

---

## Defects surfaced while measuring

Recorded, not fixed — this was a measurement pass and the code was deliberately
left alone.

1. **`WORKER_CONCURRENCY` is a dead variable.** It appears in `.env` and
   `.env.example` and **no Go code reads it** — `internal/config/config.go` has no
   such field, `runner.go` sets `receiveMax = 1` and calls `r.handle(ctx, m)`
   synchronously. Effective concurrency is one message per stage. The setting
   promises a parallelism that does not exist. Either honour it or delete it.
2. **The package-stage slowdown is misattributed** in `docs/SETUP.md` and
   `docs/RUNBOOK.md`. See §2.
3. **Two DynamoDB rows are permanently stuck at `status=uploading`** with no S3
   upload behind them (their multipart uploads were aborted at the S3 level).
   Anything scanning for in-progress jobs will see two phantoms.
4. **591 of 947 status polls returned 404** — 62% of all status traffic. The
   app's job list polls IDs from the LocalStack era that no longer exist. Cost
   impact is nil; the noise is not.
5. **~42.8 MB of orphaned source copies on the emulator**, from three jobs whose
   records are long gone. `deleteSourceCopy` works — the completed jobs' dirs are
   empty — but the startup orphan sweep in the 8A plan was never built.
6. **`scripts/verify-presign.sh`'s header still says it has not been run against
   real AWS.** It ran today: 7/7 PASS.
7. **`config/free-tier.md` quotes stale DynamoDB pricing** ($1.25/M vs the
   current $0.625/M) and a Fargate figure ~13% low.

---

## Bottom line

**Cost is a solved problem and was never the risk.** Processing 100 clips costs
1.4 cents; the whole recurring bill is storage and views, and may be zero if the
100 GB egress allowance applies. The genuine cost trap is idle Fargate, which
this architecture already avoids by not using it.

**Latency is understood well enough to plan with** — ~150 s server-side for a
10-second clip — but the per-stage breakdown is contaminated by running
everything on one laptop, and only extract is stable enough to quote.

**Throughput is unknown and the ceiling is soft.** Measuring it costs under
$0.50 and half an hour, and is the single highest-value experiment left.

**Reliability is unknown.** Nothing has failed, and nothing has been made to
fail. One app upload on a clean link is not evidence of an offline-first
uploader working — and offline-first is the product.
