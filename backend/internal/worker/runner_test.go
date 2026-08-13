package worker

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/queue"
)

// An idle runner has to block in Receive and come back the moment it is
// cancelled, and it has to come back with nil.
//
// Both halves have bitten this loop before. A receive that returns immediately
// turns the loop into a spin — under SQS that was billed per request, and here
// every claim takes SQLite's write lock, so a spinning idle worker starves the
// ones doing real work. And a cancelled context surfaces as a receive error, so
// a runner that did not recognise it would log a failure and back off on every
// shutdown.
func TestRunReturnsCleanlyOnCancel(t *testing.T) {
	q, err := queue.Open(queue.Options{
		Path:         filepath.Join(t.TempDir(), "queue.db"),
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("open queue: %v", err)
	}
	defer q.Close()

	cfg := &config.Config{
		QueuePollInterval:      50 * time.Millisecond,
		QueueMaxDeliveries:     3,
		QueueVisibilityTimeout: 5 * time.Minute,
	}

	// db and storage are nil, which is safe only because no message ever
	// arrives: everything that touches them is downstream of a receive.
	runner := NewRunner(plainStage{}, q, nil, nil, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	// Long enough for several empty receives to have come and gone.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil on a cancelled context", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}
