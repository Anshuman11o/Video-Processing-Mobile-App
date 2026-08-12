// Package config provides application configuration loaded from environment variables.
package config

import "os"

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
		WhisperModelPath:  getEnv("WHISPER_MODEL_PATH", "/models/ggml-base.bin"),
	}
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
