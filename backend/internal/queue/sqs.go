package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"

	"github.com/anshumanagarwal/dayreel/internal/events"
)

// SQS's own ceilings. Every one of these is a hard API limit, not a tuning
// choice: exceeding any of them is an InvalidParameterValue, not a clamp on
// SQS's side. They are enforced here so a caller can pass the same arguments to
// either driver and get the nearest legal behaviour instead of an error the
// SQLite driver would never have produced.
const (
	// sqsMaxDelaySeconds is the ceiling on DelaySeconds. 15 minutes is as far
	// ahead as SQS will schedule a message; the SQLite driver has no such limit,
	// so a longer delay is silently shortened rather than rejected.
	sqsMaxDelaySeconds = 900

	// sqsMaxMessages is the ceiling on MaxNumberOfMessages for one receive.
	sqsMaxMessages = 10

	// sqsMaxWaitSeconds is the ceiling on WaitTimeSeconds, i.e. the longest
	// long poll SQS supports.
	sqsMaxWaitSeconds = 20

	// sqsMaxVisibilitySeconds is the ceiling on a visibility timeout, whether
	// set on the queue or via ChangeMessageVisibility. 12 hours.
	sqsMaxVisibilitySeconds = 43200

	// deadLetterReasonAttr carries the reason a message was dead-lettered.
	//
	// The SQLite driver has a dlq_reason column; SQS has no such thing, so the
	// reason rides as a message attribute. Anything draining the DLQ has to read
	// it from there, which is the one place a DLQ consumer must know which
	// driver produced the message.
	deadLetterReasonAttr = "DayReelDeadLetterReason"

	// deadLetterSourceAttr records which queue the message came from, which the
	// body does not say — StageMessage.Stage is the stage that was asked for,
	// not necessarily the queue that held it.
	deadLetterSourceAttr = "DayReelSourceQueue"
)

// SQSOptions configures an SQSQueue. As with Options, the zero value of every
// field falls back to a package default.
type SQSOptions struct {
	// Region is the AWS region holding the queues.
	Region string

	// VisibilityTimeout is only a fallback. The authoritative value is the
	// queue's own VisibilityTimeout attribute, which is what SQS actually
	// applies on receive; this is used for LeaseExpiresAt when that attribute
	// cannot be read, and as the default extension for a Heartbeat with no
	// explicit duration.
	VisibilityTimeout time.Duration

	// MaxDeliveries is the redelivery budget before a worker should
	// dead-letter. As with the SQLite driver the broker does not enforce it: it
	// is configured centrally so every stage agrees, and scripts/aws-sqs-setup.sh
	// writes the same number into each queue's redrive policy as a backstop.
	MaxDeliveries int

	// DLQName is the queue DeadLetter moves messages to.
	DLQName string

	// Queues is the set of queue names Stats reports on. SQS has no "list the
	// queues that have messages" call that is cheap and exact, so the set is
	// configured rather than discovered.
	Queues []string
}

func (o SQSOptions) withDefaults() SQSOptions {
	if o.VisibilityTimeout <= 0 {
		o.VisibilityTimeout = defaultVisibilityTimeout
	}
	if o.MaxDeliveries <= 0 {
		o.MaxDeliveries = defaultMaxDeliveries
	}
	if o.DLQName == "" {
		o.DLQName = events.QueueDLQ
	}
	if len(o.Queues) == 0 {
		o.Queues = []string{
			events.QueueValidate,
			events.QueueExtract,
			events.QueueTranscribe,
			events.QueuePackage,
			events.QueueDLQ,
		}
	}
	return o
}

// sqsAPI is the slice of the SQS client this driver uses. It exists so the
// driver depends on eight calls rather than the whole service, which is also
// what makes the request-shaping testable without a network.
type sqsAPI interface {
	GetQueueUrl(context.Context, *sqs.GetQueueUrlInput, ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error)
	GetQueueAttributes(context.Context, *sqs.GetQueueAttributesInput, ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error)
	SendMessage(context.Context, *sqs.SendMessageInput, ...func(*sqs.Options)) (*sqs.SendMessageOutput, error)
	ReceiveMessage(context.Context, *sqs.ReceiveMessageInput, ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(context.Context, *sqs.DeleteMessageInput, ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
	ChangeMessageVisibility(context.Context, *sqs.ChangeMessageVisibilityInput, ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error)
}

// queueRef is everything this driver has to resolve once per queue name.
//
// The URL is stable for the lifetime of the queue and the visibility timeout is
// a deployment-time decision, so both are looked up on first use and cached.
// Resolving them on every operation would double the request count — and on SQS
// every request is billed, which is not true of the SQLite driver.
type queueRef struct {
	url string

	// visibilityTimeout is the queue's own attribute, and is what SQS applies
	// when it hands a message out. It is the only way to know when a lease
	// expires: the receive response says nothing about it.
	visibilityTimeout time.Duration
}

// SQSQueue is a Queue backed by Amazon SQS. It is safe for concurrent use: the
// SQS client is, and the only shared state here is the queue-reference cache.
type SQSQueue struct {
	api  sqsAPI
	opts SQSOptions

	mu   sync.RWMutex
	refs map[string]queueRef
}

// compile-time check that the driver satisfies the broker contract.
var _ Queue = (*SQSQueue)(nil)

// OpenSQS builds a driver against real SQS in opts.Region.
//
// Credentials come from the SDK's default chain — environment, shared config
// file, or the instance/task role. There is no emulator to special-case: the
// LocalStack that used to stand in for SQS is gone, and this driver has only
// ever been pointed at a real account.
func OpenSQS(ctx context.Context, opts SQSOptions) (*SQSQueue, error) {
	opts = opts.withDefaults()

	loadOpts := []func(*awsconfig.LoadOptions) error{}
	if opts.Region != "" {
		loadOpts = append(loadOpts, awsconfig.WithRegion(opts.Region))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		return nil, fmt.Errorf("queue: load aws config: %w", err)
	}

	return newSQSQueue(sqs.NewFromConfig(awsCfg), opts), nil
}

// newSQSQueue wires a driver around an already-built API client.
func newSQSQueue(api sqsAPI, opts SQSOptions) *SQSQueue {
	return &SQSQueue{
		api:  api,
		opts: opts.withDefaults(),
		refs: make(map[string]queueRef),
	}
}

// MaxDeliveries reports the configured redelivery budget, matching the SQLite
// driver's accessor of the same name. Policy, not broker behaviour, so it is not
// part of the Queue interface.
func (q *SQSQueue) MaxDeliveries() int { return q.opts.MaxDeliveries }

// DLQName reports the dead-letter queue name.
func (q *SQSQueue) DLQName() string { return q.opts.DLQName }

// resolve returns the cached URL and visibility timeout for a queue name,
// looking them up on first use.
//
// Two calls, once per queue name per process. GetQueueUrl is the only way to
// turn a name into a URL without hardcoding the account ID into the config, and
// GetQueueAttributes is the only way to learn when a lease will expire.
func (q *SQSQueue) resolve(ctx context.Context, name string) (queueRef, error) {
	q.mu.RLock()
	ref, ok := q.refs[name]
	q.mu.RUnlock()
	if ok {
		return ref, nil
	}

	out, err := q.api.GetQueueUrl(ctx, &sqs.GetQueueUrlInput{QueueName: aws.String(name)})
	if err != nil {
		return queueRef{}, fmt.Errorf("queue: resolve %s: %w", name, err)
	}
	ref.url = aws.ToString(out.QueueUrl)

	// A failure here is not fatal. The configured timeout is a close-enough
	// stand-in for LeaseExpiresAt, and refusing to receive because an attribute
	// read failed would take the pipeline down over a metric.
	ref.visibilityTimeout = q.opts.VisibilityTimeout
	attrs, err := q.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
		QueueUrl:       aws.String(ref.url),
		AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameVisibilityTimeout},
	})
	if err == nil {
		if secs, convErr := strconv.Atoi(attrs.Attributes[string(sqstypes.QueueAttributeNameVisibilityTimeout)]); convErr == nil && secs > 0 {
			ref.visibilityTimeout = time.Duration(secs) * time.Second
		}
	}

	q.mu.Lock()
	q.refs[name] = ref
	q.mu.Unlock()
	return ref, nil
}

// Send enqueues msg on queue, invisible for delay first.
func (q *SQSQueue) Send(ctx context.Context, queue string, msg *events.StageMessage, delay time.Duration) error {
	if msg == nil {
		return fmt.Errorf("queue: send to %s: message is nil", queue)
	}
	body, err := encodeStage(msg)
	if err != nil {
		return fmt.Errorf("queue: marshal message for %s: %w", queue, err)
	}

	ref, err := q.resolve(ctx, queue)
	if err != nil {
		return err
	}

	_, err = q.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:     aws.String(ref.url),
		MessageBody:  aws.String(body),
		DelaySeconds: clampDelaySeconds(delay),
	})
	if err != nil {
		return fmt.Errorf("queue: send to %s: %w", queue, err)
	}
	return nil
}

// Receive claims up to max messages from queue, long-polling for up to wait.
//
// Unlike the SQLite driver this is a genuine server-side block: SQS holds the
// connection open until a message arrives or WaitTimeSeconds elapses, so there
// is no poll interval and an idle worker makes one request every 20 seconds
// rather than four a second. Long polling is not optional here — a receive with
// wait time 0 returns immediately, and the consume loop then burns both CPU and
// money against an empty queue.
func (q *SQSQueue) Receive(ctx context.Context, queue string, max int, wait time.Duration) ([]Message, error) {
	ref, err := q.resolve(ctx, queue)
	if err != nil {
		return nil, err
	}

	out, err := q.api.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(ref.url),
		MaxNumberOfMessages: clampMaxMessages(max),
		WaitTimeSeconds:     clampWaitSeconds(wait),
		// ApproximateReceiveCount is the attempt number and the input to every
		// dead-letter decision; SentTimestamp is the only source for
		// EnqueuedAt, and without it QueueWait() reads as the entire Unix epoch.
		// Neither is returned unless it is asked for by name.
		//
		// This is MessageSystemAttributeNames, not the deprecated AttributeNames
		// the pre-SQLite client used. That client had to use the old field
		// because LocalStack 3.0 ignored the new one and returned no system
		// attributes at all, which silently pinned every delivery's receive
		// count to 1 so redeliveries logged attempt=1 forever. LocalStack is
		// gone, real SQS honours the current field, and the two must not be set
		// together — SQS rejects that combination.
		MessageSystemAttributeNames: []sqstypes.MessageSystemAttributeName{
			sqstypes.MessageSystemAttributeNameApproximateReceiveCount,
			sqstypes.MessageSystemAttributeNameSentTimestamp,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("queue: receive from %s: %w", queue, err)
	}

	claimedAt := time.Now().UTC()
	msgs := make([]Message, 0, len(out.Messages))
	for _, raw := range out.Messages {
		m, err := envelope(raw, queue, claimedAt, ref.visibilityTimeout)
		if err != nil {
			// A body that will not decode can never be processed, so leaving it
			// on the queue means redelivering it until the redrive policy takes
			// it. Surface it rather than swallowing it, exactly as the SQLite
			// driver does.
			return nil, fmt.Errorf("queue: decode message from %s: %w", queue, err)
		}
		msgs = append(msgs, m)
	}
	return msgs, nil
}

// Ack deletes a finished message.
//
// DeleteMessage is the fire-and-forget the SQLite driver's Ack is not, with one
// exception: a receipt whose visibility timeout has already expired is rejected,
// and that rejection is precisely ErrLeaseLost.
func (q *SQSQueue) Ack(ctx context.Context, m Message) error {
	ref, err := q.resolve(ctx, m.Queue)
	if err != nil {
		return err
	}

	_, err = q.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(ref.url),
		ReceiptHandle: aws.String(m.Receipt),
	})
	return leaseError("ack", m, err)
}

// Nack releases the lease early by setting the message's visibility timeout to
// backoff, so it becomes claimable again then rather than at the end of the
// queue's full timeout.
func (q *SQSQueue) Nack(ctx context.Context, m Message, backoff time.Duration) error {
	return q.changeVisibility(ctx, "nack", m, backoff)
}

// Heartbeat pushes the lease out by extend from now.
//
// ChangeMessageVisibility sets the timeout from now, not from the original
// claim, so this is the same call Nack makes with a longer duration — and, as on
// the SQLite driver, it does not count as a redelivery: ApproximateReceiveCount
// only moves when SQS hands the message out again.
func (q *SQSQueue) Heartbeat(ctx context.Context, m Message, extend time.Duration) error {
	if extend <= 0 {
		extend = q.opts.VisibilityTimeout
	}
	return q.changeVisibility(ctx, "heartbeat", m, extend)
}

func (q *SQSQueue) changeVisibility(ctx context.Context, op string, m Message, d time.Duration) error {
	ref, err := q.resolve(ctx, m.Queue)
	if err != nil {
		return err
	}

	_, err = q.api.ChangeMessageVisibility(ctx, &sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(ref.url),
		ReceiptHandle:     aws.String(m.Receipt),
		VisibilityTimeout: clampVisibilitySeconds(d),
	})
	return leaseError(op, m, err)
}

// DeadLetter copies a message to the dead-letter queue and deletes it from the
// queue it came from.
//
// The redrive policy on the real queues does NOT do this job. Redrive only fires
// once a message has been delivered maxReceiveCount times, and the runner
// dead-letters permanent failures — a codec the pipeline rejects, a corrupt file
// — on the first delivery, with the whole budget unspent. Waiting for redrive
// would redeliver a video that can never succeed two more times, several
// visibility timeouts apart, before parking it. So the move is explicit, and the
// policy stays as a safety net for messages that never reach this call at all.
//
// Unlike the SQLite driver, where dead-lettering is one UPDATE of the queue
// column, this cannot be atomic: SQS has no move operation. Send comes first
// deliberately. If the delete then fails, the message is redelivered and may be
// dead-lettered twice — a duplicate on the DLQ, which is a queue nothing
// processes automatically. The other order risks deleting a message that was
// never copied, which loses the only record of the failure.
func (q *SQSQueue) DeadLetter(ctx context.Context, m Message, reason string) error {
	dlq, err := q.resolve(ctx, q.opts.DLQName)
	if err != nil {
		return err
	}
	src, err := q.resolve(ctx, m.Queue)
	if err != nil {
		return err
	}

	body, err := encodeStage(m.Stage)
	if err != nil {
		return fmt.Errorf("queue: dead-letter message %s: %w", m.Ident(), err)
	}

	_, err = q.api.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(dlq.url),
		MessageBody: aws.String(body),
		MessageAttributes: map[string]sqstypes.MessageAttributeValue{
			deadLetterReasonAttr: stringAttr(reason),
			deadLetterSourceAttr: stringAttr(m.Queue),
		},
	})
	if err != nil {
		return fmt.Errorf("queue: dead-letter message %s to %s: %w", m.Ident(), q.opts.DLQName, err)
	}

	_, err = q.api.DeleteMessage(ctx, &sqs.DeleteMessageInput{
		QueueUrl:      aws.String(src.url),
		ReceiptHandle: aws.String(m.Receipt),
	})
	return leaseError("dead-letter", m, err)
}

// Stats returns depth per queue.
//
// SQS documents these counts as approximate, which is where the SQLite driver's
// identical caveat came from. Two differences worth knowing: this is one billed
// GetQueueAttributes request per configured queue rather than one local query
// over the whole table, and QueueStat.Oldest is always zero because SQS exposes
// the age of the oldest message as a CloudWatch metric, never as a queue
// attribute.
func (q *SQSQueue) Stats(ctx context.Context) ([]QueueStat, error) {
	names := append([]string(nil), q.opts.Queues...)
	sort.Strings(names)

	stats := make([]QueueStat, 0, len(names))
	for _, name := range names {
		ref, err := q.resolve(ctx, name)
		if err != nil {
			return nil, err
		}

		out, err := q.api.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{
			QueueUrl: aws.String(ref.url),
			AttributeNames: []sqstypes.QueueAttributeName{
				sqstypes.QueueAttributeNameApproximateNumberOfMessages,
				sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		if err != nil {
			return nil, fmt.Errorf("queue: stats for %s: %w", name, err)
		}

		st := QueueStat{
			Queue:    name,
			Visible:  atoiOrZero(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessages)]),
			InFlight: atoiOrZero(out.Attributes[string(sqstypes.QueueAttributeNameApproximateNumberOfMessagesNotVisible)]),
		}
		// Empty queues are omitted so the output matches the SQLite driver's,
		// whose GROUP BY has nothing to report for a queue with no rows.
		if st.Visible == 0 && st.InFlight == 0 {
			continue
		}
		stats = append(stats, st)
	}
	return stats, nil
}

// Close is a no-op.
//
// There is nothing to release: the SQS client owns no connection this driver
// opened, only an http.Client whose idle connections the runtime reaps, and no
// message is buffered locally waiting to be flushed. It exists because Queue
// demands it, and because the callers — which close the queue on shutdown after
// draining in-flight requests — must not have to ask which driver they hold.
func (q *SQSQueue) Close() error { return nil }

// envelope turns one SQS message into the broker envelope the pipeline expects.
//
// EnqueuedAt, ClaimedAt and LeaseExpiresAt each come from a different place, and
// only the first is something SQS actually tells us:
//
//   - EnqueuedAt is the SentTimestamp system attribute, in epoch milliseconds.
//     It is the producer's send time as SQS recorded it, which is exactly what
//     the SQLite driver stores in created_at.
//   - ClaimedAt is stamped here, on the client, because SQS reports nothing
//     about when it handed the message out. It is therefore off by the receive
//     call's own latency plus whatever clock skew exists between this host and
//     SQS — tens of milliseconds against metrics measured in seconds.
//   - LeaseExpiresAt is ClaimedAt plus the queue's visibility timeout, and
//     inherits that same skew. SQS has no "when does this lease expire" field,
//     so this is the only way to compute it, and it is a lower bound on the real
//     deadline rather than a guess: the clock started at SQS, before this
//     process saw the message, so the true expiry is a little earlier than the
//     value here. A heartbeat scheduled off LeaseRemaining() therefore fires
//     slightly late rather than slightly early, which is why the runner
//     heartbeats every 30 seconds against a 5 minute lease instead of waiting
//     for LeaseRemaining() to approach zero.
//
// Leaving these zero was the alternative, and it is worse: LeaseRemaining()
// would return a large negative duration on every message and QueueWait() would
// report 56 years, both of which look like data rather than absence.
func envelope(raw sqstypes.Message, queueName string, claimedAt time.Time, visibility time.Duration) (Message, error) {
	stage, err := decodeStage(aws.ToString(raw.Body))
	if err != nil {
		return Message{}, err
	}

	m := Message{
		MessageID:      aws.ToString(raw.MessageId),
		Queue:          queueName,
		Receipt:        aws.ToString(raw.ReceiptHandle),
		ReceiveCount:   1,
		ClaimedAt:      claimedAt,
		LeaseExpiresAt: claimedAt.Add(visibility),
		Stage:          stage,
	}

	// A receive count that is missing or unparseable falls back to 1 rather than
	// 0: this delivery is happening, so the lowest honest attempt number is one,
	// and a 0 would make the runner's budget check off by one in the direction
	// that retries forever.
	if n := atoiOrZero(raw.Attributes[string(sqstypes.MessageSystemAttributeNameApproximateReceiveCount)]); n > 0 {
		m.ReceiveCount = n
	}

	if ms := millisOrZero(raw.Attributes[string(sqstypes.MessageSystemAttributeNameSentTimestamp)]); ms > 0 {
		m.EnqueuedAt = time.UnixMilli(ms).UTC()
	} else {
		// Better a queue wait of zero than one measured from 1970.
		m.EnqueuedAt = claimedAt
	}

	return m, nil
}

// encodeStage and decodeStage are the only places the payload crosses the wire.
// The body is JSON, byte-identical to what the SQLite driver stores, so a
// message written by one driver is readable by the other — which is what makes
// switching QUEUE_DRIVER a decision about where messages live rather than a
// migration.
func encodeStage(msg *events.StageMessage) (string, error) {
	if msg == nil {
		return "", errors.New("message is nil")
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func decodeStage(body string) (*events.StageMessage, error) {
	var stage events.StageMessage
	if err := json.Unmarshal([]byte(body), &stage); err != nil {
		return nil, err
	}
	return &stage, nil
}

// clampDelaySeconds converts a Send delay into DelaySeconds.
//
// Over SQS's 900 second ceiling the delay is shortened, not rejected: the
// SQLite driver accepts any delay, and a caller that asked for an hour would
// otherwise get a hard error on one driver and a working queue on the other.
// The message arrives early rather than never, and the runner's own backoff
// ceiling (5 minutes) is well inside the limit, so nothing in this pipeline
// reaches it today.
func clampDelaySeconds(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	secs := int64(d.Round(time.Second) / time.Second)
	if secs > sqsMaxDelaySeconds {
		return sqsMaxDelaySeconds
	}
	return int32(secs)
}

// clampMaxMessages bounds a batch size to SQS's 1..10.
func clampMaxMessages(max int) int32 {
	if max <= 0 {
		return defaultMaxMessages
	}
	if max > sqsMaxMessages {
		return sqsMaxMessages
	}
	return int32(max)
}

// clampWaitSeconds bounds a long poll to SQS's 0..20 seconds.
//
// A positive wait never truncates to zero. Zero is short polling: an immediate
// return, a billed request, and a real chance of an empty response even when the
// queue has messages — and the runner's consume loop has no delay of its own
// between receives, so a wait that truncated to zero would turn an idle worker
// into a hot loop billed per request. The SQLite driver's default poll interval
// is 250ms and would land exactly there, which is why a sub-second wait rounds
// up to one second here and why config.Load defaults QUEUE_POLL_INTERVAL to 20s
// on this driver.
//
// A caller that genuinely wants a short poll passes 0 and gets one.
func clampWaitSeconds(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	secs := int64(d / time.Second)
	if secs < 1 {
		return 1
	}
	if secs > sqsMaxWaitSeconds {
		return sqsMaxWaitSeconds
	}
	return int32(secs)
}

// clampVisibilitySeconds bounds a new visibility timeout to SQS's 0..43200.
//
// Zero is legal and meaningful — it is what makes a nacked message claimable
// immediately — so a negative backoff floors at zero rather than falling back to
// a default.
func clampVisibilitySeconds(d time.Duration) int32 {
	if d <= 0 {
		return 0
	}
	secs := int64(d.Round(time.Second) / time.Second)
	if secs > sqsMaxVisibilitySeconds {
		return sqsMaxVisibilitySeconds
	}
	return int32(secs)
}

// leaseError translates an SQS API error into the broker's vocabulary, and is
// the whole reason the runner needs no knowledge of which driver it holds.
func leaseError(op string, m Message, err error) error {
	if err == nil {
		return nil
	}
	if isLeaseLost(err) {
		return fmt.Errorf("queue: %s message %s: %w", op, m.Ident(), ErrLeaseLost)
	}
	return fmt.Errorf("queue: %s message %s: %w", op, m.Ident(), err)
}

// isLeaseLost reports whether an SQS error means "your receipt is no longer
// valid", which is the same condition the SQLite driver detects as zero rows
// affected by a `WHERE receipt = ?` update.
//
// InvalidParameterValue is included on purpose and is safe here only because
// every numeric parameter this driver sends is clamped into SQS's legal range
// first. With those ruled out, the receipt handle is the only parameter left
// that SQS can call invalid — which is how it reports an expired handle in
// several cases instead of the documented ReceiptHandleIsInvalid. Adding an
// unclamped parameter to any of these calls would quietly turn a real bug into
// a silent "lease lost", so clamp first.
func isLeaseLost(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode() {
	case "ReceiptHandleIsInvalid",
		// The handle was valid but the message is no longer in flight, i.e. the
		// visibility timeout ran out. Same condition, different name.
		"MessageNotInflight",
		"InvalidParameterValue":
		return true
	default:
		return false
	}
}

func stringAttr(v string) sqstypes.MessageAttributeValue {
	return sqstypes.MessageAttributeValue{
		DataType:    aws.String("String"),
		StringValue: aws.String(v),
	}
}

// atoiOrZero and millisOrZero parse SQS's stringly-typed numeric attributes.
// Every one of them is optional and absent unless requested, so an unusable
// value is a missing measurement, not an error worth failing a delivery over.
//
// SentTimestamp gets its own parser because it is epoch milliseconds — a value
// that does not fit an int on a 32-bit build, where strconv.Atoi would return
// ErrRange and silently turn every message's enqueue time into 1970.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func millisOrZero(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}
