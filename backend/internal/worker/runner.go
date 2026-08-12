package worker

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/queue"
	"github.com/anshumanagarwal/dayreel/internal/storage"
)

const (
	// initialReceiveBackoff and maxReceiveBackoff bound the wait between failed
	// receives. Every SQS request is billed, so an unattended worker must not
	// be able to spin on a persistent error.
	initialReceiveBackoff = 1 * time.Second
	maxReceiveBackoff     = 30 * time.Second

	// heartbeatInterval is how often a running stage extends its message's
	// visibility, and heartbeatVisibilitySeconds is how far ahead each extension
	// pushes it.
	//
	// The interval is deliberately much shorter than the extension: a heartbeat
	// that fails or is delayed still leaves several minutes of headroom before
	// SQS redelivers. The extension is deliberately short enough that a crashed
	// worker's message returns in minutes rather than being held hostage.
	heartbeatInterval          = 30 * time.Second
	heartbeatVisibilitySeconds = 300
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
	queue   *queue.Client
	db      *db.DynamoDBClient
	storage *storage.S3Client

	queueName string
}

// NewRunner wires a Runner for the given stage.
func NewRunner(stage Stage, q *queue.Client, database *db.DynamoDBClient, s3 *storage.S3Client) *Runner {
	return &Runner{
		stage:     stage,
		queue:     q,
		db:        database,
		storage:   s3,
		queueName: events.QueueForStage(stage.Name()),
	}
}

// Run consumes until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) error {
	log.Printf("worker[%s] consuming %s", r.stage.Name(), r.queueName)

	backoff := initialReceiveBackoff

	for {
		if ctx.Err() != nil {
			log.Printf("worker[%s] shutting down", r.stage.Name())
			return nil
		}

		msgs, err := r.queue.Receive(ctx, r.queueName)
		if err != nil {
			// A cancelled context surfaces here as a receive error; that is a
			// clean shutdown, not a failure.
			if ctx.Err() != nil {
				log.Printf("worker[%s] shutting down", r.stage.Name())
				return nil
			}

			// Back off before retrying. A successful receive blocks for the
			// long-poll duration, but a failing one returns immediately, so
			// looping straight back round turns any persistent error — bad
			// credentials, a throttle, a deleted queue — into a hot loop
			// issuing SQS calls as fast as the network allows. That is billed
			// per request and would also flood CloudWatch. The backoff grows
			// to a ceiling so a long outage stays cheap while a blip still
			// recovers quickly.
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
// Deleting the message is what marks it done. Anything left undeleted becomes
// visible again when the visibility timeout expires, which is precisely how
// transient failures get retried — so the early returns below that skip the
// delete are deliberate, not oversights.
func (r *Runner) handle(ctx context.Context, m queue.Message) {
	stageName := r.stage.Name()

	msg, err := events.NormalizeMessage([]byte(m.Body), stageName, m.ReceiveCount)
	if err != nil {
		// An unparseable message will never parse. Retrying it three times to
		// reach the DLQ tells us nothing extra, and with the S3 suffix filter
		// removed this is the expected fate of any stray object written to the
		// raw bucket.
		log.Printf("worker[%s] discarding unusable message: %v", stageName, err)
		r.delete(ctx, m)
		return
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
			r.delete(ctx, m)
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
		// Keep the message invisible for as long as the work actually takes.
		// Without this, a stage slower than the queue's visibility timeout gets
		// its message redelivered mid-flight and a second worker starts the same
		// job. The idempotency guard above cannot catch that: it asks whether the
		// output exists, and the output does not exist until Process returns.
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
	// Not deleting the message on failure is deliberate: a job whose final state
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

	r.delete(ctx, m)
	log.Printf("worker[%s] job=%s done output=%s", stageName, msg.JobID, outputKey)
}

// heartbeat keeps a message invisible while Process runs, and returns a function
// that stops it. The returned stop is safe to call exactly once, and callers
// must call it — leaving the goroutine running would keep extending a message
// that is already finished.
//
// Extending on a ticker rather than setting one long timeout up front is what
// makes a crashed worker recover quickly: heartbeats stop the moment the process
// dies, so the message becomes visible again after one interval instead of after
// the full extension.
func (r *Runner) heartbeat(ctx context.Context, m queue.Message) func() {
	return heartbeatLoop(ctx, heartbeatInterval, func() {
		// Deliberately uses ctx, not the loop's own context: this call must not
		// be cancelled by the stop that happens the instant Process returns.
		if err := r.queue.ExtendVisibility(
			ctx, r.queueName, m.ReceiptHandle, heartbeatVisibilitySeconds,
		); err != nil {
			// Not fatal on its own. The message may simply have been deleted
			// already, and if the extension genuinely failed the worst case is a
			// redelivery the idempotency guard handles.
			log.Printf("worker[%s] heartbeat: %v", r.stage.Name(), err)
		}
	})
}

// heartbeatLoop calls beat every interval until the returned stop is called.
//
// Split from the queue call so the lifecycle — which is the part with the
// concurrency bugs in it — can be tested without an SQS client. stop blocks
// until the goroutine has exited, so no beat can land after Process has
// returned and the message has been deleted.
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

// fail records a stage failure and decides whether the message should retry.
func (r *Runner) fail(ctx context.Context, m queue.Message, msg *events.StageMessage, err error) {
	stageName := r.stage.Name()

	if !IsPermanent(err) {
		// Transient: leave the message alone so SQS redelivers it after the
		// visibility timeout. The stage stays "running" in DynamoDB, which is
		// accurate — it will be retried.
		log.Printf("worker[%s] job=%s transient failure, will retry: %v", stageName, msg.JobID, err)
		return
	}

	log.Printf("worker[%s] job=%s permanent failure: %v", stageName, msg.JobID, err)

	if dbErr := r.db.SetStageFailed(ctx, msg.JobID, stageName, err.Error()); dbErr != nil {
		// Could not record the failure. Retry rather than delete, otherwise the
		// job is stuck "running" forever with nothing left to move it.
		log.Printf("worker[%s] job=%s mark failed: %v", stageName, msg.JobID, dbErr)
		return
	}

	r.delete(ctx, m)
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

	return r.queue.Publish(ctx, nextQueue, events.NewStageMessage(
		msg.JobID,
		next,
		events.S3Ref{Bucket: r.stage.OutputBucket(), Key: outputKey},
		1, // attempt resets for the next stage
		msg.TraceID,
	))
}

func (r *Runner) delete(ctx context.Context, m queue.Message) {
	if err := r.queue.Delete(ctx, r.queueName, m.ReceiptHandle); err != nil {
		// The work is done; a failed delete just means one redelivery, which
		// the idempotency guard absorbs.
		log.Printf("worker[%s] delete message: %v", r.stage.Name(), err)
	}
}
