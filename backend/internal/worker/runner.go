package worker

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/queue"
	"github.com/anshumanagarwal/dayreel/internal/storage"
)

const (
	// initialReceiveBackoff and maxReceiveBackoff bound the wait between failed
	// receives. A failing receive returns immediately, so without a backoff a
	// persistent error — a deleted queue file, a corrupt database, a disk that
	// filled, a queue that does not exist — turns the consume loop into a hot
	// loop hammering the broker as fast as the CPU allows. It costs differently
	// on each driver and is unacceptable on both: on SQLite every claim takes
	// the write lock, so a spinning worker starves the ones doing real work; on
	// SQS every failed receive is a billed request, which is how a broken queue
	// name turns into a bill overnight.
	initialReceiveBackoff = 1 * time.Second
	maxReceiveBackoff     = 30 * time.Second

	// receiveMax is how many messages a single receive claims. One at a time
	// keeps the failure story simple; parallelism comes from running more
	// worker processes.
	receiveMax = 1

	// heartbeatInterval is how often a running stage extends its message's
	// lease.
	//
	// The interval is deliberately much shorter than the extension it asks for
	// (the configured visibility timeout, five minutes by default): a heartbeat
	// that fails or is delayed still leaves several minutes of headroom before
	// the lease expires and the message is claimable again. Extending on a
	// ticker rather than asking for one enormous lease up front is what makes a
	// crashed worker recover quickly — heartbeats stop the moment the process
	// dies, so the message returns after one visibility timeout instead of
	// being held hostage for hours.
	heartbeatInterval = 30 * time.Second

	// initialNackBackoff and maxNackBackoff bound how long a transiently failed
	// message stays invisible before its next delivery.
	//
	// SQS had no equivalent knob: a message that was simply not deleted came
	// back after the queue's visibility timeout, always the same delay. Nack
	// takes an explicit backoff, so the retry can grow with the attempt number —
	// a dependency that is restarting is retried in seconds, and one that is
	// genuinely down is not hammered every ten seconds until the delivery budget
	// runs out.
	initialNackBackoff = 10 * time.Second
	maxNackBackoff     = 5 * time.Minute
)

// Stage is one step of the pipeline. Implementations do the media work and
// return the S3 key they produced; all queue, database and retry handling
// belongs to the Runner.
type Stage interface {
	Name() models.StageName

	// OutputKey is the key this stage will write for a job. The Runner checks
	// it before calling Process, so a redelivered message does not redo work.
	OutputKey(jobID string) string

	// OutputBucket is where OutputKey lives.
	OutputBucket() string

	// Process performs the stage's work and returns the key it wrote.
	// Unrecoverable conditions must be returned via Permanent.
	Process(ctx context.Context, msg *events.StageMessage) (string, error)
}

// Finalizer is an optional Stage capability: work to do once the stage's own
// state has been recorded as completed.
//
// Only the terminal stage implements it, to mark the job itself finished. It is
// separate from Stage rather than part of it so the other three stages are
// unaffected and the Stage interface stays narrow — Process does media work and
// returns a key, and everything about job lifecycle stays out of it.
//
// The runner calls this after SetStageCompleted and before publishing onward.
type Finalizer interface {
	Finalize(ctx context.Context, msg *events.StageMessage, outputKey string) error
}

// Runner consumes a queue and applies a Stage to each message.
type Runner struct {
	stage   Stage
	queue   queue.Queue
	db      *db.DynamoDBClient
	storage *storage.S3Client

	queueName string

	// pollWait is how long one Receive blocks before returning empty, and
	// maxDeliveries is the redelivery budget a message gets before this runner
	// dead-letters it. Both come from config so every stage agrees with the
	// broker about them.
	pollWait      time.Duration
	maxDeliveries int

	// leaseExtension is how far ahead each heartbeat pushes the lease. It is the
	// broker's own visibility timeout, so a heartbeat buys exactly as much time
	// as a fresh delivery would.
	leaseExtension time.Duration
}

// NewRunner wires a Runner for the given stage.
func NewRunner(
	stage Stage, q queue.Queue,
	database *db.DynamoDBClient, s3 *storage.S3Client,
	cfg *config.Config,
) *Runner {
	return &Runner{
		stage:          stage,
		queue:          q,
		db:             database,
		storage:        s3,
		queueName:      events.QueueForStage(stage.Name()),
		pollWait:       cfg.QueuePollInterval,
		maxDeliveries:  cfg.QueueMaxDeliveries,
		leaseExtension: cfg.QueueVisibilityTimeout,
	}
}

// Run consumes until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	log.Printf("worker[%s] consuming %s (max %d deliveries, %s lease)",
		r.stage.Name(), r.queueName, r.maxDeliveries, r.leaseExtension)

	backoff := initialReceiveBackoff

	for {
		if ctx.Err() != nil {
			log.Printf("worker[%s] shutting down", r.stage.Name())
			return nil
		}

		msgs, err := r.queue.Receive(ctx, r.queueName, receiveMax, r.pollWait)
		if err != nil {
			// A cancelled context surfaces here as a receive error; that is a
			// clean shutdown, not a failure.
			if ctx.Err() != nil {
				log.Printf("worker[%s] shutting down", r.stage.Name())
				return nil
			}

			// Back off before retrying, so a persistent broker error stays cheap
			// while a blip still recovers quickly. The backoff grows to a
			// ceiling rather than without bound: an outage that ends must not
			// leave the pipeline idle for an hour afterwards.
			log.Printf("worker[%s] receive (retry in %s): %v", r.stage.Name(), backoff, err)
			select {
			case <-ctx.Done():
				log.Printf("worker[%s] shutting down", r.stage.Name())
				return nil
			case <-time.After(backoff):
			}
			if backoff < maxReceiveBackoff {
				backoff *= 2
			}
			continue
		}

		backoff = initialReceiveBackoff

		for _, m := range msgs {
			r.handle(ctx, m)
		}
	}
}

// handle processes one message and decides its fate on the queue.
//
// Acking the message is what marks it done. Anything left un-acked becomes
// claimable again when the lease expires, which is precisely how transient
// failures get retried — so the early returns below that skip the ack are
// deliberate, not oversights. The delivery-budget check at the top is what stops
// those paths retrying forever: a message that keeps coming back eventually
// arrives with its budget spent and is dead-lettered before any work is done.
func (r *Runner) handle(ctx context.Context, m queue.Message) {
	stageName := r.stage.Name()

	msg := m.Stage
	if msg == nil {
		// Both drivers decode the payload before handing the envelope over, so
		// this cannot happen with either of them today. It is guarded anyway
		// because Queue is an interface: a driver that returned an empty
		// envelope would otherwise take the whole worker down on a nil deref.
		log.Printf("worker[%s] discarding message %s with no payload", stageName, m.Ident())
		r.deadLetter(ctx, m, "message carried no stage payload")
		return
	}

	// Redrive used to belong to the queue: SQS moved a message to the DLQ once
	// its receive count passed the redrive policy's maxReceiveCount, and the
	// runner never had to think about it. Neither driver is relied on for that
	// now — only the worker knows whether a failure is worth another attempt —
	// so the budget is enforced here instead, for both. The redrive policy the
	// setup script writes on the real SQS queues is a safety net for messages
	// that escape this loop entirely, not the mechanism.
	//
	// The check runs on pickup rather than only on failure because most of the
	// ways this stage gives up do not go through fail() at all: a failed
	// SetStageRunning, a failed Finalize, a worker killed mid-stage. Those
	// simply let the lease expire, and without this they would retry forever.
	if m.ReceiveCount > r.maxDeliveries {
		log.Printf("worker[%s] job=%s delivery %d exceeds the budget of %d, dead-lettering",
			stageName, msg.JobID, m.ReceiveCount, r.maxDeliveries)
		r.giveUp(ctx, m, msg.JobID, fmt.Sprintf(
			"exceeded %d deliveries without completing stage %s", r.maxDeliveries, stageName))
		return
	}

	// The delivery count is the attempt number, not whatever the producer wrote
	// in the body: the producer only ever knows about its own first attempt.
	if m.ReceiveCount > 0 {
		msg.Attempt = m.ReceiveCount
	}

	log.Printf("worker[%s] job=%s attempt=%d key=%s", stageName, msg.JobID, msg.Attempt, msg.Input.Key)

	outputKey := r.stage.OutputKey(msg.JobID)

	done, err := r.storage.ObjectExists(ctx, r.stage.OutputBucket(), outputKey)
	if err != nil {
		log.Printf("worker[%s] job=%s idempotency check: %v", stageName, msg.JobID, err)
		return
	}

	if done {
		// The output exists, but that alone does not say whether this stage
		// finished. Two situations produce it, and they need opposite handling:
		//
		//   1. A pure duplicate delivery of work already completed. Publishing
		//      again would enqueue the next stage twice for every redelivery.
		//   2. A crash after uploading the output but before recording
		//      completion. Here the next stage was never told to start, so
		//      returning early would stall the pipeline permanently.
		//
		// The recorded stage state distinguishes them, which is why this check
		// runs before SetStageRunning — marking the stage running first would
		// overwrite the very evidence needed here.
		alreadyDone, err := r.stageAlreadyCompleted(ctx, msg.JobID)
		if err != nil {
			log.Printf("worker[%s] job=%s completion check: %v", stageName, msg.JobID, err)
			return
		}
		if alreadyDone {
			log.Printf("worker[%s] job=%s already completed, dropping duplicate", stageName, msg.JobID)
			r.ack(ctx, m, msg)
			return
		}
		log.Printf("worker[%s] job=%s output exists but stage unrecorded, resuming", stageName, msg.JobID)
	}

	if err := r.db.SetStageRunning(ctx, msg.JobID, stageName); err != nil {
		// The job row is the only record that work started. Losing that write
		// is transient — let the message come back rather than processing a job
		// whose state says nothing is happening.
		log.Printf("worker[%s] job=%s mark running: %v", stageName, msg.JobID, err)
		return
	}

	if !done {
		// Keep the lease alive for as long as the work actually takes. Without
		// this, a stage slower than the visibility timeout has its message
		// re-claimed mid-flight and a second worker starts the same job. The
		// idempotency guard above cannot catch that: it asks whether the output
		// exists, and the output does not exist until Process returns.
		stopHeartbeat := r.heartbeat(ctx, m)
		started := time.Now()
		outputKey, err = r.stage.Process(ctx, msg)
		elapsed := time.Since(started)
		stopHeartbeat()

		if err != nil {
			r.fail(ctx, m, msg, err)
			return
		}

		// Best-effort: a lost metric must never fail work that succeeded.
		if err := r.db.SetStageDuration(ctx, msg.JobID, stageName, elapsed.Milliseconds()); err != nil {
			log.Printf("worker[%s] job=%s record duration: %v", stageName, msg.JobID, err)
		}
	}

	if err := r.db.SetStageCompleted(ctx, msg.JobID, stageName, outputKey); err != nil {
		log.Printf("worker[%s] job=%s mark completed: %v", stageName, msg.JobID, err)
		return
	}

	// Finalize runs after the stage row is marked completed, never before. The
	// terminal stage uses this to mark the job itself finished, and the opposite
	// order would let a job claim completion while its own stage still said
	// running — permanently, if the process died in between.
	//
	// Not acking the message on failure is deliberate: a job whose final state
	// was never recorded should be retried, and the idempotency guard makes the
	// retry cheap.
	if f, ok := r.stage.(Finalizer); ok {
		if err := f.Finalize(ctx, msg, outputKey); err != nil {
			log.Printf("worker[%s] job=%s finalize: %v", stageName, msg.JobID, err)
			return
		}
	}

	// Record completion before publishing. The other order lets the next stage
	// start against a job whose state still claims this stage is running.
	if err := r.publishNext(ctx, msg, outputKey); err != nil {
		log.Printf("worker[%s] job=%s publish next: %v", stageName, msg.JobID, err)
		return
	}

	r.ack(ctx, m, msg)
	log.Printf("worker[%s] job=%s done output=%s", stageName, msg.JobID, outputKey)
}

// heartbeat keeps a message's lease alive while Process runs, and returns a
// function that stops it. The returned stop is safe to call exactly once, and
// callers must call it — leaving the goroutine running would keep extending a
// message that is already finished.
func (r *Runner) heartbeat(ctx context.Context, m queue.Message) func() {
	return heartbeatLoop(ctx, heartbeatInterval, func() {
		// Deliberately uses ctx, not the loop's own context: this call must not
		// be cancelled by the stop that happens the instant Process returns.
		err := r.queue.Heartbeat(ctx, m, r.leaseExtension)
		switch {
		case errors.Is(err, queue.ErrLeaseLost):
			// The lease is already gone, so another worker owns this message and
			// is redoing the job. Nothing here can reclaim it; the ack at the end
			// of the stage will report the same thing. Logged separately because
			// it means the heartbeat was too slow, which is a tuning problem and
			// not a queue fault.
			log.Printf("worker[%s] msg=%s heartbeat: lease already lost", r.stage.Name(), m.Ident())
		case err != nil:
			// Not fatal on its own: if the extension genuinely failed the worst
			// case is a redelivery the idempotency guard handles.
			log.Printf("worker[%s] msg=%s heartbeat: %v", r.stage.Name(), m.Ident(), err)
		}
	})
}

// heartbeatLoop calls beat every interval until the returned stop is called.
//
// Split from the queue call so the lifecycle — which is the part with the
// concurrency bugs in it — can be tested without a broker. stop blocks until the
// goroutine has exited, so no beat can land after Process has returned and the
// message has been acked.
func heartbeatLoop(ctx context.Context, interval time.Duration, beat func()) func() {
	beatCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-beatCtx.Done():
				return
			case <-ticker.C:
				beat()
			}
		}
	}()

	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}

// stageAlreadyCompleted reports whether this stage is recorded as completed for
// the job. A missing job is treated as not completed: the message is then
// handled normally and fails on its own terms rather than being silently
// dropped here.
func (r *Runner) stageAlreadyCompleted(ctx context.Context, jobID string) (bool, error) {
	job, err := r.db.GetJob(ctx, jobID)
	if err != nil {
		return false, err
	}
	if job == nil {
		return false, nil
	}

	state, ok := job.Stages[r.stage.Name()]
	if !ok || state == nil {
		return false, nil
	}
	return state.Status == models.StageStatusCompleted, nil
}

// retryAction is what the runner does with the message behind a failed stage.
type retryAction int

const (
	// retryLater releases the lease so the message comes back after a backoff.
	retryLater retryAction = iota

	// giveUp records the failure and parks the message on the DLQ.
	giveUp
)

// retryActionFor applies the retry policy to one failure.
//
// It is a pure function, separate from the queue calls, because it is the part
// of the port that is easy to get wrong and impossible to notice: an off-by-one
// here either burns a delivery that should have retried or retries a video that
// can never succeed, and neither shows up as an error anywhere.
//
// The permanent-versus-transient split now chooses between two queue operations,
// where under SQS it chose between deleting the message and doing nothing:
//
//   - Permanent — a corrupt file, a disallowed codec — cannot succeed on retry,
//     so it gives up regardless of how many deliveries are left. The remaining
//     attempts would fail identically and land on the DLQ anyway, several
//     visibility timeouts later, carrying nothing the first attempt did not.
//   - Transient — a throttle, a network blip, a restarting dependency — retries
//     until the delivery budget is spent. Giving up on the last delivery of the
//     budget rather than nacking one more time is deliberate: that extra
//     delivery would be rejected on sight by the budget check in handle, so it
//     would cost a redelivery and lose the failure that is actually worth
//     reading off the DLQ.
func retryActionFor(err error, receiveCount, maxDeliveries int) retryAction {
	if IsPermanent(err) {
		return giveUp
	}
	if receiveCount >= maxDeliveries {
		return giveUp
	}
	return retryLater
}

// fail records a stage failure and decides whether the message should retry.
func (r *Runner) fail(ctx context.Context, m queue.Message, msg *events.StageMessage, err error) {
	stageName := r.stage.Name()

	if retryActionFor(err, m.ReceiveCount, r.maxDeliveries) == retryLater {
		// The stage stays "running" in DynamoDB, which is accurate — it will be
		// retried.
		backoff := nackBackoff(m.ReceiveCount)
		log.Printf("worker[%s] job=%s transient failure on delivery %d of %d, retry in %s: %v",
			stageName, msg.JobID, m.ReceiveCount, r.maxDeliveries, backoff, err)
		r.nack(ctx, m, backoff)
		return
	}

	if IsPermanent(err) {
		log.Printf("worker[%s] job=%s permanent failure: %v", stageName, msg.JobID, err)
	} else {
		log.Printf("worker[%s] job=%s transient failure exhausted %d deliveries: %v",
			stageName, msg.JobID, r.maxDeliveries, err)
	}
	r.giveUp(ctx, m, msg.JobID, err.Error())
}

// giveUp records the stage as failed and dead-letters the message.
//
// The database write comes first, and its failure changes the outcome: a job
// left "running" with its message on the DLQ is a job nothing will ever move
// again, and the recorded stage state is the only way the API can tell the
// client it is over. So a failed write nacks instead — the message comes back,
// this runs again, and DynamoDB may well be answering by then. It cannot loop
// forever: the delivery budget check in handle dead-letters the message on
// sight once the budget is spent, having made one last attempt to record it.
func (r *Runner) giveUp(ctx context.Context, m queue.Message, jobID, reason string) {
	stageName := r.stage.Name()

	if err := r.db.SetStageFailed(ctx, jobID, stageName, reason); err != nil {
		log.Printf("worker[%s] job=%s mark failed: %v", stageName, jobID, err)
		if m.ReceiveCount <= r.maxDeliveries {
			r.nack(ctx, m, nackBackoff(m.ReceiveCount))
			return
		}
		// Out of deliveries. The message has to leave the queue even though the
		// job's own state is now wrong, because the alternative is redelivering
		// it forever.
		log.Printf("worker[%s] job=%s dead-lettering with the stage still marked running",
			stageName, jobID)
	}

	r.deadLetter(ctx, m, reason)
}

// nackBackoff doubles the delay with each delivery, to a ceiling.
func nackBackoff(receiveCount int) time.Duration {
	backoff := initialNackBackoff
	for i := 1; i < receiveCount; i++ {
		if backoff >= maxNackBackoff {
			return maxNackBackoff
		}
		backoff *= 2
	}
	if backoff > maxNackBackoff {
		return maxNackBackoff
	}
	return backoff
}

func (r *Runner) publishNext(ctx context.Context, msg *events.StageMessage, outputKey string) error {
	next := events.NextStage(r.stage.Name())
	if next == "" {
		return nil // last stage in the pipeline
	}

	nextQueue := events.NextQueue(r.stage.Name())
	if nextQueue == "" {
		return errors.New("next stage has no queue")
	}

	return r.queue.Send(ctx, nextQueue, events.NewStageMessage(
		msg.JobID,
		next,
		events.S3Ref{Bucket: r.stage.OutputBucket(), Key: outputKey},
		1, // attempt resets for the next stage
		msg.TraceID,
	), 0)
}

// ack marks the message done.
//
// Ack is not the fire-and-forget that SQS DeleteMessage was: it matches on the
// receipt, so a worker whose lease expired mid-stage cannot delete a message
// another worker now owns, and gets ErrLeaseLost instead. That is not a stage
// failure — the output is written and the job state is recorded — but the other
// worker is redoing (or has redone) the same job, so it is worth seeing rather
// than swallowing. There is nothing to retry: the message is no longer ours.
func (r *Runner) ack(ctx context.Context, m queue.Message, msg *events.StageMessage) {
	jobID := "?"
	if msg != nil {
		jobID = msg.JobID
	}

	err := r.queue.Ack(ctx, m)
	switch {
	case errors.Is(err, queue.ErrLeaseLost):
		log.Printf("worker[%s] job=%s msg=%s lease lost before ack: this stage ran longer "+
			"than the lease and another worker has claimed the message; its work is the one that counts",
			r.stage.Name(), jobID, m.Ident())
	case err != nil:
		// The work is done; a failed ack just means one redelivery, which the
		// idempotency guard absorbs.
		log.Printf("worker[%s] job=%s ack message %s: %v", r.stage.Name(), jobID, m.Ident(), err)
	}
}

// nack releases the lease early so the message is redelivered after backoff,
// rather than after the whole visibility timeout.
func (r *Runner) nack(ctx context.Context, m queue.Message, backoff time.Duration) {
	err := r.queue.Nack(ctx, m, backoff)
	switch {
	case errors.Is(err, queue.ErrLeaseLost):
		log.Printf("worker[%s] msg=%s lease lost before nack; another worker is already retrying it",
			r.stage.Name(), m.Ident())
	case err != nil:
		// Harmless in isolation: the lease expires on its own and the message
		// comes back anyway, just later than the backoff asked for.
		log.Printf("worker[%s] nack message %s: %v", r.stage.Name(), m.Ident(), err)
	}
}

// deadLetter parks a message on the DLQ with the reason it got there.
func (r *Runner) deadLetter(ctx context.Context, m queue.Message, reason string) {
	err := r.queue.DeadLetter(ctx, m, reason)
	switch {
	case errors.Is(err, queue.ErrLeaseLost):
		log.Printf("worker[%s] msg=%s lease lost before dead-letter; another worker owns it now",
			r.stage.Name(), m.Ident())
	case err != nil:
		// The message stays on its own queue and will be delivered again, where
		// the budget check rejects it on sight and tries this again.
		log.Printf("worker[%s] dead-letter message %s: %v", r.stage.Name(), m.Ident(), err)
	}
}
