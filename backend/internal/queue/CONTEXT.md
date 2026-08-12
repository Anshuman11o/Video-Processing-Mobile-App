# Queue Package

This package is DayReel's message broker: a self-hosted, SQS-equivalent queue backed by a single SQLite file. It replaces Amazon SQS (and the LocalStack that emulated it) for the video processing pipeline.

## Overview

The queue package defines:

- **Queue**: The broker interface — `Send`, `Receive`, `Ack`, `Nack`, `Heartbeat`, `DeadLetter`, `Stats`, `Close`
- **Message**: The delivery envelope (broker-owned), wrapping an `events.StageMessage` payload (producer-owned)
- **SQLiteQueue**: The driver, one SQLite file, no server, no cgo
- **LoggingQueue**: A decorator that logs every operation with `log/slog`
- **QueueStat**: Per-queue depth for backlog monitoring

## Files

| File | Purpose |
|------|---------|
| `queue.go` | Interface, `Message`, `QueueStat`, `ErrLeaseLost` |
| `sqlite.go` | SQLite driver: schema, atomic claim, lease operations |
| `logging.go` | `WithLogging` decorator (structured logs, no SQL) |
| `sqlite_test.go` | Driver tests, including the concurrent-claim stress test |
| `logging_test.go` | Decorator tests (attributes, log levels) |

## SQS Mapping

| SQS concept | Here |
|-------------|------|
| MessageId | `Message.ID` (SQLite rowid) |
| ReceiptHandle | `Message.Receipt` (128-bit random hex, new on every delivery) |
| ApproximateReceiveCount | `Message.ReceiveCount` |
| SentTimestamp | `Message.EnqueuedAt` |
| VisibilityTimeout | `Options.VisibilityTimeout` (default 5m) |
| ChangeMessageVisibility | `Heartbeat` |
| DeleteMessage | `Ack` |
| DelaySeconds | the `delay` argument to `Send` |
| Long polling | the `wait` argument to `Receive` |
| Redrive policy / maxReceiveCount | `Options.MaxDeliveries` + caller-driven `DeadLetter` |

## The Atomic Claim

Everything else in this package is bookkeeping around one statement:

```sql
UPDATE messages
   SET receipt = lower(hex(randomblob(16))), visible_at = ?, receive_count = receive_count + 1, claimed_at = ?
 WHERE id IN (SELECT id FROM messages WHERE queue = ? AND visible_at <= ? ORDER BY id LIMIT ?)
RETURNING id, queue, body, receipt, receive_count, created_at, claimed_at, visible_at
```

Selecting and then updating in two statements leaves a window where two workers claim the same row and transcode the same video twice. Folding it into one `UPDATE` makes SQLite hold the write lock across the selection, so a row can only be leased once — across goroutines *and* across processes. `randomblob()` is non-deterministic, so a batch claim gets one distinct receipt per row.

Every mutating statement is guarded by `WHERE id = ? AND receipt = ?`. Zero rows affected therefore has exactly one meaning — the lease expired and someone else owns the message — which surfaces as `ErrLeaseLost` rather than a silent success.

## Schema

One table, one index, created idempotently on `Open`:

```sql
CREATE TABLE IF NOT EXISTS messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  queue         TEXT    NOT NULL,   -- routing; the DLQ is a queue name, not a separate table
  body          TEXT    NOT NULL,   -- JSON events.StageMessage, stored verbatim
  receipt       TEXT,               -- NULL when unclaimed
  visible_at    INTEGER NOT NULL,   -- unix millis; delivery delay AND lease expiry
  receive_count INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  claimed_at    INTEGER,
  dlq_reason    TEXT
);
CREATE INDEX IF NOT EXISTS idx_claim ON messages(queue, visible_at, id);
```

`visible_at` does triple duty: delayed delivery, visibility timeout, and nack backoff are all "make this row invisible until time T". Dead-lettering is a single `UPDATE` of the `queue` column, so a message keeps the id that appears in every failed attempt's logs.

## Driver

`modernc.org/sqlite` (pure Go), which registers itself as **`sqlite`**, not `sqlite3`. The cgo-backed `mattn/go-sqlite3` cannot be used: the API binary is built with `CGO_ENABLED=0`.

Pragmas ride on the DSN (`?_pragma=busy_timeout(5000)&...`) because `busy_timeout` and `synchronous` are per-connection settings — a one-off `PRAGMA` through `database/sql` would only stick to whichever pooled connection ran it. `journal_mode=WAL` is persisted in the database header.

## Usage

```go
q, err := queue.Open(queue.Options{
    Path:              cfg.QueueDBPath,
    VisibilityTimeout: cfg.QueueVisibilityTimeout,
    MaxDeliveries:     cfg.QueueMaxDeliveries,
    PollInterval:      cfg.QueuePollInterval,
})
q = queue.WithLogging(q, slog.Default())
defer q.Close()

// Producer (API)
q.Send(ctx, events.QueueValidate, stageMsg, 0)

// Consumer (worker)
msgs, err := q.Receive(ctx, events.QueueValidate, 1, cfg.QueuePollInterval)
for _, m := range msgs {
    // The budget is checked on pickup, not only on failure: most of the ways a
    // stage gives up never reach the error branch below (a failed database
    // write, a worker killed mid-stage), and those simply let the lease expire.
    // Without this they would be redelivered forever.
    if m.ReceiveCount > maxDeliveries {
        q.DeadLetter(ctx, m, "exceeded the delivery budget")
        continue
    }

    if err := process(ctx, m.Stage); err != nil {
        if permanent(err) || m.ReceiveCount >= maxDeliveries {
            q.DeadLetter(ctx, m, err.Error())
            continue
        }
        q.Nack(ctx, m, backoff(m.ReceiveCount))
        continue
    }

    if err := q.Ack(ctx, m); errors.Is(err, ErrLeaseLost) {
        // Another worker re-claimed this while we were slow. Our output is a
        // duplicate; log it and move on.
    }
}
```

The real consumer is `internal/worker/runner.go`, which is the only implementation of this loop — the four stages share it and none of them talks to the queue directly. The only producers are `api.CompleteUpload` (the validate message, which is what starts a job) and the runner itself (each stage publishing the next).

Long-running stages should `Heartbeat` before `LeaseRemaining()` runs out. A heartbeat deliberately does not bump `ReceiveCount` — it is the same delivery continuing, and counting it would burn the redelivery budget of exactly the slow-but-healthy jobs it exists to protect.

## Delivery Semantics

At-least-once, same as SQS. A worker can crash after finishing its work and before acking, and the message will be redelivered — **every stage must be idempotent**. `ReceiveCount` is the retry signal, and reaching `MaxDeliveries` is the cue to dead-letter.

Dead-lettering is a worker decision, not a broker one: only the worker knows whether a failure is retryable. The broker just supplies the budget so every stage agrees on it. In practice the runner makes three different calls out of it:

| Failure | Call |
|---------|------|
| Permanent — a codec the pipeline rejects, a corrupt file | `DeadLetter` immediately, whatever the count. The remaining deliveries would fail identically and arrive carrying nothing new. |
| Transient with budget left | `Nack` with a doubling backoff, 10s to a 5m ceiling. |
| Transient on the last delivery of the budget | `DeadLetter`, with the failure that exhausted it as the reason. |

`ErrLeaseLost` from `Ack` is not a failure of the stage. It means this worker ran past its lease, another worker claimed the message, and that worker's delivery is the one that counts — the work here is a duplicate. The runner logs it and drops it; retrying would produce a second duplicate.

## Configuration

| Env | Config field | Default |
|-----|--------------|---------|
| `QUEUE_DB_PATH` | `QueueDBPath` | `./data/queue.db` |
| `QUEUE_VISIBILITY_TIMEOUT` | `QueueVisibilityTimeout` | `5m` |
| `QUEUE_MAX_DELIVERIES` | `QueueMaxDeliveries` | `3` |
| `QUEUE_POLL_INTERVAL` | `QueuePollInterval` | `250ms` |

Queue names come from `internal/events` constants (`dayreel-validate`, `dayreel-extract`, `dayreel-transcribe`, `dayreel-package`, `dayreel-dlq`), unchanged from the SQS era.

## Known Trade-offs

- **Single file, single host.** Workers must share a filesystem with the API. That is the deal SQLite offers; it is fine for a single-box pipeline and is the reason `Receive` is safe across processes and not just across goroutines.
- **Polling, not push.** SQLite has no server-side blocking read, so long polling is a ticker (`PollInterval`). An idle worker costs a few queries a second, not a pegged core. Every claim takes SQLite's write lock even when it matches nothing, which is why the runner backs off exponentially on a failing `Receive` rather than retrying immediately.
- **One visibility timeout for every queue.** SQS let each queue carry its own; here `QUEUE_VISIBILITY_TIMEOUT` applies to all of them. Transcribe, the one stage that can genuinely outrun five minutes, relies on the runner's heartbeat instead of a longer lease.
- **`Stats` counts are approximate.** A message whose lease expired but which nobody re-claimed still carries a receipt, so it reads as in-flight even though it is claimable. SQS documents its own counts as approximate for similar reasons.
