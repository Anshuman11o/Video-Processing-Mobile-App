# Events Package

This package contains stage message types and resource constants for the DayReel video processing pipeline.

## Overview

The events package defines:

- **StageMessage**: The message format sent between queues to trigger processing stages
- **S3Ref**: Reference to an S3 object (bucket + key)
- **Queue constants**: queue names for each processing stage
- **Bucket constants**: S3 bucket names for different storage tiers
- **ExtractManifest**: What the extract stage records about a clip, read by transcribe and package

The transport is the local SQLite queue in `backend/internal/queue/`, not SQS.
The queue names below are values of its `queue` column, not AWS resources, and a
`StageMessage` is stored as an opaque JSON body.

## Queue Names

| Constant | Value | Purpose |
|----------|-------|---------|
| `QueueValidate` | `dayreel-validate` | Video validation stage |
| `QueueExtract` | `dayreel-extract` | Audio/metadata extraction |
| `QueueTranscribe` | `dayreel-transcribe` | Speech-to-text transcription |
| `QueuePackage` | `dayreel-package` | HLS packaging stage |
| `QueueDLQ` | `dayreel-dlq` | Dead letter queue for failed messages |

## Bucket Names

| Constant | Value | Purpose |
|----------|-------|---------|
| `BucketRawVideos` | `dayreel-raw-videos` | Original uploaded videos |
| `BucketProcessed` | `dayreel-processed` | Intermediate processing artifacts |
| `BucketHLSOutput` | `dayreel-hls-output` | Final HLS output for streaming |

## Usage

```go
// Create a stage message
msg := events.NewStageMessage(
    jobID,
    models.StageValidate,
    events.S3Ref{Bucket: events.BucketRawVideos, Key: "uploads/video.mp4"},
    1,
    traceID,
)

// Get the next queue in the pipeline
nextQueue := events.NextQueue(models.StageValidate) // Returns QueueExtract
```

## Pipeline Flow

1. Upload complete -> `QueueValidate`
2. Validation complete -> `QueueExtract`
3. Extraction complete -> `QueueTranscribe`
4. Transcription complete -> `QueuePackage`
5. Packaging complete -> Job done

Step 1 is `POST /jobs/{id}/complete` publishing the message itself. It used to
be an S3 `ObjectCreated` notification, which is why the package once carried a
`NormalizeMessage` that could parse an S3 event envelope; real S3 cannot notify a
SQLite file, so that path — and the code for it — is gone.

Failed messages go to `QueueDLQ`, put there by the worker rather than by a
redrive policy. See `internal/worker/runner.go` for when it decides to.
