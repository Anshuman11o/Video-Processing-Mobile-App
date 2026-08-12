# Models Package

This package contains the core data models for the DayReel video processing pipeline.

## Overview

The models package defines the data structures used throughout the backend, including:

- **Job**: The main entity representing a video processing job stored in DynamoDB
- **Stage states**: Tracking information for each processing stage
- **Upload/Output info**: Metadata about uploads and processed outputs
- **Metrics**: Performance timing data

## Key Types

### Job

The `Job` struct is the primary entity, stored in DynamoDB with:
- `PK`: Partition key in format `JOB#<job_id>`
- `SK`: Sort key, always `METADATA` for the main job record
- `Stages`: Map of stage names to their current state

### Constants

- `JobStatus`: Overall job status (pending, uploading, processing, completed, failed)
- `StageStatus`: Individual stage status (pending, running, completed, failed)
- `StageName`: Processing stage names (validate, extract, transcribe, package)

## Usage

```go
// Create a new job
job := models.NewJob("video.mp4", 50*1024*1024, "video/mp4")

// Update stage status
job.SetStageRunning(models.StageValidate)
job.SetStageCompleted(models.StageValidate, "processed/video.mp4")
```

## DynamoDB Schema

All structs use both `json` and `dynamodbav` struct tags for proper serialization to both JSON APIs and DynamoDB.
