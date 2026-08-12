package cache

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/models"
)

func TestSetAndGetJob(t *testing.T) {
	c := newWithTTL(time.Minute, time.Minute)
	defer c.Close()
	ctx := context.Background()

	job := models.NewJob("clip.mp4", 1024, "video/mp4")
	job.Status = models.JobStatusProcessing
	if err := c.SetJob(ctx, job); err != nil {
		t.Fatalf("SetJob: %v", err)
	}

	got, err := c.GetJob(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got == nil {
		t.Fatal("GetJob returned nil for a fresh entry")
	}
	if got.JobID != job.JobID || got.Filename != job.Filename || got.Status != job.Status {
		t.Errorf("cached job = %+v, want %+v", got, job)
	}
	if len(got.Stages) != len(job.Stages) {
		t.Errorf("cached stages = %d, want %d", len(got.Stages), len(job.Stages))
	}
}

func TestGetJobMissReturnsNil(t *testing.T) {
	c := newWithTTL(time.Minute, time.Minute)
	defer c.Close()

	got, err := c.GetJob(context.Background(), "no-such-job")
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got != nil {
		t.Errorf("GetJob on a miss = %+v, want nil", got)
	}
}

// TestEntriesExpire is the reason the TTL exists: a job whose status changed in
// DynamoDB must stop being served from cache once the window closes.
func TestEntriesExpire(t *testing.T) {
	const ttl = 50 * time.Millisecond
	// Sweep interval far longer than the test, so this proves the read path
	// enforces expiry rather than the janitor happening to run in time.
	c := newWithTTL(ttl, time.Hour)
	defer c.Close()
	ctx := context.Background()

	job := models.NewJob("clip.mp4", 1024, "video/mp4")
	if err := c.SetJob(ctx, job); err != nil {
		t.Fatalf("SetJob: %v", err)
	}

	if got, _ := c.GetJob(ctx, job.JobID); got == nil {
		t.Fatal("entry missing immediately after SetJob")
	}

	time.Sleep(ttl + 50*time.Millisecond)

	got, err := c.GetJob(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetJob after TTL: %v", err)
	}
	if got != nil {
		t.Errorf("GetJob after TTL = %+v, want nil (entry should have expired)", got)
	}
}

// TestJanitorReclaimsExpiredEntries proves the map does not grow without bound:
// every upload mints a job ID that is never read again.
func TestJanitorReclaimsExpiredEntries(t *testing.T) {
	c := newWithTTL(20*time.Millisecond, 20*time.Millisecond)
	defer c.Close()
	ctx := context.Background()

	for i := 0; i < 25; i++ {
		if err := c.SetJob(ctx, models.NewJob("clip.mp4", 1024, "video/mp4")); err != nil {
			t.Fatalf("SetJob: %v", err)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.RLock()
		n := len(c.entries)
		c.mu.RUnlock()
		if n == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	c.mu.RLock()
	n := len(c.entries)
	c.mu.RUnlock()
	t.Errorf("janitor left %d expired entries behind, want 0", n)
}

func TestInvalidateJob(t *testing.T) {
	c := newWithTTL(time.Minute, time.Minute)
	defer c.Close()
	ctx := context.Background()

	job := models.NewJob("clip.mp4", 1024, "video/mp4")
	if err := c.SetJob(ctx, job); err != nil {
		t.Fatalf("SetJob: %v", err)
	}
	if err := c.InvalidateJob(ctx, job.JobID); err != nil {
		t.Fatalf("InvalidateJob: %v", err)
	}

	got, err := c.GetJob(ctx, job.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if got != nil {
		t.Errorf("GetJob after invalidate = %+v, want nil", got)
	}

	// Invalidating something absent is not an error — handlers call it
	// unconditionally.
	if err := c.InvalidateJob(ctx, "no-such-job"); err != nil {
		t.Errorf("InvalidateJob on a miss: %v", err)
	}
}

// TestCachedJobIsIndependentCopy guards the reason entries are stored as JSON:
// one request must not be able to mutate another's cached view.
func TestCachedJobIsIndependentCopy(t *testing.T) {
	c := newWithTTL(time.Minute, time.Minute)
	defer c.Close()
	ctx := context.Background()

	job := models.NewJob("clip.mp4", 1024, "video/mp4")
	job.Status = models.JobStatusProcessing
	if err := c.SetJob(ctx, job); err != nil {
		t.Fatalf("SetJob: %v", err)
	}

	first, _ := c.GetJob(ctx, job.JobID)
	first.Status = models.JobStatusFailed
	first.Filename = "mutated.mp4"

	second, _ := c.GetJob(ctx, job.JobID)
	if second.Status != models.JobStatusProcessing || second.Filename != "clip.mp4" {
		t.Errorf("second read saw the first caller's mutation: %+v", second)
	}
}

func TestConcurrentAccess(t *testing.T) {
	c := newWithTTL(time.Minute, 10*time.Millisecond)
	defer c.Close()
	ctx := context.Background()

	job := models.NewJob("clip.mp4", 1024, "video/mp4")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if err := c.SetJob(ctx, job); err != nil {
					t.Errorf("SetJob: %v", err)
					return
				}
				if _, err := c.GetJob(ctx, job.JobID); err != nil {
					t.Errorf("GetJob: %v", err)
					return
				}
				if err := c.InvalidateJob(ctx, job.JobID); err != nil {
					t.Errorf("InvalidateJob: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestPingAndDoubleClose(t *testing.T) {
	c := newWithTTL(time.Minute, time.Minute)

	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
