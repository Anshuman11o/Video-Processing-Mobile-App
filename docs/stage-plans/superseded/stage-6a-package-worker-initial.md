# Stage 6A: Package Worker

> **Depends on:** Stage 5A (Transcribe), Stage 4A (Extract), Stage 3A (worker harness)
> **Run in parallel with:** Stage 2B (Mobile Shell)
> **Estimated time:** 30 minutes
> **Blocks:** Stage 8B (HLS playback), Stage 10 (Terraform)
> **Milestone: this stage completes the backend E2E. Video in → playable reel out.**

## Status of This Plan

**Initial plan — written ahead of Stages 3A, 4A and 5A.** Worker-runtime
assumptions carry over from Stage 4A's plan; stage-specific dependencies on 4A's
`extract.json` and 5A's `transcript.vtt` are called out in
[Open Items](#open-items-to-confirm-after-stages-3a-4a-and-5a).

The HLS design here — ladder, segmentation, master-playlist rewriting, bucket
policy — is self-contained and should be stable.

---

## Aim

Transcode `validated.mp4` into a **3-tier adaptive-bitrate HLS ladder** with
6-second segments, attach the WebVTT transcript as a subtitle rendition, publish
a thumbnail, and mark the job **completed** with a playable URL — the value the
whole pipeline exists to produce.

---

## Components Touched

| Component | Action | Files |
|-----------|--------|-------|
| `backend/internal/worker/package/` | Create | `package.go`, `ladder.go`, `*_test.go`, `CONTEXT.md` |
| `backend/internal/hls/` | Create | `master.go`, `subtitles.go`, `*_test.go` |
| `backend/internal/media/ffmpeg.go` | Modify | Add `PackageHLS` |
| `backend/internal/db/dynamodb.go` | Modify | Add `SetJobOutput` (output + status + metrics, one update) |
| `backend/internal/config/config.go` | Modify | HLS tunables + public base URL |
| `backend/cmd/worker/main.go` | Modify | Register the package handler |
| `infra/localstack/init-aws.sh` | Modify | **Public-read bucket policy on `dayreel-hls-output`** |
| `infra/docker-compose.yml` | Modify | `worker-package` service |
| `.env.example` | Modify | New env vars |

---

## Boundaries

### Input: SQS message on `dayreel-package`

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "stage": "package",
  "input": {
    "bucket": "dayreel-processed",
    "key": "550e8400-e29b-41d4-a716-446655440000/validated.mp4"
  },
  "attempt": 1,
  "timestamp": "2026-01-15T10:31:29Z",
  "trace_id": "abc123"
}
```

### Derived inputs

| Key (bucket `dayreel-processed`) | Source | Used for |
|---|---|---|
| `{job_id}/extract.json` | Stage 4A | `width`, `height`, `duration_seconds`, `has_audio`, `frames[0].key` |
| `{job_id}/transcript.vtt` | Stage 5A | Subtitle rendition (skipped when it has no cues) |

### Output: S3 objects — bucket `dayreel-hls-output`

```
{job_id}/
  master.m3u8                     application/vnd.apple.mpegurl
  720p/playlist.m3u8              application/vnd.apple.mpegurl
  720p/segment_000.ts ...         video/mp2t
  480p/playlist.m3u8
  480p/segment_000.ts ...
  360p/playlist.m3u8
  360p/segment_000.ts ...
  subs/playlist.m3u8              application/vnd.apple.mpegurl
  subtitles.vtt                   text/vtt
  thumbnail.jpg                   image/jpeg
```

**Content types are not cosmetic here.** ExoPlayer and most browser players
sniff the response `Content-Type` to pick a demuxer; an `.m3u8` served as
`binary/octet-stream` (S3's default when unset) is a common cause of "playback
fails, no useful error."

`master.m3u8` is written **last** and is the idempotency sentinel.

### DynamoDB write on completion

One `UpdateItem` sets everything:

```json
{
  "status": "completed",
  "stages": {
    "package": {
      "status": "completed",
      "completed_at": "2026-01-15T10:33:12Z",
      "output_key": "550e8400-.../master.m3u8"
    }
  },
  "output": {
    "hls_url": "http://localhost:4566/dayreel-hls-output/550e8400-.../master.m3u8",
    "duration_seconds": 42.517,
    "thumbnail_url": "http://localhost:4566/dayreel-hls-output/550e8400-.../thumbnail.jpg"
  },
  "metrics": {
    "package_duration_ms": 41200,
    "total_processing_ms": 98430
  }
}
```

These field names come straight from `models.OutputInfo` and are what
`GET /jobs/{id}/reel` already reads — `handlers.go` returns 409 until
`status == completed` **and** `output != nil`, so both must be set in the same
update or a poller can catch an inconsistent state.

`total_processing_ms` = package completion − `job.created_at`. This is the p95
E2E latency metric from the project plan; **this stage is the only place it can
be computed**, so it must not be skipped.

### Terminal stage

No next queue. `events.NextQueue(StagePackage)` already returns `""`.

---

## HLS Ladder

| Rendition | Target box | Video bitrate | maxrate | bufsize | Audio |
|-----------|-----------|---------------|---------|---------|-------|
| 720p | 1280×720 | 2500k | 2675k | 3750k | 128k |
| 480p | 854×480 | 1200k | 1285k | 1800k | 128k |
| 360p | 640×360 | 600k | 642k | 900k | 96k |

### Rendition selection — never upscale

A rendition is included when the **source long edge ≥ the rendition's long
edge** (1280 / 854 / 640). The smallest rendition is always included regardless,
so every job produces at least one playable variant.

Scaling uses a *box fit*, not a fixed height:

```
scale=w=1280:h=720:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2
```

**This matters because the source is a phone video.** A 1080×1920 portrait clip
has a long edge of 1920, so all three tiers apply, and the box fit yields
404×720 / 270×480 / 202×360 — correct. A height-based rule
(`scale=-2:720`) would produce a 405-pixel-wide "720p" for portrait and a
wildly wrong ladder. The second `scale` forces even dimensions, which libx264
requires.

### FFmpeg invocation (all three tiers, with audio)

```bash
mkdir -p "{tmp}/out/720p" "{tmp}/out/480p" "{tmp}/out/360p"

ffmpeg -hide_banner -nostdin -y -i "{tmp}/validated.mp4" \
  -filter_complex "\
[0:v]split=3[v1][v2][v3]; \
[v1]scale=w=1280:h=720:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2[v1out]; \
[v2]scale=w=854:h=480:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2[v2out]; \
[v3]scale=w=640:h=360:force_original_aspect_ratio=decrease,scale=trunc(iw/2)*2:trunc(ih/2)*2[v3out]" \
  -map "[v1out]" -c:v:0 libx264 -b:v:0 2500k -maxrate:v:0 2675k -bufsize:v:0 3750k \
  -map "[v2out]" -c:v:1 libx264 -b:v:1 1200k -maxrate:v:1 1285k -bufsize:v:1 1800k \
  -map "[v3out]" -c:v:2 libx264 -b:v:2 600k  -maxrate:v:2 642k  -bufsize:v:2 900k \
  -map a:0 -map a:0 -map a:0 -c:a aac -b:a 128k -ac 2 -ar 48000 \
  -preset veryfast -profile:v main -level 4.0 -pix_fmt yuv420p \
  -force_key_frames "expr:gte(t,n_forced*6)" -sc_threshold 0 \
  -muxdelay 0 -muxpreload 0 \
  -f hls -hls_time 6 -hls_playlist_type vod -hls_flags independent_segments \
  -hls_segment_type mpegts \
  -hls_segment_filename "{tmp}/out/%v/segment_%03d.ts" \
  -master_pl_name master.m3u8 \
  -var_stream_map "v:0,a:0,name:720p v:1,a:1,name:480p v:2,a:2,name:360p" \
  "{tmp}/out/%v/playlist.m3u8"
```

Key choices:

- **`-force_key_frames "expr:gte(t,n_forced*6)"` rather than `-g N`.** A fixed
  GOP only aligns with 6-second segments at one specific frame rate; the
  expression is frame-rate independent, which matters when phone footage arrives
  at 29.97 or variable fps. Without aligned keyframes, players stall or
  mis-seek at rendition switches.
- **`-sc_threshold 0`** stops libx264 inserting its own keyframes on scene
  changes, which would desynchronize segment boundaries across renditions.
- **`independent_segments`** tells the player every segment starts with a
  keyframe — required for clean ABR switching.
- **`-preset veryfast`** is a deliberate quality-for-time trade; this is a demo
  pipeline with a 20-minute stage budget.
- **`-muxdelay 0 -muxpreload 0`** forces the MPEG-TS PTS to start at 0, which is
  what makes the subtitle timestamp map below predictable.
- **Directories must be pre-created** — the HLS muxer will not create the `%v`
  subdirectories itself.

### No-audio variant

When `extract.json` reports `has_audio: false`, drop the three `-map a:0` flags,
the audio codec flags, and rewrite the stream map as
`"v:0,name:720p v:1,name:480p v:2,name:360p"`. The ladder builder returns both
the filter graph and the stream map so this stays one code path with one
conditional, not two copies of a 20-line command.

---

## Subtitles

FFmpeg's `var_stream_map` can emit a subtitle group, but the result is
inconsistent across versions and awkward to control. Cheaper and more
predictable: let ffmpeg write the master playlist, then **rewrite it in Go**.

### 1. `subtitles.vtt`

A copy of `transcript.vtt` with a timestamp map inserted after the `WEBVTT`
line:

```
WEBVTT
X-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000

1
00:00:00.000 --> 00:00:04.120
...
```

**Why:** WebVTT cue times are relative to the VTT's own zero, while MPEG-TS
segments carry their own PTS timeline. `X-TIMESTAMP-MAP` ties the two together.
Get it wrong and captions appear correctly formatted but offset by seconds —
the classic symptom of omitting it entirely (ffmpeg's mpegts muxer defaults to a
1.4 s initial PTS, and Apple's tooling conventionally starts at 10 s / 900000).

`-muxdelay 0 -muxpreload 0` above should make `MPEGTS:0` correct. **Verify, do
not assume:**

```bash
ffprobe -v error -select_streams v:0 -show_entries packet=pts_time \
  -read_intervals "%+#1" -of csv=p=0 "{tmp}/out/720p/segment_000.ts"
```

If the first PTS is not ~0, set `MPEGTS:` to `round(first_pts × 90000)`. Compute
it in code from that probe rather than hardcoding — it costs one ffprobe call
and removes a whole class of "captions are 1.4 seconds early" bugs.

### 2. `subs/playlist.m3u8`

```
#EXTM3U
#EXT-X-VERSION:3
#EXT-X-TARGETDURATION:43
#EXT-X-MEDIA-SEQUENCE:0
#EXT-X-PLAYLIST-TYPE:VOD
#EXTINF:42.517,
../subtitles.vtt
#EXT-X-ENDLIST
```

`TARGETDURATION` = `ceil(duration_seconds)`. The whole transcript is one
"segment" — correct for VOD clips of this length.

### 3. Master playlist rewrite

Insert after the header lines:

```
#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",NAME="English",LANGUAGE="en",DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,URI="subs/playlist.m3u8"
```

and append `,SUBTITLES="subs"` to every `#EXT-X-STREAM-INF:` line.

```go
package hls

// InjectSubtitles adds a subtitle rendition to an ffmpeg-generated master playlist.
// Returns the input unchanged when opts.CueCount == 0.
func InjectSubtitles(master string, opts SubtitleOptions) (string, error)

// SubtitlePlaylist renders the single-entry VOD playlist for a WebVTT file.
func SubtitlePlaylist(durationSeconds float64, vttPath string) string
```

Pure string transforms over a fixed input format — fully unit-testable with a
golden master playlist fixture and no Docker.

### Empty-transcript rule

When `transcript.vtt` has **zero cues** (5A's no-audio or silent-clip path),
skip all three subtitle steps: no `subtitles.vtt`, no `subs/playlist.m3u8`, no
`EXT-X-MEDIA` line. A subtitle track advertised in the master that resolves to
nothing shows an enabled "English" caption option that renders nothing —
visibly worse than offering no captions. **This is the hard dependency carried
over from Stage 5A.**

---

## Thumbnail

Server-side `CopyObject` from `dayreel-processed/{frames[0].key}` to
`dayreel-hls-output/{job_id}/thumbnail.jpg`, with `ContentType: image/jpeg` and
`MetadataDirective: REPLACE`.

Copying (rather than downloading and re-uploading) keeps the bytes inside S3.
Putting it in the HLS bucket matters because **`dayreel-processed` expires after
7 days** while `dayreel-hls-output` is kept indefinitely (Stage 1A lifecycle) —
a thumbnail URL pointing into the processed bucket would break exactly one week
after every job.

---

## Public Read Access

`GET /jobs/{id}/reel` hands the mobile app a plain URL, and HLS playlists
reference their segments by **relative** path. Presigned URLs cannot work here —
the player would need a signature per segment, and the playlist has none.

So `dayreel-hls-output` must be publicly readable. Add to `init-aws.sh`:

```bash
awslocal s3api put-bucket-policy --bucket dayreel-hls-output --policy '{
  "Version": "2012-10-17",
  "Statement": [{
    "Sid": "PublicReadHLS",
    "Effect": "Allow",
    "Principal": "*",
    "Action": "s3:GetObject",
    "Resource": "arn:aws:s3:::dayreel-hls-output/*"
  }]
}'
```

CORS is already configured on this bucket (`GET`/`HEAD`, all origins) from
Stage 1B.

**On AWS this becomes CloudFront with Origin Access Control and a private
bucket** — the public policy is a local-development shortcut only, and Stage 10
must not carry it forward.

### The playback URL

```
{HLS_PUBLIC_BASE_URL}/{bucket}/{job_id}/master.m3u8
```

| Environment | `HLS_PUBLIC_BASE_URL` |
|---|---|
| Host machine / curl / ffplay | `http://localhost:4566` |
| **Android emulator** | `http://10.0.2.2:4566` |
| Physical device on LAN | `http://{host-lan-ip}:4566` |
| AWS | `https://{cloudfront_domain}` (bucket segment dropped) |

**The emulator case is the one that will bite.** `localhost` inside the emulator
is the emulator, not the host — a URL that plays perfectly in `ffplay` on the
dev machine will fail silently in the app. Set this env var per environment;
Stage 8B depends on getting it right.

---

## Processing Logic

1. **Idempotency:** `HeadObject` on `dayreel-hls-output/{job_id}/master.m3u8` →
   already packaged; refresh DynamoDB output/status (cheap, and repairs a crash
   between upload and DB write), delete the message, return.
2. **Read** `extract.json`; **download** `validated.mp4` and `transcript.vtt`.
   A missing transcript is tolerated as zero cues + a logged warning; a missing
   video is a permanent error.
3. **Build the ladder** from `width`/`height` and `has_audio`.
4. **Run ffmpeg.**
5. **Probe segment 0** for the initial PTS → timestamp map value.
6. **Write subtitle artifacts** (skipped when there are no cues).
7. **Rewrite the master playlist.**
8. **Upload everything** with correct content types, bounded concurrency
   (`PACKAGE_UPLOAD_CONCURRENCY`, default 8): segments and variant playlists
   first, subtitles and thumbnail next, `master.m3u8` **last**.
9. **Copy the thumbnail.**
10. **Finalize:** one `SetJobOutput` write (status + output + package/total
    metrics), invalidate Redis `job:{job_id}`, delete the SQS message, remove
    the temp directory.

### Config additions

```go
HLSPublicBaseURL         string // HLS_PUBLIC_BASE_URL, default "http://localhost:4566"
HLSSegmentSeconds        int    // HLS_SEGMENT_SECONDS, default 6
HLSPreset                string // HLS_PRESET, default "veryfast"
PackageUploadConcurrency int    // PACKAGE_UPLOAD_CONCURRENCY, default 8
PackageTimeoutSeconds    int    // PACKAGE_TIMEOUT_SECONDS, default 900
```

---

## Failure Model

| Condition | Classification | Behavior |
|-----------|----------------|----------|
| `validated.mp4` missing | Permanent | Fail stage, job `failed` |
| `extract.json` missing | Permanent | Fail stage (needed for dimensions) |
| `transcript.vtt` missing | Degrade | Package without subtitles, log warning |
| ffmpeg non-zero exit | Permanent | Fail stage; capture last 50 stderr lines into `stages.package.error` |
| ffmpeg exceeds `PACKAGE_TIMEOUT_SECONDS` | Transient | Kill, return error, redeliver |
| Partial upload then crash | Safe | `master.m3u8` absent ⇒ next attempt reprocesses; orphaned segments are overwritten |
| S3 / DynamoDB errors | Transient | Redeliver (3 receives → DLQ) |

**Transcoding is the longest operation in the pipeline** — a 60 s 1080p clip at
`veryfast` across three tiers can run 60–120 s on a laptop CPU. The 300 s queue
visibility timeout is *not* comfortable headroom here; the harness's visibility
heartbeat is mandatory for this stage, and `PACKAGE_TIMEOUT_SECONDS` (900)
exists to kill a runaway before SQS gives up on it.

---

## Docker Compose Addition

```yaml
  worker-package:
    build:
      context: ../backend
      dockerfile: Dockerfile.worker
    container_name: dayreel-worker-package
    environment:
      - STAGE=package
      - AWS_REGION=us-east-1
      - AWS_ACCESS_KEY_ID=test
      - AWS_SECRET_ACCESS_KEY=test
      - LOCALSTACK_ENDPOINT=http://localstack:4566
      - USE_LOCALSTACK=true
      - S3_PROCESSED_BUCKET=dayreel-processed
      - S3_HLS_BUCKET=dayreel-hls-output
      - DYNAMODB_TABLE=dayreel-jobs
      - REDIS_URL=redis:6379
      - HLS_PUBLIC_BASE_URL=http://localhost:4566
      - HLS_SEGMENT_SECONDS=6
      - WORKER_CONCURRENCY=1
      - WORKER_TMP_DIR=/tmp/dayreel
    depends_on:
      localstack:
        condition: service_healthy
      redis:
        condition: service_healthy
    networks:
      - dayreel-network
```

`WORKER_CONCURRENCY=1` — three simultaneous libx264 encodes already saturate the
available cores; more in-process concurrency just makes every job slower.

---

## Tasks

1. [ ] Create `internal/hls/master.go` — `InjectSubtitles`, `SubtitlePlaylist`
2. [ ] Write `internal/hls/master_test.go` with a golden master-playlist fixture
3. [ ] Create `internal/worker/package/ladder.go` — rendition selection, filter graph, `var_stream_map`
4. [ ] Write `ladder_test.go` — landscape/portrait/small-source/no-audio cases
5. [ ] Add `PackageHLS` and `ProbeFirstPTS` to `internal/media/ffmpeg.go`
6. [ ] Create `internal/worker/package/package.go` — the stage handler
7. [ ] Add `SetJobOutput` to `internal/db/dynamodb.go` (status + output + metrics in one `UpdateItem`)
8. [ ] Add `CopyObject` to `internal/storage/s3.go`
9. [ ] Add content-type mapping by extension on upload
10. [ ] Add HLS config to `config.go` and `.env.example`
11. [ ] Register the handler in `cmd/worker/main.go`
12. [ ] Add the public-read bucket policy to `infra/localstack/init-aws.sh`
13. [ ] Add `worker-package` to `infra/docker-compose.yml`
14. [ ] Run the E2E test; play the reel
15. [ ] Create `internal/worker/package/CONTEXT.md`; update `backend/CONTEXT.md`
16. [ ] Append anything non-trivial to `TROUBLESHOOTING.md`

---

## Test

```bash
cd infra && docker compose up -d --build worker-package
# Re-run init if the bucket policy is new:
docker compose exec localstack bash /etc/localstack/init/ready.d/init-aws.sh

JOB_ID="test-package-$(date +%s)"
AWS="aws --endpoint-url=http://localhost:4566"
BASE="http://localhost:4566/dayreel-hls-output/${JOB_ID}"

# Seed the outputs of stages 3A/4A/5A
$AWS s3 cp ./testdata/sample-720p.mp4 "s3://dayreel-processed/${JOB_ID}/validated.mp4"
printf 'WEBVTT\n\n1\n00:00:00.000 --> 00:00:05.000\nHello from DayReel.\n' > /tmp/t.vtt
$AWS s3 cp /tmp/t.vtt "s3://dayreel-processed/${JOB_ID}/transcript.vtt"
$AWS s3 cp ./testdata/frame.jpg "s3://dayreel-processed/${JOB_ID}/frames/frame_001.jpg"
cat > /tmp/extract.json <<EOF
{"job_id":"${JOB_ID}","duration_seconds":30.0,"width":1280,"height":720,
 "has_audio":true,"frame_count":1,
 "frames":[{"key":"${JOB_ID}/frames/frame_001.jpg","timestamp_seconds":0.0}]}
EOF
$AWS s3 cp /tmp/extract.json "s3://dayreel-processed/${JOB_ID}/extract.json"

$AWS sqs send-message \
  --queue-url http://localhost:4566/000000000000/dayreel-package \
  --message-body "{\"job_id\":\"${JOB_ID}\",\"stage\":\"package\",\"input\":{\"bucket\":\"dayreel-processed\",\"key\":\"${JOB_ID}/validated.mp4\"},\"attempt\":1,\"timestamp\":\"$(date -u +%FT%TZ)\",\"trace_id\":\"manual\"}"

sleep 90

# --- Structure ---
$AWS s3 ls --recursive "s3://dayreel-hls-output/${JOB_ID}/"

# --- Master playlist over plain HTTP (proves the public policy works) ---
curl -sf "${BASE}/master.m3u8"
# Expect: 3 × EXT-X-STREAM-INF, each carrying SUBTITLES="subs",
#         and one EXT-X-MEDIA:TYPE=SUBTITLES line

# --- Content types ---
curl -sI "${BASE}/master.m3u8"       | grep -i content-type  # application/vnd.apple.mpegurl
curl -sI "${BASE}/720p/segment_000.ts" | grep -i content-type  # video/mp2t
curl -sI "${BASE}/subtitles.vtt"     | grep -i content-type  # text/vtt

# --- Playability ---
ffprobe -v error -show_entries format=format_name,duration -of default=nw=1 "${BASE}/master.m3u8"
ffplay "${BASE}/master.m3u8"          # optional, visual check

# --- Segment duration ~6s ---
ffprobe -v error -show_entries format=duration -of default=nw=1:nk=1 "${BASE}/720p/segment_000.ts"

# --- Keyframe alignment across renditions ---
for R in 720p 480p 360p; do
  echo -n "$R first PTS: "
  ffprobe -v error -select_streams v:0 -show_entries packet=pts_time \
    -read_intervals "%+#1" -of csv=p=0 "${BASE}/${R}/segment_000.ts"
done

# --- Job state ---
curl -s "http://localhost:8080/jobs/${JOB_ID}" | jq '.status, .stages.package, .output, .metrics'
curl -s "http://localhost:8080/jobs/${JOB_ID}/reel" | jq .
# Expect 200 with hls_url and thumbnail_url
```

### Full-pipeline test (the actual milestone)

Once 3A–6A are all running, this is the E2E that closes out the backend:

```bash
JOB=$(curl -s -X POST http://localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{"filename":"test.mp4","size_bytes":10485760,"content_type":"video/mp4"}')
# ... upload parts to the presigned URLs, POST /jobs/{id}/complete ...
# then poll:
watch -n2 "curl -s http://localhost:8080/jobs/\$JOB_ID | jq '.status, .stages'"
# Expect: validate → extract → transcribe → package, all completed
ffplay "$(curl -s http://localhost:8080/jobs/$JOB_ID/reel | jq -r .hls_url)"
```

---

## Verification Checklist

- [ ] `master.m3u8` fetches over plain HTTP with no credentials
- [ ] Master lists three variants with correct `BANDWIDTH` and `RESOLUTION`
- [ ] Each `EXT-X-STREAM-INF` carries `SUBTITLES="subs"`
- [ ] One `EXT-X-MEDIA:TYPE=SUBTITLES` line pointing at `subs/playlist.m3u8`
- [ ] `subtitles.vtt` contains the `X-TIMESTAMP-MAP` header
- [ ] Segments are ~6 s; `#EXT-X-TARGETDURATION` matches
- [ ] All three renditions share the same first PTS (keyframe-aligned)
- [ ] Content types are correct for `.m3u8`, `.ts`, `.vtt`, `.jpg`
- [ ] `ffplay master.m3u8` plays with captions visible and in sync
- [ ] Captions are **not** offset by a second or more
- [ ] A **portrait** source (1080×1920) produces sensible dimensions, not a squashed ladder
- [ ] A **small** source (640×360) produces only the 360p rendition — no upscaling
- [ ] A **silent** source packages with no subtitle rendition at all
- [ ] `thumbnail.jpg` exists in the HLS bucket and loads
- [ ] `GET /jobs/{id}` shows `status: "completed"` and all four stages completed
- [ ] `GET /jobs/{id}/reel` returns 200 (was 409 before)
- [ ] `metrics.package_duration_ms` and `metrics.total_processing_ms` are populated
- [ ] Redis cache is invalidated (status flips to completed within one poll)
- [ ] Replaying the message skips reprocessing
- [ ] Temp directory is cleaned up
- [ ] Corrupt input lands on `dayreel-dlq` after 3 receives

---

## Claude Code Implementation Plan

### Approach: single agent, pure functions first

The `internal/hls` string transforms and `ladder.go` selection logic are the two
places bugs hide, and both are unit-testable without Docker or ffmpeg. Build and
test those first; the handler is plumbing around them.

### Execution order

```
1. internal/hls/master.go + tests      (Write) — golden-fixture tests, fast loop
2. worker/package/ladder.go + tests    (Write) — portrait/landscape/small/no-audio
3. media/ffmpeg.go: PackageHLS,
   ProbeFirstPTS                       (Edit)
4. storage/s3.go: CopyObject,
   content-type mapping                (Edit)  — parallel with 3
5. db/dynamodb.go: SetJobOutput        (Edit)  — parallel with 3
6. worker/package/package.go handler   (Write)
7. config.go + .env.example            (Edit)  — parallel with 6
8. cmd/worker/main.go registration     (Edit)
9. init-aws.sh bucket policy           (Edit)  — parallel with 8
10. docker-compose.yml                 (Edit)  — parallel with 8
11. docker compose up --build; re-run init  (Bash)
12. E2E: single-stage, then full pipeline   (Bash)
13. CONTEXT.md files; TROUBLESHOOTING.md    (Write)
```

Steps 3/4/5, 6/7 and 8/9/10 are independent writes and can be issued together.

### Why not subagents

Ladder selection, the ffmpeg command, and the master rewrite are three views of
one output format — a wrong assumption in any one shows up as a playback failure
attributable to any of them. Debugging that across contexts costs more than the
parallelism saves. The verification loop (`ffplay`, `ffprobe`, `curl -I`) also
needs a human-readable running commentary in one place.

### Potential blockers

| Blocker | Resolution |
|---------|------------|
| LocalStack `put-bucket-policy` rejected | Community edition supports it; if it fails, set per-object ACL `public-read` on upload as a fallback |
| Playlist 403 over curl | Bucket policy not applied — re-run `init-aws.sh`; the volume mount only runs it on first container start |
| `.m3u8` served as `binary/octet-stream` | Content type not set on `PutObject`; explicit mapping is task 9 |
| Captions offset | `X-TIMESTAMP-MAP` value wrong — probe segment 0's first PTS and compute it |
| ffmpeg output dir errors | Pre-create `out/{720p,480p,360p}`; the HLS muxer does not mkdir |
| Transcode exceeds visibility timeout | Confirm the 3A heartbeat; raise queue visibility to 900 s in `init-aws.sh` if needed |
| Encoding too slow to demo | Drop to two renditions, or `-preset ultrafast`; both are env-tunable |
| Portrait ladder looks wrong | `force_original_aspect_ratio=decrease` box fit — covered by `ladder_test.go` |

### Time estimate

- `internal/hls` + tests: ~8 min
- Ladder + tests: ~6 min
- ffmpeg wiring + handler: ~10 min
- Infra (policy, compose, config): ~4 min
- Build + E2E + playback check: ~12 min
- **Total: ~40 min** (over the 20-minute budget in `PROJECT_PLAN.md`; this is the
  most involved worker and the one the whole demo rests on)

---

## Open Items to Confirm After Stages 3A, 4A and 5A

Everything in Stage 4A's "Open Items" applies (handler interface, error
classification, `internal/queue/sqs.go`, multi-bucket `S3Client`, stage-level
DynamoDB updates, Redis invalidation from workers). Additionally:

1. **`extract.json` field names** (`width`, `height`, `duration_seconds`,
   `has_audio`, `frames[0].key`) — confirm against 4A as built.

2. **Zero-cue transcript detection.** This plan parses `transcript.vtt` and
   counts cues. If 5A's `transcript.json` reliably carries `segment_count`,
   reading that is cheaper and less brittle — decide once 5A lands.

3. **Visibility heartbeat is mandatory here**, not optional. If 3A did not build
   it, this stage must, or raise `VisibilityTimeout` on `dayreel-package` to
   900 s in `init-aws.sh`.

4. **`SetJobOutput` transactionality.** `GET /jobs/{id}/reel` requires
   `status == completed` **and** `output != nil` together. Confirm the update
   expression sets both atomically; two separate `UpdateItem` calls leave a
   window where a polling client sees completed-with-no-output.

5. **Does the worker have a Redis client?** Without cache invalidation the app
   waits up to 10 s to see completion. Tolerable, but it is the last stage
   transition a user actually waits on, so worth fixing here if 3A left it out.

6. **Validate stage's actual normalization.** This plan assumes
   `validated.mp4` is faststart H.264/AAC. If 3A only remuxes without
   transcoding, a HEVC source arrives here and libx264 will still handle it —
   but confirm, because it changes the timing budget substantially.

7. **`total_processing_ms` baseline.** Assumed `package.completed_at −
   job.created_at`, which includes upload time. If the metric should measure
   backend processing only, use `upload.completed_at` instead. The project
   plan's "p95 E2E latency" reads like the former — confirm.

8. **Stage 8B (HLS playback) needs `HLS_PUBLIC_BASE_URL` set to `10.0.2.2` for
   the Android emulator.** Flag it there; a URL that works in `ffplay` will not
   work in the emulator.

9. **Stage 10 (Terraform) must not carry the public bucket policy forward** —
   CloudFront + OAC with a private bucket instead.

---

## Notes

- **`master.m3u8` last, always.** Its presence means the whole reel is durable.
  Any crash mid-upload leaves a job that reprocesses cleanly on redelivery, and
  overwrites orphaned segments in place.

- **Segment length 6 s** is the HLS convention: long enough to keep request
  count and playlist size down, short enough that ABR reacts within a couple of
  seconds on a weak connection — which is the network this app is built for.

- **The subtitle rendition is hand-written rather than ffmpeg-generated**
  because the master rewrite is ~40 lines of pure Go with golden-fixture tests,
  versus fighting version-dependent `var_stream_map` subtitle behavior with a
  60–120 s transcode between each attempt.

- **Public bucket is a local shortcut.** Presigned URLs genuinely cannot work
  for HLS (relative segment URIs carry no signature). CloudFront + OAC is the
  production answer, and it belongs in Stage 10.

- **Cost instrumentation lands here.** Log packaged byte count, segment count,
  and encode wall time on completion — this stage is where the "cost per 100
  clips" metric from `PROJECT_PLAN.md` gets its numbers.

- **Deferred:** fMP4/CMAF segments (needed for HEVC and for sharing segments
  with DASH), per-title encoding, multiple audio tracks, `EXT-X-I-FRAME-STREAM-INF`
  trick-play playlists, and CloudFront cache-control headers.
