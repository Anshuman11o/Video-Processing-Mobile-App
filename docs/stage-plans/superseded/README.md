# Superseded stage plans

These are the **initial** 4A/5A/6A plans merged via PR #2, written before those
stages were implemented. They were developed in parallel with — and independently
of — the versions now in `docs/stage-plans/`, and the two sets collided when the
implementation branch merged.

The active plans supersede them because they record what was actually **decided
and verified**: the resolved `[DECIDE]` items, the verification results from real
runs, and the empirical findings that changed the implementation (`pkt_pts_time`
removed in ffmpeg 6, whisper.cpp exiting 0 on corrupt input, H.264 levels varying
per rendition and frame rate).

They are kept rather than deleted because the convergence is informative: written
independently, both sets arrived at the `extract.json` manifest, never upscaling
a rendition above its source, and treating package as a terminal stage. Where
they differ from the active plans, the active plans won on evidence, not
authorship.

Everything in this directory also predates the infrastructure change: Docker
Compose, LocalStack, SQS and Redis are gone, S3 and DynamoDB are real AWS, and
the queue is a local SQLite file. Read every `--endpoint-url`, `docker compose`
and SQS reference here as history. See `infra/CONTEXT.md`.
