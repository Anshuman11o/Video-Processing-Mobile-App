# DayReel Infrastructure

This directory contains the local development infrastructure configuration for the DayReel video processing application.

## Overview

The local development environment uses Docker Compose to run:

- **LocalStack** (v3.0) - AWS service emulator providing S3, SQS, and DynamoDB
- **Redis** (v7 Alpine) - In-memory cache for session management and rate limiting

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                    Local Development                        │
├─────────────────────────────────────────────────────────────┤
│                                                             │
│  ┌─────────────────┐          ┌─────────────────┐          │
│  │   LocalStack    │          │     Redis       │          │
│  │   Port: 4566    │          │   Port: 6379    │          │
│  │                 │          │                 │          │
│  │  - S3 Buckets   │          │  - Sessions     │          │
│  │  - SQS Queues   │          │  - Rate Limits  │          │
│  │  - DynamoDB     │          │  - Cache        │          │
│  └─────────────────┘          └─────────────────┘          │
│                                                             │
└─────────────────────────────────────────────────────────────┘
```

## AWS Resources (LocalStack)

### S3 Buckets

| Bucket | Purpose |
|--------|---------|
| `dayreel-raw-uploads` | Raw video uploads from mobile clients |
| `dayreel-processed-videos` | Processed/compiled videos |
| `dayreel-thumbnails` | Video thumbnail images |

All buckets have CORS configured for web/mobile access.

### SQS Queues

| Queue | Purpose | DLQ |
|-------|---------|-----|
| `dayreel-video-processing` | Video processing job queue | Yes |
| `dayreel-notifications` | Push notification delivery | Yes |

Dead Letter Queues (DLQ) capture failed messages after 3 retry attempts.

### DynamoDB Tables

| Table | Purpose | Keys |
|-------|---------|------|
| `dayreel-videos` | Video metadata and user data | PK/SK with GSI |

Single-table design with:
- Primary Key: `pk` (partition) + `sk` (sort)
- GSI1: `gsi1pk` + `gsi1sk` for access patterns

## Files

| File | Purpose |
|------|---------|
| `docker-compose.yml` | Container orchestration |
| `localstack/init-aws.sh` | AWS resource initialization script |

## Usage

### Quick Start

```bash
# From project root
make dev-up
```

### Commands

```bash
# Start infrastructure
make dev-up

# Stop infrastructure
make dev-down

# View logs
make dev-logs

# Reset all data
make dev-reset

# Test connectivity
make test-infra
```

### Manual AWS CLI Commands

```bash
# List S3 buckets
aws --endpoint-url=http://localhost:4566 s3 ls

# List SQS queues
aws --endpoint-url=http://localhost:4566 sqs list-queues

# List DynamoDB tables
aws --endpoint-url=http://localhost:4566 dynamodb list-tables

# Upload test file
aws --endpoint-url=http://localhost:4566 s3 cp test.mp4 s3://dayreel-raw-uploads/

# Send test SQS message
aws --endpoint-url=http://localhost:4566 sqs send-message \
  --queue-url http://localhost:4566/000000000000/dayreel-video-processing \
  --message-body '{"test": "message"}'
```

## Configuration

### Environment Variables

LocalStack uses these defaults:
- `AWS_ACCESS_KEY_ID=test`
- `AWS_SECRET_ACCESS_KEY=test`
- `AWS_DEFAULT_REGION=us-east-1`

### Ports

| Service | Port |
|---------|------|
| LocalStack | 4566 |
| Redis | 6379 |

## Persistence

- LocalStack data persists in a Docker volume (`localstack-data`)
- Use `make dev-reset` to clear all data

## Troubleshooting

### LocalStack not starting
1. Check Docker is running: `docker info`
2. Check port 4566 is free: `lsof -i :4566`
3. View logs: `make dev-logs`

### Redis connection refused
1. Check container status: `docker ps`
2. Check port 6379 is free: `lsof -i :6379`

### AWS CLI errors
Ensure you're using the LocalStack endpoint:
```bash
aws --endpoint-url=http://localhost:4566 <command>
```
