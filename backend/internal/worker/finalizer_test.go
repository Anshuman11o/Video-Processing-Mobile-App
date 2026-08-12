package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
)

// plainStage implements Stage and nothing else — the shape of validate, extract
// and transcribe.
type plainStage struct{}

func (plainStage) Name() models.StageName        { return models.StageValidate }
func (plainStage) OutputBucket() string          { return "bucket" }
func (plainStage) OutputKey(jobID string) string { return jobID + "/out" }
func (plainStage) Process(context.Context, *events.StageMessage) (string, error) {
	return "key", nil
}

// finalizingStage additionally implements Finalizer — the shape of package.
type finalizingStage struct {
	plainStage
	called     bool
	gotKey     string
	returnsErr error
}

func (f *finalizingStage) Finalize(_ context.Context, _ *events.StageMessage, outputKey string) error {
	f.called = true
	f.gotKey = outputKey
	return f.returnsErr
}

// The runner discovers the capability by type assertion, so the assertion itself
// is the contract worth pinning: a stage that does not implement Finalizer must
// not be treated as one.
func TestFinalizerDetection(t *testing.T) {
	var plain Stage = plainStage{}
	if _, ok := plain.(Finalizer); ok {
		t.Error("a plain Stage must not satisfy Finalizer")
	}

	var terminal Stage = &finalizingStage{}
	if _, ok := terminal.(Finalizer); !ok {
		t.Error("a stage implementing Finalize must satisfy Finalizer")
	}
}

func TestFinalizerReceivesTheOutputKey(t *testing.T) {
	stage := &finalizingStage{}

	var asFinalizer Finalizer = stage
	if err := asFinalizer.Finalize(context.Background(), &events.StageMessage{JobID: "job-1"}, "job-1/master.m3u8"); err != nil {
		t.Fatalf("finalize: %v", err)
	}

	if !stage.called {
		t.Error("Finalize was not called")
	}
	// The terminal stage builds its output URL from this, so a wrong key here
	// produces a job that claims completion and points at nothing.
	if stage.gotKey != "job-1/master.m3u8" {
		t.Errorf("got key %q, want the stage's output key", stage.gotKey)
	}
}

// A Finalize failure means the job's final state was never recorded. The runner
// must leave the message on the queue so it retries, rather than deleting it and
// stranding the job in "processing" forever.
func TestFinalizerErrorIsPropagated(t *testing.T) {
	sentinel := errors.New("dynamo unavailable")
	stage := &finalizingStage{returnsErr: sentinel}

	err := Finalizer(stage).Finalize(context.Background(), &events.StageMessage{JobID: "job-1"}, "k")
	if !errors.Is(err, sentinel) {
		t.Errorf("expected the underlying error to surface, got %v", err)
	}
}
