// Package events contains SQS message types and queue/bucket constants for the DayReel pipeline.
package events

import (
	"time"

	"github.com/anshumanagarwal/dayreel/internal/models"
)

// Queue name constants for SQS queues.
const (
	QueueValidate   = "dayreel-validate"
	QueueExtract    = "dayreel-extract"
	QueueTranscribe = "dayreel-transcribe"
	QueuePackage    = "dayreel-package"
	QueueDLQ        = "dayreel-dlq"
)

// Bucket name constants for S3 buckets.
const (
	BucketRawVideos = "dayreel-raw-videos"
	BucketProcessed = "dayreel-processed"
	BucketHLSOutput = "dayreel-hls-output"
)

// S3Ref represents a reference to an S3 object.
type S3Ref struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
}

// StageMessage represents a message sent to an SQS queue to trigger a processing stage.
type StageMessage struct {
	JobID     string           `json:"job_id"`
	Stage     models.StageName `json:"stage"`
	Input     S3Ref            `json:"input"`
	Attempt   int              `json:"attempt"`
	Timestamp time.Time        `json:"timestamp"`
	TraceID   string           `json:"trace_id"`
}

// NewStageMessage creates a new StageMessage with the current timestamp.
func NewStageMessage(jobID string, stage models.StageName, input S3Ref, attempt int, traceID string) *StageMessage {
	return &StageMessage{
		JobID:     jobID,
		Stage:     stage,
		Input:     input,
		Attempt:   attempt,
		Timestamp: time.Now().UTC(),
		TraceID:   traceID,
	}
}

// NextQueue returns the next queue for a given stage, or empty string if it's the last stage.
func NextQueue(stage models.StageName) string {
	switch stage {
	case models.StageValidate:
		return QueueExtract
	case models.StageExtract:
		return QueueTranscribe
	case models.StageTranscribe:
		return QueuePackage
	case models.StagePackage:
		return "" // Last stage, no next queue
	default:
		return ""
	}
}

// QueueForStage returns the queue name for a given stage.
func QueueForStage(stage models.StageName) string {
	switch stage {
	case models.StageValidate:
		return QueueValidate
	case models.StageExtract:
		return QueueExtract
	case models.StageTranscribe:
		return QueueTranscribe
	case models.StagePackage:
		return QueuePackage
	default:
		return ""
	}
}

// NextStage returns the next stage in the pipeline, or empty string if it's the last stage.
func NextStage(stage models.StageName) models.StageName {
	switch stage {
	case models.StageValidate:
		return models.StageExtract
	case models.StageExtract:
		return models.StageTranscribe
	case models.StageTranscribe:
		return models.StagePackage
	case models.StagePackage:
		return "" // Last stage
	default:
		return ""
	}
}
