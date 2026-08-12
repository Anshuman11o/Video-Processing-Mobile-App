package worker

import (
	"context"
	"errors"
	"log"

	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/queue"
	"github.com/anshumanagarwal/dayreel/internal/storage"
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
			log.Printf("worker[%s] receive: %v", r.stage.Name(), err)
			continue
		}

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

	if err := r.db.SetStageRunning(ctx, msg.JobID, stageName); err != nil {
		// The job row is the only record that work started. Losing that write
		// is transient — let the message come back rather than processing a job
		// whose state says nothing is happening.
		log.Printf("worker[%s] job=%s mark running: %v", stageName, msg.JobID, err)
		return
	}

	outputKey := r.stage.OutputKey(msg.JobID)

	done, err := r.storage.ObjectExists(ctx, r.stage.OutputBucket(), outputKey)
	if err != nil {
		log.Printf("worker[%s] job=%s idempotency check: %v", stageName, msg.JobID, err)
		return
	}

	if done {
		// Output already exists, so the media work is finished. Fall through to
		// completion rather than returning: a crash between uploading the
		// output and publishing downstream would otherwise leave the pipeline
		// stalled forever, with the next stage never told to start.
		log.Printf("worker[%s] job=%s output exists, skipping work", stageName, msg.JobID)
	} else {
		outputKey, err = r.stage.Process(ctx, msg)
		if err != nil {
			r.fail(ctx, m, msg, err)
			return
		}
	}

	if err := r.db.SetStageCompleted(ctx, msg.JobID, stageName, outputKey); err != nil {
		log.Printf("worker[%s] job=%s mark completed: %v", stageName, msg.JobID, err)
		return
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
