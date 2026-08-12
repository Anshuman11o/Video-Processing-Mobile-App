# config/

Reference documentation for external service constraints that affect this project.

## Files

| File | Purpose |
|------|---------|
| `aws-limits.md` | Hard limits from S3 and DynamoDB; local queue settings, worker sizing, HLS delivery |
| `free-tier.md` | AWS free tier quotas, cost traps (NAT Gateway!), budget alerts |

## Why This Exists

AWS services have constraints that silently break systems if violated:
- S3 multipart uploads need 5MB minimum parts
- S3 rejects wildcards in CORS `ExposeHeaders` (see `TROUBLESHOOTING.md`)
- DynamoDB items max 400KB
- NAT Gateway costs $32/month even idle

Rather than discover these mid-implementation, we document them upfront.

## How to Use

Before implementing anything that touches AWS:
1. Check the relevant limits in `aws-limits.md`
2. Verify we're within free tier in `free-tier.md`
3. Update these files if you discover new constraints

## Non-Obvious Decisions

- **VPC Gateway Endpoints over NAT Gateway:** S3 and DynamoDB access via gateway
  endpoints is free. NAT Gateway is the biggest cost trap for this project.
- **Public subnets for the worker VM:** Simpler and cheaper than private subnets +
  NAT. Security via tight security groups instead of network isolation.
- **No CDN, no managed container hosting:** A handful of short clips doesn't justify
  CloudFront or ECS Fargate. HLS is served directly from the HLS bucket (so that
  bucket must be player-readable), and the API and worker run as two plain Go
  processes on one small VM. See `PROJECT_PLAN.md` → "Deferred".
- **Only two AWS services are used:** S3 and DynamoDB. SQS was replaced by a
  self-hosted SQLite queue and ElastiCache by an in-process TTL cache, so their
  limits no longer bind. The limits that replaced them are settings we choose,
  not quotas AWS imposes — documented in `aws-limits.md` all the same, because
  they are the numbers that decide whether the pipeline stalls or retries.
