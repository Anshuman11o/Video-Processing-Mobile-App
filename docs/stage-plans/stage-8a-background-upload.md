# Stage 8A: Background Upload

> Status: **approved — implementation started 2026-08-13.** All seven decisions
> settled; the answers are recorded below and inline in each section.
>
> | | Settled as |
> |---|---|
> | **[DECIDE 1]** scope | **True background transfer.** The Aim wins over the Verification: the upload continues to S3 while the app is dead, subject to network. That means the Kotlin WorkManager module, not resume-on-relaunch. **This went against the plan's recommendation** — it is the larger of the two features, and the client becomes Kotlin/OkHttp rather than JS. |
> | **[DECIDE 2]** resume state | **Server-authoritative via `ListParts`.** The client persists identifiers only, never an ETag. Stage 7's uploader docstring promises the opposite and must be corrected. |
> | **[DECIDE 3]** endpoints | **Both** — the re-presign endpoint *and* a tolerant `CompleteUpload` that can derive its parts from `ListParts`. |
> | **[DECIDE 4]** abandoned uploads | **All three** — lifecycle rule, explicit `DELETE`, and a manual reaper script. |
> | **[DECIDE 5]** source file | `DocumentDir` + orphan sweep, existence check as backstop. |
> | **[DECIDE 6]** interrupt window | Debug-only inter-part delay, so there is something to kill. |
> | **[DECIDE 7]** toolchain | **Install everything, NDK included.** Disk was freed the same day: **24 GiB now, not 2.8** — the "stop below ~12 GB" fallback is moot. Node 22 via nvm alongside it (RN 0.87 needs ≥22.13; the machine had v20.11). |
>
> **Real AWS is deferred to a single run at the end**, not used for development.
> Provisioning is real work, not a flag: compose hardcodes `USE_LOCALSTACK=true`
> on every service with no `env_file`, and no real bucket, queue or table has
> ever existed. That one run is also the only chance to test anonymous-access
> enforcement and Block Public Access, which LocalStack provably cannot.
>
> **Read the finding below.** It is the reason ~80% of this stage is
> backend work that has nothing to do with Android — and that half was started
> first precisely because it needs no SDK, no emulator and no disk.
>
> **Correction, 2026-08-13, same day.** This plan was drafted against a repo
> state that changed while it was being written. Two things that it originally
> recorded as missing have since landed from the main session's Stage 7 work, and
> the affected passages are annotated inline rather than silently rewritten:
> **`UPLOAD_PART_SIZE` now exists** (`config.go`, `handlers.go`), so this
> stage's Phase 1 is deleted; and **Stage 7's mobile uploader now exists**
> (`mobile/src/upload/`, `mobile/src/storage/`, `mobile/src/hooks/`), which made
> **[DECIDE 2]** a sharper question than it was — Stage 7 had already begun
> building toward the option this plan recommends against, and **[DECIDE 2]**
> has now been settled against it.
>
> **Second correction, later the same day.** The compose value quoted above
> (`UPLOAD_PART_SIZE=262144`) is **gone, and the reasoning behind it was wrong.**
> A small part size does not work anywhere: LocalStack enforces S3's 5 MiB
> minimum exactly as S3 does, and an upload with sub-5 MiB parts uploads every
> part with a 200 and then fails the whole job at `CompleteMultipartUpload` with
> `EntityTooSmall`. Values below the minimum are now clamped in config. **This
> matters to [DECIDE 6]:** exercising the multipart path — and therefore having
> a multi-part upload to interrupt at all — requires a test clip over 5 MiB, not
> a smaller part size.

## Aim

Make an interrupted upload finishable — after a network drop, after a
backgrounding, and after the app process is killed and relaunched — without
re-uploading the bytes that already landed.

## The finding that reframes the stage

**The API has no way to re-issue presigned URLs for an existing multipart
upload.** The route table is exactly four entries (`backend/internal/api/router.go:19-22`):

```
POST /jobs                    GET  /jobs/{id}
POST /jobs/{id}/complete      GET  /jobs/{id}/reel
```

`POST /jobs` mints a **new** `models.Job` (`handlers.go:102`) and a **new** S3
multipart upload (`handlers.go:106`). There is no path that takes an existing
`job_id` and hands back usable URLs. And presigned URLs expire:
`presignExpiry = 1 * time.Hour` (`handlers.go:22`), surfaced to the client as
`expires_in: 3600` (`handlers.go:155`).

Put those together:

> **Any** resume — JavaScript or Kotlin, Android or iOS, WorkManager or a
> foreground loop — is dead one hour after the app closes. Inside that hour it
> only survives because the client happens to still hold URLs it was given once.
> Nothing in the system can re-mint them.

A native WorkManager module does not fix this. A persisted ETag array does not
fix this. **A new endpoint does**, and it is platform-independent, buildable and
testable today against LocalStack — with no Android SDK, no emulator, and no
free disk. See **[DECIDE 3]**.

That is why this plan is structured as **a backend stage with a client stage
bolted on**, rather than as the native-module stage `PROJECT_PLAN.md` describes.
Phase 1 is unblocked right now. Phase 2 is blocked on a toolchain that does not
exist (**[DECIDE 7]**).

## Components

| Component | Action |
|---|---|
| `backend/internal/api/handlers.go` | Modify — new re-presign handler; tolerant `CompleteUpload` (**[DECIDE 3]**) |
| `backend/internal/api/router.go` | Modify — one or two new routes |
| `backend/internal/storage/s3.go` | Modify — `ListParts`, `ListMultipartUploads` wrappers (**[DECIDE 2]**) |
| `backend/internal/config/config.go` | **No change needed** — `UPLOAD_PART_SIZE` landed 2026-08-13 (`config.go:49,72,79`) |
| `infra/localstack/init-aws.sh` | Modify — `AbortIncompleteMultipartUpload` lifecycle rule (**[DECIDE 4]**) |
| `infra/docker-compose.yml` | **No change needed** — `UPLOAD_PART_SIZE=262144` already on `api` (`:82`) |
| `mobile/src/upload/uploader.ts` | Modify — the `plans.length !== urls.length` guard rejects a partial URL set; resume needs a mode that does not |
| `mobile/src/storage/` | Modify — persist upload state alongside the existing `jobIndex.ts` |
| `mobile/src/screens/HomeScreen.tsx` | Modify — source-file persistence (**[DECIDE 5]**) |
| `mobile/android/app/src/main/java/com/dayreel/` | Create — **only if [DECIDE 1] picks (b)**: Kotlin WorkManager TurboModule |
| `docs/SETUP.md` | Modify — the abandoned-upload reaping rule, and `UPLOAD_PART_SIZE` next to `S3_PUBLIC_ENDPOINT` |

## Boundaries

### The new endpoint (shape depends on [DECIDE 3])

**Request:**

```
POST /jobs/{id}/upload-urls
Content-Type: application/json

{}                    # no body fields required; the server reconciles
```

**Response (200):**

```json
{
  "job_id": "550e8400-...",
  "upload_id": "s3-multipart-id",
  "part_size": 262144,
  "total_parts": 12,
  "uploaded_parts": [
    {"part_number": 1, "etag": "\"aaa\"", "size": 262144},
    {"part_number": 2, "etag": "\"bbb\"", "size": 262144}
  ],
  "upload_urls": [
    {"part_number": 3, "url": "http://10.0.2.2:4566/..."},
    {"part_number": 4, "url": "http://10.0.2.2:4566/..."}
  ],
  "expires_in": 3600
}
```

Note what this shape asserts: **`upload_urls` contains only the parts still
missing.** The server, not the client, decides what is missing. That is
**[DECIDE 2]**.

**Error responses that must be distinguishable, because the client's recovery
differs for each:**

| Condition | Status | Code | Client action |
|---|---|---|---|
| Job unknown | 404 | `NOT_FOUND` | Drop from the local index |
| Job has no `upload` (never had one, or already completed) | 409 | `NO_UPLOAD` | Nothing to resume |
| S3 `NoSuchUpload` — aborted by lifecycle or by `DELETE` | 410 | `UPLOAD_GONE` | Start over with a fresh `POST /jobs` |
| Upload already complete server-side | 409 | `ALREADY_COMPLETE` | Skip to polling |

`UPLOAD_GONE` is the one that actually happens: **[DECIDE 4]**'s lifecycle rule
will reap uploads out from under a client that was offline for a day. If it is
not distinguishable from a generic 500, the app retries forever against an
upload that no longer exists.

### S3 `ListParts` — what it actually tells us

`ListParts` returns, for a live multipart upload, every part S3 has accepted:
`PartNumber`, `ETag`, `Size`, `LastModified`. It is authoritative in a way a
client-persisted array cannot be, because a part can land and the client can die
before recording its ETag — the byte transfer and the local write are not atomic.

Paging: `ListParts` returns up to 1000 parts per call with
`IsTruncated`/`PartNumberMarker`. At `UPLOAD_PART_SIZE=256 KiB` a 3 MB fixture is
~12 parts, so paging is not exercised locally and the loop will be **written but
never executed here**. Say so rather than claiming it works.

`ListParts` is available in the pinned SDK — verified:
`~/go/pkg/mod/github.com/aws/aws-sdk-go-v2/service/s3@v1.107.1/api_op_ListParts.go`
exists. Nothing in `backend/` calls it today (verified by grep: zero hits for
`ListParts` or `ListMultipartUploads` across `backend/`).

### S3 objects

No new keys. The multipart upload writes to the same
`dayreel-raw-videos/{job_id}/{filename}` (`handlers.go:103`) it always did.

**What is new is a class of S3 state this project has never had to manage:
in-flight multipart uploads.** They are not objects. `aws s3 ls` does not show
them and neither does the S3 console's object list. They are only visible via
`ListMultipartUploads`, and they bill as storage from the moment each part lands
until the upload is completed or aborted. See **[DECIDE 4]**.

### DynamoDB

**No schema change needed.** `models.UploadInfo` (`models/job.go:57-64`) already
carries `UploadID`, `Bucket`, `Key`, `PartSize`, `TotalParts`, `CompletedAt` —
everything the re-presign handler needs to reconstruct the request. And
`db.UpdateUploadInfo` (`db/dynamodb.go:125`) already exists if anything does need
writing back.

Whether the server should *also* persist per-part progress is **[DECIDE 2]**; the
recommendation there is that it should not, because `ListParts` already holds it
and a second copy can only disagree.

---

### [DECIDE 1] — "survives app kill" means two different things, and they cost differently

**This is the stage's defining fork. Everything else is sized by the answer.**

`PROJECT_PLAN.md:408-418`, verbatim:

> **Stage 8A: Background Upload**
> **Aim:** Upload survives app kill.
>
> **Deliverables:**
> - Kotlin WorkManager native module
> - Chunked upload with ETag persistence
> - Resume from last successful part
>
> **Verification:**
> - Start upload, kill app, reopen
> - Upload continues from where it left off

The Aim implies the OS keeps transferring **while the app is gone**. That
requires a native background worker.

The Verification describes the user reopening the app and the upload picking up
from where it stopped. That is **resume-on-relaunch**, and it needs no native
code at all.

These are not two descriptions of one feature. They are two features, and the
document specifies both without noticing.

#### Options

**(a) Resume-on-relaunch, pure JavaScript.** On app start, read persisted upload
state, call `POST /jobs/{id}/upload-urls`, and continue in the foreground.

- Satisfies `PROJECT_PLAN.md`'s **Verification** step literally and completely.
- No native module, no Kotlin, no TurboModule codegen, no New Architecture
  compatibility question.
- Runs on the existing Stage 7 uploader with a persistence layer and a resume
  entry point bolted on.
- **Does not** transfer while the app is backgrounded or killed. An upload
  paused at 40% stays at 40% until the user comes back.

**(b) Kotlin WorkManager native module.** A `CoroutineWorker` (very probably a
foreground service, see below) that holds the upload across process death and
Doze.

- Satisfies the **Aim**. It is the only option that does.
- Costs: a TurboModule under `newArchEnabled=true` (codegen, a spec file, a
  `ReactPackage`), an `androidx.work` dependency, a foreground-service
  notification, `FOREGROUND_SERVICE` + `FOREGROUND_SERVICE_DATA_SYNC`
  permissions (required from API 34; `targetSdk` here is 36), and the whole
  Android background-execution rulebook — Doze, App Standby buckets, and
  Android 12+'s restriction on starting foreground services from the background.
- **It buys availability, not correctness.** Once `ListParts` reconciliation
  exists server-side (**[DECIDE 2]**), a resumed upload is correct whether it
  resumes after four seconds or four days. WorkManager makes the wait shorter.
  It does not make any outcome possible that (a) makes impossible.
- Entirely blocked on **[DECIDE 7]**. Not one line of it can be compiled today.

**(c) (a) now, (b) as a separately-scoped stretch.** Ship resume-on-relaunch as
the stage deliverable; keep the WorkManager module as an explicitly deferred
follow-up with its own verification.

**(d) WorkManager without a foreground service** — plain `OneTimeWorkRequest`,
possibly expedited. Cheaper than (b): no notification, no foreground-service
permissions.

- Recorded so it is not proposed as a shortcut. Expedited quota is limited and
  per-app; ordinary work is deferrable and Doze-suspended. A 3 MB upload may well
  complete, and a 200 MB one on a metered connection will not. It is (b)'s cost
  minus (b)'s guarantee.

#### Recommendation: **(c) — (a) as the committed deliverable, (b) explicitly deferred.**

Three reasons, in order of weight:

1. **The stage's own stated Verification is satisfied by (a).** "Start upload,
   kill app, reopen → upload continues from where it left off" is exactly what
   (a) does.
2. **The genuinely hard problem is the missing endpoint, and it is (a)'s problem
   too.** Building (b) first would mean building a native background worker on
   top of an upload that still cannot re-presign — a fast car with no road.
3. **(b) is blocked and (a) is not.** (a)'s backend half can be built and proven
   today. (b) cannot be compiled until a ~15 GB SDK fits on a disk with 2.8 GB
   free.

The honest counter-argument, recorded rather than buried: **the Aim as written
says WorkManager, and (c) does not deliver the Aim as written.** If the intent is
"the phone uploads while I put it in my pocket," (c) is a scope cut and should be
approved as one, not slipped through. That is why this is a decision and not a
recommendation in the Notes.

---

### [DECIDE 2] — where resume state lives: the client's ETag array, or S3's `ListParts`

`PROJECT_PLAN.md` says "Chunked upload with **ETag persistence**" — i.e. the
client writes down each part's ETag and replays them at completion.

**Stage 7 has already started building that.** `mobile/src/upload/uploader.ts`
(landed 2026-08-13) says so in its own docstring:

> *"sequential order means the ETags collected so far are always a contiguous
> prefix — which is the shape Stage 8A needs in order to resume. […] Callers
> should persist it as they go rather than keeping it in a closure."*

So this decision is now a live disagreement with code that exists, not an
abstract fork. Choosing (b) below means that docstring's premise is wrong and
should be corrected; choosing (a) means accepting the failure modes listed next.
Either way the comment should not be left standing as an unexamined assumption —
it is precisely the kind of "reads as decided" text this project has been bitten
by before.

Two structural notes about that file, both verified, both load-bearing for
whichever option wins:

- It already resolves URLs **by part number** (`byNumber` map), not by array
  index. Good — a partial URL set lines up correctly.
- But it guards with `if (plans.length !== urls.length) throw`, and `plans` is
  always the **full** part list from `planParts(fileSize, partSize)`. **That guard
  rejects every resume**, since a resume supplies only the missing URLs. Resume
  needs a mode that filters `plans` to the missing part numbers and asserts
  `urls ⊆ plans` instead. Keep the strict guard for the fresh-upload path — it is
  a good guard, it just encodes "this is the whole file."
- Progress (`uploadProgress(fileSize, completedBytes, sent)`) starts
  `completedBytes` at 0. A resume must seed it with the bytes already on S3 or
  the bar restarts from zero on every relaunch.

That works right up until it doesn't, and its failure mode is silent:

- A part PUT succeeds, the process is killed before the ETag is written to disk.
  The client believes part 7 is missing; S3 has it. The client re-uploads it
  (harmless, it overwrites) — but with a *different* ETag than the one it may
  have partially recorded, and the completion body can then disagree with S3.
- The persisted file is written non-atomically and truncated by the kill. Now the
  array is corrupt and nothing detects it until `CompleteMultipartUpload` fails
  with `InvalidPart`.
- The client persisted state for an upload the server later aborted
  (**[DECIDE 4]**). The ETags are real; the upload is not.

#### Options

**(a) Client-authoritative.** Client persists `{part_number, etag}`, resumes the
gaps it believes exist, sends the full array to `/complete`. What `PROJECT_PLAN`
describes.
- No backend change beyond re-presigning.
- Trusts a file written by a process that is being killed on purpose as the core
  test methodology of this stage.

**(b) Server-authoritative via `ListParts`.** The re-presign endpoint calls
`ListParts`, computes `missing = {1..total_parts} \ listed`, and returns URLs for
exactly those. The client persists only `{job_id, upload_id, key, part_size,
total_parts, local_file_path}` — identifiers, not progress.
- The source of truth is the system that actually holds the bytes.
- Progress state cannot drift, because there is only one copy.
- Costs one `ListParts` call per resume — a single API request, effectively free
  (`config/free-tier.md` Tier 2: S3 GET ≈ $0.0004/1000).
- The client still needs ETags for `POST /complete` **unless** the server also
  derives the completion list from `ListParts` — which is why this decision and
  **[DECIDE 3]** are coupled.

**(c) Hybrid: client persists, server reconciles and wins.** Client sends what it
thinks it has; server checks `ListParts` and overrides. Belt and braces.
- Strictly more code than (b), and the client's copy is never consulted for
  anything, so it is a second source of truth kept purely to be ignored.

#### Recommendation: **(b), server-authoritative.**

The client persists identifiers only. It never persists an ETag, so it can never
persist a wrong one. This is a direct departure from `PROJECT_PLAN.md`'s
"ETag persistence" deliverable and should be approved as such.

The consequence worth stating: **once the server can enumerate parts, the case
for the native module weakens further** (see **[DECIDE 1]**), because correctness
no longer depends on the client surviving anything. It only has to come back
eventually.

---

### [DECIDE 3] — the endpoint surface

Given **[DECIDE 2]**(b), what exactly gets added to `router.go`?

**(a) One route: `POST /jobs/{id}/upload-urls`.** Returns fresh URLs for missing
parts plus the `uploaded_parts` list (with ETags, straight from `ListParts`). The
client keeps assembling the completion body and posting it to the existing
`POST /jobs/{id}/complete`.
- One new handler, one new route, no change to `CompleteUpload`.
- The client still handles ETags — it just gets them from the server rather than
  from its own disk. Simpler than (b) on the backend, and the client already has
  the code path.

**(b) (a) plus a tolerant `CompleteUpload`.** If the request body's `parts` array
is empty or absent, the handler derives the completion list from `ListParts`
itself.
- The client never touches an ETag at all.
- `CompleteMultipartUpload` requires parts in ascending `PartNumber` order with
  matching ETags; deriving them from `ListParts` gets both properties for free
  and removes a class of client bug entirely.
- Touches an existing, working handler (`handlers.go:160`) — a small backward-
  compatible change (empty `parts` currently produces a call S3 rejects anyway).
- Also fixes a latent gap: `CompleteUpload` passes `req.UploadID` straight to S3
  (`handlers.go:186`) without checking it against `job.Upload.UploadID`. Deriving
  server-side sidesteps that.

**(c) A single combined `POST /jobs/{id}/resume`** returning status + URLs +
completing automatically when nothing is missing.
- Fewer round trips, but it conflates "tell me what's left" with "finish it,"
  which means the client cannot ask a question without risking a state change.

**(d) `GET /jobs/{id}/upload-status` (read-only) + `POST .../upload-urls`.** Two
routes, clean read/write split.
- The read route has no consumer that the write route does not already serve.
  Recorded and rejected as surface for its own sake.

#### Recommendation: **(b) — one new route, plus tolerant completion.**

The new route is the load-bearing part. The tolerant `CompleteUpload` is ~15
lines and eliminates the last reason for the client to hold an ETag, which is
what makes **[DECIDE 2]**(b) actually complete rather than half-done.

Keep the existing `parts`-supplied path working. Stage 7's uploader uses it, it
is verified, and removing it would mean changing working client code for no
functional gain.

**Also required, and easy to forget:** the new handler must presign through
`s3.GeneratePresignedUploadURL` (`s3.go:102`), which already routes through
`presignOptions` (`s3.go:158`) and therefore already signs against
`S3_PUBLIC_ENDPOINT`. If the new handler builds its own presign call, it will
silently sign `localstack:4566` and reproduce the exact bug Stage 7 existed to
fix. **Reuse the method; do not reimplement it.**

---

### [DECIDE 4] — abandoned multipart uploads, which cost money and are invisible

**8A is the first stage in this project that deliberately manufactures abandoned
multipart uploads.** Its core test is "kill the app mid-upload," repeated until
the resume path is trusted. Every one of those runs leaves parts in S3.

The facts, verified:

- `handlers.go` calls `AbortMultipartUpload` in exactly two places — presign
  failure (`:125`) and DynamoDB write failure (`:145`). Both are server-side error
  paths. **Nothing aborts an upload the client simply walks away from.**
- There is **no client-reachable abort route**. `router.go` has four routes and
  none of them is a cancel.
- `infra/localstack/init-aws.sh` sets CORS, a bucket policy on the HLS bucket,
  queues, a table and event notifications. It sets **no lifecycle configuration
  on any bucket** (verified by grep: zero hits for `lifecycle` or
  `AbortIncomplete` under `infra/`).
- Parts of an incomplete multipart upload **bill as storage** and do **not**
  appear in `aws s3 ls` or the console's object listing. They are visible only
  through `ListMultipartUploads`.

**The honest cost accounting**, because overstating it would be its own kind of
dishonesty: at `≤10s` clips (`config/free-tier.md`), an abandoned upload is a few
megabytes. A hundred of them is a few hundred MB, which at ~$0.023/GB-month is
under a cent. **The money is not the argument.**

The argument is the two things that are true regardless of the amount:

1. **`config/free-tier.md` imposes a naming obligation**: *"whoever (or whatever)
   brings up an AWS resource must say so explicitly at the end of that run — name
   the resource and state that it needs switching off."* A resource that does not
   appear in any listing the project uses **cannot be named**, and therefore
   cannot be torn down under that rule. That is a process failure, not a billing
   one.
2. **This is the project's recurring failure mode with the serial numbers filed
   off**: state that exists, costs something, and is invisible to every check
   currently run. 6A found it with the reel endpoint. 7 found it with the
   presigned host. Here it is again.

#### Options

**(a) Do nothing.** Status quo. Abandoned uploads accumulate silently for the
life of the LocalStack volume and, if this ever touches real AWS, forever.

**(b) `DELETE /jobs/{id}/upload` → `AbortMultipartUpload`.** A client-reachable
cancel. `s3.AbortMultipartUpload` (`s3.go:140`) already exists; this is a handler
and a route.
- Covers the polite path: the user taps Cancel, or the app decides the source
  file is gone (**[DECIDE 5]**) and gives up cleanly.
- Covers **none** of the paths this stage actually creates, all of which are "the
  app is not running any more."

**(c) An S3 lifecycle rule on `dayreel-raw-videos`:**

```json
{"Rules": [{
  "ID": "AbortIncompleteMultipartUploads",
  "Status": "Enabled",
  "Filter": {"Prefix": ""},
  "AbortIncompleteMultipartUpload": {"DaysAfterInitiation": 1}
}]}
```

- The only option that covers the app-never-comes-back case, which is the
  majority case here.
- Minimum granularity is **1 day** — the field is `DaysAfterInitiation` and takes
  an integer. There is no "1 hour" rule. So an upload abandoned at 09:00 lives
  until S3's daily lifecycle pass at least a day later. That is fine for cost and
  it means the rule is **not observable within a test session** (see below).
- Interacts with **[DECIDE 3]**'s `UPLOAD_GONE` error: after the reaping, a
  resume must fail with `NoSuchUpload` and the client must recover by starting
  over rather than looping.
- **Cannot be verified in LocalStack Community, and will fail in the most
  misleading way possible.** `docs/SETUP.md` already documents the precedent
  exactly: applying a `Deny`-anonymous bucket policy and `put-public-access-block`
  both *succeed* against LocalStack, read back correctly, and are then ignored.
  Expect `put-bucket-lifecycle-configuration` to behave the same — accepted,
  readable back, never enforced. **Do not treat "the config applied" as evidence
  the rule works.**

**(d) A sweeper**: a scheduled process calling `ListMultipartUploads` and
aborting anything older than N minutes.
- Observable within a test session, unlike (c), and tunable below one day.
- A new always-running component on a project whose budget rules say nothing
  should stay up, to solve a problem S3 solves natively. Recorded and rejected as
  a service; **kept as a script** — see below.

**(e) A manual reaper script**, `scripts/abort-stale-uploads.sh`, run by a human
or by the end of an E2E run.
- Zero standing cost, immediately observable, and it satisfies the naming
  obligation in `config/free-tier.md` because it *lists* what it is about to
  abort.

#### Recommendation: **(c) + (b) + (e).**

- **(c)** is the safety net for the real-AWS case and the case where the app never
  returns. Apply it in `init-aws.sh` **and** state in the file's comment that its
  enforcement is unverified locally, in the same voice as the existing
  public-read comment.
- **(b)** is the correct behaviour for a cancel the app knows about, and it is
  ~20 lines against an S3 method that already exists.
- **(e)** is what actually gets used during this stage's own testing, and it is
  the thing that makes abandoned uploads *visible*. It should be run — and its
  output pasted — as part of Verification.

The verifiable claim, and the one that matters for this project's habits:

```bash
# After deliberately killing an upload mid-flight:
docker exec dayreel-localstack awslocal s3 ls s3://dayreel-raw-videos/ --recursive
# EXPECT: the abandoned job's key is ABSENT — it is not an object yet

docker exec dayreel-localstack awslocal s3api list-multipart-uploads \
  --bucket dayreel-raw-videos
# EXPECT: it is HERE. This is the listing nothing in this project has ever run.
```

If those two commands disagree — and they will — the invisibility is
demonstrated, not asserted.

---

### [DECIDE 5] — the source file may not survive the kill that the ETags survive

Stage 7 **[DECIDE 3]** settled on `react-native-document-picker` with
`copyTo: 'cachesDirectory'`, reading `fileCopyUri`. That was the right call for a
foreground upload. It is a liability for a resumable one.

Two independent problems on Android:

1. **`cachesDirectory` is reclaimable.** `Context.getCacheDir()` is explicitly
   documented as space the system may delete under storage pressure, and the
   user can clear it from Settings with one tap. The host machine has **2.8 GB
   free** and an emulator image will be carved out of it — storage pressure is
   not hypothetical here.
2. **The picker's `content://` URI goes stale.** `ACTION_OPEN_DOCUMENT` grants a
   URI permission scoped to the process unless
   `takePersistableUriPermission` is called. `react-native-document-picker` does
   not call it. After a process kill the original `uri` is unusable, which is
   precisely why Stage 7 uses `fileCopyUri` — but that only moves the problem to
   problem 1.

**The resulting failure is specific and nasty:** resume works perfectly, the
server reports parts 1–7 present and hands back URLs for 8–12, and the client
then discovers it has no bytes to put in them. Every part of the mechanism this
stage builds reports success, and the upload cannot finish.

#### Options

**(a) Keep `cachesDirectory`.** Cheapest. Accept that resume fails when the cache
was reclaimed, and detect it — check the file exists before resuming, and if it
does not, abort the upload (**[DECIDE 4]**(b)) and tell the user to re-pick.
- Correct behaviour on a bad path, and it is *honest* — nothing pretends to work.
- But "upload survives app kill" is then conditional on Android's mood.

**(b) Copy to the app's document directory at job creation.**
`react-native-blob-util`'s `fs.dirs.DocumentDir` (Stage 7 already adds this
dependency). Delete on successful completion, on abort, and on a startup sweep of
orphans.
- Survives cache reclamation and Clear Cache. This is what makes resume
  dependable rather than probable.
- Costs a **full second copy of the video** in app-private storage, held for the
  life of the upload. On the emulator's small user partition that is a real
  constraint, and it makes the cleanup path load-bearing rather than tidy.
- Requires an explicit orphan sweep at app start: any file in the upload
  directory with no corresponding entry in the job index gets deleted. Without
  it, one crash leaks a video-sized file permanently.

**(c) Re-resolve the `content://` URI on relaunch** via
`takePersistableUriPermission`. Requires patching or replacing the picker
(`react-native-document-picker` is at `^9.3.1`; the maintained successor is
`@react-native-documents/picker`, which is a dependency swap on top of everything
else this stage does).
- Avoids the duplicate copy entirely — the cleanest answer in principle.
- Dependency churn on an unbuildable toolchain, and persistable grants are
  per-URI and can still be revoked. Recorded; not recommended now.

**(d) Copy to `DocumentDir` only when needed** — keep `cachesDirectory` for the
common small/fast case, and promote to `DocumentDir` only if the upload is going
to be long-lived (backgrounded, or over N parts).
- Two code paths for one thing, each half-tested. The complexity is not paid for.

#### Recommendation: **(b), with (a)'s detection kept as the backstop.**

Persist to `DocumentDir` at job creation; delete on completion, on abort, and on
an orphan sweep at app start. **And still check the file exists before resuming**,
because (b) reduces the probability of the missing-source failure without
eliminating it — the user can uninstall, the OS can be killed mid-copy, and the
file can be zero-length.

State the disk cost in `docs/SETUP.md` next to the existing device-local job
index limitation: **the app holds a full copy of every video with an unfinished
upload.**

---

### [DECIDE 6] — there is no window in which to interrupt an upload

> **Problem 1 (one part per upload) is SOLVED as of 2026-08-13** and the option
> that addressed it is struck out below. Only Problem 2 remains open, and it is
> the one that was never written down anywhere before this plan.

#### Problem 1: one part — RESOLVED, no longer 8A's work

Stage 7 **[DECIDE 8]** specified `UPLOAD_PART_SIZE` and it has now landed:
`config.go:49` (`UploadPartSize`), `config.go:72` (`getEnvBytes`), `config.go:79`
(`DefaultUploadPartSize = 5 * 1024 * 1024`), `handlers.go:28` (`h.partSize()`),
`docker-compose.yml:82` (`UPLOAD_PART_SIZE=262144` on `api`). The hardcoded
`partSize` const at the old `handlers.go:21` is gone.

A 3 MB fixture at 256 KiB per part is **12 parts**, so the multipart path is
genuinely exercised. **Verify this holds before relying on it** — one
`POST /jobs` for a 3 MB file must return 12 URLs, not 1 — because everything else
in this stage is untestable at one part per upload.

Carry forward the constraint, which has not changed: **S3 requires ≥5 MiB for
every part except the last**, so 256 KiB is **local-only** and real AWS rejects
the completion with `EntityTooSmall`. `config.go:96-101` appears to warn on
sub-minimum values, which is the right shape. This is the second setting after
`S3_PUBLIC_ENDPOINT` whose correct value depends on where the stack points, and
both belong in the same paragraph of `docs/SETUP.md`.

#### Problem 2: no interruption window

Less obvious and equally fatal. Over loopback to LocalStack, a 3 MB upload
finishes in well under a second. **There is no wall-clock window in which to kill
the app mid-upload.** A test that cannot reach the state it is testing passes
vacuously — which is precisely the failure mode this project keeps hitting.

#### Options

- ~~**(a) Land `UPLOAD_PART_SIZE` as 8A's first commit.**~~ **Already done** — see
  Problem 1 above. Retained only so the option list reads correctly against the
  discussion.
- **(b) A debug-only inter-part delay** — e.g. `UPLOAD_PART_DELAY_MS` read from
  the mobile config, default 0, set to ~500 ms during resume testing. 12 parts ×
  500 ms = a 6-second window that is trivially interruptible.
  - Test-only code in the client. Must be inert by default and obviously named.
- **(c) Emulator network throttling.** `adb shell settings`/the emulator console
  (`telnet localhost 5554` → `network speed gsm`) throttles the whole device.
  - Real conditions, no product code touched. Requires the emulator, i.e. blocked
    on **[DECIDE 7]**, and it is fiddly to reproduce exactly.
- **(d) A larger local fixture.** LocalStack is free and the `≤10s` rule in
  `config/free-tier.md` is a *real-AWS cost* constraint, not a local one. A 200 MB
  file gives a genuine multi-second upload at 5 MiB parts.
  - Needs a fixture generated, stored, and kept out of git, and the host has
    **2.8 GB free**. Also risks tripping `MaxDuration`.
- **(e) A killswitch in the uploader** — a debug button that terminates the
  process between parts, rather than racing a human's finger.
  - Deterministic. Combined with (b), makes the resume test repeatable instead of
    a stunt.

#### Recommendation: **(b), with (e) if the manual kill proves flaky.**

(b) is three lines and turns a race into a reliable test. (c) stays available once
a device exists. (d) is rejected on disk.

**Like `UPLOAD_PART_SIZE`, the inter-part delay is a local-only setting that
would be wrong in production.** It must default to 0 ms and be documented as a
local override, or it becomes the next `S3_PUBLIC_ENDPOINT`-shaped trap.

---

### [DECIDE 7] — the Android toolchain does not exist, and both 8A and 8B need it

**This decision is shared with Stage 8B, which references it rather than
repeating it.** 8B is blocked on it *entirely*; 8A is blocked on it only from
Phase 2 onward.

Verified on this machine, today:

| Fact | Value |
|---|---|
| `ANDROID_HOME` | unset |
| `~/Library/Android/sdk` | does not exist |
| `adb`, `emulator` on `PATH` | neither |
| Java | OpenJDK 21.0.6 — **present**, the one prerequisite already met |
| Host disk free (`/System/Volumes/Data`) | **2.8 GB** |
| Docker images / build cache | 4.344 GB / 2.302 GB (1.759 GB reclaimable) |

`mobile/android/build.gradle` pins: `compileSdk 37`, `targetSdk 36`,
`buildToolsVersion 37.0.0`, `ndkVersion 27.1.12297006`, `minSdk 24`, Kotlin
2.2.0, Gradle 9.4.1, `newArchEnabled=true`.

The NDK alone is multiple GB. Add an SDK platform, build-tools, one emulator
system image, and Gradle's caches and it is comfortably 15–20 GB against 2.8 GB
free. **`docs/SETUP.md` already says this and says installing into 2.8 GB will
leave a half-populated SDK, which is worse than none.**

This also means, and it should be said plainly: **the Stage 7 mobile code being
written right now cannot be built or run either.** Everything from Stage 7's
Phase 3 onward is in the same boat.

#### Options

**(a) Reclaim host disk first.** Docker holds 4.344 GB of images and 2.302 GB of
build cache (1.759 GB reclaimable). `docker builder prune -f` plus removing
non-DayReel images is the cheapest few GB available.
- Nowhere near enough on its own — it gets to maybe 6–8 GB free against a 15–20 GB
  need — and it must not delete the `whisper-models` volume (~141 MB, re-downloads)
  or force a full rebuild of images the pipeline needs.

**(b) Minimal SDK install.** `cmdline-tools` + platform 36 + build-tools 37.0.0 +
one x86_64 system image, **skipping the NDK**.
- **Whether the NDK is actually required is UNKNOWN and must be established
  before this is costed.** React Native 0.87 ships prebuilt native artifacts; a
  New Architecture app that adds no C++ of its own may never invoke the NDK. But
  `ndkVersion` is pinned in `build.gradle`, and any dependency that builds from
  source will need it. This is a ~10 GB question with a one-command answer
  (`./gradlew :app:assembleDebug` and read the failure), and it should be
  answered before anything is downloaded.

**(c) A physical Android device instead of an emulator.** Saves the system image
(the largest single item after the NDK) and is faster.
- Changes the network story completely: `10.0.2.2` is an *emulator* alias for the
  host loopback and means nothing on a real device. The device needs either
  `adb reverse tcp:8080 tcp:8080 && adb reverse tcp:4566 tcp:4566` — which makes
  `localhost` work and would require `S3_PUBLIC_ENDPOINT=http://localhost:4566`
  and an `api` restart — or the host's LAN IP with the same restart. Either way
  `docs/SETUP.md`'s "one client environment at a time" rule bites.
- Still needs platform-tools and the SDK, just not the image.

**(d) External disk / cloud build.** Point `ANDROID_SDK_ROOT` at external storage.
- Works, is slow, and adds a device that has to be present for every build.

**(e) Defer both 8A Phase 2 and all of 8B**, ship 8A Phase 1 (backend) and stop.
- The honest option if the disk cannot be freed. It leaves the project with a
  correct, tested resume **API** and no client that uses it — which is still
  strictly more than it has now, and is a real deliverable rather than a
  half-built one.

#### Recommendation: **(a) → answer (b)'s NDK question → then (b), with (e) as the standing fallback.**

Sequence, because the order is what saves the effort:

1. Reclaim what Docker is holding. Measure the result. **If the free figure after
   reclamation is under ~12 GB, stop and take (e)** rather than starting an
   install that will fail partway.
2. Install `cmdline-tools` only (a few hundred MB) and run
   `./gradlew :app:assembleDebug --dry-run`, then a real assemble, to find out
   what is genuinely required. **Do not download the NDK on the assumption that
   it is needed.**
3. Only then commit to the full install.

**Treat this as a scheduled risk with an owner and a checkpoint, not a
footnote.** It is the single most likely reason this stage and 8B do not finish.

---

## Files

| File | Action | Purpose |
|---|---|---|
| `backend/internal/api/handlers.go` | Modify | `ResumeUpload` handler; tolerant `CompleteUpload`; `AbortUpload` handler |
| `backend/internal/api/router.go` | Modify | `POST /jobs/{id}/upload-urls`, `DELETE /jobs/{id}/upload` |
| `backend/internal/api/handlers_test.go` | Create/Modify | Missing-part computation; each error code; part-size config |
| `backend/internal/storage/s3.go` | Modify | `ListParts` (paged), `ListMultipartUploads` |
| `backend/internal/storage/s3_test.go` | Modify | Presign host still public for re-issued URLs — the Stage 7 regression to guard |
| `infra/localstack/init-aws.sh` | Modify | `AbortIncompleteMultipartUpload` lifecycle rule + honest comment (**[DECIDE 4]**) |
| `infra/docker-compose.yml` | Modify | `UPLOAD_PART_SIZE=262144` on `api` |
| `scripts/abort-stale-uploads.sh` | Create | List then abort in-flight uploads (**[DECIDE 4]**(e)) |
| `scripts/verify-resume.sh` | Create | The whole stage, proven from `curl`, before any app code |
| `mobile/src/storage/uploadState.ts` | Create | Persist upload identifiers (**not** ETags — **[DECIDE 2]**), alongside the existing `jobIndex.ts` |
| `mobile/src/upload/uploader.ts` | Modify | Relax `plans.length !== urls.length` for the resume path; seed `completedBytes`; source-file existence check |
| `mobile/src/upload/resume.ts` | Create | Startup scan → `POST /jobs/{id}/upload-urls` → continue |
| `mobile/src/screens/HomeScreen.tsx` | Modify | Copy source to `DocumentDir` (**[DECIDE 5]**) |
| `mobile/src/api/client.ts` | Modify | `resumeUpload`, `abortUpload` |
| `mobile/src/types/api.ts` | Modify | New response types, generated from the Go structs as in Stage 7 **[DECIDE 5]** |
| `docs/SETUP.md` | Modify | `UPLOAD_PART_SIZE`; abandoned-upload reaping; the `DocumentDir` disk cost |
| `mobile/android/.../DayReelUploadModule.kt` | Create | **Only if [DECIDE 1] picks (b).** Not in the recommended scope |

## Tasks

1. [x] ~~`UPLOAD_PART_SIZE` in config + compose + `handlers.go`~~ — **landed
       2026-08-13** with Stage 7. Confirm it works (3 MB → 12 URLs) rather than
       assuming it
2. [ ] `storage.ListParts` with paging; `storage.ListMultipartUploads`
3. [ ] `POST /jobs/{id}/upload-urls` — reconcile, presign only what is missing
4. [ ] Tolerant `CompleteUpload` (**[DECIDE 3]**(b))
5. [ ] `DELETE /jobs/{id}/upload` (**[DECIDE 4]**(b))
6. [ ] Lifecycle rule in `init-aws.sh` + `scripts/abort-stale-uploads.sh`
7. [ ] `scripts/verify-resume.sh` — **prove the whole stage from `curl` before any
       app code exists**
8. [ ] Backend unit tests; `go build ./... && go vet ./... && go test ./...`
9. [ ] **CHECKPOINT — [DECIDE 7].** Everything above is done without an Android
       SDK. Everything below requires one.
10. [ ] `mobile/src/storage/uploadState.ts`
11. [ ] `mobile/src/upload/resume.ts` + uploader resume entry point
12. [ ] `DocumentDir` source persistence + orphan sweep (**[DECIDE 5]**)
13. [ ] Kill-and-relaunch on the emulator, repeatedly
14. [ ] Failure paths — all of them, none skipped
15. [ ] `docs/SETUP.md`, `mobile/CONTEXT.md`

## Test

```bash
# ── Phase 1: the entire stage, proven from the host, with no app. ──────────────
#
# NOTE: with S3_PUBLIC_ENDPOINT=http://10.0.2.2:4566 (the committed Android
# target), presigned URLs are signed for a host this machine does not route to,
# so this script CANNOT run as-is — see docs/SETUP.md. Flip the api service to
# http://localhost:4566 for the duration, or alias 10.0.2.2 onto lo0.

cd infra && docker compose up -d --build api

# 12 parts at UPLOAD_PART_SIZE=262144 for a 3 MB file
head -c 3145728 /dev/urandom > /tmp/clip.bin

JOB_JSON=$(curl -s -X POST localhost:8080/jobs -H 'Content-Type: application/json' \
  -d '{"filename":"clip.mp4","size_bytes":3145728,"content_type":"video/mp4"}')
JOB=$(echo "$JOB_JSON" | jq -r .job_id)
echo "$JOB_JSON" | jq '{part_size, n_urls: (.upload_urls|length)}'
# EXPECT: part_size 262144, n_urls 12
# If n_urls is 1, UPLOAD_PART_SIZE is not reaching the api container and NOTHING
# below this line is testable. Check docker-compose.yml:82 reached the process.

# Upload ONLY parts 1-3, then stop. This is a killed app.
split -b 262144 /tmp/clip.bin /tmp/part_
i=1; for f in $(ls /tmp/part_* | head -3); do
  curl -s -X PUT --data-binary @"$f" "$(echo "$JOB_JSON" | jq -r ".upload_urls[$((i-1))].url")" -o /dev/null
  i=$((i+1))
done

# The invisibility demonstration (DECIDE 4)
docker exec dayreel-localstack awslocal s3 ls s3://dayreel-raw-videos/ --recursive
# EXPECT: nothing for $JOB. Three parts are stored and nothing lists them.
docker exec dayreel-localstack awslocal s3api list-multipart-uploads --bucket dayreel-raw-videos
# EXPECT: the upload is HERE, with $JOB's key.

# THE NEW ENDPOINT — the thing this stage exists for
curl -s -X POST localhost:8080/jobs/$JOB/upload-urls | jq '{
  n_uploaded: (.uploaded_parts|length),
  missing:    [.upload_urls[].part_number],
  host:       (.upload_urls[0].url | split("/")[2])
}'
# EXPECT: n_uploaded 3, missing [4..12], host matches S3_PUBLIC_ENDPOINT
#         (NOT localstack:4566 — that regression is one careless presign away)

# Finish from the resumed URLs, then complete WITHOUT sending any ETags
# ... PUT parts 4-12 from the response ...
curl -s -X POST localhost:8080/jobs/$JOB/complete \
  -H 'Content-Type: application/json' \
  -d "{\"upload_id\":\"$(echo "$JOB_JSON" | jq -r .upload_id)\"}" | jq
# EXPECT: 200. The server derived the part list from ListParts (DECIDE 3b).

sleep 10 && curl -s localhost:8080/jobs/$JOB | jq '{status, stages}'
# EXPECT: the pipeline ran. A resumed upload is indistinguishable downstream.

# Abort path
curl -s -X DELETE localhost:8080/jobs/$JOB2/upload -o /dev/null -w '%{http_code}\n'
docker exec dayreel-localstack awslocal s3api list-multipart-uploads --bucket dayreel-raw-videos
# EXPECT: $JOB2 gone from the listing.

# ── Phase 2: the app. Blocked on [DECIDE 7]. ──────────────────────────────────
cd ../mobile && npx react-native run-android
```

## Verification

_Nothing checked off until observed._

**Phase 1 — backend, no toolchain required**

- [ ] `POST /jobs` returns **12** URLs for a 3 MB file with `UPLOAD_PART_SIZE=262144`
- [ ] `POST /jobs/{id}/upload-urls` on a fresh job returns all N parts as missing
- [ ] After PUTting parts 1–3, it returns exactly parts 4–N and lists 1–3 as
      uploaded, **with the ETags S3 reports**, not any the client sent
- [ ] The re-issued URLs' host is `S3_PUBLIC_ENDPOINT`, not `localstack:4566` —
      the Stage 7 regression guard
- [ ] A re-issued URL actually accepts a PUT with
      `S3_SKIP_SIGNATURE_VALIDATION=0` set, so the signature is genuinely valid
- [ ] Parts uploaded out of order (5, 2, 9) reconcile correctly
- [ ] Re-uploading a part that already landed is accepted and its ETag updates
- [ ] `POST /complete` with an **empty** `parts` array succeeds, deriving from
      `ListParts` (**[DECIDE 3]**(b))
- [ ] `POST /complete` with a client-supplied `parts` array **still works** —
      Stage 7's uploader must not break
- [ ] The pipeline runs on a resumed upload exactly as on a fresh one
- [ ] `DELETE /jobs/{id}/upload` removes the upload from `ListMultipartUploads`
- [ ] `POST .../upload-urls` after that abort returns **410 `UPLOAD_GONE`**, not
      a 500
- [ ] `POST .../upload-urls` on an unknown job returns 404; on a completed job,
      409
- [ ] `s3 ls` does **not** show an abandoned upload and `list-multipart-uploads`
      **does** — run both, paste both

**Phase 2 — the app. Blocked on [DECIDE 7]**

- [ ] The app builds and launches on the emulator (**this has never happened in
      this repo**)
- [ ] Upload starts, is killed at ~30%, app relaunches, upload resumes from the
      recorded part and finishes
- [ ] The resumed job reaches `completed` and `GET /jobs/{id}/reel` returns 200
- [ ] Killed **between** parts and killed **during** a part both resume
- [ ] Killed **after the last part but before `POST /complete`** resumes to a
      completion, not a stall — this is the case a client-side ETag array gets
      wrong and `ListParts` gets right
- [ ] Airplane mode mid-upload: retries, then a clear failed state; resumes on
      reconnect
- [ ] **Source file deleted while the app is dead** (`adb shell rm` the
      `DocumentDir` copy): resume detects it, aborts the upload, and tells the
      user to re-pick — it does **not** hang or loop (**[DECIDE 5]**)
- [ ] Orphan sweep at startup removes a `DocumentDir` file with no job-index entry
- [ ] The `DocumentDir` copy is deleted on successful completion
- [ ] Uninstall/reinstall loses the job index (known, `docs/SETUP.md`) and does
      **not** leave a live multipart upload that nothing can reach — the lifecycle
      rule is the only thing covering this, and locally it is unverifiable

**Cost, disk and teardown**

- [ ] `list-multipart-uploads` is **empty** at the end of the session, after
      `scripts/abort-stale-uploads.sh`
- [ ] `docker system df` — the 8 GB ceiling holds (it was at 4.3 GB images +
      2.3 GB build cache before this stage started)
- [ ] Host free disk recorded before and after (**2.8 GB** at plan time)
- [ ] Nothing provisioned on real AWS. If anything was, name it explicitly per
      `config/free-tier.md`

**Explicitly NOT verified, and why**

- [ ] The lifecycle rule's **effect**. LocalStack Community accepts and ignores
      bucket security configuration (`docs/SETUP.md`), the granularity is one
      day, and no test session lasts that long. What can be verified is that the
      configuration applies and reads back. **That is not evidence it works.**
      Only a real bucket over a real day settles it.
- [ ] `ListParts` paging beyond 1000 parts. The loop will be written and never
      executed at these fixture sizes.

## Claude Code Implementation Plan

### Recommended Approach: Ship the Backend Before the Toolchain Exists

This stage is ~80% platform-independent backend work and ~20% Android client
work, and the two halves have completely different risk profiles:

- **The backend half is unblocked, fast-looping, and `curl`-verifiable today.**
- **The client half cannot be compiled at all** (**[DECIDE 7]**), and its value is
  contingent on **[DECIDE 1]**.

So: build, test, prove and **commit** the backend before touching `mobile/`. If
**[DECIDE 7]** goes badly, the stage still lands something real and correct
instead of a half-finished native module.

This also inverts Stage 7's structure deliberately. Stage 7 front-loaded a
`curl` proof because the *backend* was uncertain. Here the backend is the certain
part and the *toolchain* is the uncertain part — so the backend goes first for
the opposite reason: to bank it.

### Pre-Flight Check

```
0a. docker system df                         # was 4.344GB images / 2.302GB cache
0b. docker builder prune -f                  # ~1.759GB reclaimable
0c. df -h /System/Volumes/Data                # was 2.8GB free. This is DECIDE 7.
0d. docker compose ps                        # 6 containers healthy
0e. curl -s localhost:8080/health
0f. Confirm DECIDE 1. If (b), the stage is roughly twice this size and is
    entirely gated on 0c. If (a)/(c), phases 1-3 are unaffected either way.
0g. Confirm DECIDE 3 — it decides whether CompleteUpload is touched.
0h. POST /jobs for a 3MB file; assert 12 upload_urls, not 1. UPLOAD_PART_SIZE
    landed 2026-08-13 (compose :82) but "configured" and "reaching the running
    container" are different claims and only one of them is testable.
0i. Confirm Stage 7's mobile half is MERGED, not just present in the working
    tree. As of 2026-08-13 mobile/src/upload/, mobile/src/storage/ and
    mobile/src/hooks/ exist but are uncommitted. Phases 5+ edit those files.
0j. Flip api -> S3_PUBLIC_ENDPOINT=http://localhost:4566 for phase 1-3 curl
    work, and REMEMBER TO RESTORE 10.0.2.2 before any emulator run.
```

### Execution Steps

```
Phase 1: DELETED — UPLOAD_PART_SIZE landed with Stage 7 on 2026-08-13.
1.  Only the check survives: POST /jobs for a 3MB file returns 12 URLs, not 1.
    Do it as step 0h of pre-flight, not as a phase.

Phase 2: Reconciliation primitives (parallel)
6a. storage/s3.go: ListParts (paged), ListMultipartUploads
6b. storage/s3_test.go
7.  go test ./internal/storage/

Phase 3: The endpoints (parallel writes, then one test pass)
8a. handlers.go: ResumeUpload  -- reuse GeneratePresignedUploadURL, do NOT
    reimplement presigning (it is what carries the S3_PUBLIC_ENDPOINT fix)
8b. handlers.go: tolerant CompleteUpload  (DECIDE 3b)
8c. handlers.go: AbortUpload             (DECIDE 4b)
8d. router.go: the two new routes
9.  handlers_test.go: missing-part computation, every error code
10. go build ./... && go vet ./... && go test ./...
11. docker compose up -d --build api
12. RUN scripts/verify-resume.sh END TO END. Nothing proceeds until it passes.
13. COMMIT.

Phase 4: Reaping   <-- SEPARATE COMMIT
14a. init-aws.sh: AbortIncompleteMultipartUpload + the honest comment
14b. scripts/abort-stale-uploads.sh
15.  Apply the lifecycle config to the RUNNING stack — init-aws.sh only runs on
     a fresh localstack (same pattern as 5A's queue timeout and 6A's policy)
16.  Verify it reads back. Record that this is NOT evidence it enforces.
17.  COMMIT.

=== HARD CHECKPOINT: everything above needs no Android SDK. ===
=== Everything below is blocked on DECIDE 7. Stop here if unresolved. ===

Phase 5: Client persistence (parallel writes)
18a. mobile/src/storage/uploadState.ts     (identifiers only, no ETags)
18b. mobile/src/api/client.ts              (resumeUpload, abortUpload)
18c. mobile/src/types/api.ts               (from the Go structs, by hand)
19.  npx tsc --noEmit

Phase 6: Resume + source persistence (parallel writes)
20a. mobile/src/upload/resume.ts
20b. mobile/src/upload/uploader.ts          (resume entry, file-exists check,
     and RELAX the plans.length !== urls.length guard for the resume path —
     it currently rejects every partial URL set; keep it strict for fresh
     uploads, and seed completedBytes so progress does not restart at 0)
20c. mobile/src/screens/HomeScreen.tsx      (DocumentDir copy, DECIDE 5)
21.  npm test    <-- part-set arithmetic and state transitions are pure; test
     them here, where the loop is a second rather than an emulator boot

Phase 7: On the device
22. Emulator: upload, kill, relaunch, resume, complete
23. EVERY failure path in Verification. Especially "killed after the last part
    but before complete" and "source file deleted while dead"
24. abort-stale-uploads.sh; confirm list-multipart-uploads is empty
25. docs/SETUP.md, mobile/CONTEXT.md, record results in this file

Phase 8 (ONLY if DECIDE 1 picks (b)): WorkManager
26. Do not start this before phase 7 is green. A native background worker
    wrapping an unproven resume is untestable in both directions at once.
```

### Parallel Opportunities

| Phase | Parallel files |
|---|---|
| 2 | `s3.go`, `s3_test.go` |
| 3 | `handlers.go` (three handlers), `router.go`, `handlers_test.go` |
| 4 | `init-aws.sh`, `abort-stale-uploads.sh` |
| 5 | `uploadState.ts`, `client.ts`, `types/api.ts` |
| 6 | `resume.ts`, `uploader.ts`, `HomeScreen.tsx` |

Phases 2 → 3 are strictly sequential; each gates the next. Phase 4 is independent
of 3 and could run alongside it, but it wants its own commit.

**Phases 5–7 conflict directly with the main session's Stage 7 mobile work.**
`mobile/src/upload/uploader.ts`, `mobile/src/storage/jobIndex.ts` and
`mobile/src/hooks/useJobPolling.ts` were created by Stage 7 on 2026-08-13 and
were still **uncommitted** when this plan was written. Do not start Phase 5 until
they are merged, or the two efforts will be editing the same files in flight.

### Subagents

The pattern that has paid off — `pkt_pts_time` in 4A, the exit-0-on-corrupt-input
trap in 5A, the HLS ladder in 6A — is delegating **empirical research with a slow,
verbose feedback loop**, never authoring.

- **Worth an agent: [DECIDE 7]'s NDK question.** "Does `./gradlew
  :app:assembleDebug` on this repo, with RN 0.87 and `newArchEnabled=true`,
  actually require the NDK?" It is a ~10 GB question, it is answerable by reading
  one build failure, and getting it wrong wastes the entire remaining disk.
  **Require the exact command and the exact error text, not a conclusion.**
- **Worth an agent: LocalStack's lifecycle behaviour.** Does
  `put-bucket-lifecycle-configuration` with `AbortIncompleteMultipartUpload`
  apply, read back, and — under any forced pass — actually abort? Given the
  documented precedent that LocalStack stores and ignores security config, the
  expected answer is "applies, reads back, never fires." **Require the exact
  commands and exact output**; a confident wrong answer here manufactures the
  false assurance **[DECIDE 4]** exists to avoid.
- **Worth an agent: the Android SDK install itself**, if it proceeds. Slow,
  enormously verbose, long tail of failures unrelated to this plan. Same argument
  2B made for `react-native init` and Stage 7 made for `pod install`.
- **Not worth an agent:** the Go handlers, the reconciliation logic, the client
  state machine. Small, and they need to be written by whoever read `handlers.go`.

Give any agent an explicit file boundary, the 8 GB Docker ceiling, and — new for
this stage — **the 2.8 GB host disk figure**, which is now the binding constraint.

### Potential Blockers

| Blocker | Resolution |
|---|---|
| **No Android SDK; 2.8 GB free disk** | **[DECIDE 7]**. The single most likely reason this stage does not finish. Phases 2–4 are deliberately built to be immune to it |
| **`UPLOAD_PART_SIZE` set but not reaching the container** | Landed 2026-08-13, but a value in compose is not a value in the process. One `POST /jobs` for 3 MB settles it: 12 URLs or 1 |
| **`plans.length !== urls.length` guard in `uploader.ts`** | Rejects every resume by construction. Found by reading Stage 7's just-landed code, not by running it |
| **No interruption window at loopback speed** | **[DECIDE 6]**(b), the debug inter-part delay. Without it the kill-and-resume test cannot reliably reach the state it tests |
| **New handler presigns against the internal endpoint** | Reuse `s3.GeneratePresignedUploadURL`. A fresh presign call would re-break Stage 7's whole reason for existing, and would look fine in code review |
| **Lifecycle rule unverifiable locally** | **[DECIDE 4]**. Documented as unverified rather than claimed. `abort-stale-uploads.sh` is the mechanism that actually runs |
| **Source file gone on resume** | **[DECIDE 5]**. Detect, abort, re-pick. The mechanism can be perfect and still have nothing to upload |
| **Stage 7's mobile half is not merged** | Phases 5–7 build on files that do not exist yet. Sequence, do not race |
| **`S3_PUBLIC_ENDPOINT=10.0.2.2` breaks host-side `curl`** | Already documented in `docs/SETUP.md`. Flip to `localhost` for Phase 1–4, restore before any emulator run — and **restoring it is the step that gets forgotten** |
| **WorkManager under New Architecture** | Only if **[DECIDE 1]**(b). A TurboModule under `newArchEnabled=true` on RN 0.87 with zero native modules in this repo today. Unbudgeted |

### Time Estimate

- ~~Phase 1 (part size)~~ — done, 2026-08-13
- Phase 2 (`ListParts`, `ListMultipartUploads`): ~20 minutes
- Phase 3 (endpoints + `verify-resume.sh`): ~50 minutes
- Phase 4 (reaping): ~25 minutes
- **Subtotal, backend, unblocked today: ~1½ hours**
- Phase 5–6 (client): ~50 minutes, **after** Stage 7's mobile half exists
- Phase 7 (device E2E + failure paths): ~50 minutes, **high variance**
- **[DECIDE 7]** (toolchain): **unbounded, 0–6 hours, possibly impossible**
- Phase 8 (WorkManager, only under **[DECIDE 1]**(b)): ~2 hours, high variance
- **Total, recommended scope: ~3¼ hours plus the toolchain, which is the whole risk**

The estimate is honest about its own shape: the backend number is reliable
because the work is ordinary and the feedback loop is a second. Everything after
the checkpoint is an estimate of work on a machine that cannot currently run it.

---

## Notes

### Interaction with Stage 8B — stated once, here

- **[DECIDE 7] (the toolchain) is shared.** 8B references it and does not restate
  the options. It gates 8B completely and 8A only from Phase 5.
- **8B needs nothing from 8A.** Playback reads `hls_url` from
  `GET /jobs/{id}/reel`, which works today. There is no ordering dependency in
  that direction.
- **8A needs nothing from 8B.**
- **They overlap in exactly one place:** both add client code to the same app, and
  both are downstream of Stage 7's mobile half landing.

### Build order — the recommendation

1. **8A Phases 2–4 (backend) first.** Unblocked today. Bankable. Removes the
   only genuinely hard problem in 8A while the toolchain question is still open.
2. **8B next, once a toolchain exists.** It is smaller, it is the actual product
   payoff (`PROJECT_PLAN.md`: "Both 8A and 8B complete = Full E2E demo"), and it
   provides the first real player in the project — which is the oracle Stage 7's
   **[DECIDE 7]** caption measurement needs and which the iOS→Android switch took
   away.
3. **8A Phase 5–7 (client resume) last.** Most expensive, and its value is
   contingent on **[DECIDE 1]**.

If a strict stage-at-a-time order is required, **8A before 8B** — because 8A has
four phases that are unblocked right now and 8B has none.

### Risks and inherited tensions

- **Inherited from Stage 7: `UPLOAD_PART_SIZE`, now built** (2026-08-13). 8A is
  untestable without it, so confirm it reaches the running container rather than
  trusting the compose file.
- **Inherited from Stage 7 [DECIDE 3]: `copyTo: 'cachesDirectory'`.** Correct for
  a foreground upload, a liability for a resumable one (**[DECIDE 5]**).
- **Inherited from Stage 7 [DECIDE 5]: the job index is device-local.** A
  reinstall loses the index and therefore loses the ability to resume *or abort*
  any upload it referenced. That is now a cost leak, not just a UX limitation.
- **Inherited from 1B: `PERSISTENCE=1` is silently ignored** in LocalStack
  Community. A LocalStack restart wipes the multipart uploads too — which is
  convenient during testing and means a restart is **not** a valid way to prove
  the abort path works.
- **A fifth thing that only works locally.** After `S3_PUBLIC_ENDPOINT`,
  `MOCK_TRANSCRIBE`, the public HLS bucket and `S3_SKIP_SIGNATURE_VALIDATION`,
  this stage adds `UPLOAD_PART_SIZE=262144` — which real AWS rejects outright with
  `EntityTooSmall`. The list of settings whose correct value depends on where the
  stack points is getting long enough to deserve one table in `docs/SETUP.md`.
- **Departure from `PROJECT_PLAN.md`, twice.** **[DECIDE 1]** may drop the
  WorkManager module; **[DECIDE 2]** drops "ETag persistence" as a client
  responsibility. Both are deliberate and both need approving as scope changes,
  not absorbed quietly.

### Deliberately not in scope

- **Upload of multiple videos concurrently.** One at a time.
- **Bandwidth-aware or metered-connection policy.** Meaningful only under
  **[DECIDE 1]**(b).
- **Progress notifications in the Android status bar.** Same.
- **Server-side upload expiry policy beyond the lifecycle rule.**
- **iOS.** The platform decision is final and Android-only.

### Uncertain, flagged rather than smoothed over

- **Whether the Android SDK can be installed on this machine at all.**
  **[DECIDE 7]**. Everything client-side is downstream of this.
- **Whether the NDK is actually required** for a New Architecture RN 0.87 build
  with no custom native code. A ~10 GB assumption either way.
- **Whether LocalStack enforces `AbortIncompleteMultipartUpload`.** Strongly
  expected not to, on the documented precedent that it accepts and ignores bucket
  security configuration.
- **Whether `ListParts` in LocalStack returns ETags identical to what the PUT
  returned.** It should. If it does not, **[DECIDE 3]**(b)'s derived completion
  fails in a way that would look like a signature bug.
- **Real-device video sizes.** The 3 MB / 12-part figures come from a `≤10s`
  fixture, not from a real recording off a real phone. At 4K the part count and
  every timing in this plan change materially.
- **Whether Stage 7's uploader can take a resume entry point without a rewrite.**
  Partially answered by reading it (`uploader.ts`, landed 2026-08-13): the URL
  lookup is by part number, which is the hard part done right, but the
  `plans.length !== urls.length` guard rejects a partial set and the progress
  accumulator starts at zero. Both are small, both are load-bearing, and neither
  has been exercised — **nothing in `mobile/` has ever run on Android.**
- **Whether Stage 7's "persist the ETags as you go" premise survives
  [DECIDE 2].** The uploader's own docstring commits to it. **[DECIDE 2]**(b) says
  the client should never hold an ETag at all. One of the two has to give.
