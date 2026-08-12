// Package config provides application configuration loaded from environment variables.
package config

import "os"

// Config holds all configuration values for the API server.
type Config struct {
	Port             string
	AWSEndpoint      string
	AWSRegion        string
	S3RawBucket      string
	S3ProcessedBucket string
	S3HLSBucket      string
	DynamoDBTable    string
	RedisURL         string
	UseLocalStack    bool
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	return &Config{
		Port:             getEnv("API_PORT", "8080"),
		AWSEndpoint:      getEnv("LOCALSTACK_ENDPOINT", ""),
		AWSRegion:        getEnv("AWS_REGION", "us-east-1"),
		S3RawBucket:      getEnv("S3_RAW_BUCKET", "dayreel-raw-videos"),
		S3ProcessedBucket: getEnv("S3_PROCESSED_BUCKET", "dayreel-processed"),
		S3HLSBucket:      getEnv("S3_HLS_BUCKET", "dayreel-hls-output"),
		DynamoDBTable:    getEnv("DYNAMODB_TABLE", "dayreel-jobs"),
		RedisURL:         getEnv("REDIS_URL", "localhost:6379"),
		UseLocalStack:    getEnv("USE_LOCALSTACK", "true") == "true",
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
