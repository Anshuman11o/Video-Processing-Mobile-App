# AWS Free Tier Quotas and Cost Traps

Reference for staying within free tier during development. Updated as we discover
constraints.

---

## Free Tier Allowances (12-month)

| Service | Free Tier | Expected Usage | Status |
|---------|-----------|----------------|--------|
| **S3** | 5 GB storage, 20,000 GET, 2,000 PUT | ~1GB for demo | OK |
| **DynamoDB** | 25 RCU, 25 WCU, 25 GB | Minimal | OK |
| **SQS** | 1M requests/month | ~1000 for demo | OK |
| **Lambda** | 1M requests, 400,000 GB-sec | Not using | N/A |
| **EC2** | 750 hrs/month t3.micro | One small VM, on-demand | OK |
| **ECR** | 500 MB storage | ~200MB images | OK |

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

The workers are long-running SQS pollers, so anything hosting them bills by the
hour whether or not clips are arriving.

**Mitigation:**
- One small VM runs all four workers as Docker containers, not one host per stage
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

## LocalStack Parity Notes

LocalStack Pro vs Community:

| Feature | Community (Free) | Pro |
|---------|------------------|-----|
| S3 | Full | Full |
| SQS | Full | Full |
| DynamoDB | Full | Full |
| Lambda | Basic | Full |

**For local dev:** Community edition covers everything we use — S3, SQS, DynamoDB.
Workers run as plain Docker containers under Compose, and HLS is served straight off
LocalStack's S3 endpoint, so none of the Pro-only services are on the critical path.

---

## Estimated Demo Costs (if deploying to AWS)

| Activity | Cost |
|----------|------|
| Process 10 demo clips | ~$0.50 |
| Run one t3.micro VM for 2 hours | Free tier |
| S3 storage (100MB) | ~$0.01 |
| SQS (1000 messages) | Free tier |
| S3 egress for HLS (1GB) | Free tier |
| **Total for demo** | **<$1** |

Safe to demo: no NAT Gateway, no CDN, and the VM stops when the demo does.
