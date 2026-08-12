// Package config provides application configuration loaded from environment variables.
package config

import (
	"log"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration values for the API server.
type Config struct {
	Port              string
	AWSRegion         string
	S3RawBucket       string
	S3ProcessedBucket string
	S3HLSBucket       string
	DynamoDBTable     string

	// Queue settings. The broker is a local SQLite file rather than SQS, so the
	// knobs that used to be queue attributes in AWS live here instead.
	QueueDBPath            string
	QueueVisibilityTimeout time.Duration
	QueueMaxDeliveries     int
	QueuePollInterval      time.Duration
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Port:              getEnv("API_PORT", "8080"),
		AWSRegion:         getEnv("AWS_REGION", "us-east-1"),
		S3RawBucket:       getEnv("S3_RAW_BUCKET", "dayreel-raw-videos"),
		S3ProcessedBucket: getEnv("S3_PROCESSED_BUCKET", "dayreel-processed"),
		S3HLSBucket:       getEnv("S3_HLS_BUCKET", "dayreel-hls-output"),
		DynamoDBTable:     getEnv("DYNAMODB_TABLE", "dayreel-jobs"),

		QueueDBPath: getEnv("QUEUE_DB_PATH", "./data/queue.db"),
		// Five minutes is the SQS default and comfortably outlives every stage
		// except transcode, which heartbeats instead.
		QueueVisibilityTimeout: getEnvDuration("QUEUE_VISIBILITY_TIMEOUT", 5*time.Minute),
		QueueMaxDeliveries:     getEnvInt("QUEUE_MAX_DELIVERIES", 3),
		QueuePollInterval:      getEnvDuration("QUEUE_POLL_INTERVAL", 250*time.Millisecond),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvDuration accepts any Go duration string ("30s", "5m", "250ms").
// A malformed value falls back to the default rather than failing startup:
// a typo in a tuning knob should not take the API down.
func getEnvDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("WARN: %s=%q is not a duration, using %s", key, raw, fallback)
		return fallback
	}
	return d
}

func getEnvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("WARN: %s=%q is not an integer, using %d", key, raw, fallback)
		return fallback
	}
	return n
}
