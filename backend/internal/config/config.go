// Package config provides application configuration loaded from environment variables.
package config

import (
	"log"
	"os"
	"strconv"
)

// Config holds all configuration values for the API server.
type Config struct {
	Port              string
	AWSEndpoint       string
	AWSRegion         string
	S3RawBucket       string
	S3ProcessedBucket string
	S3HLSBucket       string
	DynamoDBTable     string
	RedisURL          string
	UseLocalStack     bool

	// MockTranscribe skips the speech model and emits synthetic cues. Default
	// true: the budget rules allow only a handful of real runs, so every stage
	// downstream of transcribe is developed against the mock.
	MockTranscribe bool

	// S3PublicEndpoint is the base URL a player or mobile client uses to fetch
	// objects. It differs from AWSEndpoint because the in-cluster hostname
	// (http://localstack:4566) does not resolve outside the compose network.
	//
	// This is the same unresolved problem as the presigned-URL finding from
	// stage 4A. Left as configuration because the real-AWS access model is a
	// deliberate open question, not a settled default.
	S3PublicEndpoint string

	// UploadPartSize is the multipart part size, in bytes, that the API slices
	// uploads into and presigns one URL per.
	//
	// It can only be raised, never lowered: S3 requires every part except the
	// last to be at least 5 MiB, and LocalStack enforces that floor too, so a
	// smaller value is clamped rather than honoured.
	//
	// This was introduced to make short test clips upload as several parts —
	// at 5 MiB a <10s clip is a single part, and one part never iterates the
	// upload loop, sums progress across parts, retries one part while holding
	// another's ETag, or assembles a multi-entry complete. That did not work:
	// see the correction in [DECIDE 8]. Exercising the multipart path needs a
	// test file larger than 5 MiB instead.
	UploadPartSize int64

	// WhisperModelPath is where the ggml model file lives. The model is
	// downloaded at runtime rather than baked into the image, so this path is
	// expected to be a persistent volume mount — without one it re-downloads on
	// every fresh container.
	WhisperModelPath string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Port:              getEnv("API_PORT", "8080"),
		AWSEndpoint:       getEnv("LOCALSTACK_ENDPOINT", ""),
		AWSRegion:         getEnv("AWS_REGION", "us-east-1"),
		S3RawBucket:       getEnv("S3_RAW_BUCKET", "dayreel-raw-videos"),
		S3ProcessedBucket: getEnv("S3_PROCESSED_BUCKET", "dayreel-processed"),
		S3HLSBucket:       getEnv("S3_HLS_BUCKET", "dayreel-hls-output"),
		DynamoDBTable:     getEnv("DYNAMODB_TABLE", "dayreel-jobs"),
		RedisURL:          getEnv("REDIS_URL", "localhost:6379"),
		UseLocalStack:     getEnv("USE_LOCALSTACK", "true") == "true",
		MockTranscribe:    getEnv("MOCK_TRANSCRIBE", "true") == "true",
		S3PublicEndpoint:  getEnv("S3_PUBLIC_ENDPOINT", ""),
		UploadPartSize:    getEnvBytes("UPLOAD_PART_SIZE", DefaultUploadPartSize),
		WhisperModelPath:  getEnv("WHISPER_MODEL_PATH", "/models/ggml-base.bin"),
	}
}

// DefaultUploadPartSize is S3's minimum size for a non-final part. Anything
// smaller is valid to upload but fails at CompleteMultipartUpload on real S3.
const DefaultUploadPartSize int64 = 5 * 1024 * 1024

// getEnvBytes reads a positive byte count, falling back on anything unusable.
//
// A malformed or non-positive value falls back rather than failing: a bad part
// size would otherwise take the API down at boot, and the fallback is always
// the value real S3 accepts.
func getEnvBytes(key string, fallback int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("config: ignoring %s=%q, using %d", key, raw, fallback)
		return fallback
	}
	if n < DefaultUploadPartSize {
		// Clamped, not merely warned about, because a smaller part size cannot
		// work anywhere. LocalStack enforces the 5 MiB floor exactly as S3
		// does — verified 2026-08-13, EntityTooSmall on CompleteMultipartUpload
		// after all parts uploaded cleanly with 200s. Accepting the value would
		// configure a system that fails only at the very last step.
		log.Printf("config: %s=%d is below S3's %d minimum for non-final parts "+
			"and would fail at CompleteMultipartUpload; using %d",
			key, n, DefaultUploadPartSize, DefaultUploadPartSize)
		return fallback
	}
	return n
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// PublicEndpoint is the base URL for objects a player must reach.
//
// Falls back to the internal endpoint when unset, which is correct for
// container-to-container access and wrong for anything outside the compose
// network — deliberately visible rather than silently papered over.
func (c *Config) PublicEndpoint() string {
	if c.S3PublicEndpoint != "" {
		return c.S3PublicEndpoint
	}
	return c.AWSEndpoint
}
