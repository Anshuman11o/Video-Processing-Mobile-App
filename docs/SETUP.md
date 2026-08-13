# Setup and known limitations

How to run DayReel locally, and the caveats that will otherwise be discovered the
hard way.

## Running the stack

```bash
cd infra && docker compose up -d --build
curl localhost:8080/health          # {"status":"ok"}
```

Seven containers: `localstack`, `redis`, `api`, and one worker per pipeline stage
(`validate`, `extract`, `transcribe`, `package`).

Everything runs against LocalStack by default. **`docker-compose.yml` hardcodes
`USE_LOCALSTACK=true` on every service** — there is no `${...}` substitution and
no `env_file`, so a root `.env` does **not** reach the containers. Changing where
the stack points means editing compose, not `.env`.

See `config/free-tier.md` before pointing anything at real AWS: there is no free
tier on this account, the budget is $20 total, and every AWS resource must be
torn down after the run that needed it.

## Known limitations

### The job list is device-local and does not survive reinstall

The mobile app keeps its list of jobs in a file on the device, written when each
job is created. **Reinstalling the app, clearing its data, or switching
emulators loses the entire visible job history.**

The jobs themselves are not lost. They still exist in DynamoDB, their output is
still in S3, and any one of them is still reachable by ID:

```bash
curl localhost:8080/jobs/<job_id>
curl localhost:8080/jobs/<job_id>/reel
```

What is lost is only the client's knowledge of *which* IDs exist.

**Why it works this way:** there is no `GET /jobs` endpoint. The API exposes
`POST /jobs`, `GET /jobs/{id}`, `POST /jobs/{id}/complete` and
`GET /jobs/{id}/reel` — a job can be fetched by ID but not discovered. The
DynamoDB table is single-table with `PK = JOB#<uuid>`, so listing jobs would need
either a full table scan or a new GSI. On a project with a handful of runs left
before it closes, a device-local index was chosen over adding a scan that quietly
costs money on real AWS.

**If you need the history back**, the job IDs are recoverable from the API logs
(`docker logs dayreel-api | grep POST`) or by listing the processed bucket:

```bash
docker exec dayreel-localstack awslocal s3 ls s3://dayreel-processed/
```

Each top-level prefix is a job ID.

### Pipeline stages are usually invisible in the UI

`GET /jobs/{id}` is served from Redis with a 10-second TTL, and no worker
invalidates that cache.

So a job can begin and finish inside a single cached response, and the app will
then jump straight from `uploading` to `completed` without displaying any
intermediate stage. The progress display is correct when it updates; it just
often has nothing to show.

> **Correction, 2026-08-13.** This section used to assert that "the whole
> pipeline completes in about four seconds", which made stages *always*
> invisible. That figure came from the tiny synthetic clips every stage before 7
> was tested with. The first real upload from the app — 14.9 MB, 10 s of
> 1280x720 — took **36.7 s end to end** (`validate` 4.0 s, `extract` 4.1 s,
> `transcribe` 0.04 s under the mock, `package` 12.8 s). That is comfortably
> longer than the TTL, so on a real clip some stages *are* observable. The cache
> gap is still real; it is just not total.

This is a known, deliberate trade — the cache-coherence gap was left open rather
than fixed. To watch stages transition, bypass the cache and read the workers
directly:

```bash
docker logs -f dayreel-worker-validate
```

### One client environment at a time

Presigned upload URLs are signed against a specific host, so **`S3_PUBLIC_ENDPOINT`
on the `api` service determines which client can upload**, and only one at a time:

| Client | `S3_PUBLIC_ENDPOINT` |
|---|---|
| **Android Emulator (committed)** | `http://10.0.2.2:4566` |
| iOS Simulator | `http://localhost:4566` |
| Physical device | `http://<your-lan-ip>:4566` |

`10.0.2.2` is the Android emulator's alias for the host's loopback interface.

Switching targets means editing compose and restarting `api`. A URL signed for
one host returns `SignatureDoesNotMatch` to another — the signature covers the
`Host` header, so it cannot simply be rewritten.

The committed target is the **Android Emulator**. Set on both `api` (which signs
the upload URLs) and `worker-package` (which formats the `hls_url` the client is
handed); the two must name the same client environment or upload and playback
will disagree about who the client is.

#### Consequence: `verify-presign.sh` no longer runs from the host as-is

`scripts/verify-presign.sh` runs the presigned-URL negative-test matrix from the
**host**. With the committed value, URLs are signed for `10.0.2.2:4566` — an
address this machine does not route to — so the script cannot reach them and
cannot run unchanged. This is a real regression from the platform switch, not a
bug in the script.

**Re-checked 2026-08-13 and still accurate**, now that the emulator path is
verified rather than hypothetical: `curl http://10.0.2.2:4566/` from the host
times out, and no `lo0` alias has been added. Nothing about getting the app
working changed this.

**There is now a third workaround, and it is the best of the three.**
`scripts/verify-resume.sh` faces the same problem and solves it with
`curl --connect-to`, which redirects the *connection* while leaving the signed
`Host` header intact:

```bash
curl --connect-to 10.0.2.2:4566:127.0.0.1:4566 ...
```

No compose edit, no `sudo`, no machine-level network state. `verify-presign.sh`
does not use it and would have to be changed to; the two options below are what
work with the script exactly as committed.

To run the matrix unmodified, point the `api` service back at the host loopback
temporarily:

```bash
# edit infra/docker-compose.yml: api -> S3_PUBLIC_ENDPOINT=http://localhost:4566
cd infra && docker compose up -d api
./scripts/verify-presign.sh
# then restore 10.0.2.2 and restart api before running the app
```

The alternative — untested here, and left to you because it needs `sudo` and
changes machine-level network state — is to make the address host-routable by
aliasing it onto the loopback interface:

```bash
sudo ifconfig lo0 alias 10.0.2.2      # not run for you; your call
```

That would let both the emulator and the host reach the same signed host, and
would remove the need to flip compose back and forth.

### LocalStack does not enforce bucket authorization

`S3_SKIP_SIGNATURE_VALIDATION=0` in compose makes LocalStack check presigned
signatures, and it checks them properly: a corrupted signature, a rewritten
host, a swapped object key, a changed part number and a shortened expiry are
all rejected with 403.

What it does **not** check is whether a request is authenticated at all. An
unsigned `PUT` to any bucket returns 200 and writes the object, and an unsigned
`GET` reads it back. Holding a presigned URL therefore confers no privilege
locally — it is merely one way in.

**This is a LocalStack Community limitation, not a defect in this repo.** Real
S3 has Block Public Access enabled by default and denies anonymous requests.
But that is an assumption, and it is the assumption this whole upload design
rests on, so it must be **asserted when real buckets are provisioned** rather
than inherited by luck. No real-AWS provisioning exists yet — when it is
written, it must set Block Public Access explicitly and re-run the matrix with
`TARGET=aws`, where the anonymous row is a hard failure instead of a known gap.

**"Enabled by default" means at the *bucket* level, and the distinction
matters.** Since April 2023 every newly created bucket gets all four BPA
settings on. **Account-level BPA is opt-in and is not configured by default**,
so on an ordinary account there is no account-wide guardrail to disable and
making one bucket readable touches only that bucket. Where an account-level
setting *has* been turned on it overrides the bucket — S3 applies the most
restrictive combination of the two — and relaxing it then genuinely does affect
every bucket in the account. Recorded here because the opposite was assumed
during Stage 8 planning: that opening a single bucket "generally requires
disabling Block Public Access for the entire account". It does not.

**The gap cannot be closed locally, and it fails in the most misleading way.**
Applying a `Deny`-anonymous bucket policy and a full
`put-public-access-block` both **succeed** against LocalStack — the calls
return cleanly and the configuration reads back — and an unsigned `PUT`
still returns 200 afterwards. LocalStack stores the security configuration
and ignores it. So provisioning code that sets Block Public Access will look
correct here while proving nothing, and neither setting is applied in
`init-aws.sh` precisely because doing so would manufacture that false
assurance. This one is only verifiable against real S3.

The `dayreel-hls-output` bucket is the deliberate exception: it is public by
design locally, because HLS playlists reference segments by relative path and
cannot be presigned. Its real-AWS access model is still an open question — see
`config/free-tier.md`.

**This has already shipped one bug, and it is the shape to watch for.** Stage 6A
built `thumbnail_url` against `dayreel-processed`, which has no bucket policy and
no CORS. Every local run served it with a 200 — that bucket is no more protected
here than any other — so the URL looked correct in the API response, in the app,
and in 6A's own verification, while being a 403 on real S3. Fixed 2026-08-13 by
publishing the frame into the HLS bucket instead; the full argument is
**[DECIDE 6]** in `docs/stage-plans/stage-6a-package-worker.md`.

The generalisation is worth stating on its own: **a URL that resolves locally
tells you nothing about whether the bucket it names is readable.** Only the
`dayreel-hls-output` bucket has a read grant, so any client-facing URL built
against a different one is wrong regardless of what the local stack returns —
and no test that runs here can tell you so, because the failure it would catch
does not happen here. Check the bucket in the URL, not the response code.

### Transcription is mocked by default

`MOCK_TRANSCRIBE=true` in compose. Transcripts read `[mock transcript] segment N`.
This is deliberate: real runs are budgeted, and every stage downstream of
transcribe is developed against the mock. Set it to `false` and rebuild
`worker-transcribe` for a real transcription; the model downloads once (~141 MB)
into a named volume and persists after that.

### Captions: fixed, and 6A's diagnosis of the defect was wrong

> **Correction, 2026-08-13.** This section read: *"subtitle cues are offset
> against the MPEG-TS start PTS, landing roughly 112 ms early, and a cue starting
> at t=0 is dropped entirely."* Every number in that sentence came from ffmpeg,
> which ignores `X-TIMESTAMP-MAP` — the very header under evaluation. Measured
> against a real player instead, **two of the three claims were false.**

`X-TIMESTAMP-MAP` is now emitted (`backend/internal/media/subtitles.go`). What a
headless AVFoundation probe reports, on identical media, header absent vs.
present:

| | Before | After |
|---|---|---|
| Offset on a 30 fps source | **66.667 ms early** | **0.333 ms late** |
| Cue authored at t=0 | present, at a negative item time | present, at `+0.000333` |

- The offset was **66.667 ms, not ~112 ms**, and it is not a constant: it is the
  encoder's B-frame reorder delay, `2/fps`, so a 24 fps source gives 83.3 ms.
  Anything that hard-coded a single number would have been wrong for most
  sources.
- The **first cue was never dropped.** A player delivers it at a negative item
  time and shows it from the start of playback. Only ffmpeg's *reader* discards
  a cue that begins before the stream does.

**VERIFIED on AVFoundation only.** ExoPlayer demonstrably *renders* the caption
track in the app (the track picker lists `en / English`, and cues surface during
playback), but **its offset has not been measured.** It may not be 0.333 ms: the
fix anchors on the video start PTS, and a player seeding from the container start
would read these cues ~21.3 ms early instead. Full workings, and the residual
that is still UNKNOWN, in `docs/stage-plans/stage-6a-package-worker.md`.

## Android app

**VERIFIED 2026-08-13 — the app builds, installs, launches, uploads and plays.**
The first time in the project's life. `:app:assembleDebug` produces a ~147 MiB
debug APK; it runs on an emulator AVD named `dayreel-avd` and reaches the API at
`10.0.2.2:8080`.

> **Correction, 2026-08-13.** This section previously stated that the Android SDK
> was not installed, that nothing in `mobile/android/` could build, and that the
> install was **"blocked on disk — 2.8 GB free."** All of that was true when
> written and none of it is now: disk was reclaimed the same day and the full
> toolchain, NDK included, was installed and exercised. The disk figure is also
> not a budget to plan against — the SDK is **8.8 GiB** installed (NDK 2.4 GiB,
> one system image 4.3 GiB), and the data volume sits at ~6 GiB free with it in
> place.

### The toolchain, as actually installed

| Component | Value |
|---|---|
| SDK root | `~/Library/Android/sdk` |
| SDK platforms | `android-37.0` (`compileSdk`) and `android-36` (`targetSdk`) |
| Build-tools | 37.0.0 |
| NDK | 27.1.12297006 — installed, and the pinned `ndkVersion` requires it |
| System image | `android-36;google_apis;arm64-v8a` |
| AVD | `dayreel-avd` |
| Java | OpenJDK 21 |
| Node | **22.23.2 via nvm**; `mobile/.nvmrc` pins the `22` line |

`mobile/android/build.gradle` also pins `minSdk` 24, Gradle 9.4.1 via the
wrapper, Kotlin 2.2.0, and `newArchEnabled=true` — the New Architecture is on.
Two native dependencies now build and run under it: `react-native-blob-util` and
`react-native-video` v6.

**`platforms;android-37` does not exist.** Google moved to minor-versioned
platforms; the SDK repository offers `android-37.0`, `android-37.1` and
`android-37.2-beta*`, and there is no plain `android-37` to install. Ask
`sdkmanager` for `platforms;android-37.0`. This is a package-not-found failure,
so it is loud rather than subtle, but it is not what `compileSdk 37` suggests.

**Node 22 is not optional.** RN 0.87 declares
`"node": "^22.13.0 || ^24.3.0 || >= 26.0.0"`, and this machine's default node is
v20.11.0. Note that `mobile/package.json`'s own `engines` field says
`>= 22.11.0`, which is **looser than React Native's own requirement** and will
not catch a v22.11 or v22.12.

**`nvm use` fails here**, because `~/.npmrc` sets a `prefix` and nvm refuses to
proceed rather than silently overriding it. Two workarounds:

```bash
nvm use --delete-prefix 22
# or bypass nvm's shell integration entirely:
export PATH="$HOME/.nvm/versions/node/v22.23.2/bin:$PATH"
```

`ANDROID_HOME` must be set and `$ANDROID_HOME/platform-tools` on `PATH` before
`npx react-native run-android` will do anything. Gradle will read `sdk.dir` from
`mobile/android/local.properties` instead if you prefer; that file is
uncommitted and does not currently exist, so the environment is what the
verified run used.

### The document picker had to be replaced

`react-native-document-picker@9.3.1` **does not compile against RN 0.87.** It
extends `GuardedResultAsyncTask`, which RN 0.87 removed, so the Android build
fails outright. The package is deprecated with no further versions — there was
nothing to upgrade to. It is replaced by `@react-native-documents/picker`.

**That migration is not a rename.** `copyTo` is gone. Its replacement is a
separate `keepLocalCopy()` call, and **it resolves on failure rather than
throwing** — the failure is reported in a `status` field on the result. Code
that awaits it and reads the path without checking `status` will carry a broken
path forward silently. See `mobile/src/screens/HomeScreen.tsx`.

### What a verified run looked like

Job `4bd59394-a104-453b-90d0-fdd363ad1dba`: `18.mp4`, 14,947,952 bytes, uploaded
from the app as **3 real multipart parts** at the 5 MiB default, through all four
pipeline stages to `completed`, and played back over HLS in-app with captions.
Still LocalStack-only — see the AWS caveats above.
