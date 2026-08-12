# config/

Reference documentation for external service constraints that affect this project.

## Files

| File | Purpose |
|------|---------|
| `aws-limits.md` | Hard limits from S3, SQS, DynamoDB, Lambda, Fargate, CloudFront, Redis |
| `free-tier.md` | AWS free tier quotas, cost traps (NAT Gateway!), budget alerts |

## Why This Exists

AWS services have constraints that silently break systems if violated:
- S3 multipart uploads need 5MB minimum parts
- SQS messages max 256KB
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
- **Public subnets for Fargate:** Simpler and cheaper than private subnets + NAT.
  Security via tight security groups instead of network isolation.
