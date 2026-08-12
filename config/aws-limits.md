# AWS Service Limits and Constraints

Reference for constraints that bind on this project. Gathered before building
against them.

---

## S3

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Minimum part size (multipart) | 5 MB | Parts must be >= 5MB except the last |
| Maximum part size | 5 GB | Not a concern for short clips |
| Maximum parts per upload | 10,000 | 10,000 * 5MB = 50TB max, not a concern |
| Maximum object size | 5 TB | Not a concern |
| Presigned URL expiration | 7 days max | We use 1 hour; client refreshes if needed |
| Bucket name length | 3-63 chars | Use short, descriptive names |

**Multipart upload flow:**
1. `CreateMultipartUpload` → returns `UploadId`
2. `UploadPart` for each part → returns `ETag`
3. `CompleteMultipartUpload` with all ETags → S3 stitches parts

**Resume mechanism:** Persist `UploadId` and part ETags locally. On resume, continue
from last successful part.

---

## SQS

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Message size | 256 KB max | Messages carry S3 pointers, not payloads; ~300 bytes typical |
| Visibility timeout | 0s - 12 hours | Set to 5 minutes; workers extend if needed |
| Message retention | 1 minute - 14 days | Default 4 days is fine |
| Receive batch size | 1-10 messages | Use 1 for simplicity |
| DLQ redrive | After N failed receives | Set maxReceiveCount=3 |

**At-least-once delivery:** Workers must be idempotent. Check if output S3 key
exists before processing.

**Error handling:**
- Transient errors: Don't delete message, let visibility timeout expire, retry
- Permanent errors: Send to DLQ, delete original message

---

## DynamoDB

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Item size | 400 KB max | Job item with stages map is ~2KB; not a concern |
| Partition key | Required | Use `job_id` as PK |
| Sort key | Optional | Not needed; one item per job |
| RCU (read capacity unit) | 1 RCU = 4KB strongly consistent | Status polls are cheap |
| WCU (write capacity unit) | 1 WCU = 1KB | Stage updates are small |

**Single-table design:** All job data in one item. The `stages` map contains
per-stage state. One `GetItem` returns full job status.

**Access patterns:**
1. Get job by ID → `GetItem(pk=job_id)`
2. Update stage status → `UpdateItem` with SET on `stages.{stage}`
3. List recent jobs → Consider GSI on `created_at` if needed (defer)

---

## Lambda (if used for workers)

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Payload size (sync) | 6 MB | Not using sync invoke |
| Payload size (async) | 256 KB | SQS trigger, same as SQS limit |
| Execution timeout | 15 minutes max | Transcription of 60s clip may hit this |
| Memory | 128 MB - 10 GB | FFmpeg needs ~1GB, Whisper needs ~2GB |
| Ephemeral storage | 512 MB - 10 GB | Enough for video processing |
| Concurrent executions | 1000 default | Not a concern at demo scale |

**Decision:** Use ECS Fargate instead of Lambda for workers. Better for long-running
FFmpeg/Whisper tasks, easier local dev parity.

---

## ECS Fargate

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Task CPU | 0.25 - 4 vCPU | Use 1 vCPU for workers |
| Task memory | 0.5 - 30 GB | Use 2GB for transcribe worker |
| Ephemeral storage | 20 - 200 GB | Default 20GB is enough |
| Task startup time | 30-60 seconds | Cold start latency, acceptable |

**Sizing per worker:**
| Worker | CPU | Memory | Reason |
|--------|-----|--------|--------|
| Validate | 0.5 | 1 GB | FFprobe + remux is light |
| Extract | 1 | 2 GB | FFmpeg keyframe extraction |
| Transcribe | 1 | 4 GB | faster-whisper (CPU mode) |
| Package | 1 | 2 GB | FFmpeg HLS encoding |

---

## CloudFront

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Origin response timeout | 30 seconds | HLS segments are small, not a concern |
| Cache TTL | Configurable | Use long TTL for immutable HLS segments |
| Price class | All edge locations vs subset | Use PriceClass_100 (cheapest) |

**HLS caching:** Segments are immutable after creation. Use aggressive caching.
Master playlist may need shorter TTL if we ever update it.

---

## Redis (ElastiCache)

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Max item size | 512 MB | Job status is ~1KB; not a concern |
| Max connections | Varies by instance | Not a concern at demo scale |

**Usage:** Read-through cache for job status. TTL of 10 seconds. Reduces DynamoDB
reads on frequent status polls.

```
GET job:{id}:status
  → cache hit: return cached
  → cache miss: GetItem from DynamoDB, cache with TTL, return
```
