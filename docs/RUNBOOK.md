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

Everything below is LocalStack-only. **No part of this system has ever touched
real AWS.** See `config/free-tier.md` before it does.

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

### 1.2 The stack — 7 containers

```sh
cd infra && docker compose up -d --build
curl localhost:8080/health          # {"status":"ok"}
```

`localstack`, `redis`, `api`, and one worker per stage: `validate`, `extract`,
`transcribe`, `package`. LocalStack's init script (`infra/localstack/init-aws.sh`)
creates the buckets, queues, DynamoDB table and the S3→SQS notification that
starts the pipeline.

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

**It must be larger than 5 MiB.** S3 and LocalStack both enforce a 5 MiB minimum
on every part but the last, and `UPLOAD_PART_SIZE` is clamped to that floor. A
smaller clip is a single part and exercises none of the multipart, resume or
background-upload machinery. A sub-5 MiB *part* uploads with a 200 and then fails
the whole job at `CompleteMultipartUpload` with `EntityTooSmall`.

Pick the video in the app. It creates the job, hands the upload to WorkManager,
and navigates to the player screen.

### 1.5 Expect it to take ~37 seconds

VERIFIED 2026-08-13 on a 14.9 MB / 10 s 720p clip: 36.7 s end to end — validate
4.0 s, extract 4.1 s, transcribe 0.04 s (mock), package 12.8 s. Transcription is
**mocked by default** (`MOCK_TRANSCRIBE=true`); transcripts read
`[mock transcript] segment N`. That is deliberate — real runs are budgeted.

---

## 2. Docker, and what to prune when disk gets tight

Disk hitting 100% has already killed the Docker daemon on this machine once: the
VM wrote its console log until ENOSPC and the daemon died. This is not
hypothetical. **VERIFIED 2026-08-13: 9.6 GiB free of 228 GiB, 96% used.**

### 2.1 When Docker is actually needed

Only for the **pipeline** — uploads, workers, S3/SQS/DynamoDB. Pure mobile work
does not need it: `npx tsc --noEmit`, `npx jest`, `npx eslint` and
`./gradlew :app:assembleDebug` all run with the stack down. So
`cd infra && docker compose stop` is free headroom while doing app-only work.

**Backend `go test ./...` does not need Docker either** — VERIFIED 2026-08-13.
Nothing in the Go suite connects to LocalStack; `internal/media` shells out to
ffmpeg and *skips* when it is absent (`requireFFmpeg`), and the packager's
`localhost:4566` references are string assertions.

### 2.2 The measurement traps

- **`docker system df` reports images "100% reclaimable" while every one of them
  is in use.** VERIFIED today: `Images 7, ACTIVE 7, 3.21GB, RECLAIMABLE 3.21GB
  (100%)`. The figure assumes nothing is running. Those images back the 7 live
  containers and are not reclaimable without tearing down the stack.
- **`Docker.raw` is sparse. Use `du -sh`, never `ls -l`.** VERIFIED today: `ls`
  says 228 G apparent, `du` says 4.2 G actual.
- **`docker builder prune -f` does return bytes to the host — asynchronously.**
  Immediately after the prune the allocated size is unchanged; Docker Desktop's
  TRIM runs roughly 30 s later and the space appears. Measuring at the instant of
  the prune reports zero, which is how this was twice recorded as "does nothing"
  before anyone waited.

### 2.3 Never prune these

- **`docker volume prune` / `docker system prune`.** `infra_localstack-data` holds
  all job state — buckets, queues, the DynamoDB table. `infra_whisper-models`
  holds the ~141 MB Whisper model. Losing either is a real setback, and the two
  volumes together are only ~149 MB, so there is nothing to gain.
- **The images.** Rebuilding `worker-transcribe` recompiles whisper.cpp.

`docker builder prune -f` is the only routinely safe one.

### 2.4 Safe to reclaim, and what it costs

Measured 2026-08-13; ~9 GiB total.

| Item | Freed | Cost |
|---|---|---|
| `~/.gradle/caches` (3.2 G today) | 3.3 GiB | slow next Gradle build. **Keep `~/.gradle/wrapper`** (148 M) or Gradle 9.4.1 re-downloads for nothing |
| `mobile/android/app/build` (2.4 G), `app/.cxx` (384 M) | 2.4 GiB | rebuild; gitignored |
| `docker builder prune -f` | 1.6 GiB | slower image rebuilds; async, see above |
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

The Android SDK+NDK (8.8 GiB), Gradle caches, a running emulator (~1.2 GiB plus
swap pressure) and Docker do not all fit comfortably here. **Stop the emulator
when not actively testing** — cheapest ~1 GiB to get back, and pure overhead idle.

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
docker logs -f dayreel-worker-validate        # …-extract, -transcribe, -package
docker logs dayreel-api | grep POST           # job IDs, if the device lost its list
adb logcat | grep DayReelUpload               # the whole background uploader
docker exec dayreel-localstack awslocal s3 ls --recursive s3://dayreel-hls-output/
curl localhost:8080/jobs/<job_id>             # status + per-stage detail
curl localhost:8080/jobs/<job_id>/reel        # hls_url + thumbnail_url
./scripts/verify-resume.sh                    # the whole 8A resume contract, no app
./scripts/abort-stale-uploads.sh              # what is holding parts; --abort to release
./scripts/verify-presign.sh                   # cannot run as committed — see 3.2
```

### 3.2 The traps

Each of these has cost real hours. The *why* is the part that stops it happening
twice.

**`10.0.2.2` is the emulator's alias for the host loopback, and the host cannot
route to it.** Presigned URLs are signed against `S3_PUBLIC_ENDPOINT`, and SigV4
covers the `Host` header — so the URL cannot be rewritten to an address you *can*
reach without invalidating it. That is the whole point of the setting, not a bug.
Consequences: `curl http://10.0.2.2:4566/` from your Mac times out **by design**,
and `scripts/verify-presign.sh` cannot run while the committed value is in place.
The workaround is `curl --connect-to`, which redirects the TCP connection while
leaving the signed `Host` intact — `scripts/verify-resume.sh` uses it and is the
model to copy:

```sh
curl --connect-to 10.0.2.2:4566:127.0.0.1:4566 ...
```

`S3_PUBLIC_ENDPOINT` must match on **both** `api` (which signs uploads) and
`worker-package` (which formats the `hls_url`), or upload and playback will
disagree about who the client is. One client environment at a time.

**LocalStack accepts security configuration and silently ignores it.** Bucket
policies, `put-public-access-block` and lifecycle rules all apply cleanly, read
back verbatim, and do nothing. Unsigned `GET` and `PUT` succeed against any
bucket. A lifecycle rule with `DaysAfterInitiation=0` — which real S3 rejects
outright — is accepted here and never fires. **This has produced false confidence
three times**, most expensively when Stage 6A built `thumbnail_url` against
`dayreel-processed`, a bucket with no read grant: every local run served it 200,
and it would have been a 403 on real S3. The generalisation:

> **A URL that resolves locally tells you nothing about whether the bucket it
> names is readable.** Only `dayreel-hls-output` has a read grant. Check the
> bucket in the URL, not the response code.

Anything about access control is **UNVERIFIABLE locally** and must be asserted
against real S3 when buckets are first provisioned.

**Redis caches `GET /jobs/{id}` for 10 s and no worker invalidates it.** The API
invalidates on `/complete` and on abort (`handlers.go`), but the workers write
status straight to DynamoDB and never touch Redis, so stage transitions can lag
by up to 10 s or be missed entirely. On a real 37 s clip most stages *are*
observable; on the tiny synthetic clips everything before Stage 7 was tested with,
the whole pipeline finished inside one cached response and the UI jumped from
`uploading` to `completed`. Known, deliberate, left open. To watch stages
transition, read the worker logs — not the API. (`GET /jobs/{id}/reel` bypasses
the cache and reads DynamoDB directly.)

**`POST /jobs/{id}/complete` is what starts the pipeline.** It calls
`CompleteMultipartUpload`, which materialises the object, which fires the S3
`ObjectCreated` notification onto the `dayreel-validate` queue. Skip it and the
parts sit in S3 holding storage, invisible to `aws s3 ls`, and nothing runs. If a
job is stuck in `uploading` with no worker activity, this is the first thing to
check.

**Metro uses inline-requires** (`inlineRequires: true` is the RN 0.87 default in
`@react-native/metro-config`; `mobile/metro.config.js` does not override it). A
native module is therefore loaded on **first use**, not at startup. A module that
fails to resolve produces no startup error and no log line — it simply is not
there when something asks. This cost real debugging time; see §4.1.

**JS `console.log` does not reach `logcat` on this setup.** VERIFIED by
observation 2026-08-13; the mechanism is **ASSUMED** (bridgeless RN routes console
output to the Metro terminal). Watch the Metro terminal for JS, `logcat` for
native. Do not conclude from a silent `logcat` that JS did not run.

**Abandoned multipart uploads are invisible.** They appear in no object listing —
not `aws s3 ls`, not the console — and bill as storage from the first part until a
completion or an abort. `ListMultipartUploads` is the only thing that sees them.
`scripts/abort-stale-uploads.sh` lists by default and aborts only with `--abort`,
because age is the only signal separating "abandoned" from "still in progress".

**The device's job list is device-local and does not survive reinstall.** There is
no `GET /jobs`. The jobs themselves are fine — recover IDs from
`docker logs dayreel-api | grep POST` or from the prefixes in
`awslocal s3 ls s3://dayreel-processed/`.

---

## 4. Stage 8 as actually built

Plans: `docs/stage-plans/stage-8a-background-upload.md`,
`docs/stage-plans/stage-8b-hls-playback.md`. What follows is what the code does,
which in 8A's case is **ahead of its own plan document** — see the warning at the
end of §4.1.

### 4.1 8A — background upload

**VERIFIED on device 2026-08-13.** A Kotlin WorkManager worker
(`mobile/android/app/src/main/java/com/dayreel/upload/`) uploads parts with OkHttp
and survives the app being killed. WorkManager persists the request in its own
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

**VERIFIED on device 2026-08-13.** `react-native-video` v6 under RN 0.87 with
`newArchEnabled=true`. Plays the HLS master, all three renditions, captions render
and the track picker lists `en / English`. Resolution selector defaults to
`SelectedVideoTrackType.AUTO` (`mobile/src/screens/PlayerScreen.tsx`).

Dev-only UI — the HLS and thumbnail URLs, and the caption-offset probe — is gated
behind `__DEV__`, a compile-time constant, and confirmed absent from a
`--dev false` bundle. The thumbnail URL is shown as **text and deliberately not
loaded as a poster**: it points into `dayreel-processed`, which resolves here only
because LocalStack serves unsigned GETs to any bucket (§3.2).

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

All six pass as of 2026-08-13. None of them require Docker or an emulator.
