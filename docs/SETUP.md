# Setup and known limitations

How to run DayReel locally, and the caveats that will otherwise be discovered the
hard way.

## Running the stack

```bash
./scripts/dev-setup.sh              # once: toolchain, credentials, resources, .env
make api                            # HTTP API on :8080
make workers                        # all four stages in one terminal
curl localhost:8080/health          # {"status":"ok"}
```

Two Go processes and a SQLite file. **No containers, no AWS emulator, no cache
server.** `make workers` runs `validate`, `extract`, `transcribe` and `package`
as four processes with their output interleaved; `make worker STAGE=extract`
runs one on its own when you need to read a single stage's log. What was removed
and why is recorded in `infra/CONTEXT.md`.

S3 and DynamoDB are **real AWS**. There is nothing to start and nothing to seed,
so `make verify` — credentials, three buckets, one table — is the closest thing
to "is my stack up?".

### The AWS resources

Created once per account, by hand. There is no init script to recreate them on
every start, and no emulator to create them in. `./scripts/dev-setup.sh` prints
these itself when it finds anything missing:

```bash
aws s3 mb s3://dayreel-raw-videos
aws s3 mb s3://dayreel-processed
aws s3 mb s3://dayreel-hls-output
aws dynamodb create-table --table-name dayreel-jobs \
  --attribute-definitions AttributeName=pk,AttributeType=S AttributeName=sk,AttributeType=S \
  --key-schema AttributeName=pk,KeyType=HASH AttributeName=sk,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST
```

Bucket names are globally unique across all of AWS, so the defaults above are
almost certainly taken. Pick your own and put them in `.env` — the Makefile and
`dev-setup.sh` both read them from the environment rather than hardcoding, so the
names only have to be written once.

Two bucket settings the create commands do not cover:

- **`dayreel-raw-videos` needs CORS** for presigned uploads, and `ExposeHeaders`
  must be `["ETag"]` and nothing else. Real S3 rejects a wildcard there outright
  with `InvalidRequest`, which the emulator accepted for days — the full entry is
  in `TROUBLESHOOTING.md`. `ETag` is the only response header the client reads.
- **`dayreel-hls-output` needs CORS for range requests and an access model**,
  because there is no CDN in front of it and playlists cannot be presigned. That
  one is opt-in and deliberately off — see `docs/aws-public-hls.md` before
  touching it.

### The queue

The queue is the one pluggable piece. `QUEUE_DRIVER=sqlite` (the default) is a
file at `QUEUE_DB_PATH`, created on first start by whichever binary starts first,
and touches no AWS service at all. `QUEUE_DRIVER=sqs` is real Amazon SQS, and
unlike the file the queues do not create themselves:

```bash
make sqs-setup            # create the five queues (idempotent)
QUEUE_DRIVER=sqs make api
make sqs-status           # URL, depth and in-flight per queue
make sqs-teardown         # delete them when finished
```

### Where configuration actually comes from

`.env` is the switch — but only because the Makefile sources it. Compose used to
inject an environment into each container; nothing does that now, so the run
targets do `set -a; . ./.env; set +a` themselves before `go run`. The
consequence is worth knowing before it bites: **`cd backend && go run ./cmd/api`
by hand does not read `.env`**. It falls back to the defaults in
`internal/config/config.go`, which name a region and three bucket names you may
not own. Prefer `make api` and `make worker`.

**Not every target sources it, and that shows up as a path or a name that does
not match.** `LOAD_ENV` is a per-recipe shell prefix, so it applies to `api`,
`worker`, `workers` and the three `sqs-*` targets and to nothing else. Read out
of `make -n`, which is where this is easiest to see rather than argue about:

| Target | Reads `.env`? | Consequence |
|---|---|---|
| `api`, `worker`, `workers` | yes | run with `cd backend`, so a relative `QUEUE_DB_PATH` or `WHISPER_MODEL_PATH` resolves under `backend/` |
| `queue-peek`, `queue-reset` | no | look for `data/queue.db` at the project root — not the `backend/data/queue.db` the API just created |
| `verify` | no | checks the bucket and table names hard-coded as Makefile defaults, not the ones in `.env` |

Two ways out, both of them yours to pick: export the variables into make's own
environment (`export $(grep -v '^#' .env | xargs)` before running it), or use
absolute paths in `.env` so no target can disagree about where a file is. The
symptom to recognise is `make queue-peek` insisting there is no queue database
while the pipeline is visibly draining one, and `make verify` reporting the
default buckets missing on an account where yours exist.

Every setting is documented inline in `.env.example`. Nothing in the repository
contains credentials, and `.env` is git-ignored.

See `config/free-tier.md` before uploading anything: there is no free tier on
this account, the budget is $20 total, and every AWS resource must be torn down
after the run that needed it.

## Real transcription (whisper.cpp)

**Only needed for `MOCK_TRANSCRIBE=false`.** The default is `true`, the mock
needs neither a binary nor a model, and every stage downstream of transcribe was
developed against it — which is why `scripts/dev-setup.sh` *warns* about a
missing `whisper-cli` instead of failing on it.

The transcribe stage shells out to a `whisper-cli` binary on `PATH`
(`backend/internal/transcribe/whisper.go`), exactly the way the other stages
shell out to `ffmpeg`. **Nothing in the repo supplies that binary any more.**
`backend/Dockerfile.worker` still builds it from source and that image was its
only provider; now that workers run as plain processes, it has to be on the host.
The gap is invisible until the first real run, where it surfaces as an exec
failure part-way through a pipeline — a confusing place to discover a missing
dependency, which is what the setup-script warning exists to prevent.

### Building it from source

The reliable route, and the one already exercised — `Dockerfile.worker` builds
whisper.cpp v1.9.2 this way:

```bash
git clone --depth 1 --branch v1.9.2 https://github.com/ggml-org/whisper.cpp
cd whisper.cpp
cmake -B build \
  -DCMAKE_BUILD_TYPE=Release \
  -DBUILD_SHARED_LIBS=OFF \
  -DWHISPER_BUILD_TESTS=OFF \
  -DWHISPER_BUILD_SERVER=OFF \
  -DWHISPER_BUILD_EXAMPLES=ON
cmake --build build --target whisper-cli -j "$(nproc)"
sudo install build/bin/whisper-cli /usr/local/bin/whisper-cli
```

`--target whisper-cli` is load-bearing. Without it CMake builds roughly twenty
example binaries — `stream`, `talk-llama`, `bench` — and the build takes several
times longer to produce output nothing here uses. With it, the compile is about a
minute and the binary is ~3 MB.

`WHISPER_BUILD_EXAMPLES=ON` is not redundant next to it: `whisper-cli` *is* one
of the examples, so the target does not exist without it. The two flags together
are "build the examples, but only this one".

Two things the Dockerfile learned that apply to a host build just as much:

- **`BUILD_SHARED_LIBS=OFF` does not produce a static binary.** It statics
  whisper's and ggml's own libraries and leaves the result dynamically linked
  against `libstdc++` and `libgomp`. Missing either fails at exec time, not at
  build time. A machine with a working C++ toolchain already has both, which is
  why the Dockerfile installs them explicitly on a bare Alpine image and a host
  build usually needs nothing.
- **The default build bakes in the build host's CPU features.** A binary built on
  one machine can `SIGILL` on another with an older CPU — verified in stage 5A
  against arm64. Building where you run avoids it. `-DGGML_NATIVE=OFF` is the
  portable alternative, measured there as producing identical transcripts about
  15% slower.

### From a package manager

Package managers do ship whisper.cpp, and in recent versions the binary is called
`whisper-cli` — which is the name this repo execs. **Unverified: no
package-manager install has been tried for this project**, so treat any specific
command you find as a lead rather than an instruction, and check what it actually
installed. Two things make a package a real answer rather than a plausible one:

```bash
command -v whisper-cli            # the name matters; older builds shipped "main"
./scripts/dev-setup.sh            # reports whisper-cli and the model together
```

If neither holds, build from source above — that path is known to work.

### The model

`WHISPER_MODEL_PATH` (default `./models/ggml-base.bin`) points at a ggml model
file. It is **not committed**: the `base` model is 141.1 MB, `.gitignore` covers
`models/`, and it used to be a named Docker volume so it survived image rebuilds.
Now it is just a path on the machine running the worker.

You do not have to fetch it yourself. The worker downloads it on the first real
transcription, writing to a temporary name and renaming into place so an
interrupted download cannot leave a partial file for the next run to trust; a
file too small to be the model is deleted and re-fetched rather than reused.
Pre-fetching is still worth doing, because otherwise a ~141 MB download happens
inside the first job's visibility lease:

```bash
mkdir -p models
curl -L -o models/ggml-base.bin \
  https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin
```

**A relative `WHISPER_MODEL_PATH` does not mean the same directory to everything
that reads it.** `make worker` does `cd backend` before `go run`, so the worker
resolves `./models/ggml-base.bin` to `backend/models/ggml-base.bin`, while
`scripts/dev-setup.sh` checks it against whatever directory you invoked the
script from — normally the project root. The two can disagree, and the shape of
the disagreement is a setup script that reports the model missing while the
worker is happily using it, or the reverse. Set an absolute path in `.env` if you
want one copy and one answer.

Sizing, from the stage 5A verification run: transcription is roughly 0.1×
realtime on the `base` model, so a 60-second clip takes 5–8 seconds against a 5
minute lease. The stage heartbeats anyway, but the visibility timeout is not the
constraint it was predicted to be.

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
`POST /jobs`, `GET /jobs/{id}`, `POST /jobs/{id}/complete`,
`POST /jobs/{id}/upload-urls`, `DELETE /jobs/{id}/upload` and
`GET /jobs/{id}/reel` — a job can be fetched by ID but not discovered. The
DynamoDB table is single-table with `PK = JOB#<uuid>`, so listing jobs would need
either a full table scan or a new GSI. On a project with a handful of runs left
before it closes, a device-local index was chosen over adding a scan that quietly
costs money on real AWS.

**If you need the history back**, list the buckets — every top-level prefix is a
job ID:

```bash
aws s3 ls s3://dayreel-processed/     # jobs that reached at least validate
aws s3 ls s3://dayreel-raw-videos/    # every job whose upload completed
```

The API's own log is the weaker source, and worth knowing the limits of before
you go looking: it runs in the foreground under `make api` and logs one line per
request, so every call that names a job in its path (`POST /jobs/<id>/complete`,
`GET /jobs/<id>`) leaves the ID behind. `POST /jobs` does not — the ID is minted
inside the handler and returned in the body, and the log line is just the path.
A job created and then abandoned appears in no log line at all.

### Pipeline stages are usually invisible in the UI

`GET /jobs/{id}` is served from a 10-second in-process cache
(`backend/internal/cache/memory.go`), and no worker invalidates it. A job can
therefore begin and finish inside a single cached response, and the app will jump
straight from `uploading` to `completed` without displaying any intermediate
stage. The progress display is correct when it updates; it usually has nothing to
show.

This was a cache-coherence gap when the cache was Redis, and it is a **stronger**
one now. The cache is a map inside the API process and the workers are separate
processes, so a worker cannot reach it even in principle — there is no cache
server between them to invalidate. The API invalidates on
`POST /jobs/{id}/complete` and `DELETE /jobs/{id}/upload`, which are the two
transitions it owns; every stage transition after that belongs to a worker, and
none of them reach the cache. The trade was left open deliberately rather than
fixed.

**The four-second figure is an emulator measurement and is not being repeated
here.** Stage 6A timed the whole pipeline at ~4 seconds end to end with S3 on the
same machine. Against real AWS every stage adds real round trips to S3 and
DynamoDB, so that number is a floor. Whether a job still starts and finishes
inside one 10-second TTL is **unverified**; the gap is the same either way.

To watch stages transition, bypass the cache. Each worker logs its own stage to
its own stdout, so run the stage you care about on its own, and read the queue
directly:

```bash
make worker STAGE=validate    # this stage's log, uninterleaved
make queue-peek               # messages, depth and leases (SQLite driver)
make sqs-status               # the same, on QUEUE_DRIVER=sqs
```

### Presigned URLs are bound to the host they were signed for

SigV4 signs the `Host` header, so a presigned URL is valid only against the exact
host it was signed for. Rewriting the host to one the client can actually reach
returns `SignatureDoesNotMatch` — the host is inside the signature, so it cannot
simply be swapped. That is a property of the signature, not of any particular S3
implementation, and it still decides which client can upload.

**On real AWS the constraint dissolves.** The API and the phone reach the same
public regional S3 endpoint, so there is nothing to override: `S3_PUBLIC_ENDPOINT`
is empty and the SDK signs the genuine endpoint, which every client resolves
identically.

The endpoint table that used to be here — `http://10.0.2.2:4566` for the Android
emulator, `http://localhost:4566` for the iOS simulator, `http://<your-lan-ip>:4566`
for a physical device, pick exactly one per run — **was an emulator artefact and
no longer applies.** It existed because the emulator was reachable under a
different name from each client, which forced the choice. There is no choice to
make now.

What survives the change: `10.0.2.2` is still the Android emulator's alias for
the host's loopback interface, and it is still needed — for the **API**, not for
S3. `mobile/src/api/client.ts` hardcodes `http://10.0.2.2:8080`. The two settings
used to have to name the same client environment; now only the API one exists.

`S3_PUBLIC_ENDPOINT` stays in `.env.example` for the case it generalises to: a
proxy in front of S3. If you set it, it must name the host the **uploader** will
use, not the one the API uses. Both the API (which signs upload URLs) and the
package worker (which formats the `hls_url` the client is handed) read it, and
setting it for one and not the other makes upload and playback disagree about who
the client is.

#### `verify-presign.sh` runs from the host now

`scripts/verify-presign.sh` is the presigned-URL negative-test matrix. It issues
a fresh upload URL per row, breaks exactly one property of it — the signature,
the host, the object key, the part number, the expiry — and asserts that each one
is rejected. A successful upload proves nothing on its own: if the server accepts
everything, a correct signature and a garbage one both return 200.

The workaround that used to be documented here — edit `infra/docker-compose.yml`,
flip the `api` service's `S3_PUBLIC_ENDPOINT` to `localhost`, restart, run, then
remember to flip it back — **is gone along with the file it edited, and so is the
problem it worked around.** URLs are now signed for the regional S3 endpoint,
which the host reaches as readily as the phone does. Nothing has to be flipped
and nothing has to be restored afterwards.

Two things had to change in the script for it to run at all, and both are done:

- **`TARGET` is gone.** It selected between the emulator and real AWS, and only
  ever did one thing: downgrade the anonymous-`PUT` row to a tolerated gap. There
  is nothing to downgrade, so that row is now a hard failure — which is exactly
  the assertion this document previously said had to be made against real
  buckets.
- **The rewritten-host row was a no-op.** It swapped `localhost:4566` for
  `127.0.0.1:4566`, strings that do not occur in a real presigned URL, so the
  "tampered" URL was the original and the row failed for the wrong reason. It now
  rewrites the signed host to S3's dualstack endpoint for the same bucket in the
  same region: a genuinely different `Host` that still answers, so the row
  measures the signature rather than DNS. If the URL is not an `amazonaws.com`
  S3 endpoint — `S3_PUBLIC_ENDPOINT` pointed at a proxy — the row reports `SKIP`
  rather than passing vacuously.

It also cleans up after itself now, which did not matter locally and does here.
Every row mints a real multipart upload; parts of an unfinished multipart upload
bill as storage and appear in no object listing, so the script aborts every job it
created on exit via `DELETE /jobs/{id}/upload`. If it is killed before that runs,
`./scripts/abort-stale-uploads.sh --abort` is what releases them.

```bash
make api                        # another terminal, real credentials
./scripts/verify-presign.sh
```

**Unverified.** No run against a real AWS account has been recorded, so the
status codes the script expects are what SigV4 and Block Public Access specify
rather than what was observed. The rewritten-host row is the most likely to need
adjusting, because its expectation depends on the shape of the endpoint the SDK
signs. What *has* been exercised is the script's own mechanics — job creation,
each tampering transform, and the cleanup — against a stand-in server on
localhost, which says nothing about S3 and everything about the script not being
broken on its own terms.

### Bucket authorization has to be asserted, not assumed

Holding a presigned URL should be the only way in. Under the emulator it was not.
LocalStack Community checked presigned *signatures* properly — a corrupted
signature, a rewritten host, a swapped object key, a changed part number and a
shortened expiry were all rejected with 403 — but it never checked whether a
request was authenticated at all. An unsigned `PUT` to any bucket returned 200
and wrote the object, and an unsigned `GET` read it back.

**That gap went with the emulator, and its worst property went with it.**
Applying a `Deny`-anonymous bucket policy and a full `put-public-access-block`
both *succeeded* against LocalStack — the calls returned cleanly and the
configuration read back verbatim — and an unsigned `PUT` still returned 200
afterwards. Provisioning code that set Block Public Access therefore looked
correct locally while proving nothing. Nothing in the current stack can
manufacture that particular false assurance, because nothing in the current stack
pretends to be S3.

What has not changed is that the assumption underneath the upload design is still
an assumption. Real S3 has Block Public Access enabled by default for buckets
created since April 2023 and denies anonymous requests, and the whole design rests
on that being true here. **It is still unasserted:** no run of the matrix against
real buckets has been recorded. `./scripts/verify-presign.sh` is the assertion —
run it once against real buckets and write down what happened.

`dayreel-hls-output` is the deliberate exception. HLS playlists reference their
segments by relative path and cannot be presigned, so that bucket needs an access
model rather than a signed URL. Its real-AWS answer is **opt-in and off by
default** — `scripts/aws-hls-public.sh` and **`docs/aws-public-hls.md`**, which
covers the Block Public Access levels, the cost exposure, the teardown
obligations and the alternative that was not built. Read it before pointing
anything at a real bucket. Playback "worked" under the emulator for a reason that
does not hold on real S3 — unsigned reads were served to any bucket — so playback
from a real HLS bucket is **unverified** too.

### Transcription is mocked by default

`MOCK_TRANSCRIBE=true` is the default and transcripts read
`[mock transcript] segment N`. This is deliberate: real runs are budgeted, and
every stage downstream of transcribe is developed against the mock. Setting it to
`false` needs `whisper-cli` on `PATH` and a model file, neither of which the repo
supplies now that the worker container is gone — see "Real transcription
(whisper.cpp)" above.

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

**Blocked on disk, not on effort.** Host disk was critically low when this was
last measured — **2.8 GB free**. A usable SDK platform plus build-tools plus the
NDK plus one emulator system image runs well past that before Gradle caches or a
build output are counted. Re-check before you start; clear space first, because
installing into 2.8 GB will fail partway through and leave a half-populated SDK,
which is worse than none.

Budget real time for the first build regardless. It is the most likely place a
first run stalls, and the toolchain has never been exercised in this repo.
