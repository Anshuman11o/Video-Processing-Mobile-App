# config/

Reference documentation for external service constraints that affect this project.

## Files

| File | Purpose |
|------|---------|
| `aws-limits.md` | Hard limits from S3 and DynamoDB; local queue settings, worker sizing, HLS delivery, status cache |
| `free-tier.md` | Cost constraints and the $20 budget. **There is no free tier on this account** — everything bills from the first request |

## Why This Exists

AWS services have constraints that silently break systems if violated:
- S3 multipart uploads need 5MB minimum parts, and reject undersized ones only
  at `CompleteMultipartUpload`
- S3 rejects wildcards in CORS `ExposeHeaders` (see `TROUBLESHOOTING.md`)
- DynamoDB items max 400KB
- NAT Gateway costs $32/month even idle

Rather than discover these mid-implementation, we document them upfront.

## How to Use

Before implementing anything that touches AWS:
1. Check the relevant limits in `aws-limits.md`
2. Check the cost impact in `free-tier.md`. There is **no free tier** — the
   question is not "are we within the allowance" but "what does this cost, and
   is it billed per request or per hour?"
3. Update these files if you discover new constraints

Per-hour charges are the risk; per-request charges are almost free at this
scale. Anything that bills while idle needs a decided teardown time before it is
created.

## Non-Obvious Decisions

- **Only two AWS services are used:** S3 and DynamoDB. SQS was replaced by a
  self-hosted SQLite queue and ElastiCache by an in-process TTL cache, so their
  limits no longer bind. The limits that replaced them are settings we choose,
  not quotas AWS imposes — documented in `aws-limits.md` all the same, because
  they are the numbers that decide whether the pipeline stalls or retries.
- **No CDN, no managed container hosting:** A handful of short clips doesn't
  justify CloudFront or ECS Fargate. HLS is served directly from the HLS bucket
  (so that bucket must be player-readable), and the API and worker run as plain
  Go processes. See `PROJECT_PLAN.md` → "Deferred".
- **VPC Gateway Endpoints over NAT Gateway:** S3 and DynamoDB access via gateway
  endpoints is free. NAT Gateway is the biggest cost trap for this project.
- **Public subnets for the worker VM:** Simpler and cheaper than private subnets
  + NAT. Security via tight security groups instead of network isolation.
- **These files describe real AWS, not an emulator.** Since LocalStack was
  removed, every constraint here is one that binds on the first run rather than
  at deploy time.
