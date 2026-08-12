package queue

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/events"
)

// newLoggedQueue wraps a real SQLite queue so the decorator is exercised against
// the behaviour it actually has to describe, not a hand-written fake.
func newLoggedQueue(t *testing.T, mutators ...func(*Options)) (Queue, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return WithLogging(newTestQueue(t, mutators...), logger), &buf
}

// logLines parses the captured JSON log, one record per line.
func logLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func findLog(t *testing.T, buf *bytes.Buffer, msg string) map[string]any {
	t.Helper()
	for _, rec := range logLines(t, buf) {
		if rec["msg"] == msg {
			return rec
		}
	}
	t.Fatalf("no log record with msg %q in:\n%s", msg, buf.String())
	return nil
}

func TestLoggingQueueRecordsPipelineContext(t *testing.T) {
	q, buf := newLoggedQueue(t)
	ctx := context.Background()

	stage := testStage("job-logged")
	if err := q.Send(ctx, events.QueueValidate, stage, 0); err != nil {
		t.Fatalf("Send: %v", err)
	}

	sendRec := findLog(t, buf, "queue send")
	for key, want := range map[string]any{
		"queue":    events.QueueValidate,
		"job_id":   stage.JobID,
		"stage":    string(stage.Stage),
		"trace_id": stage.TraceID,
	} {
		if sendRec[key] != want {
			t.Errorf("send log %s = %v, want %v", key, sendRec[key], want)
		}
	}
	if _, ok := sendRec["duration_ms"]; !ok {
		t.Error("send log is missing duration_ms")
	}

	msgs, err := q.Receive(ctx, events.QueueValidate, 1, 0)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	recvRec := findLog(t, buf, "queue receive")
	if recvRec["receive_count"] != float64(1) {
		t.Errorf("receive log receive_count = %v, want 1", recvRec["receive_count"])
	}
	if recvRec["msg_id"] != float64(msgs[0].ID) {
		t.Errorf("receive log msg_id = %v, want %d", recvRec["msg_id"], msgs[0].ID)
	}
	if recvRec["job_id"] != stage.JobID {
		t.Errorf("receive log job_id = %v, want %v", recvRec["job_id"], stage.JobID)
	}

	if err := q.Ack(ctx, msgs[0]); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	if ackRec := findLog(t, buf, "queue ack"); ackRec["msg_id"] != float64(msgs[0].ID) {
		t.Errorf("ack log msg_id = %v, want %d", ackRec["msg_id"], msgs[0].ID)
	}
}

// TestLoggingQueueDowngradesLeaseLoss keeps a lost race out of the error budget:
// the broker behaved correctly, so the line is a warning, not an error.
func TestLoggingQueueDowngradesLeaseLoss(t *testing.T) {
	q, buf := newLoggedQueue(t, func(o *Options) { o.VisibilityTimeout = 80 * time.Millisecond })
	ctx := context.Background()

	if err := q.Send(ctx, events.QueueValidate, testStage("job-lease"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs, err := q.Receive(ctx, events.QueueValidate, 1, 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Receive: %v (%d messages)", err, len(msgs))
	}

	time.Sleep(150 * time.Millisecond)
	if _, err := q.Receive(ctx, events.QueueValidate, 1, 0); err != nil {
		t.Fatalf("second Receive: %v", err)
	}

	if err := q.Ack(ctx, msgs[0]); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("Ack error = %v, want ErrLeaseLost", err)
	}
	if rec := findLog(t, buf, "queue ack lost lease"); rec["level"] != "WARN" {
		t.Errorf("lease-loss log level = %v, want WARN", rec["level"])
	}
}

// TestLoggingQueueKeepsEmptyReceivesQuiet: an idle worker long-polls forever, so
// empty receives must not be info-level noise.
func TestLoggingQueueKeepsEmptyReceivesQuiet(t *testing.T) {
	q, buf := newLoggedQueue(t, func(o *Options) { o.PollInterval = 10 * time.Millisecond })

	if _, err := q.Receive(context.Background(), events.QueueValidate, 1, 0); err != nil {
		t.Fatalf("Receive: %v", err)
	}

	rec := findLog(t, buf, "queue receive empty")
	if rec["level"] != "DEBUG" {
		t.Errorf("empty receive log level = %v, want DEBUG", rec["level"])
	}
}

func TestLoggingQueueLogsDeadLetterAsWarning(t *testing.T) {
	q, buf := newLoggedQueue(t)
	ctx := context.Background()

	if err := q.Send(ctx, events.QueueValidate, testStage("job-dlq"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	msgs, err := q.Receive(ctx, events.QueueValidate, 1, 0)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("Receive: %v (%d messages)", err, len(msgs))
	}
	if err := q.DeadLetter(ctx, msgs[0], "gave up"); err != nil {
		t.Fatalf("DeadLetter: %v", err)
	}

	rec := findLog(t, buf, "queue dead-letter")
	if rec["level"] != "WARN" {
		t.Errorf("dead-letter log level = %v, want WARN", rec["level"])
	}
	if rec["reason"] != "gave up" {
		t.Errorf("dead-letter log reason = %v, want %q", rec["reason"], "gave up")
	}
}

func TestLoggingQueueLogsStats(t *testing.T) {
	q, buf := newLoggedQueue(t)
	ctx := context.Background()

	if err := q.Send(ctx, events.QueueValidate, testStage("job-stats"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
	stats, err := q.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats) != 1 || stats[0].Visible != 1 {
		t.Fatalf("stats = %+v, want one queue with Visible=1", stats)
	}

	if rec := findLog(t, buf, "queue stats"); rec["visible"] != float64(1) {
		t.Errorf("stats log visible = %v, want 1", rec["visible"])
	}
}

// TestWithLoggingNilLoggerFallsBack: a nil logger must not panic on the first
// send in production.
func TestWithLoggingNilLoggerFallsBack(t *testing.T) {
	q := WithLogging(newTestQueue(t), nil)
	if err := q.Send(context.Background(), events.QueueValidate, testStage("job-nil-logger"), 0); err != nil {
		t.Fatalf("Send: %v", err)
	}
}
