# Stage 3B: Local Queue (SQLite)

> **Run in parallel with:** Stage 2A (Go API)
> **Depends on:** Stage 1A (Data Schemas)
> **Blocks:** Stage 3A (Validate Worker) and every stage after it

## Aim

Make a self-hosted SQLite queue at `data/queue.db` the default broker, keeping
the properties the pipeline actually depends on — visibility timeout,
at-least-once delivery, redelivery counting, lease heartbeats and dead-lettering
— so the local stack is two Go processes and a file instead of a container fleet.

> **Superseded in part.** This stage originally replaced SQS outright, and the
> code shipped that way. SQS was subsequently restored as a *second driver*
> behind the same `Queue` interface, selected by `QUEUE_DRIVER` (`sqlite` by
> default, `sqs` opt-in), because "the workers must share a filesystem with the
> API" is a real ceiling on a single-host queue. Nothing below changed: the
> interface, the SQLite driver and the message contract are all as planned. What
> changed is that SQLite is now the default rather than the only option — see
> `backend/internal/queue/CONTEXT.md` for where the two drivers' semantics
> differ.

## Components

| Component | Action |
|-----------|--------|
| `backend/internal/queue/` | Create |
| `backend/internal/events/` | Reuse — `StageMessage` is the payload, unchanged |
| `backend/cmd/api/` | Modify — enqueue the validate message on upload complete |
| `backend/internal/worker/runner.go` | Modify — claim/ack against the `Queue` interface, whichever driver is behind it |
| `data/queue.db` | Runtime artifact, gitignored |

## Boundaries

### Queue Message

The queue is a transport for the existing `events.StageMessage`. It is stored as
a JSON blob in the `body` column and is not interpreted by the queue.

```json
{
  "job_id": "550e8400-e29b-41d4-a716-446655440000",
  "stage": "validate",
  "input": {"bucket": "dayreel-raw-videos", "key": "550e8400/input.mp4"},
  "attempt": 1,
  "timestamp": "2026-08-12T10:30:00Z",
  "trace_id": "..."
}
```

Two "message" types meet here and must not be confused. `events.StageMessage` is
the **payload**: owned by the producer, a contract between stages, stored and
returned verbatim. `queue.Message` is the **envelope**: owned by the broker,
describing the delivery — which lease you hold, how many times this payload has
been handed out, when it was enqueued.

### Table Schema

```sql
CREATE TABLE IF NOT EXISTS messages (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  queue         TEXT    NOT NULL,
  body          TEXT    NOT NULL,   -- the StageMessage JSON, opaque here
  receipt       TEXT,               -- set on claim, regenerated on every delivery
  visible_at    INTEGER NOT NULL,   -- epoch ms; the visibility timeout
  receive_count INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,   -- epoch ms
  claimed_at    INTEGER,            -- epoch ms, for the stage-duration metric
  dlq_reason    TEXT
);

CREATE INDEX IF NOT EXISTS idx_claim ON messages (queue, visible_at, id);
```

Timestamps are epoch milliseconds rather than RFC3339 text so the claim
predicate is an integer comparison on an indexed column. `make queue-peek`
renders them with `datetime(visible_at/1000, 'unixepoch')`, and depends on these
exact column names.

### Go Interface

```go
package queue

// Queue is the broker contract. Receive must claim atomically, so two workers
// can never hold a lease on the same message at the same time.
type Queue interface {
    Send(ctx context.Context, queue string, msg *events.StageMessage, delay time.Duration) error
    Receive(ctx context.Context, queue string, max int, wait time.Duration) ([]Message, error)
    Ack(ctx context.Context, m Message) error
    Nack(ctx context.Context, m Message, backoff time.Duration) error
    Heartbeat(ctx context.Context, m Message, extend time.Duration) error
    DeadLetter(ctx context.Context, m Message, reason string) error
    Stats(ctx context.Context) ([]QueueStat, error)
    Close() error
}

// ErrLeaseLost is returned by Ack, Nack, Heartbeat and DeadLetter when the
// receipt no longer matches: the visibility timeout expired and someone else
// owns the message now.
var ErrLeaseLost = errors.New("queue: lease lost, receipt no longer valid")
```

The vocabulary stays deliberately close to SQS — receipt handles, visibility
timeout, receive count, long polling — so the pipeline reads the same way it did
before, and so moving back to a hosted broker is a driver swap rather than a
rewrite. That prediction was tested: restoring SQS was `internal/queue/sqs.go`
plus a factory, with no change to the runner, the stages or the message
contract.

### Queue Names

Unchanged from the SQS design. On the SQLite driver they are values of the
`queue` column; on the SQS driver they are the SQS queue names that
`scripts/aws-sqs-setup.sh` creates. `QueueValidate`, `QueueExtract`,
`QueueTranscribe`, `QueuePackage`, `QueueDLQ` in
`backend/internal/events/messages.go`.

### Configuration

| Variable | Default | Meaning |
|----------|---------|---------|
| `QUEUE_DRIVER` | `sqlite` | Which broker: `sqlite` or `sqs`. Added when SQS returned as a second driver |
| `QUEUE_DB_PATH` | `./data/queue.db` | SQLite only. The file, created on first open; its parent directory is created by `queue.FromConfig` |
| `QUEUE_VISIBILITY_TIMEOUT` | `5m` | How long a claim hides a message. On SQS it must match the queue's own attribute |
| `QUEUE_MAX_DELIVERIES` | `3` | Deliveries before dead-lettering |
| `QUEUE_POLL_INTERVAL` | `250ms` (sqlite) / `20s` (sqs) | How long one receive waits. A local ticker on SQLite; `WaitTimeSeconds` on SQS, where 250ms would mean a billed short poll |

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/queue/queue.go` | Create | `Queue` interface, `Message`, `QueueStat`, `ErrLeaseLost` |
| `backend/internal/queue/sqlite.go` | Create | Schema, Send, Receive, Ack, Nack, Heartbeat, DeadLetter, Stats |
| `backend/internal/queue/logging.go` | Create | `WithLogging` decorator — one structured line per operation |
| `backend/internal/queue/sqlite_test.go` | Create | Concurrency, expiry, heartbeat, DLQ and long-poll tests |
| `backend/internal/queue/CONTEXT.md` | Create | Package documentation |
| `backend/cmd/api/main.go` | Modify | Open the queue, pass to handlers |
| `backend/internal/api/handlers.go` | Modify | Enqueue validate on upload complete |
| `backend/go.mod` | Modify | Add `modernc.org/sqlite` |
| `Makefile` | Modify | `queue-peek`, `queue-reset` |

## Tasks

_Ordered implementation steps. Check off as you complete._

1. [ ] Add `modernc.org/sqlite` to `go.mod`
2. [ ] Open the DB with WAL, `busy_timeout` and `synchronous=NORMAL` on the DSN
3. [ ] Create the `messages` table and claim index on open (idempotent)
4. [ ] Implement `Send`, including delayed delivery
5. [ ] Implement `Receive` as a single `UPDATE … RETURNING`, polling until `wait`
6. [ ] Implement `Ack` (delete, matched on id **and** receipt)
7. [ ] Implement `Nack` with backoff, and `DeadLetter` with a reason
8. [ ] Implement `Heartbeat` — extends the lease without incrementing the count
9. [ ] Implement `Stats` for `queue-peek`-style visibility
10. [ ] Wire the API to enqueue the validate message on `POST /jobs/{id}/complete`
11. [ ] Point `worker/runner.go` at the queue interface
12. [ ] Write the concurrency test (many goroutines, one message)
13. [ ] Write the visibility-expiry and dead-letter tests
14. [ ] Write `backend/internal/queue/CONTEXT.md`

### The claim statement

This is the whole stage, really:

```sql
UPDATE messages
   SET receipt       = lower(hex(randomblob(16))),
       visible_at    = ?,                  -- now + visibility timeout
       receive_count = receive_count + 1,
       claimed_at    = ?
 WHERE id IN (
   SELECT id FROM messages
    WHERE queue = ? AND visible_at <= ?    -- now
    ORDER BY id
    LIMIT ?
 )
RETURNING id, queue, body, receipt, receive_count, created_at, claimed_at, visible_at;
```

The inner `SELECT` runs inside the same statement as the `UPDATE`, so SQLite
holds the write lock across both. A `SELECT` followed by a separate `UPDATE`
does not: two workers read the same row, both update it, both process the same
job. That is the bug this statement exists to prevent. `randomblob()` is
non-deterministic, so a batch claim gets one receipt per row rather than one
shared receipt.

## Test

```bash
cd backend && go test ./internal/queue/...
```

The tests that carry the stage:

| Test | What it pins |
|------|--------------|
| `TestConcurrentClaimNeverDoubleDelivers` | Many goroutines, one message, exactly one winner. Fails if `Receive` is rewritten as SELECT-then-UPDATE |
| `TestLeaseExpiryRedelivers` | An unacked claim becomes claimable again, `receive_count` one higher |
| `TestAckAfterLeaseLossIsRejected` | A stale receipt cannot delete someone else's message |
| `TestHeartbeatExtendsLeaseWithoutCountingAsRedelivery` | A slow stage can hold its lease without spending its delivery budget |
| `TestDeadLetterAfterMaxDeliveries` | The row moves to `dayreel-dlq` keeping its `id` |
| `TestMessagesSurviveReopen` | The queue is durable across process restarts |

End-to-end, by hand:

```bash
make queue-reset
make api                      # enqueues validate on POST /jobs/{id}/complete
make queue-peek               # row in dayreel-validate
make worker STAGE=validate    # in another terminal
make queue-peek               # row gone, or in dayreel-dlq after 3 failures
```

## Verification

- [ ] `go test ./internal/queue/...` passes, including the concurrency test
- [ ] `data/queue.db` is created on first run, and `make queue-reset` removes it
- [ ] `make queue-peek` shows a message appear, `receive_count` increment on
      claim, and the row disappear on ack
- [ ] A worker that panics mid-stage leaves a row that becomes claimable again
      after `QUEUE_VISIBILITY_TIMEOUT`, with `receive_count` one higher
- [ ] A message failed 3 times ends up with `queue = 'dayreel-dlq'`, same `id`,
      and a `dlq_reason`
- [ ] `go build ./...` succeeds with `CGO_ENABLED=0`

## Notes

- **A visibility timeout is a timestamp, not a process.** `visible_at` is a
  column; a message becomes claimable again simply because `WHERE visible_at <=
  now` starts matching it. Nothing sweeps expired leases, there is no reaper
  goroutine, and a crashed worker needs no cleanup — its messages come back on
  their own.

- **`Receive` must be one statement.** A single `UPDATE … RETURNING` is atomic
  under SQLite's write lock. `SELECT` then `UPDATE` is two statements with a gap
  between them, and two workers polling the same queue will both claim the same
  row. The concurrency test exists specifically to catch a future refactor that
  splits them.

- **Ack matches on `receipt`, not just `id`.** A worker whose lease expired
  mid-stage will eventually finish and call `Ack`. By then the message may have
  been re-claimed by someone else with a new receipt, and the slow worker's
  delete must not land. It returns `ErrLeaseLost` rather than failing silently:
  "my work was thrown away" is exactly the condition a worker must be able to
  see and log.

- **Dead-lettering is one column change.** `UPDATE messages SET queue =
  'dayreel-dlq', dlq_reason = ? WHERE id = ?`. Not a copy-and-delete: copying is
  two statements that can half-fail, and it loses the original `id`, which is
  the only thing tying a dead letter back to its history. The DLQ is the same
  table.

- **The redelivery budget is enforced by the worker, not the broker.** Only the
  worker knows whether a failure is retryable, so `MaxDeliveries` is configured
  centrally and consulted at the call site. The broker never dead-letters on its
  own.

- **Driver is `modernc.org/sqlite`, not `mattn/go-sqlite3`.** `mattn` is a cgo
  binding; the build runs with `CGO_ENABLED=0` (originally because of the
  container image, now simply because a static binary is easier to move onto a
  VM). `modernc.org/sqlite` is a pure-Go transpilation of SQLite, so it builds
  with no toolchain changes. It is slower, which does not matter for a queue
  moving a handful of messages per clip.

- **WAL mode and `busy_timeout`.** Two processes (API writing, worker claiming)
  share the file. WAL lets the reader proceed during a write; `busy_timeout`
  turns the remaining lock contention into a short wait instead of an immediate
  `SQLITE_BUSY` error. Both are set on the DSN, not once at open, because
  `database/sql` pools connections and per-connection pragmas would otherwise
  apply to only one of them.

- **Long polling is a ticker, not a blocking wait.** SQLite has no server-side
  notification, so `Receive` retries the claim every `QUEUE_POLL_INTERVAL` until
  `wait` elapses. At 250ms an idle worker costs four queries a second against a
  local file and adds at most a quarter second of latency per stage. A condition
  variable is not available across processes and is not worth a second IPC
  mechanism.

- **Deferred: multi-host.** The queue is a file on one disk. This is fine for one
  VM and fine for local development, and it is the exact point at which a real
  broker becomes necessary if a second worker host ever appears. The `Queue`
  interface is what a hosted broker would implement, so the swap is contained.
