package queue

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	// Pure-Go SQLite driver. It registers itself as "sqlite" (not "sqlite3").
	// The cgo-backed mattn/go-sqlite3 is deliberately avoided: the API image is
	// built with CGO_ENABLED=0 and would not link against it.
	_ "modernc.org/sqlite"

	"github.com/anshumanagarwal/dayreel/internal/events"
)

const (
	defaultVisibilityTimeout = 5 * time.Minute
	defaultPollInterval      = 250 * time.Millisecond
	defaultMaxDeliveries     = 3
	defaultMaxMessages       = 1
)

// schema is applied on every Open. Every statement is idempotent, so opening an
// existing queue file is the same code path as creating a new one.
//
// journal_mode is persisted in the database header, so setting it here sticks;
// busy_timeout and synchronous are per-connection and are set on the DSN
// instead, otherwise they would only apply to whichever pooled connection
// happened to run this block.
const schema = `
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA synchronous  = NORMAL;

CREATE TABLE IF NOT EXISTS messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  queue         TEXT    NOT NULL,
  body          TEXT    NOT NULL,
  receipt       TEXT,
  visible_at    INTEGER NOT NULL,
  receive_count INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  claimed_at    INTEGER,
  dlq_reason    TEXT
);
CREATE INDEX IF NOT EXISTS idx_claim ON messages(queue, visible_at, id);
`

// claimSQL is the heart of the broker: it selects and leases in a single
// statement so the claim is atomic.
//
// The obvious implementation — SELECT the oldest visible row, then UPDATE it —
// has a window between the two statements in which a second worker can select
// the same row, and both workers then process the same video. Folding it into
// one UPDATE makes SQLite hold the write lock across the whole selection, so a
// row can only be leased once.
//
// randomblob() is non-deterministic, so SQLite evaluates it per row: a batch
// claim of five messages gets five distinct receipts, not one shared receipt.
const claimSQL = `
UPDATE messages
   SET receipt       = lower(hex(randomblob(16))),
       visible_at    = ?,
       receive_count = receive_count + 1,
       claimed_at    = ?
 WHERE id IN (
   SELECT id FROM messages
    WHERE queue = ? AND visible_at <= ?
    ORDER BY id
    LIMIT ?
 )
RETURNING id, queue, body, receipt, receive_count, created_at, claimed_at, visible_at
`

// Options configures a SQLiteQueue. The zero value of every field falls back to
// the package default, so callers only set what they care about.
type Options struct {
	// Path is the SQLite file. Its directory must already exist.
	Path string

	// VisibilityTimeout is how long a claim hides a message from other workers.
	VisibilityTimeout time.Duration

	// MaxDeliveries is the redelivery budget before a worker should
	// dead-letter. The broker does not enforce it — dead-lettering is a worker
	// decision, because only the worker knows whether the failure was
	// retryable — but the budget is configured centrally so every stage agrees.
	MaxDeliveries int

	// PollInterval is how often Receive retries the claim while long-polling.
	PollInterval time.Duration

	// DLQName is the queue DeadLetter moves messages to.
	DLQName string
}

func (o Options) withDefaults() Options {
	if o.VisibilityTimeout <= 0 {
		o.VisibilityTimeout = defaultVisibilityTimeout
	}
	if o.PollInterval <= 0 {
		o.PollInterval = defaultPollInterval
	}
	if o.MaxDeliveries <= 0 {
		o.MaxDeliveries = defaultMaxDeliveries
	}
	if o.DLQName == "" {
		o.DLQName = events.QueueDLQ
	}
	return o
}

// SQLiteQueue is a Queue backed by one SQLite file. It is safe for concurrent
// use, and safe across processes: correctness comes from SQLite's own write
// lock, not from in-process coordination.
type SQLiteQueue struct {
	db   *sql.DB
	opts Options
}

// compile-time check that the driver satisfies the broker contract.
var _ Queue = (*SQLiteQueue)(nil)

// Open opens (creating it if needed) the queue database at opts.Path and applies
// the schema.
func Open(opts Options) (*SQLiteQueue, error) {
	if opts.Path == "" {
		return nil, fmt.Errorf("queue: open: path is required")
	}
	opts = opts.withDefaults()

	db, err := sql.Open("sqlite", dsn(opts.Path))
	if err != nil {
		return nil, fmt.Errorf("queue: open %s: %w", opts.Path, err)
	}

	// SQLite serialises writers itself, and almost every queue operation is a
	// write. Keeping a small pool means connections stay warm (each new one
	// re-runs the DSN pragmas) while still letting a Stats read overlap with a
	// claim; contention beyond that is absorbed by busy_timeout.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("queue: apply schema: %w", err)
	}

	return &SQLiteQueue{db: db, opts: opts}, nil
}

// dsn builds the connection string. modernc.org/sqlite applies each _pragma to
// every connection it opens, which is the only reliable way to set
// per-connection pragmas behind database/sql's pool.
func dsn(path string) string {
	return "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
}

// MaxDeliveries reports the configured redelivery budget so workers can decide
// when to dead-letter. It is not part of the Queue interface because it is
// policy, not broker behaviour.
func (q *SQLiteQueue) MaxDeliveries() int { return q.opts.MaxDeliveries }

// DLQName reports the dead-letter queue name.
func (q *SQLiteQueue) DLQName() string { return q.opts.DLQName }

// Send enqueues msg on queue, invisible for delay first.
func (q *SQLiteQueue) Send(ctx context.Context, queue string, msg *events.StageMessage, delay time.Duration) error {
	if msg == nil {
		return fmt.Errorf("queue: send to %s: message is nil", queue)
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("queue: marshal message for %s: %w", queue, err)
	}
	if delay < 0 {
		delay = 0
	}

	now := time.Now().UTC()
	_, err = q.db.ExecContext(ctx,
		`INSERT INTO messages (queue, body, visible_at, created_at) VALUES (?, ?, ?, ?)`,
		queue, string(body), millis(now.Add(delay)), millis(now),
	)
	if err != nil {
		return fmt.Errorf("queue: send to %s: %w", queue, err)
	}
	return nil
}

// Receive claims up to max messages from queue, long-polling for up to wait.
//
// Polling is a ticker rather than a busy loop: an idle stage worker should cost
// four queries a second, not a pegged core. SQLite has no server-side blocking
// read, so this is the honest equivalent of SQS long polling.
func (q *SQLiteQueue) Receive(ctx context.Context, queue string, max int, wait time.Duration) ([]Message, error) {
	if max <= 0 {
		max = defaultMaxMessages
	}
	deadline := time.Now().Add(wait)

	for {
		msgs, err := q.claim(ctx, queue, max)
		if err != nil {
			return nil, err
		}
		if len(msgs) > 0 {
			return msgs, nil
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, nil
		}

		sleep := q.opts.PollInterval
		if remaining < sleep {
			sleep = remaining
		}
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (q *SQLiteQueue) claim(ctx context.Context, queue string, max int) ([]Message, error) {
	now := time.Now().UTC()
	nowMS := millis(now)
	expiry := millis(now.Add(q.opts.VisibilityTimeout))

	rows, err := q.db.QueryContext(ctx, claimSQL, expiry, nowMS, queue, nowMS, max)
	if err != nil {
		return nil, fmt.Errorf("queue: claim from %s: %w", queue, err)
	}
	defer rows.Close()

	var msgs []Message
	for rows.Next() {
		var (
			m         Message
			body      string
			receipt   sql.NullString
			createdMS int64
			claimedMS sql.NullInt64
			visibleMS int64
		)
		if err := rows.Scan(&m.ID, &m.Queue, &body, &receipt, &m.ReceiveCount, &createdMS, &claimedMS, &visibleMS); err != nil {
			return nil, fmt.Errorf("queue: scan claimed message from %s: %w", queue, err)
		}

		var stage events.StageMessage
		if err := json.Unmarshal([]byte(body), &stage); err != nil {
			// A body that will not decode can never be processed, so leaving it
			// on the queue would mean redelivering it forever. Surface it: the
			// caller can dead-letter by id.
			return nil, fmt.Errorf("queue: decode message %d from %s: %w", m.ID, queue, err)
		}

		m.Receipt = receipt.String
		m.MessageID = strconv.FormatInt(m.ID, 10)
		m.EnqueuedAt = fromMillis(createdMS)
		if claimedMS.Valid {
			m.ClaimedAt = fromMillis(claimedMS.Int64)
		}
		m.LeaseExpiresAt = fromMillis(visibleMS)
		m.Stage = &stage
		msgs = append(msgs, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: read claimed messages from %s: %w", queue, err)
	}
	return msgs, nil
}

// Ack deletes a finished message. The receipt is part of the WHERE clause on
// purpose: if the lease expired and someone else re-claimed the message, this
// delete matches nothing and the other worker's delivery survives.
func (q *SQLiteQueue) Ack(ctx context.Context, m Message) error {
	res, err := q.db.ExecContext(ctx,
		`DELETE FROM messages WHERE id = ? AND receipt = ?`, m.ID, m.Receipt)
	if err != nil {
		return fmt.Errorf("queue: ack message %d: %w", m.ID, err)
	}
	return leaseCheck(res, "ack", m.ID)
}

// Nack releases the lease early and makes the message claimable again after
// backoff, for a failure the worker expects to succeed on retry.
func (q *SQLiteQueue) Nack(ctx context.Context, m Message, backoff time.Duration) error {
	if backoff < 0 {
		backoff = 0
	}
	res, err := q.db.ExecContext(ctx,
		`UPDATE messages SET visible_at = ?, receipt = NULL, claimed_at = NULL WHERE id = ? AND receipt = ?`,
		millis(time.Now().UTC().Add(backoff)), m.ID, m.Receipt)
	if err != nil {
		return fmt.Errorf("queue: nack message %d: %w", m.ID, err)
	}
	return leaseCheck(res, "nack", m.ID)
}

// Heartbeat pushes the lease out by extend from now, for work that runs longer
// than the visibility timeout.
//
// It deliberately leaves receive_count alone: a heartbeat is the same delivery
// continuing, and counting it would burn the redelivery budget of exactly the
// slow-but-healthy jobs the heartbeat exists to protect.
func (q *SQLiteQueue) Heartbeat(ctx context.Context, m Message, extend time.Duration) error {
	if extend <= 0 {
		extend = q.opts.VisibilityTimeout
	}
	res, err := q.db.ExecContext(ctx,
		`UPDATE messages SET visible_at = ? WHERE id = ? AND receipt = ?`,
		millis(time.Now().UTC().Add(extend)), m.ID, m.Receipt)
	if err != nil {
		return fmt.Errorf("queue: heartbeat message %d: %w", m.ID, err)
	}
	return leaseCheck(res, "heartbeat", m.ID)
}

// DeadLetter parks a message on the dead-letter queue with the reason it got
// there.
//
// The move is a single column update rather than a copy-and-delete, so it cannot
// half-succeed and the message keeps its id — the same id that appears in the
// logs of every failed attempt, which is what makes a DLQ drain debuggable.
// visible_at is reset to now as well, otherwise the message would stay
// invisible on the DLQ until the dead lease expired.
func (q *SQLiteQueue) DeadLetter(ctx context.Context, m Message, reason string) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE messages SET queue = ?, receipt = NULL, claimed_at = NULL, visible_at = ?, dlq_reason = ? WHERE id = ? AND receipt = ?`,
		q.opts.DLQName, millis(time.Now().UTC()), reason, m.ID, m.Receipt)
	if err != nil {
		return fmt.Errorf("queue: dead-letter message %d: %w", m.ID, err)
	}
	return leaseCheck(res, "dead-letter", m.ID)
}

// Stats returns depth per queue.
//
// A message whose lease has expired but which nobody has re-claimed yet still
// carries a receipt, so it counts as in-flight here even though it is claimable.
// That mirrors SQS, whose own counts are documented as approximate, and it keeps
// the query a single pass over the table.
func (q *SQLiteQueue) Stats(ctx context.Context) ([]QueueStat, error) {
	nowMS := millis(time.Now().UTC())

	rows, err := q.db.QueryContext(ctx, `
SELECT queue,
       SUM(CASE WHEN receipt IS NULL AND visible_at <= ? THEN 1 ELSE 0 END),
       SUM(CASE WHEN receipt IS NOT NULL THEN 1 ELSE 0 END),
       MIN(CASE WHEN receipt IS NULL AND visible_at <= ? THEN created_at END)
  FROM messages
 GROUP BY queue
 ORDER BY queue`, nowMS, nowMS)
	if err != nil {
		return nil, fmt.Errorf("queue: stats: %w", err)
	}
	defer rows.Close()

	var stats []QueueStat
	for rows.Next() {
		var (
			st       QueueStat
			oldestMS sql.NullInt64
		)
		if err := rows.Scan(&st.Queue, &st.Visible, &st.InFlight, &oldestMS); err != nil {
			return nil, fmt.Errorf("queue: scan stats: %w", err)
		}
		if oldestMS.Valid {
			st.Oldest = time.Since(fromMillis(oldestMS.Int64))
		}
		stats = append(stats, st)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: read stats: %w", err)
	}
	return stats, nil
}

// Close closes the underlying database.
func (q *SQLiteQueue) Close() error {
	if err := q.db.Close(); err != nil {
		return fmt.Errorf("queue: close: %w", err)
	}
	return nil
}

// leaseCheck turns "matched no rows" into ErrLeaseLost. Every mutating statement
// is guarded by `receipt = ?`, so zero rows affected has exactly one meaning:
// this worker no longer holds the lease.
func leaseCheck(res sql.Result, op string, id int64) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("queue: %s message %d: rows affected: %w", op, id, err)
	}
	if n == 0 {
		return fmt.Errorf("queue: %s message %d: %w", op, id, ErrLeaseLost)
	}
	return nil
}

// millis and fromMillis are the only places time crosses the storage boundary.
// Unix milliseconds keep the comparisons in visible_at as plain integer
// arithmetic, which is what the idx_claim index can actually use.
func millis(t time.Time) int64 { return t.UnixMilli() }

func fromMillis(ms int64) time.Time { return time.UnixMilli(ms).UTC() }
