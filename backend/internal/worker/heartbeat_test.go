package worker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeartbeatLoopBeatsUntilStopped(t *testing.T) {
	var beats atomic.Int32

	stop := heartbeatLoop(context.Background(), 10*time.Millisecond, func() {
		beats.Add(1)
	})

	time.Sleep(75 * time.Millisecond)
	stop()

	if got := beats.Load(); got < 3 {
		t.Errorf("expected several beats in 75ms at a 10ms interval, got %d", got)
	}
}

// The whole point of stop() blocking is that a beat cannot land after Process
// has returned and the message has been deleted — extending the visibility of a
// deleted message is a spurious error at best, and at worst hides a real one.
func TestHeartbeatLoopStopIsSynchronous(t *testing.T) {
	var (
		mu      sync.Mutex
		stopped bool
		late    bool
	)

	stop := heartbeatLoop(context.Background(), 5*time.Millisecond, func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			late = true
		}
	})

	time.Sleep(30 * time.Millisecond)

	stop()
	mu.Lock()
	stopped = true
	mu.Unlock()

	// Well past several intervals; nothing may fire now.
	time.Sleep(40 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if late {
		t.Error("a beat fired after stop() returned")
	}
}

// The runner calls stop unconditionally after Process, including on the error
// path, so a double call must not panic on a closed channel.
func TestHeartbeatLoopStopIsIdempotent(t *testing.T) {
	stop := heartbeatLoop(context.Background(), time.Hour, func() {})
	stop()
	stop()
	stop()
}

// A cancelled parent context must tear the loop down on its own, so SIGTERM
// during a long stage does not leave a goroutine extending a message forever.
func TestHeartbeatLoopStopsWithParentContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	var beats atomic.Int32
	stop := heartbeatLoop(ctx, 5*time.Millisecond, func() { beats.Add(1) })
	defer stop()

	time.Sleep(20 * time.Millisecond)
	cancel()
	time.Sleep(20 * time.Millisecond)

	settled := beats.Load()
	time.Sleep(30 * time.Millisecond)

	if got := beats.Load(); got != settled {
		t.Errorf("beats continued after the parent context was cancelled: %d then %d", settled, got)
	}
}

// A stage faster than one interval is the common case — validate and extract
// both finish in about a second — and must cost nothing.
func TestHeartbeatLoopNoBeatForFastWork(t *testing.T) {
	var beats atomic.Int32

	stop := heartbeatLoop(context.Background(), time.Hour, func() { beats.Add(1) })
	stop()

	if got := beats.Load(); got != 0 {
		t.Errorf("expected no beats for work shorter than the interval, got %d", got)
	}
}
