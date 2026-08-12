// Package queue is DayReel's message broker, with two interchangeable drivers
// behind one interface: a durable, at-least-once queue backed by a single
// SQLite file (the default), and Amazon SQS.
//
// The SQLite driver came first and took over from SQS, implementing the subset
// of SQS semantics the pipeline actually depends on: visibility timeouts,
// receipt handles, redelivery counts, delayed delivery, long polling and a
// dead-letter queue. Keeping the vocabulary that close to SQS is what made
// bringing SQS back a driver, not a rewrite — QUEUE_DRIVER picks between them
// and nothing above this package knows which one is running. See
// [FromConfig] for the selection, and CONTEXT.md for where the two genuinely
// differ.
//
// Two "message" types meet in this package and must not be confused:
//
//   - events.StageMessage is the payload. It is owned by the producer, its wire
//     format is a contract between pipeline stages, and the queue only ever
//     stores and returns it verbatim.
//   - queue.Message is the envelope. It is owned by the broker and describes the
//     delivery: which lease you hold, how many times this payload has been
//     handed out, when it was enqueued. Workers use it for acking and for
//     metrics; they never persist it.
package queue

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/events"
)

// ErrLeaseLost is returned by Ack, Nack, Heartbeat and DeadLetter when the
// receipt no longer matches the stored one, which means the visibility timeout
// expired and another worker has since claimed the message.
//
// It is a distinct sentinel rather than a silent no-op because "my work was
// thrown away" is exactly the condition a worker must be able to see: swallowing
// it would make duplicate processing invisible, and treating it as a generic
// error would make it look like the queue is broken when it is behaving
// correctly. Callers should log it and drop the work they just finished — the
// new lease holder is now responsible for that message.
//
// Every driver must produce it for the same condition. SQS says the same thing
// with a ReceiptHandleIsInvalid (or a bare InvalidParameterValue) API error, and
// the SQS driver translates those, so the runner's handling is identical on both
// brokers rather than "works on SQLite, mysterious API error on SQS".
var ErrLeaseLost = errors.New("queue: lease lost, receipt no longer valid")

// Message is the delivery envelope handed to a worker by Receive. It wraps the
// producer's payload with everything the worker needs to manage its lease and to
// emit useful metrics.
type Message struct {
	// ID is the SQLite rowid. It is stable for the lifetime of the message,
	// including a move to the DLQ.
	//
	// It is zero on any driver that does not number its messages — SQS hands
	// out a UUID — so nothing outside this package should format it directly.
	// Use [Message.Ident], which is the identifier whichever driver is running.
	ID int64

	// MessageID is the broker's own identifier for this message, as text: the
	// SQLite rowid rendered as a decimal, or the SQS MessageId. Like ID it is
	// stable across redeliveries, which is what makes it worth logging — every
	// attempt at the same video carries the same value.
	MessageID string

	// Queue is the queue this message was claimed from.
	Queue string

	// Receipt proves the holder still owns the lease, like an SQS
	// ReceiptHandle. It is regenerated on every delivery, so a receipt from an
	// expired lease can never mutate a message another worker now owns.
	Receipt string

	// ReceiveCount is how many times this message has been delivered, like SQS
	// ApproximateReceiveCount. It is what drives the dead-letter decision.
	ReceiveCount int

	// EnqueuedAt is when the producer sent the message (SQS SentTimestamp).
	// EnqueuedAt against ClaimedAt is the queue-wait metric.
	EnqueuedAt time.Time

	// ClaimedAt is when this worker took the lease. ClaimedAt against
	// completion time is the stage-duration metric.
	ClaimedAt time.Time

	// LeaseExpiresAt is when redelivery becomes possible. A worker that expects
	// to run past it must Heartbeat before it arrives.
	LeaseExpiresAt time.Time

	// Stage is the decoded payload.
	Stage *events.StageMessage
}

// Ident names this message in a log line or an error message, using whichever
// identifier the driver supplies.
//
// Callers used to format Message.ID with %d, which was correct while SQLite was
// the only driver and prints a useless "msg=0" for every SQS delivery. Going
// through Ident keeps the runner driver-agnostic: it never has to ask which
// broker is underneath to write a line an operator can grep for.
func (m Message) Ident() string {
	if m.MessageID != "" {
		return m.MessageID
	}
	return strconv.FormatInt(m.ID, 10)
}

// QueueWait reports how long the message sat on the queue before this delivery.
func (m Message) QueueWait() time.Duration {
	return m.ClaimedAt.Sub(m.EnqueuedAt)
}

// LeaseRemaining reports how much of the visibility timeout is left. A worker
// should Heartbeat well before this reaches zero; once it does, the message is
// claimable by anyone and this worker's Ack will fail with ErrLeaseLost.
func (m Message) LeaseRemaining() time.Duration {
	return time.Until(m.LeaseExpiresAt)
}

// QueueStat is a point-in-time snapshot of one queue's depth, the local
// equivalent of the SQS ApproximateNumberOfMessages* attributes.
type QueueStat struct {
	// Queue is the queue name.
	Queue string

	// Visible is the number of messages claimable right now.
	Visible int

	// InFlight is the number of messages leased to a worker and not yet acked.
	InFlight int

	// Oldest is the age of the oldest visible message, or zero when nothing is
	// visible. It is the backlog signal worth alerting on: depth alone does not
	// distinguish a busy queue from a stuck one.
	//
	// It is always zero on the SQS driver. SQS publishes the equivalent
	// (ApproximateAgeOfOldestMessage) as a CloudWatch metric and not as a queue
	// attribute, so there is no way to read it from a GetQueueAttributes call;
	// see CONTEXT.md.
	Oldest time.Duration
}

// Queue is the broker contract. Implementations must be safe for concurrent use
// by multiple goroutines, and Receive must claim messages atomically so that two
// workers can never hold a lease on the same message at the same time.
type Queue interface {
	// Send enqueues a payload, optionally invisible for delay first.
	Send(ctx context.Context, queue string, msg *events.StageMessage, delay time.Duration) error

	// Receive claims up to max messages, long-polling for up to wait before
	// giving up. It returns an empty slice (not an error) when nothing is
	// available.
	Receive(ctx context.Context, queue string, max int, wait time.Duration) ([]Message, error)

	// Ack deletes a message the worker finished. Returns ErrLeaseLost if the
	// lease expired first.
	Ack(ctx context.Context, m Message) error

	// Nack releases a lease early and makes the message claimable again after
	// backoff.
	Nack(ctx context.Context, m Message, backoff time.Duration) error

	// Heartbeat extends the lease by extend from now, for work that outlives
	// the default visibility timeout. It must not count as a redelivery.
	Heartbeat(ctx context.Context, m Message, extend time.Duration) error

	// DeadLetter moves a message to the dead-letter queue with a reason.
	DeadLetter(ctx context.Context, m Message, reason string) error

	// Stats returns one entry per non-empty queue, sorted by queue name.
	Stats(ctx context.Context) ([]QueueStat, error)

	// Close releases the underlying resources.
	Close() error
}
