// Package cache provides Redis caching for job status lookups.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/models"
)

const (
	jobKeyPrefix = "job:"
	defaultTTL   = 10 * time.Second
)

// RedisClient wraps the Redis client for job caching.
type RedisClient struct {
	client *redis.Client
}

// NewRedisClient creates a new RedisClient connected to the configured Redis URL.
func NewRedisClient(cfg *config.Config) *RedisClient {
	client := redis.NewClient(&redis.Options{
		Addr: cfg.RedisURL,
	})
	return &RedisClient{client: client}
}

// Ping checks the Redis connection.
func (r *RedisClient) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// GetJob retrieves a cached job by ID. Returns nil if not found (cache miss).
func (r *RedisClient) GetJob(ctx context.Context, jobID string) (*models.Job, error) {
	key := jobKeyPrefix + jobID
	data, err := r.client.Get(ctx, key).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("redis get: %w", err)
	}

	var job models.Job
	if err := json.Unmarshal(data, &job); err != nil {
		return nil, fmt.Errorf("unmarshal cached job: %w", err)
	}
	return &job, nil
}

// SetJob caches a job with the default 10s TTL.
func (r *RedisClient) SetJob(ctx context.Context, job *models.Job) error {
	key := jobKeyPrefix + job.JobID
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job for cache: %w", err)
	}
	return r.client.Set(ctx, key, data, defaultTTL).Err()
}

// InvalidateJob removes a job from the cache.
func (r *RedisClient) InvalidateJob(ctx context.Context, jobID string) error {
	key := jobKeyPrefix + jobID
	return r.client.Del(ctx, key).Err()
}

// Close closes the Redis connection.
func (r *RedisClient) Close() error {
	return r.client.Close()
}
