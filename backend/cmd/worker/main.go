// Package main is the entry point for DayReel pipeline workers.
//
// One binary serves every stage; WORKER_STAGE selects which.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/queue"
	"github.com/anshumanagarwal/dayreel/internal/storage"
	"github.com/anshumanagarwal/dayreel/internal/worker"
	"github.com/anshumanagarwal/dayreel/internal/worker/extract"
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

	queueClient, err := queue.New(ctx, cfg)
	if err != nil {
		log.Fatalf("create sqs client: %v", err)
	}

	stage, err := buildStage(stageName, cfg, s3Client)
	if err != nil {
		log.Fatalf("%v", err)
	}

	log.Printf("worker starting: stage=%s localstack=%v endpoint=%s",
		stageName, cfg.UseLocalStack, cfg.AWSEndpoint)

	if err := worker.NewRunner(stage, queueClient, dbClient, s3Client).Run(ctx); err != nil {
		log.Fatalf("worker: %v", err)
	}

	log.Println("worker exited")
}

// buildStage maps a stage name to its implementation. Stages 4A-6A land here.
func buildStage(name models.StageName, cfg *config.Config, s3Client *storage.S3Client) (worker.Stage, error) {
	switch name {
	case models.StageValidate:
		return validate.New(s3Client, cfg.S3ProcessedBucket, validate.DefaultLimits), nil
	case models.StageExtract:
		// Extract reads and writes the same bucket: its input is validate's
		// output. OutputKey is derived from the job ID rather than from the
		// input key, so a bad input cannot cause a write outside its own prefix.
		return extract.New(s3Client, cfg.S3ProcessedBucket, extract.DefaultOptions), nil
	case models.StageTranscribe, models.StagePackage:
		return nil, unimplementedStageError(name)
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
