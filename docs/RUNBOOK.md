# DayReel Runbook

The page to open at 2am when the pipeline is silent. Commands to get from a cold
machine to a video moving through the pipeline, where to look when it does not,
and what Stage 8 actually shipped.

This is **not** a stage plan and not a design doc. Those exist and are long:
`docs/SETUP.md` for setup and the full caveat list, `docs/stage-plans/` for the
reasoning behind every decision. This file links out rather than restating.

**Evidence labels are used deliberately.** This project has repeatedly been
burned by green results that meant nothing, so nothing here implies more coverage
than exists:

- **VERIFIED** — observed on this machine, dated.
- **ASSUMED** — believed, with a reason, never measured.
- **UNKNOWN** — not measured, and do not guess.

**This runs on real AWS, and as of 2026-08-13 it has.** Account `384627056323`,
IAM user `dayreel-dev`, `us-east-1`. S3 and DynamoDB are the genuine services; the
queue is a SQLite file and the cache is a map inside the API process. There are no
containers anywhere in the pipeline. The line this file used to carry here — *"no
part of this system has ever touched real AWS"* — **is false as of today**: both
the backend path and the app path have been driven end to end against that
account, §1.5 and §1.6. Read `config/free-tier.md` anyway; there is no free tier
on this account and the budget is $20 total.

| Resource | Name |
|---|---|
| Raw uploads | `dayreel-raw-videos-3962bf6d` |
| Intermediates | `dayreel-processed-3962bf6d` |
| HLS output | `dayreel-hls-output-3962bf6d` |
| Job table | `dayreel-jobs` |

> ### OUTSTANDING TEARDOWN — the HLS bucket is public right now
>
> `dayreel-hls-output-3962bf6d` was opened for anonymous `GetObject` on
> 2026-08-13 to make playback work, and **has deliberately been left open at the
> user's explicit choice.** It is the one thing in this project that leaves a real
> AWS account worse than it found it. Close it when the demo is done:
>
> ```sh
> ./scripts/aws-hls-public.sh disable --yes
> ```
>
> What `enable` changed, what it displaced, and the blast radius:
> `docs/aws-public-hls.md`. Evidence that it was genuinely needed, and that it
> touched only that one bucket: §3.2.

---

## 1. Run it

Verified end to end 2026-08-13. Run the blocks in order.

### 1.1 Toolchain

RN 0.87 requires Node ≥ 22.13; this machine's default `node` is v20.11.0, so the
version has to be selected explicitly every session.

```sh
# Either of these works. VERIFIED 2026-08-13 that plain `nvm use 22` succeeds:
nvm use 22
# ...or bypass nvm entirely, which is what the verified runs used:
export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$PATH"

export ANDROID_HOME="$HOME/Library/Android/sdk"; export ANDROID_SDK_ROOT="$ANDROID_HOME"
export PATH="$ANDROID_HOME/platform-tools:$ANDROID_HOME/emulator:$PATH"
```

> **Correction to `docs/SETUP.md`.** SETUP says `nvm use` fails here because
> `~/.npmrc` sets a `prefix`, and prescribes `nvm use --delete-prefix 22`. That
> file **no longer exists** (VERIFIED 2026-08-13: `~/.npmrc` absent, `nvm use 22`
> exits 0 and yields v22.23.2). `--delete-prefix` is harmless but no longer
> required. Keep the `export PATH` form in scripts and non-interactive shells,
> where nvm's shell function is not loaded at all.

`ANDROID_HOME` on `PATH` is not optional — Gradle and `adb` both need it, and
`mobile/android/local.properties` does not exist in this checkout.

### 1.2 The stack — two Go processes and a file

```sh
make verify                         # credentials, three buckets, one table
make api                            # HTTP API on :8080, foreground
make workers                        # validate, extract, transcribe, package
curl localhost:8080/health          # {"status":"ok"}
```

**There is nothing to start and nothing to seed.** No containers, no AWS
emulator, no cache server: `cd infra && docker compose up -d --build` no longer
works, and `infra/` now holds a single `CONTEXT.md` recording what was removed and
why. S3 and DynamoDB are real and were created once by hand, so `make verify` is
the closest thing this stack has to "is my stack up?". The queue creates itself —
`QUEUE_DRIVER=sqlite` is the default and `backend/data/queue.db` appears on
whichever binary starts first. `QUEUE_DRIVER=sqs` is real SQS behind the same
interface and needs `make sqs-setup` first.

Use the Makefile rather than `go run`. `make api` and `make worker` do `cd
backend` first, and every target sources `.env` itself — Compose used to inject an
environment into each container and nothing does that now. **`cd backend && go run
./cmd/api` by hand does not read `.env`**; it falls back to the defaults in
`internal/config/config.go`, which name three bucket names somebody else owns.

> **`make workers` used to show nothing when you redirected it, and the fix is
> why the recipe looks odd.** It piped each stage through `sed "s/^/[$s] /"` to
> label the lines, and **`sed` block-buffers when its stdout is not a TTY** — so
> `make workers > run.log` printed the startup echo and then nothing,
> indefinitely, while all four workers ran perfectly and their output sat in 4 KiB
> of buffer. Unobservable in exactly the case you redirect for. VERIFIED
> 2026-08-13. The prefix is now a `while read` loop instead, which buffers per
> line, so a redirected `make workers` streams. There is no portable `sed` flag
> for this — `-u` is GNU, `-l` is BSD — which is why the loop rather than a flag.
>
> If you are on a checkout where `workers` still pipes through `sed`, the symptom
> is a silent log file and the workaround is `make worker STAGE=<stage>` per
> stage: that recipe has no filter in the path and has always streamed.

### 1.3 Emulator, Metro, app

```sh
emulator -avd dayreel-avd                 # the only AVD; keep it running
adb reverse tcp:8081 tcp:8081             # Metro must be reachable from the device
cd mobile && npx react-native start       # a debug APK loads its JS from Metro

# in another shell, with the same PATH exports:
cd mobile/android && ./gradlew :app:assembleDebug
adb install -r app/build/outputs/apk/debug/app-debug.apk
adb shell am start -n com.dayreel/.MainActivity
```

The debug APK is ~147 MiB. The app reaches the API at `http://10.0.2.2:8080`
(`mobile/src/api/client.ts`), which needs no `adb reverse` — only Metro does.

### 1.4 A video to push through it

```sh
adb push ./some-clip.mp4 /sdcard/Movies/
```

**It must be larger than 5 MiB.** S3 enforces a 5 MiB minimum on every part but
the last, and `UPLOAD_PART_SIZE` is clamped to that floor. A smaller clip is a
single part and exercises none of the multipart, resume or background-upload
machinery. A sub-5 MiB *part* uploads with a 200 and then fails the whole job at
`CompleteMultipartUpload` with `EntityTooSmall`.

Pick the video in the app. It creates the job, hands the upload to WorkManager,
and navigates to the player screen.

### 1.5 Expect it to take ~2m30s

**VERIFIED 2026-08-13 on real AWS.** Job
`a0a28d77-c364-4b69-adde-7d3723478064`: a 14,947,952-byte / 10 s / 1280x720 clip,
uploaded from the host as **3 real multipart parts** through presigned URLs, all
four stages succeeding on the **first attempt**, **~2m30s wall clock** — validate
11.8 s, extract 55.6 s, transcribe 3.8 s (mock), package 77.6 s.

**That is four times the old 36.7 s figure, and the substrate is the reason, not
a regression.** The 36.7 s was measured with S3 emulated on the same machine.
Every stage now makes real network round trips to S3, and `package` is the worst
of them because it uploads every segment of every rendition individually. The
direction is the helpful one: a slower pipeline makes stages *more* visible in the
UI, not less (§3.2).

Transcription is **mocked by default** (`MOCK_TRANSCRIBE=true`); transcripts read
`[mock transcript] segment N`. That is deliberate — real runs are budgeted, and
`whisper-cli` is now a host dependency because no image supplies it any more
(`docs/SETUP.md`). **Real transcription on this substrate is UNVERIFIED.**

### 1.6 The same clip from the app — also verified

**VERIFIED 2026-08-13 on real AWS.** Job
`5cbc3ee0-a5b4-4411-98bc-6955ed98940a`: the same clip picked in the app on
emulator `dayreel-avd`, uploaded by the Kotlin WorkManager background uploader —
logcat tag `DayReelUpload`, `round 1: 0/3 on S3, uploading 3 missing` →
`uploaded 3 parts; completing` — all four stages completed, and the reel **played
in-app to 00:10/00:10 with the caption track rendering** (`[mock transcript]
segment 4` visible on screen). ExoPlayer via AndroidX Media3 1.8.0.

That is the whole product path on real infrastructure: picker, background
uploader, presigned multipart, four stages, HLS playback with captions. What it
does **not** cover, none of which may be read as verified from it:

- **Real (non-mock) transcription** — UNVERIFIED on this substrate.
- **Upload resume across an app kill on real AWS** — UNVERIFIED here. The resume
  contract itself was verified on the emulator substrate (§4.1); this run
  completed in one pass and killed nothing.
- **Doze and long uploads** — UNKNOWN, as they were before.

---

## 2. Disk pressure, and what to prune when it gets tight

Disk hitting 100% has already killed the Docker daemon on this machine once: the
VM wrote its console log until ENOSPC and the daemon died. This is not
hypothetical. **VERIFIED 2026-08-13: 9.6 GiB free of 228 GiB, 96% used.**

### 2.1 The pipeline needs no containers at all

This section used to be about Docker. **Its premise is gone.** The API, the four
workers, the queue and the cache are two Go binaries and a SQLite file; S3 and
DynamoDB are remote. Nothing in the backend loop starts a container, so the old
advice — `docker compose stop` for free headroom while doing app-only work — has
nothing left to stop. Quitting Docker Desktop entirely costs the pipeline nothing.

**Docker is not unused, though, so do not act as if it were.** A Docker image is
still the route to the Whisper model: `infra_whisper-models` survived the compose
removal with the 141.1 MB `ggml-base.bin` in it, and `infra-worker-transcribe:latest`
is what copies it back out (`docs/SETUP.md`, "The model"). Real transcription is a
separate piece of work in flight. The claim to carry is "the pipeline itself needs
no containers", not "Docker is gone".

**Backend `go test ./...` needs nothing running** — VERIFIED 2026-08-13.
`internal/media` shells out to ffmpeg and *skips* when it is absent
(`requireFFmpeg`), and nothing in the suite opens a socket to AWS.

### 2.2 The measurement traps

Still worth keeping, because Docker remains on this disk even though the pipeline
no longer uses it.

- **`Docker.raw` is sparse. Use `du -sh`, never `ls -l`.** VERIFIED 2026-08-13:
  `ls` says 228 G apparent, `du` says 4.2 G actual.
- **`docker builder prune -f` does return bytes to the host — asynchronously.**
  Immediately after the prune the allocated size is unchanged; Docker Desktop's
  TRIM runs roughly 30 s later and the space appears. Measuring at the instant of
  the prune reports zero, which is how this was twice recorded as "does nothing"
  before anyone waited.
- **`docker system df`'s "reclaimable" column assumes nothing is running.**
  Recorded while the compose stack still existed: `Images 7, ACTIVE 7, 3.21GB,
  RECLAIMABLE 3.21GB (100%)` — 100% reclaimable while all seven backed live
  containers. With nothing running the figure is closer to honest now, but it is
  still a statement about running containers, not about what you still need.

### 2.3 Do not prune the whisper volume

- **`docker volume prune` / `docker system prune`.** `infra_whisper-models` holds
  the ~141 MB `ggml-base.bin`, and `infra-worker-transcribe:latest` is what gets
  it out of there. Losing both means a 141 MB re-download.
- **`infra_localstack-data` no longer holds anything the project needs.** It was
  all the job state when the emulator was the substrate; job state is DynamoDB and
  S3 now. Dropping it is **ASSUMED** safe — nothing in the current stack reads it,
  and that has not been measured because the two volumes together were only
  ~149 MB. There is nothing to gain from being right about it.

`docker builder prune -f` is still the only routinely safe one, and it now buys
back space for a toolchain that is not using it.

### 2.4 Safe to reclaim, and what it costs

Measured 2026-08-13; ~9 GiB total.

| Item | Freed | Cost |
|---|---|---|
| `~/.gradle/caches` (3.2 G today) | 3.3 GiB | slow next Gradle build. **Keep `~/.gradle/wrapper`** (148 M) or Gradle 9.4.1 re-downloads for nothing |
| `mobile/android/app/build` (2.4 G), `app/.cxx` (384 M) | 2.4 GiB | rebuild; gitignored |
| `docker builder prune -f` | 1.6 GiB | slower image rebuilds; async, see above. Nothing in the pipeline rebuilds an image any more |
| `mobile/node_modules/*/android/{build,.cxx}` | 1.0 GiB | rebuild — `react-native-screens`' NDK output is most of it |
| `~/.npm/_cacache`, `~/Library/Caches/Homebrew` | ~0.1 GiB | re-download |

### 2.5 Judgement calls, not routine cleanup

- **`~/.cache/huggingface`** was 7.9 GiB of model weights (faster-whisper,
  wav2vec2, PaddleOCR, vit-gpt2) belonging to *other projects on this machine*.
  **DayReel uses none of them** — it uses whisper.cpp with `ggml-base.bin` in the
  `infra_whisper-models` volume, and Stage 5A explicitly rejected faster-whisper.
  It was the single largest lever available and was cleared with the user's
  explicit approval. It is now empty. Deleting is reversible but means a slow
  re-download; never do it unasked.
- **Swap and sleepimage** — ~8 GiB of swapfiles plus a ~2 GiB sleepimage. A reboot
  clears them, but most swap is in active use and re-grows under the same
  workload; the sleepimage is the durable part. Do not delete swapfiles directly.

### 2.6 The constraint worth stating plainly

The Android SDK+NDK (8.8 GiB), Gradle caches and a running emulator (~1.2 GiB
plus swap pressure) do not all fit comfortably here — and that is *without*
counting the Docker VM, which is now dead weight for this project. **Stop the
emulator when not actively testing** — cheapest ~1 GiB to get back, and pure
overhead idle.

```sh
# fires once when free space drops below 3 GiB
until [ "$(df -k /System/Volumes/Data | tail -1 | awk '{print $4}')" -lt 3145728 ]; do sleep 30; done
echo "WARNING: disk below 3 GiB"
```

This caught the drop to 2.7 GiB today in time to stop an idle emulator before the
daemon died.

---

## 3. Debugging

### 3.1 Where to look, in order

```sh
make worker STAGE=validate                    # …extract, transcribe, package — one stage, its own stdout
make queue-peek                               # depth, receive counts and leases, per queue
adb logcat | grep DayReelUpload               # the whole background uploader
aws s3 ls --recursive s3://$S3_HLS_BUCKET/<job_id>/
curl localhost:8080/jobs/<job_id>             # status + per-stage detail
curl localhost:8080/jobs/<job_id>/reel        # hls_url + thumbnail_url
./scripts/verify-resume.sh                    # the whole 8A resume contract, no app — costs money
./scripts/abort-stale-uploads.sh              # what is holding parts; --abort to release
./scripts/verify-presign.sh                   # the presign negative matrix; runs from the host now
```

**There are no container logs to tail.** Each worker is a process and logs its own
stage to its own stdout, so the stage you care about is the one you start on its
own — `make worker STAGE=extract` — and `make workers` interleaves all four behind
a `[stage]` label (see the buffering note in §1.2). The API logs one line per request
in the foreground under `make api`; every call that names a job in its path leaves
the ID behind, but `POST /jobs` does not, because the ID is minted inside the
handler and returned in the body.

`make queue-peek` is the SQLite driver's view — depth per queue, receive count and
lease state per message, read straight out of `backend/data/queue.db`. On
`QUEUE_DRIVER=sqs` the equivalent is `make sqs-status`.

`scripts/verify-presign.sh` runs as committed now that URLs are signed for the
real regional endpoint. **No run of it against real buckets has been recorded**,
so what it expects is what SigV4 and Block Public Access specify rather than what
was observed — see `docs/SETUP.md`.

### 3.2 The traps

Each of these has cost real hours. The *why* is the part that stops it happening
twice.

**`10.0.2.2:8080` is still the API's address from the emulator, and the host
cannot route to it.** `10.0.2.2` is the Android emulator's alias for the host's
loopback interface, and `mobile/src/api/client.ts` hardcodes
`http://10.0.2.2:8080`. Curling that address from your Mac times out **by
design**. That part has not changed.

**The presigned-URL half of this trap dissolved on real AWS.**
`S3_PUBLIC_ENDPOINT` is now **empty**: the SDK signs the genuine regional S3
endpoint, and the host and the phone resolve it identically. There is no address
to override, nothing to keep in sync between the API and the package worker, and
no `curl --connect-to` incantation to remember — `scripts/verify-resume.sh` and
`scripts/verify-presign.sh` both run from the host as committed. The `4566`
endpoint table that used to live here was an emulator artefact and is gone with
it.

What has *not* changed, and never will, is the fact underneath: **SigV4 covers the
`Host` header**, so a presigned URL cannot be string-rewritten to a different
address without invalidating it. `S3_PUBLIC_ENDPOINT` stays in `.env.example` for
the case that generalises to — a proxy in front of S3. If you ever set it, it must
name the host the **uploader** will use, and it must be set for the API (which
signs uploads) and the package worker (which formats the `hls_url`) together, or
upload and playback disagree about who the client is.

**A URL that resolves tells you nothing about whether the bucket is readable —
and this is now VERIFIED on real S3 rather than predicted.** LocalStack used to
accept bucket policies, `put-public-access-block` and lifecycle rules, read them
back verbatim, and enforce none of them: unsigned `GET` and `PUT` succeeded
against any bucket. That produced false confidence three times, most expensively
when Stage 6A built `thumbnail_url` against `dayreel-processed`, a bucket with no
read grant — every local run served it 200. The docs predicted a 403 on real S3.

**They were right. VERIFIED 2026-08-13:** `dayreel-hls-output-3962bf6d` started
with all four Block Public Access settings ON and no bucket policy, and an
anonymous `GET` of `master.m3u8` returned **403**. Playback was genuinely blocked.
`./scripts/aws-hls-public.sh enable` — which relaxes the two policy-guarding BPA
settings and attaches a `GetObject`-only policy on `/*` — fixed it: `master.m3u8`,
all three variant playlists, the segments, `subs/playlist.m3u8`, `subs_000.vtt`
and `thumbnail.jpg` all return **200** anonymously, while an anonymous bucket
**LIST still returns 403**. The grant is per-object read, not a directory.

**Account-level BPA is not configured on this account.** VERIFIED 2026-08-13:
`aws s3control get-public-access-block` returns
`NoSuchPublicAccessBlockConfiguration`, so opening that one bucket touched that
one bucket and nothing else in the account. This was **ASSUMED** before — and
Stage 8 planning assumed the *opposite*, that opening a single bucket required
disabling an account-wide guardrail. It did not.

> Only `dayreel-hls-output-3962bf6d` has a read grant. Any client-facing URL
> built against a different bucket is wrong regardless of what it returns. Check
> the bucket in the URL, not the response code — and remember this one is open
> **right now** (teardown note at the top of this file).

The raw bucket is already configured for the upload path, VERIFIED 2026-08-13:
CORS with `ExposeHeaders: ["ETag"]` and nothing else — real S3 rejects a wildcard
there outright with `InvalidRequest`, which the emulator accepted for days — plus
an `abort-incomplete-multipart` lifecycle rule at 1 day.

**`GET /jobs/{id}` is served from a 10 s in-process cache, and no worker can
invalidate it.** The cache is a map inside the API process
(`backend/internal/cache/memory.go`). It used to be Redis, and the coherence gap
is **stronger** now, not weaker: the workers are separate processes, so a worker
cannot reach the API's cache *even in principle* — there is no cache server
between them to invalidate. The API invalidates on `POST /jobs/{id}/complete` and
`DELETE /jobs/{id}/upload`, the two transitions it owns; every stage transition
after that belongs to a worker and none of them reach it. On a ~2m30s real-AWS run
most stages *are* observable, comfortably longer than the TTL; on the tiny
synthetic clips everything before Stage 7 was tested with, the whole pipeline
finished inside one cached response and the UI jumped from `uploading` to
`completed`. Known, deliberate, left open. To watch stages transition, read the
worker logs and `make queue-peek` — not the API. (`GET /jobs/{id}/reel` bypasses
the cache and reads DynamoDB directly.)

**`POST /jobs/{id}/complete` is what starts the pipeline.** It calls
`CompleteMultipartUpload` and then **enqueues the validate message itself**. There
is no S3 event notification any more: real S3 cannot notify a SQLite file, so the
API does directly what the emulator's bucket notification used to do
(`infra/CONTEXT.md` → Deferred). Skip the call and the parts sit in S3 holding
storage, invisible to `aws s3 ls`, billed, and nothing runs. If a job is stuck in
`uploading` with no worker activity, this is the first thing to check.

**Metro uses inline-requires** (`inlineRequires: true` is the RN 0.87 default in
`@react-native/metro-config`; `mobile/metro.config.js` does not override it). A
native module is therefore loaded on **first use**, not at startup. A module that
fails to resolve produces no startup error and no log line — it simply is not
there when something asks. This cost real debugging time; see §4.1.

**JS `console.log` does not reach `logcat` on this setup.** VERIFIED by
observation 2026-08-13; the mechanism is **ASSUMED** (bridgeless RN routes console
output to the Metro terminal). Watch the Metro terminal for JS, `logcat` for
native. Do not conclude from a silent `logcat` that JS did not run.

**Abandoned multipart uploads are invisible, and now they cost real money.** They
appear in no object listing — not `aws s3 ls`, not the console — and bill as
storage from the first part until a completion or an abort. `ListMultipartUploads`
is the only thing that sees them. `scripts/abort-stale-uploads.sh` lists by
default and aborts only with `--abort`, because age is the only signal separating
"abandoned" from "still in progress". The raw bucket's 1-day
`abort-incomplete-multipart` lifecycle rule is a backstop, not the mechanism — it
sweeps up a day later, after a day of billing.

**The device's job list is device-local and does not survive reinstall.** There is
no `GET /jobs`. The jobs themselves are fine — recover IDs from the API's
foreground log under `make api`, or from the top-level prefixes in the buckets
themselves:

```sh
aws s3 ls s3://$S3_PROCESSED_BUCKET/    # jobs that reached at least validate
aws s3 ls s3://$S3_RAW_BUCKET/          # every job whose upload completed
```

---

## 4. Stage 8 as actually built

Plans: `docs/stage-plans/stage-8a-background-upload.md`,
`docs/stage-plans/stage-8b-hls-playback.md`. What follows is what the code does,
which in 8A's case is **ahead of its own plan document** — see the warning at the
end of §4.1.

### 4.1 8A — background upload

**VERIFIED on device 2026-08-13.** A Kotlin WorkManager worker
(`mobile/android/app/src/main/java/com/dayreel/upload/`) uploads parts with OkHttp
and survives the app being killed. The uploader has since also driven a full job
against **real S3** (§1.6); the kill-and-resume half of the claim was verified on
the emulator substrate and has **not** been repeated on real AWS. WorkManager persists the request in its own
database and re-registers it with JobScheduler, so the OS restarts the process
headless — no Activity, no React instance, no JavaScript — purely to run `doWork`
again. A JS uploader dies with the process and cannot come back until a human
opens the app; that is the whole argument for the Kotlin module.

The division of labour: JS creates the job (`POST /jobs`), records it in the local
index *before* the transfer starts, and hands WorkManager a job ID and file path
(`mobile/src/upload/uploadVideo.ts`). It then only ever asks what happened. The
worker asks `POST /jobs/{id}/upload-urls` what is actually missing — answered from
S3's own `ListParts`, not from anything the client remembers — and calls
`/complete` itself. **The client persists identifiers, never progress.** That is
what makes the worker safe to kill at any instant: there is no partially-written
local state for a kill to corrupt.

**The module is registered as a LEGACY module, not a TurboModule, and that detail
is load-bearing.** Under the New Architecture a Java module flagged
`isTurboModule = true` is resolved in two halves: the package supplies the Java
instance, and C++ asks `DefaultTurboModuleManagerDelegate::javaModuleProvider` for
a **codegen-generated spec** to bind it to JSI. That provider is built only from a
`codegenConfig` block in `mobile/package.json` — and there isn't one. So the C++
half returned `nullptr`, `TurboModuleRegistry.get` returned null, and
`getLegacyModule` skipped the module for being flagged a TurboModule. It resolved
via **neither** path.

Nothing announced this. `isBackgroundUploadAvailable()` returned false, HomeScreen
fell back — silently, by design — to Stage 7's foreground uploader, and the app
behaved exactly as it had before 8A existed: green build, working upload, none of
the feature. Measured on device rather than reasoned about:

```
before  turboRegistryGet=false nativeModulesLookup=false resolved=false
after   turboRegistryGet=true  nativeModulesLookup=true  resolved=true
```

`isTurboModule = false` routes it through the interop binding, which builds the
JSI wrapper by reflecting over `@ReactMethod` and needs no generated spec. That
path is on by default in this version and is how every non-codegen'd native module
in the app already works. **This is the single most confusing thing in the stage**,
and inline-requires (§3.2) is why it stayed hidden: the module is never touched at
startup, so nothing fails until something asks. The full workings are in
`DayReelUploadPackage.kt`'s docstring — read it before changing that flag.

**The client verifies part *sizes* on resume, not just presence.** `ListParts`
answers "which part numbers exist", not "are they intact" — and killing the worker
mid-PUT, which is this stage's own test method, left part 2 of 3 at 524,288 bytes
of an expected 5,242,880. The next run read "2/3 on S3", uploaded only part 3, and
completed. It was caught **only because S3 rejects a non-final part under 5 MiB**;
a truncation landing above 5 MiB would have assembled an object with a hole in the
middle that every downstream stage processed as a video. The worker now compares
every listed part against what the file says it should be, and refuses to complete
unless the bytes on S3 equal the source length. A successful
`CompleteMultipartUpload` is evidence S3 was willing to assemble an object, not
that the object is the file.

**Known gap — no foreground service actually runs.** `setForeground` is refused
(`mAllowStartForeground false`); Android 12+ can decline to start one from the
background. The code treats refusal as a downgrade to ordinary deferrable work
rather than a failure, which is right — the upload continues. Permissions are
declared in the manifest but `POST_NOTIFICATIONS` is never requested at runtime.
**VERIFIED for a ~40 s upload. Doze behaviour and long uploads are UNKNOWN.**

**Known gap — a truncated part cannot be recovered client-side.**
`/upload-urls` treats a wrong-sized part as *present*, so it will never presign a
URL to overwrite one. The worker's only option is to abort and fail with
`PART_SIZE_MISMATCH`, which it does rather than finalise a corrupt object or retry
forever against one. **The server-side fix — re-presigning wrong-sized parts in
the missing-part computation — is not done.**

> **The 8A plan document's status header is STALE.** It still reads *"the headline
> feature is NOT verified … no background upload has ever been observed to run"*,
> written before the two fixes above landed (`b35e3f1`, `81e943c`). Trust the code
> and this section over that header.

### 4.2 8B — playback

**VERIFIED on device 2026-08-13, and re-verified against real AWS the same day**
(§1.6 — played to 00:10/00:10 with captions on screen). `react-native-video` v6
under RN 0.87 with `newArchEnabled=true`, ExoPlayer via AndroidX Media3 1.8.0.
Plays the HLS master, all three renditions, captions render and the track picker
lists `en / English`. Resolution selector defaults to
`SelectedVideoTrackType.AUTO` (`mobile/src/screens/PlayerScreen.tsx`).

Dev-only UI — the HLS and thumbnail URLs, and the caption-offset probe — is gated
behind `__DEV__`, a compile-time constant, and confirmed absent from a
`--dev false` bundle. The thumbnail URL is shown as **text and deliberately not
loaded as a poster**. It used to point into `dayreel-processed`, which resolved
only because the emulator served unsigned GETs to any bucket; the package stage
now publishes the frame into the HLS bucket instead (`3dc776e`), and VERIFIED
2026-08-13 `thumbnail.jpg` returns 200 anonymously from real S3 — **because that
bucket has been deliberately opened (§3.2), not because everything happens to be
served locally.** Close the bucket and it is a 403 again, correctly.

**Open issue, stated plainly: ExoPlayer reads captions 66.8 ms LATE.**

| Player | Anchors on | Offset |
|---|---|---|
| AVFoundation | video start PTS (`MPEGTS:6000`) | 0.333 ms late |
| ExoPlayer | zero | **66.8 ms late** |

Measured on two boundaries independently (3.000 → 3.0668 s, 6.000 → 6.0668 s,
±1.2 ms), so it is a constant shift, not a one-cue artefact. `66.8 ms` is
`6000/90000` — the header's own `MPEGTS` value — so ExoPlayer behaves as if it
*adds* what AVFoundation *subtracts*. The `X-TIMESTAMP-MAP` fix
(`backend/internal/media/subtitles.go`) was validated on AVFoundation and **the
project ships Android**: for ExoPlayer alone it trades one 66.7 ms error for the
same error mirrored. Undecided.

**The mechanism is inferred from arithmetic, not read out of ExoPlayer's
`WebvttExtractor`. A different `MPEGTS` value's effect therefore CANNOT be
predicted without re-measuring.** The probe on `PlayerScreen` in dev builds is the
instrument — use it; do not reason about it. Note also that 8B's own plan document
still lists the ExoPlayer measurement as NOT DONE; the numbers live in
`docs/stage-plans/stage-6a-package-worker.md` under "ExoPlayer, measured".

---

## 5. Before pushing

```sh
cd backend && go build ./... && go vet ./... && go test ./...
cd mobile  && npx tsc --noEmit && npx jest --ci && npx eslint src __tests__
```

All six pass as of 2026-08-13. None of them require containers or an emulator, and
none of them talk to AWS.
