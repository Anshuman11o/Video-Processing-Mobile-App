# Stage 1B: Local Infrastructure (Docker + LocalStack)

> **SUPERSEDED.** Docker Compose, LocalStack and Redis have been removed from the
> project. S3 and DynamoDB now point at a real AWS account, SQS is replaced by a
> SQLite queue (`docs/stage-plans/stage-3b-local-queue.md`), and Redis by an
> in-process TTL cache. Everything below describes infrastructure that no longer
> exists; it is kept as the record of what was tried and why it went.
> See `infra/CONTEXT.md` for what replaced it, and `PROJECT_PLAN.md` Stage 1B
> for the stage as it now stands: creating three buckets and one table in a real
> account, once.

> **Run in parallel with:** Stage 1A (Data Schemas)
> **Estimated time:** 20 minutes
> **Blocks:** All subsequent stages (nothing runs without infra)

## Aim

One command (`docker-compose up`) brings up the complete local development
environment: LocalStack (S3, SQS, DynamoDB), Redis, and initializes all AWS
resources automatically.

---

## Components Touched

| Component | Action | Files |
|-----------|--------|-------|
| `infra/` | Create | `docker-compose.yml` |
| `infra/localstack/` | Create | `init-aws.sh` |
| `scripts/` | Create | `dev-setup.sh` |
| Root | Create | `Makefile` |

---

## Architecture Decisions

### LocalStack Configuration

| Service | LocalStack Port | Purpose |
|---------|-----------------|---------|
| S3 | 4566 | Video storage, HLS output |
| SQS | 4566 | Worker queue triggers |
| DynamoDB | 4566 | Job state |
| All | 4566 | LocalStack uses single gateway port |

**LocalStack image:** `localstack/localstack:3.0` (stable, well-documented)

### Redis Configuration

| Setting | Value | Reason |
|---------|-------|--------|
| Port | 6379 | Standard Redis port |
| Persistence | None | Ephemeral cache only |
| Memory | 128MB | More than enough for status cache |

### Network

- **Network name:** `dayreel-network`
- **Driver:** bridge
- All services on same network for DNS resolution (`localstack:4566`, `redis:6379`)

---

## Docker Compose Structure

### `infra/docker-compose.yml`

```yaml
version: '3.8'

services:
  localstack:
    image: localstack/localstack:3.0
    container_name: dayreel-localstack
    ports:
      - "4566:4566"            # LocalStack Gateway
      - "4510-4559:4510-4559"  # External services port range
    environment:
      - SERVICES=s3,sqs,dynamodb
      - DEBUG=0
      - PERSISTENCE=1
      - DOCKER_HOST=unix:///var/run/docker.sock
      - AWS_DEFAULT_REGION=us-east-1
      - AWS_ACCESS_KEY_ID=test
      - AWS_SECRET_ACCESS_KEY=test
    volumes:
      - "./localstack/init-aws.sh:/etc/localstack/init/ready.d/init-aws.sh"
      - "localstack-data:/var/lib/localstack"
      - "/var/run/docker.sock:/var/run/docker.sock"
    networks:
      - dayreel-network
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:4566/_localstack/health"]
      interval: 10s
      timeout: 5s
      retries: 5

  redis:
    image: redis:7-alpine
    container_name: dayreel-redis
    ports:
      - "6379:6379"
    command: redis-server --maxmemory 128mb --maxmemory-policy allkeys-lru
    networks:
      - dayreel-network
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5

networks:
  dayreel-network:
    driver: bridge

volumes:
  localstack-data:
```

---

## AWS Resource Initialization

### `infra/localstack/init-aws.sh`

```bash
#!/bin/bash
set -euo pipefail

echo "Initializing CaptionClips AWS resources..."

ENDPOINT="http://localhost:4566"
REGION="us-east-1"

# Common AWS CLI options
AWS_OPTS="--endpoint-url=$ENDPOINT --region=$REGION"

# ============================================================================
# S3 Buckets
# ============================================================================

echo "Creating S3 buckets..."

aws $AWS_OPTS s3 mb s3://dayreel-raw-videos || true
aws $AWS_OPTS s3 mb s3://dayreel-processed || true
aws $AWS_OPTS s3 mb s3://dayreel-hls-output || true

# Enable CORS on HLS bucket for browser playback
aws $AWS_OPTS s3api put-bucket-cors --bucket dayreel-hls-output --cors-configuration '{
  "CORSRules": [
    {
      "AllowedHeaders": ["*"],
      "AllowedMethods": ["GET", "HEAD"],
      "AllowedOrigins": ["*"],
      "ExposeHeaders": ["ETag"],
      "MaxAgeSeconds": 3600
    }
  ]
}'

echo "S3 buckets created."

# ============================================================================
# SQS Queues
# ============================================================================

echo "Creating SQS queues..."

# Create DLQ first (other queues reference it)
DLQ_URL=$(aws $AWS_OPTS sqs create-queue \
  --queue-name dayreel-dlq \
  --attributes '{
    "MessageRetentionPeriod": "1209600",
    "VisibilityTimeout": "300"
  }' \
  --query 'QueueUrl' --output text)

DLQ_ARN=$(aws $AWS_OPTS sqs get-queue-attributes \
  --queue-url "$DLQ_URL" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' --output text)

echo "DLQ created: $DLQ_URL"

# Create worker queues with DLQ redrive policy
for QUEUE in dayreel-validate dayreel-extract dayreel-transcribe dayreel-package; do
  aws $AWS_OPTS sqs create-queue \
    --queue-name "$QUEUE" \
    --attributes '{
      "VisibilityTimeout": "300",
      "MessageRetentionPeriod": "86400",
      "RedrivePolicy": "{\"deadLetterTargetArn\":\"'"$DLQ_ARN"'\",\"maxReceiveCount\":\"3\"}"
    }' || true
  echo "Queue created: $QUEUE"
done

echo "SQS queues created."

# ============================================================================
# DynamoDB Table
# ============================================================================

echo "Creating DynamoDB table..."

aws $AWS_OPTS dynamodb create-table \
  --table-name dayreel-jobs \
  --attribute-definitions \
    AttributeName=pk,AttributeType=S \
    AttributeName=sk,AttributeType=S \
  --key-schema \
    AttributeName=pk,KeyType=HASH \
    AttributeName=sk,KeyType=RANGE \
  --billing-mode PAY_PER_REQUEST \
  || true

# Wait for table to be active
aws $AWS_OPTS dynamodb wait table-exists --table-name dayreel-jobs

echo "DynamoDB table created."

# ============================================================================
# S3 Event Notifications (trigger validate queue on upload)
# ============================================================================

echo "Configuring S3 event notifications..."

VALIDATE_QUEUE_ARN=$(aws $AWS_OPTS sqs get-queue-attributes \
  --queue-url "$(aws $AWS_OPTS sqs get-queue-url --queue-name dayreel-validate --query 'QueueUrl' --output text)" \
  --attribute-names QueueArn \
  --query 'Attributes.QueueArn' --output text)

aws $AWS_OPTS s3api put-bucket-notification-configuration \
  --bucket dayreel-raw-videos \
  --notification-configuration '{
    "QueueConfigurations": [
      {
        "QueueArn": "'"$VALIDATE_QUEUE_ARN"'",
        "Events": ["s3:ObjectCreated:CompleteMultipartUpload", "s3:ObjectCreated:Put"],
        "Filter": {
          "Key": {
            "FilterRules": [
              {"Name": "suffix", "Value": ".mp4"}
            ]
          }
        }
      }
    ]
  }'

echo "S3 event notifications configured."

# ============================================================================
# Summary
# ============================================================================

echo ""
echo "============================================"
echo "CaptionClips local infrastructure initialized!"
echo "============================================"
echo ""
echo "S3 Buckets:"
aws $AWS_OPTS s3 ls
echo ""
echo "SQS Queues:"
aws $AWS_OPTS sqs list-queues --query 'QueueUrls' --output table
echo ""
echo "DynamoDB Tables:"
aws $AWS_OPTS dynamodb list-tables --query 'TableNames' --output table
echo ""
echo "Endpoints:"
echo "  S3:       http://localhost:4566"
echo "  SQS:      http://localhost:4566"
echo "  DynamoDB: http://localhost:4566"
echo "  Redis:    localhost:6379"
echo ""
```

---

## Development Scripts

### `scripts/dev-setup.sh`

```bash
#!/bin/bash
set -euo pipefail

# CaptionClips Development Setup
# Run this once to set up your local environment

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(dirname "$SCRIPT_DIR")"

echo "Setting up CaptionClips development environment..."

# Check prerequisites
command -v docker >/dev/null 2>&1 || { echo "Docker is required but not installed. Aborting."; exit 1; }
command -v docker-compose >/dev/null 2>&1 || command -v "docker compose" >/dev/null 2>&1 || { echo "Docker Compose is required but not installed. Aborting."; exit 1; }
command -v aws >/dev/null 2>&1 || { echo "AWS CLI is required but not installed. Install with: brew install awscli"; exit 1; }

# Create .env from example if it doesn't exist
if [ ! -f "$PROJECT_ROOT/.env" ]; then
  cp "$PROJECT_ROOT/.env.example" "$PROJECT_ROOT/.env"
  echo "Created .env from .env.example"
fi

# Start infrastructure
echo "Starting Docker containers..."
cd "$PROJECT_ROOT/infra"
docker-compose up -d

# Wait for LocalStack to be healthy
echo "Waiting for LocalStack to be ready..."
timeout 60 bash -c 'until curl -sf http://localhost:4566/_localstack/health | grep -q "running"; do sleep 2; done'

# Wait for Redis to be ready
echo "Waiting for Redis to be ready..."
timeout 30 bash -c 'until docker exec dayreel-redis redis-cli ping | grep -q PONG; do sleep 1; done'

echo ""
echo "Development environment ready!"
echo ""
echo "Verify with:"
echo "  aws --endpoint-url=http://localhost:4566 s3 ls"
echo "  aws --endpoint-url=http://localhost:4566 sqs list-queues"
echo "  aws --endpoint-url=http://localhost:4566 dynamodb list-tables"
echo "  redis-cli ping"
```

### `Makefile` (project root)

```makefile
.PHONY: dev-up dev-down dev-logs dev-reset test-infra

# Start local development environment
dev-up:
	cd infra && docker-compose up -d
	@echo "Waiting for services..."
	@sleep 5
	@echo "Services ready. Run 'make test-infra' to verify."

# Stop local development environment
dev-down:
	cd infra && docker-compose down

# View logs
dev-logs:
	cd infra && docker-compose logs -f

# Reset everything (removes volumes)
dev-reset:
	cd infra && docker-compose down -v
	cd infra && docker-compose up -d

# Test infrastructure is working
test-infra:
	@echo "Testing S3..."
	@aws --endpoint-url=http://localhost:4566 s3 ls
	@echo ""
	@echo "Testing SQS..."
	@aws --endpoint-url=http://localhost:4566 sqs list-queues
	@echo ""
	@echo "Testing DynamoDB..."
	@aws --endpoint-url=http://localhost:4566 dynamodb list-tables
	@echo ""
	@echo "Testing Redis..."
	@redis-cli ping
	@echo ""
	@echo "All infrastructure tests passed!"
```

---

## Environment Variables

### Required in `.env`

```bash
# Already in .env.example, verify these are set:
LOCALSTACK_ENDPOINT=http://localhost:4566
AWS_ACCESS_KEY_ID=test
AWS_SECRET_ACCESS_KEY=test
AWS_DEFAULT_REGION=us-east-1
REDIS_URL=localhost:6379
```

---

## Tasks

1. [ ] Create `infra/` directory structure
2. [ ] Write `infra/docker-compose.yml`
3. [ ] Write `infra/localstack/init-aws.sh` and make executable
4. [ ] Write `scripts/dev-setup.sh` and make executable
5. [ ] Write `Makefile` at project root
6. [ ] Run `docker-compose up -d`
7. [ ] Verify all resources created with `make test-infra`
8. [ ] Create `infra/CONTEXT.md`

---

## Test

```bash
# Start everything
make dev-up

# Wait for init script to complete (watch logs)
make dev-logs
# Look for "CaptionClips local infrastructure initialized!"

# Verify resources
make test-infra
```

**Expected output:**

```
Testing S3...
2024-01-15 10:30:00 dayreel-hls-output
2024-01-15 10:30:00 dayreel-processed
2024-01-15 10:30:00 dayreel-raw-videos

Testing SQS...
----------------------------
|       ListQueues         |
+---------------------------+
| http://sqs.us-east-1...  |
+---------------------------+

Testing DynamoDB...
---------------
| ListTables  |
+-------------+
| dayreel-jobs|
+-------------+

Testing Redis...
PONG

All infrastructure tests passed!
```

---

## Verification Checklist

- [ ] `docker-compose up -d` starts without errors
- [ ] LocalStack health endpoint returns healthy
- [ ] Redis responds to `ping` with `PONG`
- [ ] 3 S3 buckets exist: `dayreel-raw-videos`, `dayreel-processed`, `dayreel-hls-output`
- [ ] 5 SQS queues exist: validate, extract, transcribe, package, dlq
- [ ] DynamoDB table `dayreel-jobs` exists
- [ ] S3 event notification configured on raw-videos bucket
- [ ] CORS configured on hls-output bucket
- [ ] `make dev-reset` cleanly rebuilds everything

---

## Claude Code Implementation Plan

### Recommended Approach: Sequential with Background Wait

Infrastructure setup involves file creation followed by Docker operations with
wait times. Best executed with direct tool calls, using background execution
for the Docker startup.

### Execution Steps

```
1. Create directory structure
   Tool: Bash
   Command: mkdir -p infra/localstack scripts

2. Write docker-compose.yml
   Tool: Write
   File: infra/docker-compose.yml
   Content: [YAML from above]

3. Write init-aws.sh
   Tool: Write
   File: infra/localstack/init-aws.sh
   Content: [Bash script from above]

4. Make init script executable
   Tool: Bash
   Command: chmod +x infra/localstack/init-aws.sh

5. Write dev-setup.sh
   Tool: Write
   File: scripts/dev-setup.sh
   Content: [Bash script from above]

6. Make dev-setup executable
   Tool: Bash
   Command: chmod +x scripts/dev-setup.sh

7. Write Makefile
   Tool: Write
   File: Makefile
   Content: [Makefile from above]

8. Start Docker Compose
   Tool: Bash (background)
   Command: cd infra && docker-compose up -d

9. Wait for LocalStack health
   Tool: Bash
   Command: timeout 60 bash -c 'until curl -sf http://localhost:4566/_localstack/health | grep -q "running"; do sleep 2; done'

10. Verify infrastructure
    Tool: Bash
    Command: make test-infra

11. Create CONTEXT.md
    Tool: Write
    File: infra/CONTEXT.md
```

### Parallel Opportunities Within This Stage

| Can Run Together | Why |
|------------------|-----|
| Steps 2-6 | All file writes, no dependencies between them |
| Step 8 (docker up) + Step 7 (Makefile) | Docker starts while writing Makefile |

### Why Not Subagents?

- Sequential dependencies (can't test infra before Docker starts)
- File writes are fast, don't benefit from parallelization
- Docker startup is the bottleneck, can't parallelize that

### Potential Blockers

| Blocker | Detection | Resolution |
|---------|-----------|------------|
| Docker not running | `docker info` fails | Prompt user to start Docker Desktop |
| Port 4566 in use | `docker-compose up` fails | `lsof -i :4566`, kill conflicting process |
| Port 6379 in use | Redis fails to start | `lsof -i :6379`, kill or use different port |
| AWS CLI not installed | `aws` command not found | `brew install awscli` |
| LocalStack init fails | Health check times out | Check `docker-compose logs localstack` |

### Pre-Flight Check (Run First)

```bash
# Verify prerequisites before starting
docker info > /dev/null 2>&1 && echo "Docker: OK" || echo "Docker: NOT RUNNING"
command -v aws > /dev/null && echo "AWS CLI: OK" || echo "AWS CLI: NOT INSTALLED"
lsof -i :4566 > /dev/null 2>&1 && echo "Port 4566: IN USE" || echo "Port 4566: OK"
lsof -i :6379 > /dev/null 2>&1 && echo "Port 6379: IN USE" || echo "Port 6379: OK"
```

### Time Estimate

- File writes: ~2 minutes
- Docker image pull (first time): ~3-5 minutes
- LocalStack initialization: ~30 seconds
- Verification: ~10 seconds
- **Total:** ~5-8 minutes (first run), ~2 minutes (subsequent)

---

## Notes

- **PERSISTENCE=1** in LocalStack means data survives container restart. Use
  `docker-compose down -v` to truly reset.

- **S3 event notifications** are configured to trigger the validate queue when
  `.mp4` files are uploaded. This enables the "bytes land, pipeline starts"
  pattern without API involvement.

- **CORS on HLS bucket** is required for browser-based HLS players. ExoPlayer
  (Android native) doesn't need it, but VLC/browser testing does.

- **PAY_PER_REQUEST** billing mode for DynamoDB means no capacity provisioning
  needed. LocalStack doesn't enforce this anyway, but it matches production config.
