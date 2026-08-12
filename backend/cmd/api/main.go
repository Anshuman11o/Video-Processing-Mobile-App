// Package main is the entry point for the DayReel API server.
package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/api"
	"github.com/anshumanagarwal/dayreel/internal/cache"
	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/queue"
	"github.com/anshumanagarwal/dayreel/internal/storage"
)

func main() {
	cfg := config.Load()
	ctx := context.Background()

	log.Printf("Starting DayReel API on port %s", cfg.Port)
	log.Printf("AWS region %s, buckets %s/%s/%s, table %s",
		cfg.AWSRegion, cfg.S3RawBucket, cfg.S3ProcessedBucket, cfg.S3HLSBucket, cfg.DynamoDBTable)

	// Initialize S3 client
	s3Client, err := storage.NewS3Client(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to create S3 client: %v", err)
	}

	// Initialize DynamoDB client
	dbClient, err := db.NewDynamoDBClient(ctx, cfg)
	if err != nil {
		log.Fatalf("Failed to create DynamoDB client: %v", err)
	}

	// Initialize the in-process job status cache
	jobCache := cache.New(cfg)
	defer jobCache.Close()

	// Initialize the queue. Unlike S3 and DynamoDB this is not optional: with no
	// broker the API can accept uploads it can never process, so a failure here
	// is fatal rather than a degraded mode.
	queueClient, err := openQueue(cfg)
	if err != nil {
		log.Fatalf("Failed to open queue: %v", err)
	}
	log.Printf("Queue ready at %s", cfg.QueueDBPath)

	// Create handler and router
	handler := api.NewHandler(s3Client, dbClient, jobCache, queueClient, cfg)
	router := api.NewRouter(handler)

	// Create server
	srv := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      router,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start server in goroutine
	go func() {
		log.Printf("API server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server error: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("Shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Not log.Fatalf: exiting here would skip the queue close below, and a
	// half-finished shutdown must not also leave the database unflushed.
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("ERROR: server forced to shutdown: %v", err)
	}

	// Closed only after Shutdown has drained the in-flight requests. The other
	// order is a request still writing its validate message to a database that
	// has been shut underneath it — the one failure mode that loses a video
	// whose bytes are already in S3.
	if err := queueClient.Close(); err != nil {
		log.Printf("ERROR: close queue: %v", err)
	}

	log.Println("Server exited")
}

// openQueue creates the queue database directory and opens the broker, wrapped
// in structured logging so every send is traceable.
func openQueue(cfg *config.Config) (queue.Queue, error) {
	// SQLite will not create missing parent directories, and the default path
	// (./data/queue.db) lives in one that is gitignored and therefore absent on
	// a fresh checkout.
	if dir := filepath.Dir(cfg.QueueDBPath); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}

	q, err := queue.Open(queue.Options{
		Path:              cfg.QueueDBPath,
		VisibilityTimeout: cfg.QueueVisibilityTimeout,
		MaxDeliveries:     cfg.QueueMaxDeliveries,
		PollInterval:      cfg.QueuePollInterval,
	})
	if err != nil {
		return nil, err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	return queue.WithLogging(q, logger), nil
}
