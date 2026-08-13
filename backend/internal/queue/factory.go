package queue

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/anshumanagarwal/dayreel/internal/config"
)

// FromConfig builds the queue driver QUEUE_DRIVER names and wraps it in the
// logging decorator.
//
// This is the only place in the codebase that knows more than one driver exists.
// The API and both worker binaries used to each open SQLite themselves, with the
// mkdir and the decorator copy-pasted between them; adding a second driver to
// that shape would have meant a switch statement in every entry point, drifting
// apart the first time one of them was edited. Everything above this function
// takes a Queue and cannot tell which broker answered.
//
// The name is FromConfig rather than Open because Open is the SQLite
// constructor, which predates the interface and is still what the driver's own
// tests call.
func FromConfig(ctx context.Context, cfg *config.Config, logger *slog.Logger) (Queue, error) {
	switch cfg.QueueDriver {
	case config.QueueDriverSQLite:
		q, err := openSQLiteFromConfig(cfg)
		if err != nil {
			return nil, err
		}
		return WithLogging(q, logger), nil

	case config.QueueDriverSQS:
		q, err := OpenSQS(ctx, SQSOptions{
			Region:            cfg.AWSRegion,
			VisibilityTimeout: cfg.QueueVisibilityTimeout,
			MaxDeliveries:     cfg.QueueMaxDeliveries,
		})
		if err != nil {
			return nil, err
		}
		return WithLogging(q, logger), nil

	default:
		// Naming the valid values matters more than usual here: the failure this
		// prevents is a typo in QUEUE_DRIVER silently selecting a default and a
		// deployment that believes it is running on SQS quietly writing messages
		// into a local file no worker on another host can see.
		return nil, fmt.Errorf("queue: unknown QUEUE_DRIVER %q, want %q or %q",
			cfg.QueueDriver, config.QueueDriverSQLite, config.QueueDriverSQS)
	}
}

// openSQLiteFromConfig opens the local broker, creating the directory the file
// lives in first.
//
// The mkdir is SQLite's alone and stays inside this branch: SQLite will not
// create missing parent directories, and the default path (./data/queue.db) is
// gitignored and therefore absent on a fresh checkout, so without this a worker
// started before the API fails on a path that is only missing its directory.
// Running it for the SQS driver would create an empty data/ directory that
// nothing ever opens.
func openSQLiteFromConfig(cfg *config.Config) (*SQLiteQueue, error) {
	if dir := filepath.Dir(cfg.QueueDBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("queue: create %s: %w", dir, err)
		}
	}

	return Open(Options{
		Path:              cfg.QueueDBPath,
		VisibilityTimeout: cfg.QueueVisibilityTimeout,
		MaxDeliveries:     cfg.QueueMaxDeliveries,
		PollInterval:      cfg.QueuePollInterval,
	})
}
