# Queue Package

This package is DayReel's message broker. It has **two drivers behind one interface**:

- **SQLite** (`QUEUE_DRIVER=sqlite`, the default) — a self-hosted, SQS-equivalent queue in a single file. No server, no account, no per-request cost.
- **SQS** (`QUEUE_DRIVER=sqs`) — real Amazon SQS.

SQLite is the default and SQS is the supported backup. Everything above this package — the API handler, `worker.Runner`, all four stages — takes a `queue.Queue` and cannot tell which one answered.

## Overview

The queue package defines:

- **Queue**: The broker interface — `Send`, `Receive`, `Ack`, `Nack`, `Heartbeat`, `DeadLetter`, `Stats`, `Close`
- **Message**: The delivery envelope (broker-owned), wrapping an `events.StageMessage` payload (producer-owned)
- **SQLiteQueue**: One SQLite file, no server, no cgo
- **SQSQueue**: Amazon SQS
- **FromConfig**: The driver selector — the only place that knows two drivers exist
- **LoggingQueue**: A decorator that logs every operation with `log/slog`
- **QueueStat**: Per-queue depth for backlog monitoring

## Files

| File | Purpose |
|------|---------|
| `queue.go` | Interface, `Message`, `QueueStat`, `ErrLeaseLost` |
| `sqlite.go` | SQLite driver: schema, atomic claim, lease operations |
| `sqs.go` | SQS driver: queue-URL cache, clamping, error translation |
| `factory.go` | `FromConfig` — reads `QUEUE_DRIVER`, wraps in `WithLogging` |
| `logging.go` | `WithLogging` decorator (structured logs, no SQL) |
| `sqlite_test.go` | Driver tests, including the concurrent-claim stress test |
| `sqs_test.go` | Clamping, error mapping, envelope and body round-trip — no network |
| `logging_test.go` | Decorator tests (attributes, log levels) |

## Choosing a Driver

```
QUEUE_DRIVER=sqlite   # default
QUEUE_DRIVER=sqs
```

Anything else is rejected at startup by `FromConfig`, with a message naming both valid values. That is deliberate: a typo silently falling back to the default would give you a deployment that believes it is on SQS while writing messages into a local file no other host can see.

```go
q, err := queue.FromConfig(ctx, cfg, logger)   // both binaries do exactly this
defer q.Close()
```

Pick **SQS** when workers must run on more than one machine, when you want the queue to survive the box, or when you want AWS to hold messages while every worker is down for a deploy. Pick **SQLite** — which is to say, change nothing — for everything else.

## Where the Two Drivers Differ

Same interface, same message bodies, same `ErrLeaseLost`. These are the things that genuinely change when you flip `QUEUE_DRIVER`:

| | SQLite | SQS |
|---|---|---|
| **Cost** | None. It is a file. | Billed per request. An idle worker long-polling costs one request per 20s per stage; `Stats` costs one per queue per call. |
| **Reach** | Workers must share a filesystem with the API. | Any host with credentials. |
| **`Receive` batch size** | Unbounded (`max` is honoured as given). | Capped at **10**. Larger values are clamped. |
| **`Receive` wait** | A local ticker at `QUEUE_POLL_INTERVAL`; any `wait` is honoured. | Server-side long poll, granularity one second, capped at **20s**. A positive sub-second wait rounds **up to 1s** rather than down to 0 — see below. |
| **`Send` delay** | Unbounded. | Capped at **900s** (15 min). Longer delays are shortened, not rejected. |
| **Delivery** | At-least-once. | At-least-once, and more so — SQS may deliver a message twice even without a lease expiry, because its distributed storage is what it is. |
| **`Message.ID`** | The SQLite rowid. | Zero — SQS identifies messages by UUID. Use `Message.Ident()`; `Message.MessageID` holds the text form on both. |
| **`QueueStat.Oldest`** | Exact, from `created_at`. | **Always zero.** SQS publishes the equivalent as the CloudWatch metric `ApproximateAgeOfOldestMessage`, never as a queue attribute. |
| **Dead-letter reason** | The `dlq_reason` column. | The `DayReelDeadLetterReason` message attribute (source queue in `DayReelSourceQueue`). |
| **Dead-lettering** | One `UPDATE` of the `queue` column. Atomic; the message keeps its id. | `SendMessage` to the DLQ then `DeleteMessage` from the source. Cannot be atomic — see below. |
| **Redrive policy** | Does not exist. | Exists on the real queues, and is **a safety net only** — see below. |
| **Visibility timeout** | `QUEUE_VISIBILITY_TIMEOUT` in this process. | The queue's own attribute, set by `scripts/aws-sqs-setup.sh`. AWS enforces that one; the config value is only a fallback. |
| **Lease deadline** | Stored (`visible_at`) and exact. | Computed client-side — see "Timestamps on SQS". |

`QUEUE_DB_PATH` is **SQLite-only** — there is no file on SQS.

`QUEUE_POLL_INTERVAL` is read on both paths but means different things, which is why its *default* depends on the driver (`config.defaultPollInterval`):

| Driver | Default | What it is |
|---|---|---|
| `sqlite` | `250ms` | A local ticker's cadence. Four queries a second against a file, costing nothing. |
| `sqs` | `20s` | `WaitTimeSeconds` — SQS's maximum long poll, and the only default that keeps four idle workers inside the free tier. |

The hazard this defends against: `WaitTimeSeconds` has one-second granularity, so 250ms would express as **0**, which is short polling — an immediate return and a billed request. The runner's consume loop has no delay of its own between receives, so an idle worker would spin as fast as the network allows and bill for every turn. Two things prevent it: the driver-shaped default above, and `clampWaitSeconds` rounding any *positive* sub-second wait up to 1s. An explicit `wait` of 0 still gets a genuine short poll, which is what a caller asking for 0 means.

### Explicit dead-lettering vs the redrive policy

Both drivers dead-letter the same way: the **worker** decides, not the broker. `worker.Runner` calls `DeadLetter` directly, and it calls it for permanent failures — a codec the pipeline rejects, a corrupt file — on the *first* delivery, with the entire budget unspent.

The redrive policy `scripts/aws-sqs-setup.sh` writes onto each SQS queue does **not** implement that. Redrive only fires after `maxReceiveCount` deliveries, so waiting for it would redeliver a video that can never succeed two more times, several visibility timeouts apart, before parking it. The policy is there for the messages that never reach `DeadLetter` at all: a worker killed mid-stage, a body that will not decode, a bug in the loop itself. Treating it as the mechanism is the mistake to avoid — change one and you have not changed the other.

`DeadLetter` on SQS is a send followed by a delete, and the two can half-succeed. The send comes first on purpose: a failed delete leaves a duplicate on a queue nothing drains automatically, whereas the other order would delete a message that was never copied and lose the only record of why the pipeline gave up.

### Timestamps on SQS

SQS reports one of the three envelope timestamps and infers the rest:

- **`EnqueuedAt`** is the `SentTimestamp` system attribute (epoch millis), requested by name on every receive. It is the producer's send time as SQS recorded it — the same thing `created_at` is on SQLite.
- **`ClaimedAt`** is stamped by the client at receive. SQS says nothing about when it handed the message out, so this is off by the receive call's latency plus clock skew: tens of milliseconds against metrics measured in seconds.
- **`LeaseExpiresAt`** is `ClaimedAt` plus **the queue's own `VisibilityTimeout` attribute**, read via `GetQueueAttributes` on first use and cached alongside the queue URL. SQS has no "when does this lease expire" field, so this is the only way to compute it — and it is a *lower bound* on the truth, since the real clock started at SQS before this process saw the message. `LeaseRemaining()` therefore reads slightly optimistic, which is why the runner heartbeats on a fixed 30s ticker against a 5m lease instead of waiting for `LeaseRemaining()` to approach zero.

Leaving these zero was the alternative and it is worse: `QueueWait()` would report decades and `LeaseRemaining()` a large negative on every message, both of which look like data rather than absence. If `ApproximateReceiveCount` or `SentTimestamp` is missing, `ReceiveCount` floors at 1 and `EnqueuedAt` falls back to `ClaimedAt` — a queue wait of zero, not one measured from 1970.

### `ErrLeaseLost`

The runner branches on this sentinel and must behave identically on both drivers.

| Driver | Condition |
|--------|-----------|
| SQLite | A mutating statement guarded by `WHERE id = ? AND receipt = ?` affected zero rows. |
| SQS | The API returned `ReceiptHandleIsInvalid`, `MessageNotInflight`, or a bare `InvalidParameterValue`. |

`InvalidParameterValue` is included because SQS reports an expired receipt handle that way in several cases. It is only safe to read it that way because **every numeric parameter this driver sends is clamped into SQS's legal range first** — with those ruled out, the receipt handle is the one parameter left that SQS can call invalid. Adding an unclamped parameter to `Delete`/`ChangeMessageVisibility` would quietly turn a real bug into a silent "lease lost".

## SQS Mapping

The SQLite driver was written against SQS's vocabulary, which is what made restoring SQS a driver rather than a rewrite:

| SQS concept | SQLite driver |
|-------------|---------------|
| MessageId | `Message.ID` (rowid) |
| ReceiptHandle | `Message.Receipt` (128-bit random hex, new on every delivery) |
| ApproximateReceiveCount | `Message.ReceiveCount` |
| SentTimestamp | `Message.EnqueuedAt` |
| VisibilityTimeout | `Options.VisibilityTimeout` (default 5m) |
| ChangeMessageVisibility | `Heartbeat` |
| DeleteMessage | `Ack` |
| DelaySeconds | the `delay` argument to `Send` |
| Long polling | the `wait` argument to `Receive` |
| Redrive policy / maxReceiveCount | `Options.MaxDeliveries` + caller-driven `DeadLetter` |

## The Atomic Claim (SQLite)

Everything else in the SQLite driver is bookkeeping around one statement:

```sql
UPDATE messages
   SET receipt = lower(hex(randomblob(16))), visible_at = ?, receive_count = receive_count + 1, claimed_at = ?
 WHERE id IN (SELECT id FROM messages WHERE queue = ? AND visible_at <= ? ORDER BY id LIMIT ?)
RETURNING id, queue, body, receipt, receive_count, created_at, claimed_at, visible_at
```

Selecting and then updating in two statements leaves a window where two workers claim the same row and transcode the same video twice. Folding it into one `UPDATE` makes SQLite hold the write lock across the selection, so a row can only be leased once — across goroutines *and* across processes. `randomblob()` is non-deterministic, so a batch claim gets one distinct receipt per row.

Every mutating statement is guarded by `WHERE id = ? AND receipt = ?`. Zero rows affected therefore has exactly one meaning — the lease expired and someone else owns the message — which surfaces as `ErrLeaseLost` rather than a silent success.

SQS provides the equivalent guarantee itself: a message is invisible to every other consumer for the duration of its visibility timeout, and a stale receipt handle is rejected.

## Schema (SQLite)

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

`modernc.org/sqlite` (pure Go), which registers itself as **`sqlite`**, not `sqlite3`. The cgo-backed `mattn/go-sqlite3` cannot be used: the API binary is built with `CGO_ENABLED=0`.

Pragmas ride on the DSN (`?_pragma=busy_timeout(5000)&...`) because `busy_timeout` and `synchronous` are per-connection settings — a one-off `PRAGMA` through `database/sql` would only stick to whichever pooled connection ran it. `journal_mode=WAL` is persisted in the database header.

## Provisioning (SQS)

The SQLite driver creates its own table on first open. SQS queues have to exist before anything starts:

```
make sqs-setup      # create the DLQ, then the four stage queues with a redrive policy
make sqs-status     # URL, depth and in-flight per queue
make sqs-teardown   # delete them, after typing the region back
```

`scripts/aws-sqs-setup.sh` writes `VisibilityTimeout` and `maxReceiveCount` from `QUEUE_VISIBILITY_TIMEOUT` and `QUEUE_MAX_DELIVERIES`, so the queues and the workers agree. They must be run with the same environment the workers get; a queue enforcing a 5 minute lease against workers that believe they have 10 is the failure this avoids.

Two things bite here. SQS refuses to recreate a queue name for **60 seconds** after deletion (`QueueDeletedRecently`), so a teardown/create cycle needs a pause. And the script refuses to run with `AWS_ENDPOINT_URL` set: there is no emulator any more, and a stale value creates queues somewhere the workers will never look while `status` reports them as present.

## Usage

```go
q, err := queue.FromConfig(ctx, cfg, logger)
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

The real consumer is `internal/worker/runner.go`, which is the only implementation of this loop — the four stages share it and none of them talks to the queue directly. The only producers are `api.CompleteUpload` (the validate message, which is what starts a job) and the runner itself (each stage publishing the next). None of them branches on the driver.

Long-running stages should `Heartbeat` before `LeaseRemaining()` runs out. A heartbeat deliberately does not bump `ReceiveCount` on either driver — it is the same delivery continuing, and counting it would burn the redelivery budget of exactly the slow-but-healthy jobs it exists to protect. On SQS this is `ChangeMessageVisibility`, which only moves `ApproximateReceiveCount` when SQS actually hands the message out again.

## Delivery Semantics

At-least-once on both drivers. A worker can crash after finishing its work and before acking, and the message will be redelivered — **every stage must be idempotent**. `ReceiveCount` is the retry signal, and reaching `MaxDeliveries` is the cue to dead-letter.

Dead-lettering is a worker decision, not a broker one: only the worker knows whether a failure is retryable. The broker just supplies the budget so every stage agrees on it. In practice the runner makes three different calls out of it:

| Failure | Call |
|---------|------|
| Permanent — a codec the pipeline rejects, a corrupt file | `DeadLetter` immediately, whatever the count. The remaining deliveries would fail identically and arrive carrying nothing new. |
| Transient with budget left | `Nack` with a doubling backoff, 10s to a 5m ceiling. |
| Transient on the last delivery of the budget | `DeadLetter`, with the failure that exhausted it as the reason. |

`ErrLeaseLost` from `Ack` is not a failure of the stage. It means this worker ran past its lease, another worker claimed the message, and that worker's delivery is the one that counts — the work here is a duplicate. The runner logs it and drops it; retrying would produce a second duplicate.

## Configuration

| Env | Config field | Default | Applies to |
|-----|--------------|---------|------------|
| `QUEUE_DRIVER` | `QueueDriver` | `sqlite` | selection |
| `QUEUE_DB_PATH` | `QueueDBPath` | `./data/queue.db` | SQLite only |
| `QUEUE_POLL_INTERVAL` | `QueuePollInterval` | `250ms` on SQLite, `20s` on SQS | both, with a driver-shaped default |
| `QUEUE_VISIBILITY_TIMEOUT` | `QueueVisibilityTimeout` | `5m` | both |
| `QUEUE_MAX_DELIVERIES` | `QueueMaxDeliveries` | `3` | both |
| `AWS_REGION` | `AWSRegion` | `us-east-1` | SQS only |

Queue names come from `internal/events` constants (`dayreel-validate`, `dayreel-extract`, `dayreel-transcribe`, `dayreel-package`, `dayreel-dlq`), unchanged since the SQS era and used verbatim as SQS queue names.

## Testing

`sqs_test.go` never opens a socket and never needs credentials. It covers what the driver decides on its own: the clamping (delay > 900s, batch > 10, wait > 20s, visibility > 12h), the error translation to `ErrLeaseLost`, the envelope built from a receive response, and the body round-trip through `events.StageMessage`.

It does not cover, and says so in the file: that a visibility timeout actually hides a message, that long polling blocks server-side, that `ApproximateReceiveCount` increments, or that the redrive policy fires. Those are SQS's behaviour rather than this package's, and asserting them against a fake would only assert that the fake was written to match the assertion.

## Known Trade-offs

- **SQLite: single file, single host.** Workers must share a filesystem with the API. That is the deal SQLite offers; it is fine for a single-box pipeline and is the reason `Receive` is safe across processes and not just across goroutines. It is also the reason the SQS driver exists.
- **SQLite: polling, not push.** SQLite has no server-side blocking read, so long polling is a ticker (`PollInterval`). An idle worker costs a few queries a second, not a pegged core. Every claim takes SQLite's write lock even when it matches nothing, which is why the runner backs off exponentially on a failing `Receive` rather than retrying immediately. On SQS the same backoff protects a bill instead of a lock.
- **One visibility timeout for every queue.** SQS lets each queue carry its own; here `QUEUE_VISIBILITY_TIMEOUT` is applied to all of them by both the driver and the setup script. Transcribe, the one stage that can genuinely outrun five minutes, relies on the runner's heartbeat instead of a longer lease.
- **`Stats` counts are approximate on both.** On SQLite a message whose lease expired but which nobody re-claimed still carries a receipt, so it reads as in-flight even though it is claimable. SQS documents its own counts as approximate for similar reasons. On SQS, `Stats` is also one billed request per queue and omits `Oldest` entirely.
- **`Close` is a no-op on SQS.** There is nothing to release — no connection this driver opened, no buffered message to flush. It exists because the interface demands it, and because callers must not have to ask which driver they hold before shutting down.
