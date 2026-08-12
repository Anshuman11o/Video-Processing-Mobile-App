# Stage 3B: Local Queue (SQLite)

> **Run in parallel with:** Stage 2A (Go API)
> **Depends on:** Stage 1A (Data Schemas)
> **Blocks:** Stage 3A (Validate Worker) and every stage after it

## Aim

Replace SQS with a self-hosted SQLite queue at `data/queue.db` that keeps the
four properties the pipeline actually depends on — visibility timeout,
at-least-once delivery, delivery counting, and dead-lettering — so the local
stack is two Go processes and a file instead of a container fleet.

## Components

| Component | Action |
|-----------|--------|
| `backend/internal/queue/` | Create |
| `backend/internal/events/` | Reuse — `StageMessage` is the payload, unchanged |
| `backend/cmd/api/` | Modify — enqueue the validate message on upload complete |
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

### Table Schema

```sql
CREATE TABLE IF NOT EXISTS messages (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  queue       TEXT    NOT NULL,
  body        TEXT    NOT NULL,
  receipt     TEXT,                        -- set on claim, cleared on release
  deliveries  INTEGER NOT NULL DEFAULT 0,
  visible_at  TEXT    NOT NULL,            -- RFC3339 UTC; the visibility timeout
  created_at  TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_claim ON messages (queue, visible_at, id);
```

### Go Interface

```go
package queue

type Message struct {
    ID         int64
    Queue      string
    Body       []byte
    Receipt    string
    Deliveries int
}

// Enqueue inserts a message visible immediately.
func (q *Queue) Enqueue(ctx context.Context, name string, body []byte) error

// Claim atomically takes one visible message and hides it for the visibility
// timeout. Returns (nil, nil) when the queue is empty.
func (q *Queue) Claim(ctx context.Context, name string) (*Message, error)

// Ack deletes a claimed message. Matched on receipt.
func (q *Queue) Ack(ctx context.Context, id int64, receipt string) error

// Nack makes a message visible again immediately, or dead-letters it if it has
// reached QUEUE_MAX_DELIVERIES.
func (q *Queue) Nack(ctx context.Context, id int64, receipt string) error
```

### Queue Names

Unchanged from the SQS design — they are now values of the `queue` column, not
AWS resources. `QueueValidate`, `QueueExtract`, `QueueTranscribe`,
`QueuePackage`, `QueueDLQ` in `backend/internal/events/messages.go`.

### Configuration

| Variable | Default | Meaning |
|----------|---------|---------|
| `QUEUE_DB_PATH` | `./data/queue.db` | SQLite file, created on first open |
| `QUEUE_VISIBILITY_TIMEOUT` | `5m` | How long a claim hides a message |
| `QUEUE_MAX_DELIVERIES` | `3` | Deliveries before dead-lettering |
| `QUEUE_POLL_INTERVAL` | `250ms` | Idle re-check interval |

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/queue/sqlite.go` | Create | Schema, Enqueue, Claim, Ack, Nack |
| `backend/internal/queue/sqlite_test.go` | Create | Concurrency and expiry tests |
| `backend/internal/queue/CONTEXT.md` | Create | Package documentation |
| `backend/cmd/api/main.go` | Modify | Open the queue, pass to handlers |
| `backend/internal/api/handlers.go` | Modify | Enqueue validate on upload complete |
| `backend/go.mod` | Modify | Add `modernc.org/sqlite` |

## Tasks

_Ordered implementation steps. Check off as you complete._

1. [ ] Add `modernc.org/sqlite` to `go.mod`
2. [ ] Open the DB, `PRAGMA journal_mode=WAL`, `PRAGMA busy_timeout=5000`
3. [ ] Create the `messages` table and claim index on open (idempotent)
4. [ ] Implement `Enqueue`
5. [ ] Implement `Claim` as a single `UPDATE … RETURNING`
6. [ ] Implement `Ack` (delete, matched on id **and** receipt)
7. [ ] Implement `Nack` with dead-lettering at `QUEUE_MAX_DELIVERIES`
8. [ ] Wire the API to enqueue the validate message on `POST /jobs/{id}/complete`
9. [ ] Write the concurrency test (two goroutines, one message)
10. [ ] Write the visibility-expiry test
11. [ ] Write `backend/internal/queue/CONTEXT.md`

### The claim statement

This is the whole stage, really:

```sql
UPDATE messages
   SET receipt    = ?,                  -- fresh UUID
       visible_at = ?,                  -- now + visibility timeout
       deliveries = deliveries + 1
 WHERE id = (
   SELECT id FROM messages
    WHERE queue = ? AND visible_at <= ?  -- now
    ORDER BY id
    LIMIT 1
 )
RETURNING id, body, receipt, deliveries;
```

The inner `SELECT` runs inside the same statement as the `UPDATE`, so SQLite
holds the write lock across both. A `SELECT` followed by a separate `UPDATE`
does not: two workers read the same row, both update it, both process the same
job. That is the bug this statement exists to prevent.

## Test

```bash
cd backend && go test ./internal/queue/...
```

Two tests carry the stage:

```go
// One message, two concurrent claimers. Exactly one gets it.
func TestClaimIsExclusive(t *testing.T) {
    q.Enqueue(ctx, "dayreel-validate", []byte(`{}`))
    // 8 goroutines call Claim; count non-nil results == 1
}

// A claim that is never acked becomes claimable again once visible_at passes.
func TestVisibilityTimeoutExpires(t *testing.T) {
    q := newQueue(t, WithVisibilityTimeout(50*time.Millisecond))
    q.Enqueue(ctx, "dayreel-validate", []byte(`{}`))
    m1, _ := q.Claim(ctx, "dayreel-validate")   // deliveries == 1
    m2, _ := q.Claim(ctx, "dayreel-validate")   // nil — still hidden
    time.Sleep(60 * time.Millisecond)
    m3, _ := q.Claim(ctx, "dayreel-validate")   // same id, deliveries == 2
}
```

End-to-end, by hand:

```bash
make queue-reset
make api        # enqueues a validate message on POST /jobs/{id}/complete
make queue-peek # row in dayreel-validate, deliveries=0
make worker     # in another terminal
make queue-peek # row gone, or in dayreel-dlq after 3 failures
```

## Verification

- [ ] `go test ./internal/queue/...` passes, including the concurrency test
- [ ] `TestClaimIsExclusive` fails if `Claim` is rewritten as SELECT-then-UPDATE
- [ ] `data/queue.db` is created on first run, and `make queue-reset` removes it
- [ ] `make queue-peek` shows a message appear, `deliveries` increment on claim,
      and the row disappear on ack
- [ ] A worker that panics mid-stage leaves a row that becomes claimable again
      after `QUEUE_VISIBILITY_TIMEOUT`, with `deliveries` one higher
- [ ] A message failed 3 times ends up with `queue = 'dayreel-dlq'`, same `id`
- [ ] `go build ./...` succeeds with `CGO_ENABLED=0`

## Notes

- **A visibility timeout is a timestamp, not a process.** `visible_at` is a
  column; a message becomes claimable again simply because `WHERE visible_at <=
  now` starts matching it. Nothing sweeps expired leases, there is no reaper
  goroutine, and a crashed worker needs no cleanup — its messages come back on
  their own. This is why the whole queue is ~150 lines.

- **`Claim` must be one statement.** A single `UPDATE … RETURNING` is atomic
  under SQLite's write lock. `SELECT` then `UPDATE` is two statements with a gap
  between them, and two workers polling the same queue will both claim the same
  row. The exclusivity test exists specifically to catch a future refactor that
  splits them.

- **`Ack` matches on `receipt`, not just `id`.** A worker whose lease expired
  mid-stage will eventually finish and call `Ack`. By then the message may have
  been re-claimed by someone else with a new receipt, and the slow worker's
  delete must be a no-op — otherwise it deletes a message another worker is
  actively processing. Matching on receipt makes the stale ack silently do
  nothing, which is the correct at-least-once behaviour.

- **Dead-lettering is one column change.** `UPDATE messages SET queue =
  'dayreel-dlq' WHERE id = ?`. Not a copy-and-delete: copying is two statements
  that can half-fail, and it loses the original `id`, which is the only thing
  tying a dead letter back to its history. The DLQ is the same table.

- **Driver is `modernc.org/sqlite`, not `mattn/go-sqlite3`.** `backend/Dockerfile`
  builds with `CGO_ENABLED=0`; `mattn` is a cgo binding and will not link.
  `modernc.org/sqlite` is a pure-Go transpilation of SQLite, so it builds under
  the existing Dockerfile with no toolchain changes. It is slower, which does
  not matter for a queue moving a handful of messages per clip.

- **WAL mode and `busy_timeout`.** Two processes (API writing, worker claiming)
  share the file. WAL lets the reader proceed during a write; `busy_timeout`
  turns the remaining lock contention into a short wait instead of an immediate
  `SQLITE_BUSY` error.

- **Deferred: long-poll.** SQLite has no notification mechanism, so idle workers
  poll every `QUEUE_POLL_INTERVAL`. At 250ms that is a negligible amount of CPU
  for one process and adds at most a quarter second of latency per stage. A
  condition variable shared between API and worker is not possible across
  processes, and is not worth a second IPC mechanism.

- **Deferred: multi-host.** The queue is a file on one disk. This is fine for one
  VM and fine for local development, and it is the exact point at which a real
  broker becomes necessary if a second worker host ever appears. The `Queue`
  interface is what a hosted broker would implement, so the swap is contained.
