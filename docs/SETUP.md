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

**Every target sources it now, which was not true until recently.** `LOAD_ENV`
is a per-recipe shell prefix and it used to be on `api`, `worker`, `workers` and
the `sqs-*` targets only. `queue-peek`, `queue-reset` and `verify` went without,
so they read the Makefile's own defaults: `queue-peek` looked for
`data/queue.db` at the project root and reported no queue database while the
pipeline was visibly draining `backend/data/queue.db`, and `verify` checked
three bucket names somebody else owns rather than yours. Both now read `.env`.

Relative paths still need care, because `make api` and `make worker` do `cd
backend` first. `QUEUE_DB_PATH=./data/queue.db` therefore means
`backend/data/queue.db`, and `queue-peek` resolves it the same way deliberately.
`WHISPER_MODEL_PATH` has the same shape but one reader that does *not* follow
the rule — `scripts/dev-setup.sh` checks it against whatever directory you ran
the script from. An absolute path in `.env` removes the ambiguity everywhere.

One precedence surprise worth knowing: `set -a; . ./.env` overwrites variables
that are already exported, so `.env` beats the surrounding environment rather
than the other way round. `S3_RAW_BUCKET=x make verify` loses to whatever `.env`
says. Edit `.env` to change what these targets see.

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

**On macOS the whole answer is two commands** — see the two sections below for
why each is the right one:

```bash
brew install whisper-cpp                       # the binary, v1.9.2, Metal-accelerated
docker run --rm -v infra_whisper-models:/m -v "$PWD/backend/models":/out \
  --entrypoint sh infra-worker-transcribe:latest -c 'cp /m/ggml-base.bin /out/'
```

### Building it from source

**Not the recommended route any more on macOS — see "From a package manager"
below, which is now verified.** Keep this for platforms with no package, and for
the reasoning, which still applies to any host build. `Dockerfile.worker` builds
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

**VERIFIED 2026-08-13 on macOS arm64 — this is now the recommended route, and
the source build above is the fallback.** Homebrew ships the same version the
Dockerfile compiles, as a bottle, so there is no compile at all:

```bash
brew install whisper-cpp          # installs whisper-cli 1.9.2 into /opt/homebrew/bin
```

That is the whole install. It lands as `whisper-cli` — the name this repo execs —
and needs no `PATH` change on a normal Homebrew setup, because `/opt/homebrew/bin`
is already there. It pulls in `ggml` and `sdl2-compat`; the three come to ~19 MB.

It beats the source build on two counts beyond convenience:

- **It is the same version**, 1.9.2, that `Dockerfile.worker` pins. Verified by
  `whisper-cli --version` against the binary inside `infra-worker-transcribe`.
- **It is GPU-accelerated.** The bottle loads Metal and BLAS backends
  (`libggml-metal.so`, `libggml-blas.so`), which the plain CMake invocation above
  does not configure. Measured: a 7.6-second clip transcribes in ~1.2 s including
  model load, about 0.15× realtime.

Two things make any *other* package a real answer rather than a plausible one —
check both before trusting one:

```bash
command -v whisper-cli            # the name matters; older builds shipped "main"
./scripts/dev-setup.sh            # reports whisper-cli and the model together
```

If neither holds, build from source above — that path is known to work.

### Why not run the binary out of the old Docker image

`infra-worker-transcribe:latest` still exists locally and still contains a
working `/usr/local/bin/whisper-cli`, so a shell wrapper on `PATH` that forwarded
each invocation to `docker run` was considered. **It works** — bind-mounting a Go
`os.MkdirTemp` directory (macOS puts these under `/var/folders/…`) succeeds, and
files the container writes come back owned by the host user, so the stage's
`os.RemoveAll` cleanup is fine. It was rejected anyway:

- The binary is Alpine/musl **linux/arm64** and cannot exec on the host, so every
  transcription pays a container start. Measured 6.0 s against the native 4.4 s
  on the same clip, and the gap widens on longer audio because the container is
  CPU-only — no Metal.
- A wrapper would have to rewrite **three independent host paths** into container
  paths (`-m` model, `-f` audio from the extract stage, `-of` output base in a
  fresh temp dir), which live in three unrelated directory trees. That is real
  parsing logic in shell, and it silently breaks the moment a path moves.
- It reintroduces a hard Docker dependency into a local loop that was
  deliberately made containerless (`infra/CONTEXT.md`).

The image is still worth keeping for one thing — recovering the model, below.

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

**Before downloading, check whether you already have it.** The old compose stack
kept the model in a named Docker volume, `infra_whisper-models`, and removing
compose did not remove the volume — it survives with the full 141.1 MB file in
it. Copying it out is faster than a re-download and costs no bandwidth:

```bash
docker volume ls | grep whisper-models        # is it still there?
mkdir -p backend/models
docker run --rm -v infra_whisper-models:/m -v "$PWD/backend/models":/out \
  --entrypoint sh infra-worker-transcribe:latest -c 'cp /m/ggml-base.bin /out/'
```

The file that comes out is byte-identical to the published one
(sha256 `60ed5bc3dd14eea856493d334349b405782ddcaf0028d4b5df4088345fba2efe`,
147,951,465 bytes) and lands owned by the host user. Do **not** `docker volume
prune` while this is the only copy.

**A relative `WHISPER_MODEL_PATH` does not mean the same directory to everything
that reads it.** `make worker` does `cd backend` before `go run`, so the worker
resolves `./models/ggml-base.bin` to `backend/models/ggml-base.bin`, while
`scripts/dev-setup.sh` checks it against whatever directory you invoked the
script from — normally the project root. The two can disagree, and the shape of
the disagreement is a setup script that reports the model missing while the
worker is happily using it, or the reverse. Set an absolute path in `.env` if you
want one copy and one answer.

There is a third reader, and it has the most expensive failure mode: **`go test`
runs each package with its working directory set to that package's own
directory.** A test that constructs `WhisperCPP` with the default relative path
resolves it to `backend/internal/transcribe/models/ggml-base.bin`, finds nothing,
and — because `ensureModel` downloads on miss — quietly pulls a *second* 141 MB
copy into the source tree. `.gitignore` covers `models/`, so it does not even
show up in `git status`. Observed 2026-08-13. Pass an absolute path to any test
that exercises the real binary.

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
begin and finish inside a single cached response, and the app then jumps straight
from `uploading` to `completed` without displaying any intermediate stage. The
progress display is correct when it updates; it may simply have nothing to show.

This was a cache-coherence gap when the cache was Redis, and it is a **stronger**
one now. The cache is a map inside the API process and the workers are separate
processes, so a worker cannot reach it even in principle — there is no cache
server between them to invalidate. The API invalidates on
`POST /jobs/{id}/complete` and `DELETE /jobs/{id}/upload`, which are the two
transitions it owns; every stage transition after that belongs to a worker, and
none of them reach the cache. The trade was left open deliberately rather than
fixed.

> **Correction, 2026-08-13.** This section used to assert that "the whole
> pipeline completes in about four seconds", which made stages *always*
> invisible. That figure came from the tiny synthetic clips every stage before 7
> was tested with. The first real upload from the app — 14.9 MB, 10 s of
> 1280x720 — took **36.7 s end to end** (`validate` 4.0 s, `extract` 4.1 s,
> `transcribe` 0.04 s under the mock, `package` 12.8 s). That is comfortably
> longer than the TTL, so on a real clip some stages *are* observable. The cache
> gap is still real; it is just not total.

That 36.7 s was measured with S3 emulated on the same machine, so it is a
**floor** rather than an estimate: every stage now adds real round trips to S3
and DynamoDB across the network. The direction is the helpful one — a slower
pipeline makes stages *more* visible, not less — so the headline conclusion holds
a fortiori. The 12.8 s package stage is the one to watch, since it uploads every
segment of every rendition individually.

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

**"Enabled by default" means at the *bucket* level, and the distinction
matters.** Since April 2023 every newly created bucket gets all four BPA settings
on. **Account-level BPA is opt-in and is not configured by default**, so on an
ordinary account there is no account-wide guardrail to disable, and making one
bucket readable touches only that bucket. Where an account-level setting *has*
been turned on it overrides the bucket — S3 applies the most restrictive
combination of the two — and relaxing it then genuinely does affect every bucket
in the account. Recorded because the opposite was assumed during Stage 8
planning: that opening a single bucket "generally requires disabling Block Public
Access for the entire account". It does not.

`dayreel-hls-output` is the deliberate exception. HLS playlists reference their
segments by relative path and cannot be presigned, so that bucket needs an access
model rather than a signed URL. Its real-AWS answer is **opt-in and off by
default** — `scripts/aws-hls-public.sh` and **`docs/aws-public-hls.md`**, which
covers the Block Public Access levels, the cost exposure, the teardown
obligations and the alternative that was not built. Read it before pointing
anything at a real bucket. Playback "worked" under the emulator for a reason that
does not hold on real S3 — unsigned reads were served to any bucket — so playback
from a real HLS bucket is **unverified** too.

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

`MOCK_TRANSCRIBE=true` is the default and transcripts read
`[mock transcript] segment N`. This is deliberate: real runs are budgeted, and
every stage downstream of transcribe is developed against the mock. Setting it to
`false` needs `whisper-cli` on `PATH` and a model file, neither of which the repo
supplies now that the worker container is gone — see "Real transcription
(whisper.cpp)" above.

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

**That run was against LocalStack, and the substrate has changed since.** The
emulator, Redis and the containers are gone; S3 and DynamoDB are real AWS and the
queue is a local file. What the run proves is the *application* path — the app's
picker, the multipart uploader, all four stages, the playlist a player will
accept — none of which the substrate change touched. What it does not prove is
anything about S3 authorization, because the emulator enforced none: see the
caveats above. The equivalent run against real AWS has not been recorded.
