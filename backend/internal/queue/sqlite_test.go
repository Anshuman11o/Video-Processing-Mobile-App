package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
)

// newTestQueue opens a queue on a fresh temp file. Every test gets its own
// database file rather than an in-memory one: the whole point of the design is
// how SQLite behaves on disk under concurrent writers, and ":memory:" would test
// something else.
func newTestQueue(t *testing.T, mutators ...func(*Options)) *SQLiteQueue {
	t.Helper()

	opts := Options{
		Path:              filepath.Join(t.TempDir(), "queue.db"),
		VisibilityTimeout: 30 * time.Second,
		MaxDeliveries:     3,
		PollInterval:      5 * time.Millisecond,
		DLQName:           events.QueueDLQ,
	}
	for _, m := range mutators {
		m(&opts)
	}

	q, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return q
}

func testStage(jobID string) *events.StageMessage {
	return events.NewStageMessage(
		jobID,
		models.StageValidate,
		events.S3Ref{Bucket: events.BucketRawVideos, Key: jobID + "/clip.mp4"},
		1,
		"trace-"+jobID,
	)
}

// receiveOne claims exactly one message and fails the test if none is available.
func receiveOne(t *testing.T, q *SQLiteQueue, queue string) Message {
	t.Helper()
	msgs, err := q.Receive(context.Background(), queue, 1, 0)
	if err != nil {
		t.Fatalf("Receive(%s): %v", queue, err)
	}
	if len(msgs) != 1 {
		t.Fatalf("Receive(%s): got %d messages, want 1", queue, len(msgs))
	}
	return msgs[0]
}

// countRows is a white-box helper for assertions the public API deliberately
// does not expose (row survival, dlq_reason).
func countRows(t *testing.T, q *SQLiteQueue, where string, args ...any) int {
	t.Helper()
	var n int
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return n
}

// --- 1. Send / Receive / Ack round trip ---

func TestSendReceiveAckRoundTrip(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	sent := testStage("job-round-trip")
	if err := q.Send(ctx, events.QueueValidate, sent, 0); err != nil {
		t.Fatalf("Send: %v", err)
	}

	before := time.Now()
	got := receiveOne(t, q, events.QueueValidate)

	// Payload must survive byte for byte — the queue is a pipe, not a parser.
	if got.Stage.JobID != sent.JobID {
		t.Errorf("JobID = %q, want %q", got.Stage.JobID, sent.JobID)
	}
	if got.Stage.Stage != sent.Stage {
		t.Errorf("Stage = %q, want %q", got.Stage.Stage, sent.Stage)
	}
	if got.Stage.Input != sent.Input {
		t.Errorf("Input = %+v, want %+v", got.Stage.Input, sent.Input)
	}
	if got.Stage.Attempt != sent.Attempt {
		t.Errorf("Attempt = %d, want %d", got.Stage.Attempt, sent.Attempt)
	}
	if got.Stage.TraceID != sent.TraceID {
		t.Errorf("TraceID = %q, want %q", got.Stage.TraceID, sent.TraceID)
	}
	if !got.Stage.Timestamp.Equal(sent.Timestamp.Truncate(time.Nanosecond)) {
		t.Errorf("Timestamp = %v, want %v", got.Stage.Timestamp, sent.Timestamp)
	}

	// Envelope must be populated: these fields are what makes the queue
	// observable.
	if got.ID == 0 {
		t.Error("ID = 0, want a rowid")
	}
	if got.Queue != events.QueueValidate {
		t.Errorf("Queue = %q, want %q", got.Queue, events.QueueValidate)
	}
	if len(got.Receipt) != 32 {
		t.Errorf("Receipt = %q, want 32 hex chars", got.Receipt)
	}
	if got.ReceiveCount != 1 {
		t.Errorf("ReceiveCount = %d, want 1", got.ReceiveCount)
	}
	if got.EnqueuedAt.IsZero() || got.ClaimedAt.IsZero() {
		t.Errorf("EnqueuedAt = %v, ClaimedAt = %v, want both set", got.EnqueuedAt, got.ClaimedAt)
	}
	if !got.LeaseExpiresAt.After(before) {
		t.Errorf("LeaseExpiresAt = %v, want in the future", got.LeaseExpiresAt)
	}

	if err := q.Ack(ctx, got); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n := countRows(t, q, "1=1"); n != 0 {
		t.Errorf("rows after ack = %d, want 0", n)
	}

	// A queue drained by an ack really is empty.
	msgs, err := q.Receive(ctx, events.QueueValidate, 1, 0)
	if err != nil {
		t.Fatalf("Receive after ack: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("Receive after ack returned %d messages, want 0", len(msgs))
	}
}

// --- 2. Concurrent claim (the headline test) ---

// TestConcurrentClaimNeverDoubleDelivers is the test the whole design exists to
// pass. One message, several workers racing to claim it, repeated enough times
// that a select-then-update race would show up: with a two-statement claim this
// fails within a handful of iterations.
func TestConcurrentClaimNeverDoubleDelivers(t *testing.T) {
	const (
		iterations  = 250
		workers     = 4
		maxAttempts = 25
	)

	q := newTestQueue(t)
	ctx := context.Background()
	const queueName = "concurrent-claim"

	totalClaims := 0
	for i := 0; i < iterations; i++ {
		if err := q.Send(ctx, queueName, testStage("job-race"), 0); err != nil {
			t.Fatalf("iteration %d: Send: %v", i, err)
		}

		var (
			claimed  atomic.Int32
			start    = make(chan struct{})
			wg       sync.WaitGroup
			mu       sync.Mutex
			winners  []Message
			firstErr error
		)

		wg.Add(workers)
		for w := 0; w < workers; w++ {
			go func() {
				defer wg.Done()
				<-start // release every worker into the claim at once
				for attempt := 0; attempt < maxAttempts && claimed.Load() == 0; attempt++ {
					msgs, err := q.Receive(ctx, queueName, 1, 0)
					if err != nil {
						mu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						mu.Unlock()
						return
					}
					if len(msgs) > 0 {
						claimed.Add(int32(len(msgs)))
						mu.Lock()
						winners = append(winners, msgs...)
						mu.Unlock()
						return
					}
				}
			}()
		}
		close(start)
		wg.Wait()

		if firstErr != nil {
			t.Fatalf("iteration %d: Receive: %v", i, firstErr)
		}
		if got := claimed.Load(); got != 1 {
			t.Fatalf("iteration %d: %d workers claimed the message, want exactly 1", i, got)
		}
		if len(winners) != 1 {
			t.Fatalf("iteration %d: %d winners, want 1", i, len(winners))
		}
		totalClaims += len(winners)

		// The winner owns a valid lease and can drain the message, leaving the
		// queue clean for the next iteration.
		if err := q.Ack(ctx, winners[0]); err != nil {
			t.Fatalf("iteration %d: Ack by winner: %v", i, err)
		}
	}

	if totalClaims != iterations {
		t.Errorf("total claims = %d, want %d (one per message)", totalClaims, iterations)
	}
	if n := countRows(t, q, "1=1"); n != 0 {
		t.Errorf("rows left = %d, want 0", n)
	}
}

// TestBatchClaimGivesDistinctReceipts guards the randomblob-per-row assumption:
// if SQLite folded it into a constant, a batch claim would hand every message the
// same receipt and acking one would ack them all.
func TestBatchClaimGivesDistinctReceipts(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		if err := q.Send(ctx, events.QueueExtract, testStage("job-batch"), 0); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	msgs, err := q.Receive(ctx, events.QueueExtract, 5, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 5 {
		t.Fatalf("claimed %d messages, want 5", len(msgs))
	}

	seen := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		if seen[m.Receipt] {
			t.Fatalf("duplicate receipt %q across a batch claim", m.Receipt)
		}
		seen[m.Receipt] = true
	}

	// Acking one must remove exactly one.
	if err := q.Ack(ctx, msgs[0]); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if n := countRows(t, q, "1=1"); n != 4 {
		t.Errorf("rows after acking one of five = %d, want 4", n)
	}
}

// --- 3. Lease expiry ---

func TestLeaseExpiryRedelivers(t *testing.T) {
	q := newTestQueue(t, func(o *Options) { o.VisibilityTimeout = 100 * time.Millisecond })
	ctx := context.Background()

	if err := q.Send(ctx, events.QueueValidate, testStage("job-expiry"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}

	first := receiveOne(t, q, events.QueueValidate)
	if first.ReceiveCount != 1 {
		t.Fatalf("first ReceiveCount = %d, want 1", first.ReceiveCount)
	}

	// Still leased: nobody else may take it.
	msgs, err := q.Receive(ctx, events.QueueValidate, 1, 0)
	if err != nil {
		t.Fatalf("Receive during lease: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("claimed %d messages while leased, want 0", len(msgs))
	}

	// A worker that dies without acking must not strand the message.
	time.Sleep(200 * time.Millisecond)

	second := receiveOne(t, q, events.QueueValidate)
	if second.ID != first.ID {
		t.Errorf("redelivered ID = %d, want %d", second.ID, first.ID)
	}
	if second.ReceiveCount != 2 {
		t.Errorf("redelivered ReceiveCount = %d, want 2", second.ReceiveCount)
	}
	if second.Receipt == first.Receipt {
		t.Error("redelivery reused the old receipt; the dead worker could still ack")
	}
}

// --- 4. Heartbeat ---

func TestHeartbeatExtendsLeaseWithoutCountingAsRedelivery(t *testing.T) {
	q := newTestQueue(t, func(o *Options) { o.VisibilityTimeout = 150 * time.Millisecond })
	ctx := context.Background()

	if err := q.Send(ctx, events.QueueTranscribe, testStage("job-heartbeat"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	claimed := receiveOne(t, q, events.QueueTranscribe)

	// Extend before the original lease would have expired.
	time.Sleep(80 * time.Millisecond)
	if err := q.Heartbeat(ctx, claimed, 400*time.Millisecond); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	// Past the original expiry, the message must still be ours.
	time.Sleep(150 * time.Millisecond)
	msgs, err := q.Receive(ctx, events.QueueTranscribe, 1, 0)
	if err != nil {
		t.Fatalf("Receive after heartbeat: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("claimed %d messages past the original lease, want 0 (heartbeat did not hold)", len(msgs))
	}

	// The extended lease still expires eventually, and when it does the message
	// must look like a second delivery, not a third: a heartbeat is the same
	// delivery continuing.
	time.Sleep(450 * time.Millisecond)
	redelivered := receiveOne(t, q, events.QueueTranscribe)
	if redelivered.ReceiveCount != 2 {
		t.Errorf("ReceiveCount = %d, want 2 (heartbeat must not bump it)", redelivered.ReceiveCount)
	}
}

// --- 5. Ack after lease loss ---

func TestAckAfterLeaseLossIsRejected(t *testing.T) {
	q := newTestQueue(t, func(o *Options) { o.VisibilityTimeout = 100 * time.Millisecond })
	ctx := context.Background()

	if err := q.Send(ctx, events.QueuePackage, testStage("job-lease-loss"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}

	workerA := receiveOne(t, q, events.QueuePackage)
	time.Sleep(200 * time.Millisecond) // A stalls past its lease
	workerB := receiveOne(t, q, events.QueuePackage)

	if workerA.Receipt == workerB.Receipt {
		t.Fatal("worker B got worker A's receipt")
	}

	// A finishes late and tries to ack work B now owns.
	err := q.Ack(ctx, workerA)
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("worker A Ack error = %v, want ErrLeaseLost", err)
	}

	// A's stale ack must not have deleted B's message.
	if n := countRows(t, q, "id = ?", workerA.ID); n != 1 {
		t.Fatalf("message rows after stale ack = %d, want 1 (worker A deleted B's message)", n)
	}

	// B still holds a valid lease.
	if err := q.Ack(ctx, workerB); err != nil {
		t.Fatalf("worker B Ack: %v", err)
	}
	if n := countRows(t, q, "1=1"); n != 0 {
		t.Errorf("rows after B's ack = %d, want 0", n)
	}
}

func TestHeartbeatAndNackAlsoDetectLeaseLoss(t *testing.T) {
	q := newTestQueue(t, func(o *Options) { o.VisibilityTimeout = 80 * time.Millisecond })
	ctx := context.Background()

	if err := q.Send(ctx, events.QueueValidate, testStage("job-stale-ops"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stale := receiveOne(t, q, events.QueueValidate)
	time.Sleep(150 * time.Millisecond)
	fresh := receiveOne(t, q, events.QueueValidate)

	if err := q.Heartbeat(ctx, stale, time.Minute); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale Heartbeat error = %v, want ErrLeaseLost", err)
	}
	if err := q.Nack(ctx, stale, 0); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale Nack error = %v, want ErrLeaseLost", err)
	}
	if err := q.DeadLetter(ctx, stale, "stale"); !errors.Is(err, ErrLeaseLost) {
		t.Errorf("stale DeadLetter error = %v, want ErrLeaseLost", err)
	}

	// None of the stale operations may have disturbed the live lease.
	if n := countRows(t, q, "id = ? AND queue = ? AND receipt = ?", fresh.ID, events.QueueValidate, fresh.Receipt); n != 1 {
		t.Error("a stale operation modified the message the fresh worker owns")
	}
}

// --- 6. Dead letter ---

func TestDeadLetterAfterMaxDeliveries(t *testing.T) {
	const maxDeliveries = 3
	q := newTestQueue(t, func(o *Options) {
		o.VisibilityTimeout = 5 * time.Second
		o.MaxDeliveries = maxDeliveries
	})
	ctx := context.Background()

	sent := testStage("job-poison")
	if err := q.Send(ctx, events.QueueExtract, sent, 0); err != nil {
		t.Fatalf("Send: %v", err)
	}

	const reason = "ffprobe: unsupported codec"
	var deliveries int
	for {
		m := receiveOne(t, q, events.QueueExtract)
		deliveries++

		if m.ReceiveCount >= q.MaxDeliveries() {
			if err := q.DeadLetter(ctx, m, reason); err != nil {
				t.Fatalf("DeadLetter: %v", err)
			}
			break
		}
		if err := q.Nack(ctx, m, 0); err != nil {
			t.Fatalf("Nack: %v", err)
		}
		if deliveries > maxDeliveries+1 {
			t.Fatal("message never reached the delivery budget")
		}
	}

	if deliveries != maxDeliveries {
		t.Errorf("deliveries before dead-lettering = %d, want %d", deliveries, maxDeliveries)
	}

	// It must have left the source queue entirely...
	msgs, err := q.Receive(ctx, events.QueueExtract, 1, 0)
	if err != nil {
		t.Fatalf("Receive from source after dead-letter: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("source queue still holds %d messages, want 0", len(msgs))
	}

	// ...and arrived on the DLQ, immediately claimable, payload intact.
	dead := receiveOne(t, q, events.QueueDLQ)
	if dead.Stage.JobID != sent.JobID {
		t.Errorf("DLQ payload JobID = %q, want %q", dead.Stage.JobID, sent.JobID)
	}
	if dead.ReceiveCount <= maxDeliveries {
		t.Errorf("DLQ ReceiveCount = %d, want > %d (history is preserved)", dead.ReceiveCount, maxDeliveries)
	}

	var storedReason sql.NullString
	if err := q.db.QueryRow(`SELECT dlq_reason FROM messages WHERE id = ?`, dead.ID).Scan(&storedReason); err != nil {
		t.Fatalf("read dlq_reason: %v", err)
	}
	if storedReason.String != reason {
		t.Errorf("dlq_reason = %q, want %q", storedReason.String, reason)
	}
}

// --- 7. Nack backoff ---

func TestNackBackoffDelaysRedelivery(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	if err := q.Send(ctx, events.QueueValidate, testStage("job-backoff"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	m := receiveOne(t, q, events.QueueValidate)

	const backoff = 300 * time.Millisecond
	if err := q.Nack(ctx, m, backoff); err != nil {
		t.Fatalf("Nack: %v", err)
	}

	// Invisible for the whole backoff — otherwise a failing stage would spin.
	msgs, err := q.Receive(ctx, events.QueueValidate, 1, 0)
	if err != nil {
		t.Fatalf("Receive during backoff: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("claimed %d messages during backoff, want 0", len(msgs))
	}

	time.Sleep(backoff + 100*time.Millisecond)

	retried := receiveOne(t, q, events.QueueValidate)
	if retried.ID != m.ID {
		t.Errorf("retried ID = %d, want %d", retried.ID, m.ID)
	}
	if retried.ReceiveCount != 2 {
		t.Errorf("retried ReceiveCount = %d, want 2", retried.ReceiveCount)
	}
}

func TestSendDelayHidesMessage(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	const delay = 250 * time.Millisecond
	if err := q.Send(ctx, events.QueuePackage, testStage("job-delayed"), delay); err != nil {
		t.Fatalf("Send: %v", err)
	}

	msgs, err := q.Receive(ctx, events.QueuePackage, 1, 0)
	if err != nil {
		t.Fatalf("Receive before delay elapsed: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("claimed %d delayed messages, want 0", len(msgs))
	}

	time.Sleep(delay + 100*time.Millisecond)
	if got := receiveOne(t, q, events.QueuePackage); got.ReceiveCount != 1 {
		t.Errorf("ReceiveCount = %d, want 1", got.ReceiveCount)
	}
}

// --- 8. Stats ---

func TestStatsReportsVisibleAndInFlight(t *testing.T) {
	q := newTestQueue(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := q.Send(ctx, events.QueueValidate, testStage("job-stats"), 0); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	if err := q.Send(ctx, events.QueueExtract, testStage("job-stats-extract"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// A delayed message is not claimable, so it must not count as visible.
	if err := q.Send(ctx, events.QueueExtract, testStage("job-stats-delayed"), time.Hour); err != nil {
		t.Fatalf("Send delayed: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // let the oldest message accrue a measurable age

	byQueue := statsByQueue(t, q)
	if got := byQueue[events.QueueValidate]; got.Visible != 3 || got.InFlight != 0 {
		t.Errorf("validate stats = %+v, want Visible=3 InFlight=0", got)
	}
	if got := byQueue[events.QueueExtract]; got.Visible != 1 || got.InFlight != 0 {
		t.Errorf("extract stats = %+v, want Visible=1 InFlight=0 (delayed message excluded)", got)
	}
	if got := byQueue[events.QueueValidate]; got.Oldest <= 0 {
		t.Errorf("validate Oldest = %v, want a positive backlog age", got.Oldest)
	}

	// Claiming moves one message from visible to in-flight.
	claimed := receiveOne(t, q, events.QueueValidate)
	byQueue = statsByQueue(t, q)
	if got := byQueue[events.QueueValidate]; got.Visible != 2 || got.InFlight != 1 {
		t.Errorf("validate stats after claim = %+v, want Visible=2 InFlight=1", got)
	}

	// Acking removes it from both.
	if err := q.Ack(ctx, claimed); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	byQueue = statsByQueue(t, q)
	if got := byQueue[events.QueueValidate]; got.Visible != 2 || got.InFlight != 0 {
		t.Errorf("validate stats after ack = %+v, want Visible=2 InFlight=0", got)
	}

	// Nacking returns an in-flight message to visible.
	claimed = receiveOne(t, q, events.QueueValidate)
	if err := q.Nack(ctx, claimed, 0); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	byQueue = statsByQueue(t, q)
	if got := byQueue[events.QueueValidate]; got.Visible != 2 || got.InFlight != 0 {
		t.Errorf("validate stats after nack = %+v, want Visible=2 InFlight=0", got)
	}

	// An empty queue reports nothing rather than a zero row.
	if _, ok := byQueue[events.QueueDLQ]; ok {
		t.Error("stats included the empty DLQ")
	}
}

func statsByQueue(t *testing.T, q *SQLiteQueue) map[string]QueueStat {
	t.Helper()
	stats, err := q.Stats(context.Background())
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	out := make(map[string]QueueStat, len(stats))
	for _, s := range stats {
		out[s.Queue] = s
	}
	return out
}

// --- Long polling ---

func TestReceiveLongPollsUntilMessageArrives(t *testing.T) {
	q := newTestQueue(t, func(o *Options) { o.PollInterval = 20 * time.Millisecond })
	ctx := context.Background()

	go func() {
		time.Sleep(80 * time.Millisecond)
		if err := q.Send(ctx, events.QueueValidate, testStage("job-longpoll"), 0); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()

	start := time.Now()
	msgs, err := q.Receive(ctx, events.QueueValidate, 1, 2*time.Second)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("long poll took %v, want it to return promptly after the send", elapsed)
	}
}

func TestReceiveReturnsEmptyWhenWaitElapses(t *testing.T) {
	q := newTestQueue(t, func(o *Options) { o.PollInterval = 20 * time.Millisecond })

	start := time.Now()
	msgs, err := q.Receive(context.Background(), events.QueueValidate, 1, 150*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("got %d messages from an empty queue, want 0", len(msgs))
	}
	if elapsed < 150*time.Millisecond {
		t.Errorf("Receive returned after %v, want it to wait the full 150ms", elapsed)
	}
	if elapsed > time.Second {
		t.Errorf("Receive waited %v, want it to give up near the deadline", elapsed)
	}
}

func TestReceiveRespectsContextCancellation(t *testing.T) {
	q := newTestQueue(t, func(o *Options) { o.PollInterval = 20 * time.Millisecond })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := q.Receive(ctx, events.QueueValidate, 1, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Receive error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Receive took %v to notice cancellation", elapsed)
	}
}

// --- Persistence ---

// TestMessagesSurviveReopen is the difference between this and an in-memory
// channel: a restart of the API or a worker must not lose queued videos.
func TestMessagesSurviveReopen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "queue.db")
	ctx := context.Background()

	first, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.Send(ctx, events.QueueValidate, testStage("job-durable"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	got := receiveOne(t, second, events.QueueValidate)
	if got.Stage.JobID != "job-durable" {
		t.Errorf("JobID = %q, want %q", got.Stage.JobID, "job-durable")
	}
}

func TestOpenRequiresPath(t *testing.T) {
	if _, err := Open(Options{}); err == nil {
		t.Fatal("Open with no path returned nil error")
	}
}

func TestOptionDefaults(t *testing.T) {
	q := newTestQueue(t, func(o *Options) {
		o.VisibilityTimeout = 0
		o.PollInterval = 0
		o.MaxDeliveries = 0
		o.DLQName = ""
	})

	if q.opts.VisibilityTimeout != defaultVisibilityTimeout {
		t.Errorf("VisibilityTimeout = %v, want %v", q.opts.VisibilityTimeout, defaultVisibilityTimeout)
	}
	if q.opts.PollInterval != defaultPollInterval {
		t.Errorf("PollInterval = %v, want %v", q.opts.PollInterval, defaultPollInterval)
	}
	if q.MaxDeliveries() != defaultMaxDeliveries {
		t.Errorf("MaxDeliveries = %d, want %d", q.MaxDeliveries(), defaultMaxDeliveries)
	}
	if q.DLQName() != events.QueueDLQ {
		t.Errorf("DLQName = %q, want %q", q.DLQName(), events.QueueDLQ)
	}
}
