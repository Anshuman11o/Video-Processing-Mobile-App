package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/queue"
)

// poolQueue is a Queue that hands out an endless supply of payload-less
// messages and blocks inside DeadLetter, recording how many handlers sit there
// at once.
//
// A nil Stage payload is the lever: handle() rejects it on the first branch and
// exits through deadLetter, which touches the queue and nothing else. That is
// what makes this test possible with a nil db and nil storage — every other
// path through handle reaches S3 or DynamoDB within a few lines. The point
// under test is the dispatch loop, not the media work, so a branch that reaches
// the queue and stops is exactly the right one to instrument.
type poolQueue struct {
	release chan struct{} // closed to let every blocked handler finish

	mu         sync.Mutex
	inFlight   int
	maxSeen    int
	handled    int
	dispatched chan struct{} // signalled once per handler entering DeadLetter
}

func newPoolQueue() *poolQueue {
	return &poolQueue{
		release:    make(chan struct{}),
		dispatched: make(chan struct{}, 64),
	}
}

func (q *poolQueue) Receive(ctx context.Context, _ string, max int, wait time.Duration) ([]queue.Message, error) {
	// One message per call, matching receiveMax. Never block for the full poll
	// interval: this queue is always ready, which keeps the test fast and means
	// any serialisation observed is the pool's doing, not the poll's.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	return []queue.Message{{ID: 1, MessageID: "1", Queue: "q", Receipt: "r"}}, nil
}

func (q *poolQueue) DeadLetter(ctx context.Context, _ queue.Message, _ string) error {
	q.mu.Lock()
	q.inFlight++
	q.handled++
	if q.inFlight > q.maxSeen {
		q.maxSeen = q.inFlight
	}
	q.mu.Unlock()

	select {
	case q.dispatched <- struct{}{}:
	default:
	}

	// Hold the handler open so concurrent arrivals overlap observably. Without
	// this every handler would finish before the next was dispatched and the
	// peak would read 1 at any pool size.
	select {
	case <-q.release:
	case <-ctx.Done():
	}

	q.mu.Lock()
	q.inFlight--
	q.mu.Unlock()
	return nil
}

func (q *poolQueue) peak() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.maxSeen
}

func (q *poolQueue) Send(context.Context, string, *events.StageMessage, time.Duration) error {
	return nil
}
func (q *poolQueue) Ack(context.Context, queue.Message) error                      { return nil }
func (q *poolQueue) Nack(context.Context, queue.Message, time.Duration) error      { return nil }
func (q *poolQueue) Heartbeat(context.Context, queue.Message, time.Duration) error { return nil }
func (q *poolQueue) Stats(context.Context) ([]queue.QueueStat, error)              { return nil, nil }
func (q *poolQueue) Close() error                                                  { return nil }

// runPool starts a runner at the given concurrency, waits for want handlers to
// be in flight (or times out), and returns the observed peak.
func runPool(t *testing.T, concurrency, want int) *poolQueue {
	t.Helper()

	q := newPoolQueue()
	cfg := &config.Config{
		QueuePollInterval:      time.Millisecond,
		QueueMaxDeliveries:     3,
		QueueVisibilityTimeout: 5 * time.Minute,
		WorkerConcurrency:      concurrency,
	}

	runner := NewRunner(plainStage{}, q, nil, nil, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	// Wait until `want` handlers have reported in, rather than sleeping a fixed
	// duration: a sleep long enough to be reliable on a loaded machine is long
	// enough to make the suite slow, and one short enough to be fast is flaky.
	deadline := time.After(3 * time.Second)
	for i := 0; i < want; i++ {
		select {
		case <-q.dispatched:
		case <-deadline:
			t.Fatalf("only %d handler(s) dispatched, wanted %d", i, want)
		}
	}

	// Let everything drain, then confirm Run returns — the WaitGroup must not
	// hold shutdown open forever.
	close(q.release)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return; a handler or the WaitGroup is stuck")
	}
	return q
}

// The headline: WORKER_CONCURRENCY=2 must actually put two messages in flight.
//
// This is the property that did not exist before. WORKER_CONCURRENCY sat in
// .env and .env.example while no Go code read it, so a deployment could believe
// it had parallelism it did not have — the loop called handle synchronously and
// processed exactly one message at a time whatever the file said.
func TestConcurrencyRunsHandlersInParallel(t *testing.T) {
	q := runPool(t, 2, 2)
	if got := q.peak(); got < 2 {
		t.Errorf("peak in-flight handlers = %d, want 2: the pool is not running them in parallel", got)
	}
}

// And it must not exceed the pool size. A semaphore that leaks a slot degrades
// into an unbounded goroutine spawner, which on this pipeline means an
// unbounded number of concurrent ffmpeg processes — the failure would look like
// the machine dying, not like a test going red.
func TestConcurrencyNeverExceedsThePoolSize(t *testing.T) {
	q := runPool(t, 2, 2)

	// Give the loop room to over-dispatch if the bound is broken. By this point
	// release is closed, so an unbounded loop would have spun many more.
	time.Sleep(50 * time.Millisecond)

	if got := q.peak(); got > 2 {
		t.Errorf("peak in-flight handlers = %d, want at most 2: the pool bound leaked", got)
	}
}

// Concurrency 1 must behave exactly as the loop did before this change, since
// it is the default and every existing deployment runs it.
func TestConcurrencyOneIsStrictlySerial(t *testing.T) {
	q := runPool(t, 1, 1)
	if got := q.peak(); got != 1 {
		t.Errorf("peak in-flight handlers = %d, want exactly 1 at concurrency 1", got)
	}
}

// A misconfigured pool size must not deadlock. Zero would make a semaphore that
// can never be acquired: the worker would poll forever, process nothing, and
// log nothing to say why.
func TestConcurrencyZeroIsClampedNotDeadlocked(t *testing.T) {
	q := runPool(t, 0, 1)
	if got := q.peak(); got != 1 {
		t.Errorf("peak in-flight handlers = %d, want 1 after clamping a zero pool size", got)
	}
}
