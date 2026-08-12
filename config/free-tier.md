# AWS Free Tier Quotas and Cost Traps

Reference for staying within free tier during development. Updated as we discover
constraints.

---

## Free Tier Allowances (12-month)

| Service | Free Tier | Expected Usage | Status |
|---------|-----------|----------------|--------|
| **S3** | 5 GB storage, 20,000 GET, 2,000 PUT | ~1GB for demo | OK |
| **DynamoDB** | 25 RCU, 25 WCU, 25 GB | Minimal | OK |
| **Lambda** | 1M requests, 400,000 GB-sec | Not using | N/A |
| **EC2** | 750 hrs/month t3.micro | One small VM, on-demand | OK |

Only S3 and DynamoDB are actually used. The queue is a SQLite file on local disk
and costs nothing; the status cache lives in the API process. ECR is no longer
listed because nothing is containerized — the VM runs the Go binaries directly.

## Always-Free Tier

| Service | Free Tier | Notes |
|---------|-----------|-------|
| **DynamoDB** | 25 RCU/WCU perpetual | On-demand mode may cost more |
| **Lambda** | 1M requests/month perpetual | — |
| **CloudWatch** | 10 custom metrics, 5GB logs | — |

---

## COST TRAPS

### NAT Gateway (~$32/month)

**Mostly moot now** — this section was written for Fargate tasks in private subnets,
and we no longer run Fargate. A single VM in a public subnet never needs a NAT
Gateway. Kept because the trap reappears the moment anyone moves compute into a
private subnet, and because the mitigation is what keeps the VM deployment cheap too.

**The trap:** NAT Gateway costs ~$0.045/hour + data processing fees. Running 24/7 =
~$32/month minimum, idle or not.

**Mitigation:**
- Put the worker VM in a **public subnet** with a public IP
- Use **VPC Gateway Endpoints** for S3 and DynamoDB (free)
- Use tight security groups instead of relying on private subnet isolation

```hcl
# VPC Endpoints (FREE)
resource "aws_vpc_endpoint" "s3" {
  vpc_id       = aws_vpc.main.id
  service_name = "com.amazonaws.${var.region}.s3"
  vpc_endpoint_type = "Gateway"
  route_table_ids = [aws_route_table.public.id]
}

resource "aws_vpc_endpoint" "dynamodb" {
  vpc_id       = aws_vpc.main.id
  service_name = "com.amazonaws.${var.region}.dynamodb"
  vpc_endpoint_type = "Gateway"
  route_table_ids = [aws_route_table.public.id]
}
```

### Always-On Compute

The worker is a long-running poller, so anything hosting it bills by the hour
whether or not clips are arriving.

**Mitigation:**
- One small VM runs the API and the worker as two processes, not one host per stage
- Start it for the demo, stop it after — don't leave it running
- t3.micro is free tier for 12 months (750 hrs/month)

### EBS/EFS Storage

Not used in this architecture, but watch for accidental creation.

### Data Transfer

- **Into AWS:** Free
- **Out of AWS:** First 100GB/month free, then $0.09/GB
- **Cross-region:** $0.02/GB

**Mitigation:** Keep everything in one region. HLS egress comes straight out of S3;
at demo volume (~1GB) that sits inside the first 100GB free. A CDN would cut egress
cost at real viewer volume, but there is no real viewer volume here.

### CloudWatch Logs

- First 5GB/month free
- Then $0.50/GB ingested

**Mitigation:** Set log retention to 7 days. Don't log video bytes.

---

## Budget Alerts

Set up billing alerts before deploying to AWS:

```bash
# Via AWS CLI
aws budgets create-budget \
  --account-id $AWS_ACCOUNT_ID \
  --budget file://budget.json \
  --notifications-with-subscribers file://notifications.json
```

**Recommended thresholds:**
- Alert at $5 (something's wrong)
- Alert at $10 (stop and investigate)
- Hard limit at $20 (paranoid safety)

---

## Local Development Costs

There is no emulator. Local development talks to the same real S3 buckets and
DynamoDB table as everything else, so development traffic counts against the
free tier alongside demo traffic.

| Local activity | Cost |
|----------------|------|
| Iterating on the pipeline (a few dozen clips) | Well inside 2,000 PUT / 20,000 GET |
| Job status polling during dev | Well inside 25 RCU |
| Queue operations | Free — SQLite file on local disk |
| Status cache | Free — in-process map |

**Watch for:** a retry loop that re-uploads or re-processes without bound will
burn the 2,000 monthly PUTs faster than anything else here. `QUEUE_MAX_DELIVERIES=3`
and the DLQ exist partly to make that impossible.

**Why no emulator:** LocalStack was dropped both for the ~1 GB of RAM it cost and
because its divergences from real S3 hid a genuine bug — see `TROUBLESHOOTING.md`.
Free-tier headroom is large enough that developing against real AWS is cheaper
than debugging the difference.

---

## Estimated Demo Costs (if deploying to AWS)

| Activity | Cost |
|----------|------|
| Process 10 demo clips | ~$0.50 |
| Run one t3.micro VM for 2 hours | Free tier |
| S3 storage (100MB) | ~$0.01 |
| Queue (1000 messages) | $0 — local SQLite file |
| S3 egress for HLS (1GB) | Free tier |
| **Total for demo** | **<$1** |

Safe to demo: no NAT Gateway, no CDN, and the VM stops when the demo does.
