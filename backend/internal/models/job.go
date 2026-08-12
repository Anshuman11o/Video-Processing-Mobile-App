// Package models contains the data models for the DayReel video processing pipeline.
package models

import (
	"time"

	"github.com/google/uuid"
)

// JobStatus represents the overall status of a processing job.
type JobStatus string

const (
	JobStatusPending    JobStatus = "pending"
	JobStatusUploading  JobStatus = "uploading"
	JobStatusProcessing JobStatus = "processing"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
)

// StageStatus represents the status of an individual processing stage.
type StageStatus string

const (
	StageStatusPending   StageStatus = "pending"
	StageStatusRunning   StageStatus = "running"
	StageStatusCompleted StageStatus = "completed"
	StageStatusFailed    StageStatus = "failed"
)

// StageName represents the name of a processing stage.
type StageName string

const (
	StageValidate   StageName = "validate"
	StageExtract    StageName = "extract"
	StageTranscribe StageName = "transcribe"
	StagePackage    StageName = "package"
)

// AllStages returns all stage names in processing order.
func AllStages() []StageName {
	return []StageName{StageValidate, StageExtract, StageTranscribe, StagePackage}
}

// StageState represents the state of an individual processing stage.
type StageState struct {
	Status      StageStatus `json:"status" dynamodbav:"status"`
	StartedAt   *time.Time  `json:"started_at,omitempty" dynamodbav:"started_at,omitempty"`
	CompletedAt *time.Time  `json:"completed_at,omitempty" dynamodbav:"completed_at,omitempty"`
	Attempts    int         `json:"attempts" dynamodbav:"attempts"`
	Error       string      `json:"error,omitempty" dynamodbav:"error,omitempty"`
	OutputKey   string      `json:"output_key,omitempty" dynamodbav:"output_key,omitempty"`
}

// UploadInfo contains information about the multipart upload.
type UploadInfo struct {
	UploadID    string     `json:"upload_id" dynamodbav:"upload_id"`
	Bucket      string     `json:"bucket" dynamodbav:"bucket"`
	Key         string     `json:"key" dynamodbav:"key"`
	PartSize    int64      `json:"part_size" dynamodbav:"part_size"`
	TotalParts  int        `json:"total_parts" dynamodbav:"total_parts"`
	CompletedAt *time.Time `json:"completed_at,omitempty" dynamodbav:"completed_at,omitempty"`
}

// OutputInfo contains information about the processed output.
type OutputInfo struct {
	HLSURL          string  `json:"hls_url" dynamodbav:"hls_url"`
	DurationSeconds float64 `json:"duration_seconds" dynamodbav:"duration_seconds"`
	ThumbnailURL    string  `json:"thumbnail_url,omitempty" dynamodbav:"thumbnail_url,omitempty"`
}

// Metrics contains timing metrics for the processing job.
type Metrics struct {
	UploadDurationMs    int64 `json:"upload_duration_ms,omitempty" dynamodbav:"upload_duration_ms,omitempty"`
	TotalProcessingMs   int64 `json:"total_processing_ms,omitempty" dynamodbav:"total_processing_ms,omitempty"`
	ValidateDurationMs  int64 `json:"validate_duration_ms,omitempty" dynamodbav:"validate_duration_ms,omitempty"`
	ExtractDurationMs   int64 `json:"extract_duration_ms,omitempty" dynamodbav:"extract_duration_ms,omitempty"`
	TranscribeDurationMs int64 `json:"transcribe_duration_ms,omitempty" dynamodbav:"transcribe_duration_ms,omitempty"`
	PackageDurationMs   int64 `json:"package_duration_ms,omitempty" dynamodbav:"package_duration_ms,omitempty"`
}

// Job represents a video processing job in DynamoDB.
type Job struct {
	// DynamoDB keys
	PK string `json:"pk" dynamodbav:"pk"`
	SK string `json:"sk" dynamodbav:"sk"`

	// Job identification
	JobID string `json:"job_id" dynamodbav:"job_id"`

	// File metadata
	Filename    string `json:"filename" dynamodbav:"filename"`
	SizeBytes   int64  `json:"size_bytes" dynamodbav:"size_bytes"`
	ContentType string `json:"content_type" dynamodbav:"content_type"`

	// Status tracking
	Status    JobStatus `json:"status" dynamodbav:"status"`
	CreatedAt time.Time `json:"created_at" dynamodbav:"created_at"`
	UpdatedAt time.Time `json:"updated_at" dynamodbav:"updated_at"`

	// Upload information
	Upload *UploadInfo `json:"upload,omitempty" dynamodbav:"upload,omitempty"`

	// Processing stages
	Stages map[StageName]*StageState `json:"stages" dynamodbav:"stages"`

	// Output information
	Output *OutputInfo `json:"output,omitempty" dynamodbav:"output,omitempty"`

	// Performance metrics
	Metrics *Metrics `json:"metrics,omitempty" dynamodbav:"metrics,omitempty"`
}

// NewJob creates a new Job with the given parameters and initializes all stages to pending.
func NewJob(filename string, sizeBytes int64, contentType string) *Job {
	now := time.Now().UTC()
	jobID := uuid.New().String()

	job := &Job{
		PK:          "JOB#" + jobID,
		SK:          "METADATA",
		JobID:       jobID,
		Filename:    filename,
		SizeBytes:   sizeBytes,
		ContentType: contentType,
		Status:      JobStatusPending,
		CreatedAt:   now,
		UpdatedAt:   now,
		Stages:      make(map[StageName]*StageState),
		Metrics:     &Metrics{},
	}

	// Initialize all stages to pending
	for _, stage := range AllStages() {
		job.Stages[stage] = &StageState{
			Status:   StageStatusPending,
			Attempts: 0,
		}
	}

	return job
}

// SetStageRunning marks a stage as running and increments the attempt counter.
func (j *Job) SetStageRunning(stage StageName) {
	now := time.Now().UTC()
	j.UpdatedAt = now

	if state, ok := j.Stages[stage]; ok {
		state.Status = StageStatusRunning
		state.StartedAt = &now
		state.Attempts++
	}
}

// SetStageCompleted marks a stage as completed with the given output key.
func (j *Job) SetStageCompleted(stage StageName, outputKey string) {
	now := time.Now().UTC()
	j.UpdatedAt = now

	if state, ok := j.Stages[stage]; ok {
		state.Status = StageStatusCompleted
		state.CompletedAt = &now
		state.OutputKey = outputKey
		state.Error = ""
	}
}

// SetStageFailed marks a stage as failed with the given error message.
func (j *Job) SetStageFailed(stage StageName, err string) {
	now := time.Now().UTC()
	j.UpdatedAt = now

	if state, ok := j.Stages[stage]; ok {
		state.Status = StageStatusFailed
		state.CompletedAt = &now
		state.Error = err
	}

	j.Status = JobStatusFailed
}
