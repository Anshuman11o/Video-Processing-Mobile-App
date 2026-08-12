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
simulators loses the entire visible job history.**

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
| iOS Simulator | `http://localhost:4566` |
| Android Emulator | `http://10.0.2.2:4566` |
| Physical device | `http://<your-lan-ip>:4566` |

Switching targets means editing compose and restarting `api`. A URL signed for
one host returns `SignatureDoesNotMatch` to another — the signature covers the
`Host` header, so it cannot simply be rewritten.

The committed target is the **iOS Simulator**.

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

## iOS app

CocoaPods has never been run in this repo — there is no `Podfile.lock` and no
`Pods/`. The first setup will need:

```bash
cd mobile/ios && pod install
```

Budget real time for this; it is the most likely place a first run stalls.
