# scripts/

Developer setup and verification scripts. Nothing here is on the runtime path —
these exist so a fresh checkout fails loudly and early instead of halfway
through an upload.

## Files

| File | Purpose | Writes? |
|------|---------|---------|
| `dev-setup.sh` | One-time preflight: toolchain, AWS credentials, buckets, table, `.env`, queue directory | no |
| `aws-sqs-setup.sh` | `create` / `status` / `teardown` for the five SQS queues, used only when `QUEUE_DRIVER=sqs` | yes |
| `aws-hls-public.sh` | `enable` / `disable` public read on the HLS bucket. Opt-in and off by default — read `docs/aws-public-hls.md` first | yes |
| `abort-stale-uploads.sh` | List, and with `--abort` release, multipart uploads that were started and never finished | with `--abort` |
| `verify-presign.sh` | Negative-test matrix: breaks one property of a presigned URL per row and asserts each is rejected | creates and aborts jobs |
| `verify-resume.sh` | Proves the resume path from curl: kill an upload mid-way, finish it later, with no client-held ETags | uploads ~23 MiB, runs the pipeline once |

The two `verify-*.sh` scripts are the ones that cost money. They upload real
parts to real S3, and `verify-resume.sh` runs the whole pipeline. Neither has
been run against a real account yet — see "Unverified" below.

## What dev-setup.sh checks

| Check | Why it matters |
|-------|----------------|
| `go` | The API and worker run as plain `go run` processes |
| `ffmpeg` / `ffprobe` | Every pipeline stage shells out to them |
| `aws` CLI | Used to verify the buckets and table exist |
| `sqlite3` CLI | Optional — only `make queue-peek` needs it |
| `aws sts get-caller-identity` | Real credentials are required; there is no emulator |
| Three S3 buckets, one DynamoDB table | Created once per account, not per run |
| `.env` present | Copied from `.env.example` if missing, with a warning to fill in keys |
| `data/` exists | The queue opens the SQLite file but does not create its parent directory |

It exits non-zero on any hard failure, so it is safe to chain.

## Non-Obvious Decisions

- **No infrastructure is started.** The script used to run `docker compose up`
  and wait on LocalStack and Redis health checks. Both are gone
  (see `infra/CONTEXT.md`), so setup is purely verification. Running the stack
  is `make api` and `make worker`.

- **Resource names come from the environment, defaulting to the real ones.** The
  previous version printed a resource list that never matched what the init
  script actually created (`dayreel-raw-uploads` vs `dayreel-raw-videos`,
  `dayreel-videos` vs `dayreel-jobs`), which sent people looking for buckets that
  did not exist. Names are now defined once at the top of the script and used for
  both the checks and the summary, so the two cannot drift.

- **Credentials are checked before resources.** A bad key otherwise shows up as
  "bucket missing" three times over, which is a misleading diagnosis.

- **`aws-sqs-setup.sh` is the exception that writes.** `dev-setup.sh` only reads;
  the SQS script creates and deletes queues, because unlike the SQLite driver
  those cannot create themselves on first open. It is gated three ways: it
  refuses to run with `AWS_ENDPOINT_URL` set (there is no emulator, and a stale
  value creates queues where the workers will never look), `teardown` prompts for
  the region rather than taking a `--yes` flag, and `create` names every billable
  queue it made at the end of the run as `config/free-tier.md` requires.

- **Bucket CORS is described, not applied.** The script never writes to the
  account — it only reads. Bucket policy is a one-time decision on a real,
  billed account, and a setup script that silently reconfigures it is worse than
  a message telling you what to configure. The `ExposeHeaders` trap is called out
  by name because it is the one that has already cost us an afternoon
  (`TROUBLESHOOTING.md`).

- **Every script talks to real AWS with ambient credentials.** There is no
  `TARGET` or `CONTAINER` switch any more. Three of these scripts used to default
  to `docker exec dayreel-localstack awslocal` with real AWS as the opt-in path,
  which meant that after the containers were removed the plain documented
  invocation failed for all three. Real AWS is now the only path, so what the
  usage line shows is what runs.

## Unverified

Nothing here has been run against a real AWS account end to end. The scripts were
written against the emulator and adapted; the adaptation is mechanical and
reviewed, but "it should work" is not "it worked".

- `verify-presign.sh` and `verify-resume.sh` expect the status codes SigV4 and
  Block Public Access specify, not codes anyone has observed here. The
  rewritten-host row in each is the most likely to need adjusting, because its
  expectation depends on the shape of the endpoint the SDK signs.
- The buckets and table themselves do exist (created 2026-08-12), so
  `dev-setup.sh` and `make verify` have been exercised against them.

## Deferred

`test-upload.sh` (CLI: upload a clip, poll status until the reel is ready) is
listed in `PROJECT_PLAN.md` but not written yet. It needs the pipeline to
actually run end-to-end first.
