# AWS Service Limits and Constraints

Reference for constraints that bind on this project. Gathered before building
against them.

---

## S3

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Minimum part size (multipart) | 5 MB | Parts must be >= 5MB except the last. `UPLOAD_PART_SIZE` sits on the floor |
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

**The 5 MiB floor fails late.** A part below the minimum is accepted with a 200
and only rejected at `CompleteMultipartUpload`, with `EntityTooSmall` — so the
whole job dies after every part has apparently succeeded. Test clips must be
larger than 5 MiB to exercise the multipart path at all.

---

## Queue

`QUEUE_DRIVER` picks the broker: `sqlite` (default, `data/queue.db`) or `sqs`.
Both are implemented in `backend/internal/queue/`. Most of these are settings we
choose rather than limits AWS imposes, but they bind the same way: get them wrong
and the pipeline stalls or reprocesses.

| Setting | SQLite | SQS | Impact on DayReel |
|---------|--------|-----|-------------------|
| Message size | No hard limit | 256 KiB per message | Messages carry S3 pointers, not payloads; ~300 bytes typical |
| Visibility timeout | 5 minutes (`QUEUE_VISIBILITY_TIMEOUT`) | Same, but the **queue's own attribute** is what AWS enforces | Must exceed the slowest stage; transcribe is the risk |
| Max deliveries | 3 (`QUEUE_MAX_DELIVERIES`) | Same, plus the redrive policy as a backstop | Then the message moves to `dayreel-dlq` |
| Poll interval | 250ms (`QUEUE_POLL_INTERVAL`) | Not used — long poll, 20s ceiling | On SQLite this is the latency floor per stage |
| Receive batch | Unbounded | 10 messages maximum | The runner claims one at a time either way |
| Send delay | Unbounded | 900s maximum | The runner's backoff ceiling is 5m, well inside it |
| Concurrent claimers | One writer at a time | Unbounded | SQLite serializes writes; fine at this volume |
| Cost | None | Billed per request | An idle SQS worker long-polls once per 20s per stage |

**At-least-once delivery:** Workers must be idempotent. Check if output S3 key
exists before processing. This is unchanged from the SQS design — the guarantee
is the same, and so is the obligation.

**Error handling:**
- Transient errors: don't ack; the visibility timeout expires and the message
  is claimed again on its own
- Permanent errors: nack, which dead-letters once `deliveries` hits the max

**The one that will bite:** a stage that takes longer than the visibility timeout
gets its message re-claimed by another worker while it is still running. Either
raise the timeout above the p99 stage duration or make the stage cheap to repeat.

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
| Payload size (async) | 256 KB | Would need an event source we no longer have |
| Execution timeout | 15 minutes max | Transcription of 60s clip may hit this |
| Memory | 128 MB - 10 GB | FFmpeg needs ~1GB, Whisper needs ~2GB |
| Ephemeral storage | 512 MB - 10 GB | Enough for video processing |
| Concurrent executions | 1000 default | Not a concern at demo scale |

**Decision:** Run the worker as a long-lived process instead of Lambda. Better
for long-running FFmpeg/Whisper tasks, and no 15-minute ceiling.

---

## Worker Hosting

The worker is a plain Go process: `go run ./cmd/worker` locally, the same binary
on a single small VM if we deploy. No containers and no managed container
service — at this volume there is nothing to autoscale, and the binary is
identical either way.

**Sizing per stage:**
| Stage | CPU | Memory | Reason |
|-------|-----|--------|--------|
| Validate | 0.5 | 1 GB | FFprobe + remux is light |
| Extract | 1 | 2 GB | FFmpeg keyframe extraction |
| Transcribe | 1 | 4 GB | faster-whisper (CPU mode) |
| Package | 1 | 2 GB | FFmpeg HLS encoding |

All four stages live in one worker process on one host. Peak is transcribe, so
~2 vCPU / 4 GB covers the whole set as long as stages don't run concurrently on
the same clip. Dropping the containers, the AWS emulator and the cache server
freed 1.5–2.5 GB, which is most of a t3.micro.

---

## HLS Delivery

Reels are served **directly from the HLS bucket** on S3. No CDN.

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Bucket read access | Must be reachable by the player | Bucket policy allows public read on `hls-output` |
| CORS | Required for browser/ExoPlayer range requests | Set once on the bucket, by hand or later by Terraform |
| CORS `ExposeHeaders` | No wildcards allowed | Use `["ETag"]`; `"x-amz-meta-*"` is rejected with `InvalidRequest` |
| Cache-Control | Set per object | Long TTL on immutable segments, short on `master.m3u8` |

**HLS caching:** Segments are immutable after creation, so a long `Cache-Control`
max-age is safe and lets the client do the caching a CDN would otherwise do. The
master playlist gets a shorter TTL in case we ever rewrite it.

---

## Status Cache (in-process, not ElastiCache)

Job status is cached in a map inside the Go API process, with a 10-second TTL.
Not Redis, not ElastiCache: one process reads this cache, the working set is a
few hundred bytes per in-flight job, and a separate cache server costs more RAM
than the whole rest of the local stack.

| Constraint | Value | Impact on DayReel |
|------------|-------|-------------------|
| Entry size | ~1 KB per job | Bounded by in-flight jobs, which is single digits |
| Lifetime | Process lifetime | Restarting the API drops the cache; next poll hits DynamoDB |
| Sharing | None — per process | A second API instance has its own cache and its own 10s of staleness |

**Usage:** Read-through cache for job status. Reduces DynamoDB reads on frequent
status polls. Status polling at 2-second intervals gets ~80% cache hits.

```
get job:{id}
  → cache hit and not expired: return cached
  → otherwise: GetItem from DynamoDB, store with TTL, return
```

**What this costs:** cache coherence across API instances. Two instances can
serve status up to 10 seconds apart. DynamoDB remains the only truth, the TTL is
short, and there is one instance — so this is a real trade, just a cheap one.
