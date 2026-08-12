# AWS Cost Constraints

> **There is no free tier on this account.** Confirmed 2026-08-12. Every request,
> every GB-hour and every byte stored is billed from the first one. This document
> previously assumed a 12-month free tier; that assumption was wrong and has been
> removed rather than annotated, because a stale allowance table is worse than no
> table.

---

## The budget

| Constraint | Value |
|---|---|
| **Hard ceiling** | **$20 total** — not per month, total |
| Test clip length | **≤ 10 seconds** |
| Default posture | **Everything runs on LocalStack.** AWS spend is opt-in, never incidental |

$20 is small enough that a single misconfiguration can consume it in a day. The
controls below are ordered by how much damage they can do, not by how likely they
are.

---

## What actually costs money here

Per-request pricing is close to irrelevant at this scale. **Time-based charges
are the entire risk.** A resource billed per hour spends money while you sleep;
a resource billed per request spends money only when you use it.

### Tier 1 — can consume the whole budget unattended

| Trap | Cost if left running | Control |
|---|---|---|
| **NAT Gateway** | ~$32/month | Never create one. Public subnets + VPC Gateway Endpoints for S3/DynamoDB (endpoints are free) |
| **Fargate, 4 workers 24/7** | ~$115/month | Never run workers always-on. On-demand only, scale to zero |
| **ElastiCache Redis** | ~$12/month (smallest node) | Do not deploy. Redis stays a local container; the API's cache is not load-bearing |
| **Idle anything** | varies | Anything billed per hour must be torn down after use, not stopped |

Any single item in this table exceeds the total budget within one month. **All
three of the named services are currently local-only, and should stay that way.**

### Tier 2 — bounded, but worth not being stupid about

| Service | Rough rate | Realistic exposure at ≤10s clips |
|---|---|---|
| S3 PUT | ~$0.005 / 1,000 | 22 objects/job (1 manifest + 1 audio + ≤20 frames) → ~$0.0001/job |
| S3 GET | ~$0.0004 / 1,000 | negligible |
| S3 storage | ~$0.023 / GB-month | A 10s clip is a few MB; hundreds of jobs ≈ pennies |
| DynamoDB on-demand | ~$1.25 / M writes | ~8 writes/job → negligible |
| SQS | ~$0.40 / M requests | see below — this is the one with a failure mode |
| Data egress | $0.09/GB after 100GB | Keep one region. Do not pull processed video back down repeatedly |

At ≤10s clips, **processing cost per job is a fraction of a cent.** The budget is
not threatened by throughput. It is threatened by leaving something switched on.

---

## SQS: the one request-based risk

Workers long-poll continuously. That is the correct design — it is what keeps an
idle worker from spinning — but it means an *idle* worker still issues requests
forever.

Idle cost, per worker: `WaitTimeSeconds = 20` → 3 requests/min → ~130k/month →
**~$0.05/month**. Four workers ≈ $0.21/month. Acceptable.

**The failure mode is a failed receive.** A successful receive blocks for the
full 20s; a *failing* one returns immediately. Looping straight back round turns
any persistent error — expired credentials, a throttle, a deleted queue — into a
hot loop issuing requests as fast as the network allows. At a few hundred
requests/second that is millions per day, and it bills while also flooding
CloudWatch Logs.

**Controlled** in `internal/worker/runner.go` — failed receives back off from 1s
to a 30s ceiling, resetting on the first success. Added 2026-08-12 specifically
because of this budget. Do not remove it, and preserve the equivalent in any new
consume loop.

---

## Levers if spend needs tightening further

Not applied, because they change product behaviour rather than infrastructure —
listed so the choice is available and costed.

| Lever | Where | Effect |
|---|---|---|
| Lower the duration ceiling | `internal/worker/validate/validate.go` → `DefaultLimits.MaxDurationSeconds` (currently **600s**) | Test clips are ≤10s but the pipeline still accepts 10 minutes. Dropping this to e.g. 60s makes an accidental large upload fail fast instead of processing |
| Lower the frame cap | `internal/worker/extract` → `DefaultOptions.MaxFrames` (**20**) | Fewer S3 PUTs. Marginal at this scale |
| Shorten log retention | CloudWatch, if deployed | 7 days. Never log video bytes |

The duration ceiling is the meaningful one: it is the only guard between a
mis-selected 4K movie and a long, billed ffmpeg run.

---

## Before any AWS deployment

1. **Set billing alerts first**, not after — alerts are free and are the only
   thing that catches a mistake while it is still small.
2. Deploy nothing that bills per hour without deciding when it gets torn down.
3. Prefer running the whole pipeline locally. LocalStack Community covers S3,
   SQS and DynamoDB, which is everything stages 1A–6A touch.

```bash
aws budgets create-budget \
  --account-id $AWS_ACCOUNT_ID \
  --budget file://budget.json \
  --notifications-with-subscribers file://notifications.json
```

Thresholds, against a $20 total:

- **$2** — something is running that should not be
- **$5** — stop and investigate before doing anything else
- **$10** — half the budget is gone; tear down and reassess
- **$20** — hard ceiling

---

## LocalStack Parity

| Feature | Community (Free) | Pro |
|---------|------------------|-----|
| S3 | Full | Full |
| SQS | Full | Full |
| DynamoDB | Full | Full |
| Lambda | Basic | Full |
| ECS | No | Yes |
| CloudFront | No | Yes |
| Transcribe | No | Yes |
| Persistence (`PERSISTENCE=1`) | No — silently ignored | Yes |

Two things this table has already cost us:

- **Transcribe is Pro-only**, which is part of why 5A uses local Whisper. That
  choice now also saves money: it is compute on hardware already paid for.
- **`PERSISTENCE=1` is silently ignored in Community.** It is set in
  `docker-compose.yml` and does nothing. A LocalStack restart wipes queues,
  buckets and job rows — this changed how 3A's transient-failure test had to be
  written.

---

## Estimated cost, current plan

| Activity | Cost |
|----------|------|
| All local development (LocalStack) | **$0** |
| Process 100 clips at ≤10s, if deployed | ~$0.05 |
| Fargate, on-demand, 2 hours | ~$0.32 |
| S3 storage, ~1 GB for a month | ~$0.02 |
| **One NAT Gateway left up for a month** | **~$32 — over budget on its own** |

The pipeline is cheap. The infrastructure around it is not.
