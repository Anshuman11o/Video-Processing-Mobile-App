package models

import (
	"encoding/json"
	"testing"
)

func TestNewJob(t *testing.T) {
	job := NewJob("test-video.mp4", 1024*1024*50, "video/mp4")

	// Verify basic fields
	if job.JobID == "" {
		t.Error("JobID should not be empty")
	}
	if job.Filename != "test-video.mp4" {
		t.Errorf("Filename = %q, want %q", job.Filename, "test-video.mp4")
	}
	if job.SizeBytes != 1024*1024*50 {
		t.Errorf("SizeBytes = %d, want %d", job.SizeBytes, 1024*1024*50)
	}
	if job.ContentType != "video/mp4" {
		t.Errorf("ContentType = %q, want %q", job.ContentType, "video/mp4")
	}
	if job.Status != JobStatusPending {
		t.Errorf("Status = %q, want %q", job.Status, JobStatusPending)
	}

	// Verify PK and SK format
	expectedPK := "JOB#" + job.JobID
	if job.PK != expectedPK {
		t.Errorf("PK = %q, want %q", job.PK, expectedPK)
	}
	if job.SK != "METADATA" {
		t.Errorf("SK = %q, want %q", job.SK, "METADATA")
	}

	// Verify all stages are initialized to pending
	allStages := AllStages()
	if len(job.Stages) != len(allStages) {
		t.Errorf("Stages count = %d, want %d", len(job.Stages), len(allStages))
	}

	for _, stage := range allStages {
		state, ok := job.Stages[stage]
		if !ok {
			t.Errorf("Stage %q not found in Stages map", stage)
			continue
		}
		if state.Status != StageStatusPending {
			t.Errorf("Stage %q status = %q, want %q", stage, state.Status, StageStatusPending)
		}
		if state.Attempts != 0 {
			t.Errorf("Stage %q attempts = %d, want %d", stage, state.Attempts, 0)
		}
	}

	// Verify timestamps are set
	if job.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}
	if job.UpdatedAt.IsZero() {
		t.Error("UpdatedAt should not be zero")
	}
}

func TestNewJob_AllStagesInitialized(t *testing.T) {
	job := NewJob("video.mp4", 1000, "video/mp4")

	expectedStages := []StageName{StageValidate, StageExtract, StageTranscribe, StagePackage}

	for _, stage := range expectedStages {
		state, exists := job.Stages[stage]
		if !exists {
			t.Errorf("Stage %q should exist", stage)
			continue
		}
		if state.Status != StageStatusPending {
			t.Errorf("Stage %q status = %q, want %q", stage, state.Status, StageStatusPending)
		}
	}
}

func TestJob_JSONMarshal(t *testing.T) {
	job := NewJob("test.mp4", 5000, "video/mp4")

	// Marshal to JSON
	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Failed to marshal job: %v", err)
	}

	// Unmarshal back
	var unmarshaled Job
	if err := json.Unmarshal(data, &unmarshaled); err != nil {
		t.Fatalf("Failed to unmarshal job: %v", err)
	}

	// Verify key fields
	if unmarshaled.JobID != job.JobID {
		t.Errorf("JobID = %q, want %q", unmarshaled.JobID, job.JobID)
	}
	if unmarshaled.Filename != job.Filename {
		t.Errorf("Filename = %q, want %q", unmarshaled.Filename, job.Filename)
	}
	if unmarshaled.Status != job.Status {
		t.Errorf("Status = %q, want %q", unmarshaled.Status, job.Status)
	}

	// Verify stages are preserved
	if len(unmarshaled.Stages) != len(job.Stages) {
		t.Errorf("Stages count = %d, want %d", len(unmarshaled.Stages), len(job.Stages))
	}

	for stage, originalState := range job.Stages {
		unmarshaledState, ok := unmarshaled.Stages[stage]
		if !ok {
			t.Errorf("Stage %q not found after unmarshal", stage)
			continue
		}
		if unmarshaledState.Status != originalState.Status {
			t.Errorf("Stage %q status = %q, want %q", stage, unmarshaledState.Status, originalState.Status)
		}
	}
}

func TestJob_JSONContainsExpectedFields(t *testing.T) {
	job := NewJob("test.mp4", 5000, "video/mp4")

	data, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("Failed to marshal job: %v", err)
	}

	// Parse as generic map to check field names
	var m map[string]interface{}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("Failed to unmarshal to map: %v", err)
	}

	// Check required fields exist with correct JSON names
	requiredFields := []string{"pk", "sk", "job_id", "filename", "size_bytes", "content_type", "status", "created_at", "updated_at", "stages"}
	for _, field := range requiredFields {
		if _, ok := m[field]; !ok {
			t.Errorf("Required field %q not found in JSON output", field)
		}
	}
}

func TestSetStageRunning(t *testing.T) {
	job := NewJob("test.mp4", 1000, "video/mp4")
	originalUpdatedAt := job.UpdatedAt

	job.SetStageRunning(StageValidate)

	state := job.Stages[StageValidate]
	if state.Status != StageStatusRunning {
		t.Errorf("Status = %q, want %q", state.Status, StageStatusRunning)
	}
	if state.Attempts != 1 {
		t.Errorf("Attempts = %d, want %d", state.Attempts, 1)
	}
	if state.StartedAt == nil {
		t.Error("StartedAt should be set")
	}
	if !job.UpdatedAt.After(originalUpdatedAt) && job.UpdatedAt != originalUpdatedAt {
		t.Error("UpdatedAt should be updated")
	}
}

func TestSetStageCompleted(t *testing.T) {
	job := NewJob("test.mp4", 1000, "video/mp4")
	job.SetStageRunning(StageValidate)

	job.SetStageCompleted(StageValidate, "processed/test.mp4")

	state := job.Stages[StageValidate]
	if state.Status != StageStatusCompleted {
		t.Errorf("Status = %q, want %q", state.Status, StageStatusCompleted)
	}
	if state.OutputKey != "processed/test.mp4" {
		t.Errorf("OutputKey = %q, want %q", state.OutputKey, "processed/test.mp4")
	}
	if state.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestSetStageFailed(t *testing.T) {
	job := NewJob("test.mp4", 1000, "video/mp4")
	job.SetStageRunning(StageValidate)

	job.SetStageFailed(StageValidate, "invalid video format")

	state := job.Stages[StageValidate]
	if state.Status != StageStatusFailed {
		t.Errorf("Status = %q, want %q", state.Status, StageStatusFailed)
	}
	if state.Error != "invalid video format" {
		t.Errorf("Error = %q, want %q", state.Error, "invalid video format")
	}
	if job.Status != JobStatusFailed {
		t.Errorf("Job Status = %q, want %q", job.Status, JobStatusFailed)
	}
}
