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
| **CloudFront** | 1 TB transfer, 10M requests | ~1GB for demo | OK |
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

**The biggest trap.** NAT Gateway costs ~$0.045/hour + data processing fees.
Running 24/7 = ~$32/month minimum.

**Mitigation:**
- Place Fargate tasks in **public subnets** with public IPs
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

### Fargate (~$0.04/hour per task)

Fargate is not free tier. Running 4 workers 24/7 = ~$115/month.

**Mitigation:**
- Scale to zero when idle (SQS-triggered scaling)
- For demo, run tasks on-demand, not always-on
- Or use Lambda for lighter workers (validate)

### EBS/EFS Storage

Not used in this architecture, but watch for accidental creation.

### Data Transfer

- **Into AWS:** Free
- **Out of AWS:** First 100GB/month free, then $0.09/GB
- **Cross-region:** $0.02/GB

**Mitigation:** Keep everything in one region. Use CloudFront for HLS delivery
(cheaper egress).

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
| ECS | No | Yes |
| CloudFront | No | Yes |

**For local dev:** Community edition covers S3, SQS, DynamoDB. Run workers as
Docker containers directly (not ECS). CloudFront not needed locally.

---

## Estimated Demo Costs (if deploying to AWS)

| Activity | Cost |
|----------|------|
| Process 10 demo clips | ~$0.50 |
| Run Fargate for 2 hours | ~$0.32 |
| S3 storage (100MB) | ~$0.01 |
| SQS (1000 messages) | Free tier |
| CloudFront (1GB) | Free tier |
| **Total for demo** | **~$1** |

Safe to demo without NAT Gateway and with on-demand Fargate.
