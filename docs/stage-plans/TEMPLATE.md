# Stage N: [Stage Name]

> Write this plan immediately before starting the stage. Get it reviewed before
> writing code.

## Aim

_One sentence: what this stage achieves._

## Components

_Exact services, modules, or directories this stage touches._

| Component | Action |
|-----------|--------|
| `backend/cmd/api/` | Create |
| `infra/docker-compose.yml` | Modify |

## Boundaries

_Input/output at each boundary, with exact data shapes._

### API Boundary (if applicable)

**Request:**
```
POST /jobs
Content-Type: application/json

{
  "filename": "clip.mp4",
  "size_bytes": 15000000
}
```

**Response:**
```json
{
  "job_id": "uuid",
  "upload_id": "s3-multipart-id",
  "upload_urls": ["presigned-url-part-1", "presigned-url-part-2"],
  "part_size": 5242880
}
```

### SQS Message (if applicable)

```json
{
  "job_id": "uuid",
  "stage": "validate",
  "input_key": "raw-videos/uuid/input.mp4",
  "attempt": 1,
  "timestamp": "2024-01-15T10:30:00Z"
}
```

### S3 Objects (if applicable)

| Bucket | Key Pattern | Content |
|--------|-------------|---------|
| `raw-videos` | `{job_id}/input.mp4` | Original upload |
| `processed` | `{job_id}/validated.mp4` | Normalized MP4 |

### DynamoDB Item (if applicable)

```json
{
  "pk": "JOB#uuid",
  "sk": "JOB#uuid",
  "job_id": "uuid",
  "filename": "clip.mp4",
  "created_at": "2024-01-15T10:30:00Z",
  "stages": {
    "validate": {
      "status": "pending|processing|complete|failed",
      "started_at": "...",
      "completed_at": "...",
      "attempts": 0,
      "error": null
    }
  }
}
```

## Files

_Files to create or modify, with brief description._

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/api/handlers.go` | Create | HTTP handlers |
| `backend/internal/models/job.go` | Create | Job struct and DynamoDB ops |
| `infra/docker-compose.yml` | Modify | Add API service |

## Tasks

_Ordered implementation steps. Check off as you complete._

1. [ ] Define DynamoDB table schema in `init-aws.sh`
2. [ ] Create Job model with marshal/unmarshal
3. [ ] Implement CreateJob handler
4. [ ] Implement GetJob handler
5. [ ] Wire up routes in main.go
6. [ ] Add API service to docker-compose.yml
7. [ ] Write integration test

## Test

_The specific test that proves this stage works._

```bash
# Start services
docker-compose up -d

# Create a job
JOB_ID=$(curl -s -X POST localhost:8080/jobs \
  -H "Content-Type: application/json" \
  -d '{"filename":"test.mp4","size_bytes":1000000}' \
  | jq -r '.job_id')

# Verify job exists
curl -s localhost:8080/jobs/$JOB_ID | jq .

# Expected: job_id matches, status is "pending"
```

## Verification

_What you can observe to confirm the stage works. Be specific._

- [ ] `docker-compose up` starts without errors
- [ ] `curl POST /jobs` returns 201 with job_id and presigned URLs
- [ ] `curl GET /jobs/{id}` returns the job with pending status
- [ ] DynamoDB scan shows the job item
- [ ] Logs show no errors

## Notes

_Any non-obvious decisions, gotchas, or deferred items._

- We're using single-table design for DynamoDB; all entities share the jobs table
- Presigned URLs expire in 1 hour; client must refresh if upload takes longer
