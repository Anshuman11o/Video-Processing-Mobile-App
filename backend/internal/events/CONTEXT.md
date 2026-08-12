# Events Package

This package contains SQS message types and AWS resource constants for the DayReel video processing pipeline.

## Overview

The events package defines:

- **StageMessage**: The message format sent between SQS queues to trigger processing stages
- **S3Ref**: Reference to an S3 object (bucket + key)
- **Queue constants**: SQS queue names for each processing stage
- **Bucket constants**: S3 bucket names for different storage tiers

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

Failed messages (after retries) go to `QueueDLQ`.
