package queue

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/events"
)

// LoggingQueue wraps any Queue and emits a structured log line per operation.
//
// Observability is a decorator rather than a logger field on SQLiteQueue so the
// driver stays a driver: the SQL layer has no opinion about log levels or
// formats, the decorator has no opinion about SQL, and a test can exercise
// either half alone. It also means a second decorator (metrics, tracing) slots
// in without touching the broker.
type LoggingQueue struct {
	inner Queue
	log   *slog.Logger
}

var _ Queue = (*LoggingQueue)(nil)

// WithLogging wraps q so every operation is logged. A nil logger falls back to
// slog.Default().
func WithLogging(q Queue, logger *slog.Logger) Queue {
	if logger == nil {
		logger = slog.Default()
	}
	return &LoggingQueue{inner: q, log: logger}
}

func (l *LoggingQueue) Send(ctx context.Context, queue string, msg *events.StageMessage, delay time.Duration) error {
	start := time.Now()
	err := l.inner.Send(ctx, queue, msg, delay)

	attrs := append([]any{slog.String("queue", queue), slog.Int64("delay_ms", delay.Milliseconds())}, stageAttrs(msg)...)
	attrs = append(attrs, durationAttr(start))
	if err != nil {
		l.log.ErrorContext(ctx, "queue send failed", append(attrs, slog.Any("error", err))...)
		return err
	}
	l.log.InfoContext(ctx, "queue send", attrs...)
	return nil
}

func (l *LoggingQueue) Receive(ctx context.Context, queue string, max int, wait time.Duration) ([]Message, error) {
	start := time.Now()
	msgs, err := l.inner.Receive(ctx, queue, max, wait)

	base := []any{slog.String("queue", queue), slog.Int("max", max), durationAttr(start)}
	switch {
	case err != nil:
		l.log.ErrorContext(ctx, "queue receive failed", append(base, slog.Any("error", err))...)
		return nil, err
	case len(msgs) == 0:
		// An idle worker long-polls forever; logging empty receives at info
		// level would drown every other line in the pipeline.
		l.log.DebugContext(ctx, "queue receive empty", base...)
		return msgs, nil
	}

	for _, m := range msgs {
		attrs := append(messageAttrs(m), slog.Int64("queue_wait_ms", m.QueueWait().Milliseconds()), durationAttr(start))
		l.log.InfoContext(ctx, "queue receive", attrs...)
	}
	return msgs, nil
}

func (l *LoggingQueue) Ack(ctx context.Context, m Message) error {
	start := time.Now()
	err := l.inner.Ack(ctx, m)

	attrs := append(messageAttrs(m), slog.Int64("held_ms", time.Since(m.ClaimedAt).Milliseconds()), durationAttr(start))
	return l.finish(ctx, "queue ack", attrs, err)
}

func (l *LoggingQueue) Nack(ctx context.Context, m Message, backoff time.Duration) error {
	start := time.Now()
	err := l.inner.Nack(ctx, m, backoff)

	attrs := append(messageAttrs(m), slog.Int64("backoff_ms", backoff.Milliseconds()), durationAttr(start))
	return l.finish(ctx, "queue nack", attrs, err)
}

func (l *LoggingQueue) Heartbeat(ctx context.Context, m Message, extend time.Duration) error {
	start := time.Now()
	err := l.inner.Heartbeat(ctx, m, extend)

	attrs := append(messageAttrs(m), slog.Int64("extend_ms", extend.Milliseconds()), durationAttr(start))
	return l.finish(ctx, "queue heartbeat", attrs, err)
}

func (l *LoggingQueue) DeadLetter(ctx context.Context, m Message, reason string) error {
	start := time.Now()
	err := l.inner.DeadLetter(ctx, m, reason)

	attrs := append(messageAttrs(m), slog.String("reason", reason), durationAttr(start))
	if err != nil {
		return l.finish(ctx, "queue dead-letter", attrs, err)
	}
	// A dead letter is never routine: it is a video the pipeline gave up on.
	l.log.WarnContext(ctx, "queue dead-letter", attrs...)
	return nil
}

func (l *LoggingQueue) Stats(ctx context.Context) ([]QueueStat, error) {
	start := time.Now()
	stats, err := l.inner.Stats(ctx)
	if err != nil {
		l.log.ErrorContext(ctx, "queue stats failed", slog.Any("error", err), durationAttr(start))
		return nil, err
	}
	for _, s := range stats {
		l.log.InfoContext(ctx, "queue stats",
			slog.String("queue", s.Queue),
			slog.Int("visible", s.Visible),
			slog.Int("in_flight", s.InFlight),
			slog.Int64("oldest_ms", s.Oldest.Milliseconds()),
			durationAttr(start),
		)
	}
	return stats, nil
}

func (l *LoggingQueue) Close() error {
	err := l.inner.Close()
	if err != nil {
		l.log.Error("queue close failed", slog.Any("error", err))
		return err
	}
	l.log.Info("queue closed")
	return nil
}

// finish logs the outcome of a lease-guarded operation. ErrLeaseLost is a warning
// rather than an error: the broker did the right thing, the worker simply lost a
// race, and paging on it would train people to ignore real queue errors.
func (l *LoggingQueue) finish(ctx context.Context, msg string, attrs []any, err error) error {
	switch {
	case errors.Is(err, ErrLeaseLost):
		l.log.WarnContext(ctx, msg+" lost lease", append(attrs, slog.Any("error", err))...)
	case err != nil:
		l.log.ErrorContext(ctx, msg+" failed", append(attrs, slog.Any("error", err))...)
	default:
		l.log.InfoContext(ctx, msg, attrs...)
	}
	return err
}

// messageAttrs is the standard attribute set for an envelope: enough to follow a
// single video through every stage and every retry.
func messageAttrs(m Message) []any {
	attrs := []any{
		slog.String("queue", m.Queue),
		slog.Int64("msg_id", m.ID),
		slog.Int("receive_count", m.ReceiveCount),
	}
	return append(attrs, stageAttrs(m.Stage)...)
}

// stageAttrs pulls the payload identifiers out of a StageMessage, tolerating nil
// so a logging failure can never mask the error being logged.
func stageAttrs(msg *events.StageMessage) []any {
	if msg == nil {
		return nil
	}
	return []any{
		slog.String("job_id", msg.JobID),
		slog.String("stage", string(msg.Stage)),
		slog.String("trace_id", msg.TraceID),
	}
}

func durationAttr(start time.Time) slog.Attr {
	return slog.Int64("duration_ms", time.Since(start).Milliseconds())
}
