# scripts/

Developer setup and verification scripts. Nothing here is on the runtime path —
these exist so a fresh checkout fails loudly and early instead of halfway
through an upload.

## Files

| File | Purpose |
|------|---------|
| `dev-setup.sh` | One-time preflight: toolchain, AWS credentials, buckets, table, `.env` |

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

## Deferred

`test-upload.sh` (CLI: upload a clip, poll status until the reel is ready) is
listed in `PROJECT_PLAN.md` but not written yet. It needs the pipeline to
actually run end-to-end first.
