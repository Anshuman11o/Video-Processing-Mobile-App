# Setup and known limitations

How to run DayReel locally, and the caveats that will otherwise be discovered the
hard way.

## Running the stack

```bash
cd infra && docker compose up -d --build
curl localhost:8080/health          # {"status":"ok"}
```

Six containers: `localstack`, `redis`, `api`, and one worker per pipeline stage
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
invalidates that cache. The whole pipeline completes in about four seconds.

So a job can begin and finish inside a single cached response, and the app will
typically jump straight from `uploading` to `completed` without displaying any
intermediate stage. The progress display is correct when it updates; it just
usually has nothing to show.

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

To run the matrix, point the `api` service back at the host loopback
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

### Captions are slightly early, and the first cue is missing

A known defect from stage 6A: subtitle cues are offset against the MPEG-TS start
PTS, landing roughly 112 ms early, and a cue starting at t=0 is dropped entirely.
See `docs/stage-plans/stage-6a-package-worker.md` for the full diagnosis.

## Android app

**The Android SDK is not installed on this machine.** Verified: `ANDROID_HOME`
is unset, `~/Library/Android/sdk` does not exist, and neither `adb` nor
`emulator` is on `PATH`. Nothing in `mobile/android/` can build until that is
fixed. Java 21 is present, which is the one prerequisite already satisfied.

`ANDROID_HOME` must be set (and `$ANDROID_HOME/platform-tools` on `PATH`) before
`npx react-native run-android` will do anything. Gradle also reads
`mobile/android/local.properties` for `sdk.dir` if you prefer that over the
environment.

What `mobile/android/build.gradle` pins, and therefore what the SDK install must
provide:

| Component | Required |
|---|---|
| SDK platform | 37 (`compileSdk`) and 36 (`targetSdk`) |
| Build-tools | 37.0.0 |
| NDK | 27.1.12297006 |
| Min SDK | 24 |
| Emulator system image | any API 24+; API 36 matches `targetSdk` |

Gradle 9.4.1 (via the wrapper), Kotlin 2.2.0, and `newArchEnabled=true` — the
New Architecture is on, which matters for any native dependency added later.

**Blocked on disk, not on effort.** Host disk is critically low — **2.8 GB
free**. A usable SDK platform plus build-tools plus the NDK plus one emulator
system image runs well past that before Gradle caches or a build output are
counted. Clear space first; installing into 2.8 GB will fail partway through and
leave a half-populated SDK, which is worse than none.

Budget real time for the first build regardless. It is the most likely place a
first run stalls, and the toolchain has never been exercised in this repo.
