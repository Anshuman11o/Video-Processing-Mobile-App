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
| Expected AWS runs | **1–5 total, then the project closes** |
| Default posture | **S3 and DynamoDB are real from the first run.** There is no emulator any more; the queue and the status cache are local and free |

## Everything is on-demand — nothing stays up

This project has a **finite life**: a handful of runs for development and
testing, then it is closed. Nothing here is a service that needs to keep running,
so **no AWS resource should outlive the run that needed it.**

That single rule is what keeps the budget safe. Per-run cost is pennies; the only
way to overspend is to leave something switched on after the run that justified
it. A resource forgotten for a month costs more than every run combined.

**Obligation when anything is provisioned:** whoever (or whatever) brings up an
AWS resource must say so explicitly at the end of that run — name the resource
and state that it needs switching off or disconnecting. Not "consider tearing
down"; an explicit reminder naming what is still live. A resource that was
created silently will be forgotten silently.

This applies to anything billed by time — VMs, NAT Gateways, managed container
tasks, managed cache nodes, load balancers, provisioned DynamoDB capacity, EBS
volumes, Elastic IPs left unattached. It does not apply to S3 objects or
DynamoDB items, which bill by size and are trivial at this scale, though they
are still worth deleting at the end.

**Teardown check after every AWS run.** The architecture uses only S3 and
DynamoDB, so every line below should come back empty — this is a check that
nothing was created by accident, not a list of things we run:

```bash
aws ec2 describe-instances --filters Name=instance-state-name,Values=running
aws ec2 describe-nat-gateways --filter Name=state,Values=available
aws ecs list-tasks --cluster <cluster>            # expect none — no container hosting
aws elasticache describe-cache-clusters           # expect none — the cache is in-process
aws ec2 describe-addresses                        # unattached Elastic IPs still bill
```

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
| **Managed containers (Fargate), 4 workers 24/7** | ~$115/month | Not in the architecture. The four stages are four processes on one host; if that host is ever a VM, it is stopped when the demo ends |
| **ElastiCache Redis** | ~$12/month (smallest node) | Not in the architecture. Job status is cached in a map inside the API process |
| **Idle anything** | varies | Anything billed per hour must be torn down after use, not stopped |

Any single item in this table exceeds the total budget within one month. **None
of them is in the architecture.** The container fleet and the cache server were
removed outright along with the emulator (see `infra/CONTEXT.md`); a NAT Gateway
was never created. None should arrive without a decided teardown time.

### Tier 2 — bounded, but worth not being stupid about

| Service | Rough rate | Realistic exposure at ≤10s clips |
|---|---|---|
| S3 PUT | ~$0.005 / 1,000 | 22 objects/job (1 manifest + 1 audio + ≤20 frames) → ~$0.0001/job |
| S3 GET | ~$0.0004 / 1,000 | negligible |
| S3 storage | ~$0.023 / GB-month | A 10s clip is a few MB; hundreds of jobs ≈ pennies |
| DynamoDB on-demand | ~$1.25 / M writes | ~8 writes/job → negligible |
| Queue operations | **$0** | A SQLite file on local disk. No service, no requests, no bill |
| Status cache | **$0** | A map in the API process |
| Data egress | $0.09/GB after 100GB | Keep one region. HLS is served straight from S3, so viewer traffic is egress — there is no CDN absorbing it |

At ≤10s clips, **processing cost per job is a fraction of a cent.** The budget is
not threatened by throughput. It is threatened by leaving something switched on.

---

## The polling loop: no longer a billing risk, still a bug risk

This section used to be about SQS. Polling the queue is now free — it is a
`SELECT` against a local file — so the "idle worker bills forever" problem is
gone with the service that caused it.

**The failure mode survives the migration.** A poll that fails returns
immediately rather than waiting out its interval, so looping straight back round
turns any persistent error — a locked database, a corrupt file, a revoked
credential on the S3 call that follows — into a hot loop. It no longer costs
money; it costs a pegged core and a log file that fills the disk. And the same
loop shape does still touch billed services: every claimed message leads to S3
and DynamoDB calls, so a redelivery storm is a request-billed storm.

**Controlled** in `internal/worker/runner.go` — failed receives back off from 1s
to a 30s ceiling, resetting on the first success. Added 2026-08-12 specifically
because of this budget. Do not remove it, and preserve the equivalent in any new
consume loop. `QUEUE_MAX_DELIVERIES=3` and the DLQ are the second half of the
same guard: a message that cannot succeed stops being retried.

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
3. The pipeline already runs locally against real S3 and DynamoDB, so there is
   nothing to "move to AWS" except the compute. Deployment means one small VM
   running the two binaries — see `PROJECT_PLAN.md` Stage 10.

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

## Local development costs

There is no emulator. Local development talks to the same real buckets and table
as everything else, so development traffic bills alongside demo traffic. It is
still pennies, because both services bill per request.

| Local activity | Cost |
|----------------|------|
| Iterating on the pipeline (a few dozen clips) | ~22 PUTs and a handful of GETs per job → cents |
| Job status polling during dev | Reads, mostly absorbed by the in-process cache |
| Queue operations | **$0** — SQLite file on local disk |
| Status cache | **$0** — in-process map |
| ffmpeg and Whisper | **$0** — compute on hardware already paid for |

**Watch for:** an unbounded retry that re-uploads or re-processes. That is the
only local activity that can generate real request volume, which is part of why
`QUEUE_MAX_DELIVERIES=3` and the DLQ exist.

### Why there is no emulator

LocalStack was dropped for two reasons: the ~1 GB of RAM it cost, and the fact
that its divergences from real S3 hid a genuine bug (`TROUBLESHOOTING.md`, the
CORS `ExposeHeaders` entry). Developing against real AWS is cheaper than
debugging the difference.

Two constraints from the emulator era are kept because they still explain
decisions in the code:

- **Transcription was never emulated** (Pro-only), which is part of why 5A uses
  local Whisper. That choice also saves money: it is compute on hardware already
  paid for, and `MOCK_TRANSCRIBE=true` is the default besides.
- **The emulator did not persist state between restarts** on the free tier,
  which is why 3A's transient-failure test was written not to depend on
  surviving a restart. The test is still correct; the reason has changed from
  "the emulator forgets" to "the queue file is disposable and `make queue-reset`
  is a normal thing to do".

---

## Estimated cost, current plan

| Activity | Cost |
|----------|------|
| Local development against real S3 + DynamoDB | pennies per session |
| Process 100 clips at ≤10s | ~$0.05 |
| One small VM running both binaries, 2 hours | ~$0.02 |
| S3 storage, ~1 GB for a month | ~$0.02 |
| HLS egress, ~1 GB (no CDN in front) | ~$0.09 |
| **One NAT Gateway left up for a month** | **~$32 — over budget on its own** |

The pipeline is cheap. The infrastructure around it is not — which is most of
why there is so little of it left.
