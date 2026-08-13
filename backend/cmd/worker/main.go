// Package main is the entry point for DayReel pipeline workers.
//
// One binary serves every stage; WORKER_STAGE selects which.
package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/queue"
	"github.com/anshumanagarwal/dayreel/internal/storage"
	"github.com/anshumanagarwal/dayreel/internal/transcribe"
	"github.com/anshumanagarwal/dayreel/internal/worker"
	"github.com/anshumanagarwal/dayreel/internal/worker/extract"
	"github.com/anshumanagarwal/dayreel/internal/worker/packager"
	transcribeworker "github.com/anshumanagarwal/dayreel/internal/worker/transcribe"
	"github.com/anshumanagarwal/dayreel/internal/worker/validate"
)

func main() {
	stageName := models.StageName(os.Getenv("WORKER_STAGE"))
	if stageName == "" {
		log.Fatal("WORKER_STAGE is required (validate, extract, transcribe, package)")
	}

	cfg := config.Load()

	// Cancel on SIGINT/SIGTERM. The runner finishes or abandons its current
	// message; an abandoned message simply becomes visible again.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	s3Client, err := storage.NewS3Client(ctx, cfg)
	if err != nil {
		log.Fatalf("create s3 client: %v", err)
	}

	dbClient, err := db.NewDynamoDBClient(ctx, cfg)
	if err != nil {
		log.Fatalf("create dynamodb client: %v", err)
	}

	// The same broker the API writes to, whichever QUEUE_DRIVER names. Nothing
	// coordinates the stage runners in this process: on SQLite the database's
	// own write lock is what makes a claim safe across processes, on SQS the
	// visibility timeout is, and either way the four stages can be four separate
	// binaries.
	queueLogger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	queueClient, err := queue.FromConfig(ctx, cfg, queueLogger)
	if err != nil {
		log.Fatalf("open queue: %v", err)
	}
	defer func() {
		if err := queueClient.Close(); err != nil {
			log.Printf("worker[%s] close queue: %v", stageName, err)
		}
	}()

	stage, err := buildStage(stageName, cfg, s3Client, dbClient)
	if err != nil {
		log.Fatalf("%v", err)
	}

	log.Printf("worker starting: stage=%s region=%s queue_driver=%s",
		stageName, cfg.AWSRegion, cfg.QueueDriver)

	if err := worker.NewRunner(stage, queueClient, dbClient, s3Client, cfg).Run(ctx); err != nil {
		log.Fatalf("worker: %v", err)
	}

	log.Println("worker exited")
}

// buildStage maps a stage name to its implementation. Stages 4A-6A land here.
func buildStage(
	name models.StageName, cfg *config.Config,
	s3Client *storage.S3Client, dbClient *db.DynamoDBClient,
) (worker.Stage, error) {
	switch name {
	case models.StageValidate:
		return validate.New(s3Client, cfg.S3ProcessedBucket, validate.DefaultLimits), nil
	case models.StageExtract:
		// Extract reads and writes the same bucket: its input is validate's
		// output. OutputKey is derived from the job ID rather than from the
		// input key, so a bad input cannot cause a write outside its own prefix.
		return extract.New(s3Client, cfg.S3ProcessedBucket, extract.DefaultOptions), nil
	case models.StageTranscribe:
		// The transcriber is chosen per job because the mock needs the clip
		// duration, which only the manifest knows.
		newDecoder := func(m *events.ExtractManifest) transcribe.Transcriber {
			if cfg.MockTranscribe {
				return transcribe.Mock{DurationSeconds: m.DurationSeconds}
			}
			return transcribe.NewWhisperCPP(cfg.WhisperModelPath)
		}
		if cfg.MockTranscribe {
			log.Printf("worker[transcribe] MOCK_TRANSCRIBE=true — no model will be run")
		}
		return transcribeworker.New(s3Client, cfg.S3ProcessedBucket, newDecoder), nil
	case models.StagePackage:
		// The terminal stage. It alone takes a database client, because it alone
		// finishes the job via worker.Finalizer.
		return packager.New(
			s3Client, dbClient,
			cfg.S3HLSBucket, cfg.S3ProcessedBucket,
			packager.DefaultOptions(cfg.PublicEndpoint(), cfg.AWSRegion),
		), nil
	default:
		return nil, unknownStageError(name)
	}
}

func unimplementedStageError(name models.StageName) error {
	return &stageError{msg: "stage " + string(name) + " is not implemented yet"}
}

func unknownStageError(name models.StageName) error {
	return &stageError{msg: "unknown WORKER_STAGE " + string(name)}
}

type stageError struct{ msg string }

func (e *stageError) Error() string { return e.msg }
