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
		WhisperModelPath:  getEnv("WHISPER_MODEL_PATH", "/models/ggml-base.bin"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
