# DayReel Infrastructure

**This directory is currently empty apart from this file.** It is a placeholder
for the Terraform stage (`infra/terraform/`, Stage 10). Nothing in the local
development loop lives here any more.

## What used to be here, and why it's gone

| Removed | Replaced by |
|---------|-------------|
| `docker-compose.yml` | Nothing — the API and worker run as plain `go run` processes |
| `localstack/init-aws.sh` | Nothing — S3 and DynamoDB are real AWS, created once by hand |

The project processes a handful of 10–60 second clips. Containers, an AWS
emulator, and a separate cache server cost 1.5–2.5 GB of RAM and buy nothing at
that scale. Removing them means the local stack is two Go binaries and a file.

Component by component:

- **LocalStack** emulated S3, SQS and DynamoDB. S3 and DynamoDB now point at a
  real AWS account, which is both cheaper in RAM and more honest — LocalStack's
  behaviour diverges from real S3 in ways that hide bugs (see
  `TROUBLESHOOTING.md`, the CORS `ExposeHeaders` entry).
- **SQS** is replaced by a self-hosted SQLite queue at `data/queue.db`,
  implemented in `backend/internal/queue/`. It keeps the properties the pipeline
  actually depends on: visibility timeout, at-least-once delivery, delivery
  counting, and a dead-letter queue. See
  `docs/stage-plans/stage-3b-local-queue.md`.
- **Redis** is replaced by an in-process TTL cache inside the Go API. A separate
  cache server for one map with a 10-second TTL, read by one process, was never
  worth its own container.
- **Docker Compose** had nothing left to orchestrate once those three were gone.
- **CloudFront** and **ECS Fargate** were dropped earlier (commit `bc61001`).

## Local development

There is no infrastructure to start. From the project root:

```bash
./scripts/dev-setup.sh   # one-time: check toolchain, credentials, resources
make verify              # confirm AWS credentials, buckets and table
make api                 # run the API
make worker              # run the pipeline worker
```

## AWS resources (real account)

These are created once per account, by hand or by Terraform later. They are not
recreated on every start, so there is no init script.

### S3 Buckets

| Bucket | Purpose |
|--------|---------|
| `dayreel-raw-videos` | Original uploads from mobile clients |
| `dayreel-processed` | Intermediate artifacts (frames, audio, transcript) |
| `dayreel-hls-output` | Final HLS output, served directly to the player |

`dayreel-raw-videos` needs CORS for presigned-URL uploads; `dayreel-hls-output`
needs CORS for player range requests and must be readable by the player, since
there is no CDN in front of it.

### DynamoDB Table

| Table | Purpose | Keys |
|-------|---------|------|
| `dayreel-jobs` | Job state, one item per job | `pk` (partition) + `sk` (sort) |

Single-table design; the `stages` map holds per-stage state. DynamoDB is the
only source of truth.

### Queues

Not AWS. The five logical queues (`dayreel-validate`, `dayreel-extract`,
`dayreel-transcribe`, `dayreel-package`, `dayreel-dlq`) are values of a `queue`
column in `data/queue.db`.

## Deferred

**Terraform.** When it lands it goes in `infra/terraform/` and provisions the
buckets, the table, and a single small VM running both Go binaries. No compose
file returns — the VM runs the same `go run`/built binaries, not containers.

**S3 event notifications.** LocalStack's init script wired
`s3:ObjectCreated:*` → validate queue so uploads started the pipeline without
API involvement. Real S3 cannot notify a SQLite file. The API now enqueues the
validate message itself on `POST /jobs/{id}/complete`, which it was already
positioned to do. Reinstating event-driven start needs a real broker and is
deferred with the rest of the AWS deployment.
