// Package cache provides a short-lived, in-process cache for job status lookups.
//
// It replaces the previous Redis client. The cache exists to absorb mobile
// clients polling GET /jobs/{id} every second or two while a video processes;
// several job screens polling at that rate collapse into roughly one DynamoDB
// read per job per TTL. Nothing here is a source of truth, so a process restart
// losing the whole map costs one extra DynamoDB read per job — which is why an
// entire network service was not worth keeping for it.
//
// The TTL is also the ceiling on how stale an answer can be, because no worker
// invalidates: the map lives in the API process and the stage workers are
// separate processes. It is configurable through JOB_CACHE_TTL and defaults to
// config.DefaultJobCacheTTL, which documents the trade.
package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/models"
)

const (
	// defaultTTL is the fallback for a missing or unusable configured TTL. It
	// tracks config.DefaultJobCacheTTL rather than restating the number, so the
	// documented default and the one actually applied cannot drift apart.
	defaultTTL = config.DefaultJobCacheTTL

	// sweepInterval is how often the janitor reclaims expired entries. It is
	// independent of the TTL: reads enforce expiry on their own, so this only
	// bounds how long dead entries occupy memory.
	sweepInterval = 30 * time.Second
)

type entry struct {
	data      []byte
	expiresAt time.Time
}

// Cache is a TTL map of job snapshots, safe for concurrent use.
//
// Expiry is enforced twice, on purpose. Reads check the deadline so a stale job
// can never be served between sweeps — correctness must not depend on a
// goroutine's schedule. A janitor then sweeps in the background, because every
// upload mints a fresh job ID that is polled for a minute and never read again:
// with lazy expiry alone, entries for finished jobs would sit in the map forever
// and the "cache" would be an unbounded leak.
type Cache struct {
	mu      sync.RWMutex
	entries map[string]entry

	ttl  time.Duration
	stop chan struct{}
	once sync.Once
}

// New creates a Cache with the configured TTL and starts its janitor.
//
// A nil config or a non-positive TTL falls back to defaultTTL rather than being
// honoured. Zero would make every entry expire before it could be read — a
// cache that only ever costs a map write — and a negative one is meaningless;
// neither is worth taking the API down for, and neither is what someone setting
// JOB_CACHE_TTL meant.
func New(cfg *config.Config) *Cache {
	ttl := defaultTTL
	if cfg != nil && cfg.JobCacheTTL > 0 {
		ttl = cfg.JobCacheTTL
	}
	return newWithTTL(ttl, sweepInterval)
}

func newWithTTL(ttl, sweep time.Duration) *Cache {
	c := &Cache{
		entries: make(map[string]entry),
		ttl:     ttl,
		stop:    make(chan struct{}),
	}
	go c.janitor(sweep)
	return c
}

// Ping reports cache health. An in-process map is always reachable; the method
// stays so main.go's startup check and any future backing store keep the same
// shape.
func (c *Cache) Ping(_ context.Context) error { return nil }

// GetJob returns a cached job by ID, or nil on a miss or an expired entry.
func (c *Cache) GetJob(_ context.Context, jobID string) (*models.Job, error) {
	c.mu.RLock()
	e, ok := c.entries[jobID]
	c.mu.RUnlock()

	if !ok || time.Now().After(e.expiresAt) {
		return nil, nil
	}

	var job models.Job
	if err := json.Unmarshal(e.data, &job); err != nil {
		return nil, fmt.Errorf("unmarshal cached job %s: %w", jobID, err)
	}
	return &job, nil
}

// SetJob caches a job for the configured TTL.
//
// The job is stored as JSON rather than as a pointer. Handing every caller the
// same *models.Job would let one request mutate another's response and would be
// a data race under concurrent polling; serialising keeps the value semantics
// the Redis implementation had for free.
func (c *Cache) SetJob(_ context.Context, job *models.Job) error {
	if job == nil {
		return fmt.Errorf("cache set: job is nil")
	}
	data, err := json.Marshal(job)
	if err != nil {
		return fmt.Errorf("marshal job for cache: %w", err)
	}

	c.mu.Lock()
	c.entries[job.JobID] = entry{data: data, expiresAt: time.Now().Add(c.ttl)}
	c.mu.Unlock()
	return nil
}

// InvalidateJob drops a job from the cache, for when the API knows the status
// just changed and must not serve the old one for another whole TTL.
func (c *Cache) InvalidateJob(_ context.Context, jobID string) error {
	c.mu.Lock()
	delete(c.entries, jobID)
	c.mu.Unlock()
	return nil
}

// Close stops the janitor and releases the entries. It is safe to call twice.
func (c *Cache) Close() error {
	c.once.Do(func() {
		close(c.stop)
		c.mu.Lock()
		c.entries = make(map[string]entry)
		c.mu.Unlock()
	})
	return nil
}

func (c *Cache) janitor(sweep time.Duration) {
	ticker := time.NewTicker(sweep)
	defer ticker.Stop()

	for {
		select {
		case <-c.stop:
			return
		case now := <-ticker.C:
			c.mu.Lock()
			for id, e := range c.entries {
				if now.After(e.expiresAt) {
					delete(c.entries, id)
				}
			}
			c.mu.Unlock()
		}
	}
}
