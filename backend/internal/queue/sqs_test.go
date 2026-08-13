package queue

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/aws/smithy-go"

	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
)

// These tests never talk to SQS, and none of them needs credentials or an
// endpoint. What they cover is everything this driver decides on its own: the
// arguments it shapes before a call, the errors it translates after one, and the
// envelope it builds from a response.
//
// What they deliberately do not cover, because it is not observable without a
// real queue:
//
//   - that a visibility timeout actually hides a message, and that a second
//     receive after it expires returns the same message with the count bumped.
//     Both are SQS's behaviour, not this package's.
//   - that long polling blocks server-side for WaitTimeSeconds.
//   - that the redrive policy scripts/aws-sqs-setup.sh writes ever fires. It is
//     a safety net for messages that escape the runner entirely, and provoking
//     it means letting a real message be delivered maxReceiveCount times.
//   - the SDK's own retry and credential resolution.
//
// Asserting any of those against a fake would only be asserting that the fake
// was written to match the assertion.

func testStageMessage(jobID string) *events.StageMessage {
	return events.NewStageMessage(
		jobID,
		models.StageValidate,
		events.S3Ref{Bucket: events.BucketRawVideos, Key: "raw/" + jobID + ".mp4"},
		1,
		"trace-"+jobID,
	)
}

// ── Clamping ────────────────────────────────────────────────────────────────
//
// Each of these is an SQS API ceiling. Exceeding one is an InvalidParameterValue
// from AWS, so the clamp is what lets a caller hand the same arguments to either
// driver.

func TestClampDelaySeconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want int32
	}{
		{"zero", 0, 0},
		{"negative floors at zero", -5 * time.Second, 0},
		{"sub-second rounds to nearest", 400 * time.Millisecond, 0},
		{"sub-second rounds up past half", 1600 * time.Millisecond, 2},
		{"whole seconds pass through", 30 * time.Second, 30},
		{"exactly the ceiling", 900 * time.Second, sqsMaxDelaySeconds},
		{"over the ceiling clamps", time.Hour, sqsMaxDelaySeconds},
		{"absurdly over the ceiling clamps", 30 * 24 * time.Hour, sqsMaxDelaySeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampDelaySeconds(tc.in); got != tc.want {
				t.Errorf("clampDelaySeconds(%s) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestClampMaxMessages(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   int
		want int32
	}{
		{"zero falls back to the package default", 0, defaultMaxMessages},
		{"negative falls back to the package default", -3, defaultMaxMessages},
		{"one", 1, 1},
		{"exactly the ceiling", 10, sqsMaxMessages},
		{"over the ceiling clamps", 100, sqsMaxMessages},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampMaxMessages(tc.in); got != tc.want {
				t.Errorf("clampMaxMessages(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestClampWaitSeconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want int32
	}{
		// Only an explicit "do not wait" gets short polling.
		{"zero", 0, 0},
		{"negative floors at zero", -time.Second, 0},
		// The SQLite driver's default poll interval. Truncating it to 0 would
		// make an idle worker a hot loop billed per request, so it rounds up.
		{"a sub-second wait rounds up rather than becoming a short poll", 250 * time.Millisecond, 1},
		{"partial seconds truncate rather than round", 4900 * time.Millisecond, 4},
		{"exactly the ceiling", 20 * time.Second, sqsMaxWaitSeconds},
		{"over the ceiling clamps", time.Minute, sqsMaxWaitSeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampWaitSeconds(tc.in); got != tc.want {
				t.Errorf("clampWaitSeconds(%s) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestClampVisibilitySeconds(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   time.Duration
		want int32
	}{
		// Zero is legal and meaningful: it is what makes a nacked message
		// claimable straight away.
		{"zero means immediately claimable", 0, 0},
		{"negative floors at zero", -time.Minute, 0},
		{"the runner's first nack backoff", 10 * time.Second, 10},
		{"the default lease", 5 * time.Minute, 300},
		{"exactly the ceiling", 12 * time.Hour, sqsMaxVisibilitySeconds},
		{"over the ceiling clamps", 24 * time.Hour, sqsMaxVisibilitySeconds},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampVisibilitySeconds(tc.in); got != tc.want {
				t.Errorf("clampVisibilitySeconds(%s) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// ── Error translation ───────────────────────────────────────────────────────

// TestIsLeaseLostRecognisesExpiredReceipts pins the codes that mean "someone
// else owns this message now". Getting this wrong is invisible in the happy
// path and expensive otherwise: an unmapped code reaches the runner as a generic
// error, which logs it as a broker fault and pages someone over a race the
// broker handled correctly.
func TestIsLeaseLostRecognisesExpiredReceipts(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{
			"the documented code",
			&sqstypes.ReceiptHandleIsInvalid{Message: aws.String("The receipt handle has expired")},
			true,
		},
		{
			"the code SQS actually returns for an expired handle in some cases",
			&smithy.GenericAPIError{Code: "InvalidParameterValue", Message: "Value for parameter ReceiptHandle is invalid"},
			true,
		},
		{
			"the handle was fine but the lease already ran out",
			&sqstypes.MessageNotInflight{},
			true,
		},
		{
			"a queue that does not exist is a configuration error, not a lost lease",
			&sqstypes.QueueDoesNotExist{},
			false,
		},
		{
			"throttling is transient and must stay retryable",
			&smithy.GenericAPIError{Code: "RequestThrottled"},
			false,
		},
		{
			"a plain error is not an API error at all",
			errors.New("dial tcp: connection refused"),
			false,
		},
		{"no error", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLeaseLost(tc.err); got != tc.want {
				t.Errorf("isLeaseLost(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsLeaseLostSeesThroughWrapping matters because the SDK does not hand back
// the bare API error: it arrives inside an operation error, and errors.As is the
// only thing that finds it.
func TestIsLeaseLostSeesThroughWrapping(t *testing.T) {
	wrapped := &smithy.OperationError{
		ServiceID:     "SQS",
		OperationName: "DeleteMessage",
		Err:           &sqstypes.ReceiptHandleIsInvalid{},
	}
	if !isLeaseLost(wrapped) {
		t.Error("isLeaseLost did not see the API error through the SDK's operation wrapper")
	}
}

// TestLeaseErrorProducesErrLeaseLost checks the sentinel the runner actually
// matches on, not just the predicate behind it.
func TestLeaseErrorProducesErrLeaseLost(t *testing.T) {
	m := Message{MessageID: "d290f1ee-6c54-4b01-90e6-d701748f0851", Queue: events.QueueValidate}

	err := leaseError("ack", m, &sqstypes.ReceiptHandleIsInvalid{})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("leaseError = %v, want it to wrap ErrLeaseLost", err)
	}
	if !strings.Contains(err.Error(), m.MessageID) {
		t.Errorf("leaseError = %q, want it to name the message %q", err, m.MessageID)
	}

	if err := leaseError("ack", m, nil); err != nil {
		t.Errorf("leaseError with no error = %v, want nil", err)
	}

	// Anything else must stay an ordinary error. Reporting a network failure as
	// a lost lease would make the runner drop work it should have retried.
	other := errors.New("dial tcp: connection refused")
	err = leaseError("ack", m, other)
	if errors.Is(err, ErrLeaseLost) {
		t.Errorf("leaseError(%v) reported a lost lease", other)
	}
	if !errors.Is(err, other) {
		t.Errorf("leaseError = %v, want it to wrap the original error", err)
	}
}

// ── Body encoding ───────────────────────────────────────────────────────────

// TestStageBodyRoundTrips is what makes QUEUE_DRIVER a runtime choice rather
// than a migration: the body is the same JSON the SQLite driver stores, so a
// message written by one driver is readable by the other.
func TestStageBodyRoundTrips(t *testing.T) {
	want := testStageMessage("job-roundtrip")
	want.Attempt = 3

	body, err := encodeStage(want)
	if err != nil {
		t.Fatalf("encodeStage: %v", err)
	}

	got, err := decodeStage(body)
	if err != nil {
		t.Fatalf("decodeStage: %v", err)
	}

	if got.JobID != want.JobID {
		t.Errorf("JobID = %q, want %q", got.JobID, want.JobID)
	}
	if got.Stage != want.Stage {
		t.Errorf("Stage = %q, want %q", got.Stage, want.Stage)
	}
	if got.Input != want.Input {
		t.Errorf("Input = %+v, want %+v", got.Input, want.Input)
	}
	if got.Attempt != want.Attempt {
		t.Errorf("Attempt = %d, want %d", got.Attempt, want.Attempt)
	}
	if got.TraceID != want.TraceID {
		t.Errorf("TraceID = %q, want %q", got.TraceID, want.TraceID)
	}
	// Timestamps go through RFC 3339 and come back as a different time.Time
	// with the same instant, so compare instants rather than structs.
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Errorf("Timestamp = %s, want %s", got.Timestamp, want.Timestamp)
	}
}

func TestEncodeStageRejectsNil(t *testing.T) {
	if _, err := encodeStage(nil); err == nil {
		t.Error("encodeStage(nil) succeeded, want an error")
	}
}

// ── The delivery envelope ───────────────────────────────────────────────────

// TestEnvelopePopulatesTheFieldsSQSSupplies covers the mapping that has no
// second chance: get SentTimestamp wrong and QueueWait() reports decades, get
// LeaseExpiresAt wrong and LeaseRemaining() is negative on every message.
func TestEnvelopePopulatesTheFieldsSQSSupplies(t *testing.T) {
	stage := testStageMessage("job-envelope")
	body, err := encodeStage(stage)
	if err != nil {
		t.Fatalf("encodeStage: %v", err)
	}

	claimedAt := time.Date(2026, 8, 12, 10, 0, 30, 0, time.UTC)
	sentAt := claimedAt.Add(-90 * time.Second)

	raw := sqstypes.Message{
		MessageId:     aws.String("d290f1ee-6c54-4b01-90e6-d701748f0851"),
		ReceiptHandle: aws.String("AQEBw...receipt"),
		Body:          aws.String(body),
		Attributes: map[string]string{
			"ApproximateReceiveCount": "2",
			"SentTimestamp":           "1786528740000",
		},
	}
	// Keep the fixture honest: the literal above must be the same instant the
	// test reasons about.
	if got := time.UnixMilli(1786528740000).UTC(); !got.Equal(sentAt) {
		t.Fatalf("fixture SentTimestamp is %s, want %s", got, sentAt)
	}

	m, err := envelope(raw, events.QueueValidate, claimedAt, 5*time.Minute)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	if m.MessageID != *raw.MessageId {
		t.Errorf("MessageID = %q, want %q", m.MessageID, *raw.MessageId)
	}
	if m.Ident() != *raw.MessageId {
		t.Errorf("Ident() = %q, want the SQS MessageId %q", m.Ident(), *raw.MessageId)
	}
	if m.ID != 0 {
		t.Errorf("ID = %d, want 0: SQS does not number its messages", m.ID)
	}
	if m.Queue != events.QueueValidate {
		t.Errorf("Queue = %q, want %q", m.Queue, events.QueueValidate)
	}
	if m.Receipt != *raw.ReceiptHandle {
		t.Errorf("Receipt = %q, want %q", m.Receipt, *raw.ReceiptHandle)
	}
	if m.ReceiveCount != 2 {
		t.Errorf("ReceiveCount = %d, want 2", m.ReceiveCount)
	}
	if m.Stage == nil || m.Stage.JobID != stage.JobID {
		t.Fatalf("Stage = %+v, want the decoded payload for %s", m.Stage, stage.JobID)
	}

	if !m.EnqueuedAt.Equal(sentAt) {
		t.Errorf("EnqueuedAt = %s, want %s", m.EnqueuedAt, sentAt)
	}
	if !m.ClaimedAt.Equal(claimedAt) {
		t.Errorf("ClaimedAt = %s, want %s", m.ClaimedAt, claimedAt)
	}
	if want := claimedAt.Add(5 * time.Minute); !m.LeaseExpiresAt.Equal(want) {
		t.Errorf("LeaseExpiresAt = %s, want %s", m.LeaseExpiresAt, want)
	}
	if want := 90 * time.Second; m.QueueWait() != want {
		t.Errorf("QueueWait() = %s, want %s", m.QueueWait(), want)
	}
}

// TestEnvelopeWithoutSystemAttributes pins the fallbacks. A missing
// ApproximateReceiveCount used to be the pre-SQLite client's silent bug — every
// redelivery logged attempt=1 — so the floor is 1, never 0: a 0 would make the
// runner's budget check off by one in the direction that retries forever.
func TestEnvelopeWithoutSystemAttributes(t *testing.T) {
	body, err := encodeStage(testStageMessage("job-bare"))
	if err != nil {
		t.Fatalf("encodeStage: %v", err)
	}

	claimedAt := time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC)
	m, err := envelope(sqstypes.Message{Body: aws.String(body)}, events.QueueExtract, claimedAt, time.Minute)
	if err != nil {
		t.Fatalf("envelope: %v", err)
	}

	if m.ReceiveCount != 1 {
		t.Errorf("ReceiveCount = %d, want 1", m.ReceiveCount)
	}
	// Not the zero time: a queue wait measured from 1970 looks like a
	// catastrophic backlog rather than a missing measurement.
	if !m.EnqueuedAt.Equal(claimedAt) {
		t.Errorf("EnqueuedAt = %s, want it to fall back to ClaimedAt %s", m.EnqueuedAt, claimedAt)
	}
	if m.QueueWait() != 0 {
		t.Errorf("QueueWait() = %s, want 0", m.QueueWait())
	}
}

func TestEnvelopeRejectsAnUndecodableBody(t *testing.T) {
	_, err := envelope(
		sqstypes.Message{Body: aws.String("{not json")},
		events.QueueValidate, time.Now(), time.Minute,
	)
	if err == nil {
		t.Error("envelope accepted a body that will not decode")
	}
}

// ── Driver wiring ───────────────────────────────────────────────────────────

// fakeSQS records what the driver asked for and answers with whatever the test
// set. It is deliberately dumb: it enforces none of SQS's own rules, so no test
// here can accidentally assert that the fake is faithful.
type fakeSQS struct {
	mu    sync.Mutex
	calls []string

	visibilityTimeout string
	sent              []*sqs.SendMessageInput
	deleted           []*sqs.DeleteMessageInput
	visibilityChanges []*sqs.ChangeMessageVisibilityInput
	received          []sqstypes.Message

	deleteErr error
}

func (f *fakeSQS) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, name)
}

func (f *fakeSQS) GetQueueUrl(_ context.Context, in *sqs.GetQueueUrlInput, _ ...func(*sqs.Options)) (*sqs.GetQueueUrlOutput, error) {
	f.record("GetQueueUrl")
	return &sqs.GetQueueUrlOutput{
		QueueUrl: aws.String("https://sqs.us-east-1.amazonaws.com/000000000000/" + aws.ToString(in.QueueName)),
	}, nil
}

func (f *fakeSQS) GetQueueAttributes(_ context.Context, _ *sqs.GetQueueAttributesInput, _ ...func(*sqs.Options)) (*sqs.GetQueueAttributesOutput, error) {
	f.record("GetQueueAttributes")
	return &sqs.GetQueueAttributesOutput{
		Attributes: map[string]string{"VisibilityTimeout": f.visibilityTimeout},
	}, nil
}

func (f *fakeSQS) SendMessage(_ context.Context, in *sqs.SendMessageInput, _ ...func(*sqs.Options)) (*sqs.SendMessageOutput, error) {
	f.record("SendMessage")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, in)
	return &sqs.SendMessageOutput{MessageId: aws.String("generated")}, nil
}

func (f *fakeSQS) ReceiveMessage(_ context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.record("ReceiveMessage")
	return &sqs.ReceiveMessageOutput{Messages: f.received}, nil
}

func (f *fakeSQS) DeleteMessage(_ context.Context, in *sqs.DeleteMessageInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error) {
	f.record("DeleteMessage")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, in)
	return &sqs.DeleteMessageOutput{}, f.deleteErr
}

func (f *fakeSQS) ChangeMessageVisibility(_ context.Context, in *sqs.ChangeMessageVisibilityInput, _ ...func(*sqs.Options)) (*sqs.ChangeMessageVisibilityOutput, error) {
	f.record("ChangeMessageVisibility")
	f.mu.Lock()
	defer f.mu.Unlock()
	f.visibilityChanges = append(f.visibilityChanges, in)
	return &sqs.ChangeMessageVisibilityOutput{}, nil
}

func newFakeDriver(t *testing.T, api *fakeSQS) *SQSQueue {
	t.Helper()
	if api.visibilityTimeout == "" {
		api.visibilityTimeout = "300"
	}
	return newSQSQueue(api, SQSOptions{VisibilityTimeout: time.Minute})
}

// TestReceiveUsesTheQueuesOwnVisibilityTimeout is the reason resolve reads
// GetQueueAttributes at all. The queue's attribute is what AWS enforces; the
// config value is only what this process believes, and computing
// LeaseExpiresAt from the wrong one produces a heartbeat schedule that is
// confidently late.
func TestReceiveUsesTheQueuesOwnVisibilityTimeout(t *testing.T) {
	body, err := encodeStage(testStageMessage("job-lease"))
	if err != nil {
		t.Fatalf("encodeStage: %v", err)
	}

	api := &fakeSQS{
		// Ten minutes on the queue against one minute in the config below.
		visibilityTimeout: "600",
		received: []sqstypes.Message{{
			MessageId:     aws.String("msg-1"),
			ReceiptHandle: aws.String("receipt-1"),
			Body:          aws.String(body),
		}},
	}
	q := newFakeDriver(t, api)

	msgs, err := q.Receive(context.Background(), events.QueueValidate, 1, time.Second)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	lease := msgs[0].LeaseExpiresAt.Sub(msgs[0].ClaimedAt)
	if lease != 10*time.Minute {
		t.Errorf("lease = %s, want the queue's own 10m, not the configured 1m", lease)
	}
}

// TestResolveCachesQueueLookups guards a cost, not a correctness property: on
// SQS every request is billed, and re-resolving on each operation would double
// the request count of the whole pipeline for values that never change.
func TestResolveCachesQueueLookups(t *testing.T) {
	api := &fakeSQS{}
	q := newFakeDriver(t, api)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if err := q.Send(ctx, events.QueueValidate, testStageMessage("job-cache"), 0); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}

	var lookups int
	for _, c := range api.calls {
		if c == "GetQueueUrl" || c == "GetQueueAttributes" {
			lookups++
		}
	}
	if lookups != 2 {
		t.Errorf("resolved the queue %d times over 3 sends, want 2 lookups total: %v", lookups, api.calls)
	}
}

// TestDeadLetterCopiesBeforeDeleting pins the ordering. SQS has no move
// operation, so the two calls can half-succeed; sending first means a failure
// leaves a duplicate on a queue nothing drains automatically, while deleting
// first would lose the only record of why the pipeline gave up on a video.
func TestDeadLetterCopiesBeforeDeleting(t *testing.T) {
	api := &fakeSQS{}
	q := newFakeDriver(t, api)

	stage := testStageMessage("job-dlq")
	err := q.DeadLetter(context.Background(), Message{
		MessageID: "msg-dlq",
		Queue:     events.QueueExtract,
		Receipt:   "receipt-dlq",
		Stage:     stage,
	}, "unsupported codec")
	if err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	var order []string
	for _, c := range api.calls {
		if c == "SendMessage" || c == "DeleteMessage" {
			order = append(order, c)
		}
	}
	if len(order) != 2 || order[0] != "SendMessage" || order[1] != "DeleteMessage" {
		t.Fatalf("call order = %v, want SendMessage then DeleteMessage", order)
	}

	if len(api.sent) != 1 {
		t.Fatalf("sent %d messages, want 1", len(api.sent))
	}
	sent := api.sent[0]
	if !strings.HasSuffix(aws.ToString(sent.QueueUrl), events.QueueDLQ) {
		t.Errorf("sent to %q, want the DLQ %q", aws.ToString(sent.QueueUrl), events.QueueDLQ)
	}
	// The reason has nowhere else to live: SQS has no equivalent of the SQLite
	// driver's dlq_reason column, so anything draining the DLQ reads it here.
	if got := aws.ToString(sent.MessageAttributes[deadLetterReasonAttr].StringValue); got != "unsupported codec" {
		t.Errorf("%s = %q, want the dead-letter reason", deadLetterReasonAttr, got)
	}
	if got := aws.ToString(sent.MessageAttributes[deadLetterSourceAttr].StringValue); got != events.QueueExtract {
		t.Errorf("%s = %q, want %q", deadLetterSourceAttr, got, events.QueueExtract)
	}

	if len(api.deleted) != 1 || aws.ToString(api.deleted[0].ReceiptHandle) != "receipt-dlq" {
		t.Errorf("deleted = %+v, want one delete of receipt-dlq from the source queue", api.deleted)
	}
	if !strings.HasSuffix(aws.ToString(api.deleted[0].QueueUrl), events.QueueExtract) {
		t.Errorf("deleted from %q, want the source queue %q", aws.ToString(api.deleted[0].QueueUrl), events.QueueExtract)
	}
}

// TestDeadLetterReportsALostLease closes the loop from an SQS error code to the
// sentinel the runner branches on, through a real driver call.
func TestDeadLetterReportsALostLease(t *testing.T) {
	api := &fakeSQS{deleteErr: &sqstypes.ReceiptHandleIsInvalid{}}
	q := newFakeDriver(t, api)

	err := q.DeadLetter(context.Background(), Message{
		MessageID: "msg-lost",
		Queue:     events.QueueExtract,
		Receipt:   "stale",
		Stage:     testStageMessage("job-lost"),
	}, "gave up")
	if !errors.Is(err, ErrLeaseLost) {
		t.Errorf("DeadLetter = %v, want ErrLeaseLost", err)
	}
}

// TestNackAndHeartbeatSetTheVisibilityTimeout: both are the same SQS call, and
// the difference that matters is which duration each one asks for.
func TestNackAndHeartbeatSetTheVisibilityTimeout(t *testing.T) {
	api := &fakeSQS{}
	q := newFakeDriver(t, api)
	ctx := context.Background()
	m := Message{MessageID: "msg-vis", Queue: events.QueueValidate, Receipt: "receipt-vis"}

	if err := q.Nack(ctx, m, 45*time.Second); err != nil {
		t.Fatalf("Nack: %v", err)
	}
	// Zero extension falls back to the configured timeout — one minute here.
	if err := q.Heartbeat(ctx, m, 0); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := q.Heartbeat(ctx, m, 30*time.Hour); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	want := []int32{45, 60, sqsMaxVisibilitySeconds}
	if len(api.visibilityChanges) != len(want) {
		t.Fatalf("got %d visibility changes, want %d", len(api.visibilityChanges), len(want))
	}
	for i, w := range want {
		if got := api.visibilityChanges[i].VisibilityTimeout; got != w {
			t.Errorf("visibility change %d = %d, want %d", i, got, w)
		}
	}
}

// TestSQSOptionsDefaults: the queue set Stats walks has to default to the five
// the pipeline uses, because SQS offers no cheap, exact "which queues have
// messages" call to discover them with.
func TestSQSOptionsDefaults(t *testing.T) {
	opts := SQSOptions{}.withDefaults()

	if opts.VisibilityTimeout != defaultVisibilityTimeout {
		t.Errorf("VisibilityTimeout = %s, want %s", opts.VisibilityTimeout, defaultVisibilityTimeout)
	}
	if opts.MaxDeliveries != defaultMaxDeliveries {
		t.Errorf("MaxDeliveries = %d, want %d", opts.MaxDeliveries, defaultMaxDeliveries)
	}
	if opts.DLQName != events.QueueDLQ {
		t.Errorf("DLQName = %q, want %q", opts.DLQName, events.QueueDLQ)
	}
	if len(opts.Queues) != 5 {
		t.Fatalf("Queues = %v, want the five pipeline queues", opts.Queues)
	}
}
