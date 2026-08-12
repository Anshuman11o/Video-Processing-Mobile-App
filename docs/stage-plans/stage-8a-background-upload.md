# Stage 8A: Background Upload

> Status: **draft — not approved, and not yet approvable.** Written 2026-08-12,
> immediately after the Stage 7 plan was drafted. Six decisions open below.
>
> **Every one of them is downstream of a Stage 7 decision that has not been
> made.** Stage 7 is itself a draft with seven open items, and two of them
> ([DECIDE 1] and [DECIDE 3]) change what this stage *is*, not merely how it is
> built. This plan is therefore written to be **revisited after Stage 7 lands**,
> and the [WAIT] markers below record exactly what each item is waiting on.

> **Substrate superseded.** Docker Compose, LocalStack, SQS and Redis are gone;
> S3 and DynamoDB are real AWS and the queue is a local SQLite file. See
> `infra/CONTEXT.md`. Nothing in this stage depended on the emulator except the
> endpoints it uploads to — which is one more reason to revisit the [WAIT]
> items after Stage 7, since the host-binding problem they inherit from Stage 7
> no longer exists.

## Aim

Make an upload survive the app being killed. Pick a clip, start uploading, force
stop the app, relaunch — and the upload continues from the last part that
actually landed in S3, without the user doing anything.

`PROJECT_PLAN.md` names the deliverables: *"Kotlin WorkManager native module /
Chunked upload with ETag persistence / Resume from last successful part"*, and
the verification: *"Start upload, kill app, reopen — upload continues from where
it left off."*

**This is the stage the project's own README is about.** The repository
description leads with *"resuming from the exact byte it stopped at even after
app is killed."* Everything up to here has been pipeline; this is the claim.

---

## The Stage 7 problem, stated first

Stage 7 commits to **iOS Simulator** as its verification target
(`stage-7-upload-integration.md` **[DECIDE 2]**) and lists **Android** under
*"Deliberately not in scope."* The reason given is sound: the presigned host is
fixed per API process, so only one client environment can be served at a time,
and the iOS toolchain is the one with a verified path on this machine.

Stage 8A is a **Kotlin WorkManager** module. Android only.

So the two stages target different platforms, and the seam between them has
never been exercised:

| | Stage 7 target | Stage 8A target |
|---|---|---|
| Platform | iOS Simulator | Android Emulator |
| API host | `http://localhost:8080` | `http://10.0.2.2:8080` |
| `S3_PUBLIC_ENDPOINT` | `http://localhost:4566` | `http://10.0.2.2:4566` |
| Toolchain verified? | Xcode present; **`pod install` never run** | **Never run at all** |

Two consequences worth stating before any code is planned:

1. **The API cannot serve both at once.** Moving to Android means restarting the
   API with a different `S3_PUBLIC_ENDPOINT`, which invalidates the iOS path
   simultaneously. Stage 7's verification and Stage 8A's verification cannot be
   green at the same time on one API process. This is not a bug to fix here; it
   is a property of Stage 7 **[DECIDE 1](a)** that this stage inherits.
2. **After 8A, no single platform has both a verified upload and background
   resume** — unless Stage 7's happy path is re-run on Android first. That
   re-run, not the Kotlin module, is the real first task of this stage.

### [WAIT] — what this stage cannot start without

| Stage 7 item | Why 8A blocks on it |
|---|---|
| **[DECIDE 1]** — presign against public endpoint, or proxy through the API | **Forks the stage.** If the fallback (b) is taken and uploads proxy through the API, the Kotlin worker PUTs to `POST /jobs/{id}/parts/{n}` instead of to S3, and ETag persistence largely disappears — the API owns the multipart state. Roughly half this plan would be rewritten |
| **[DECIDE 3]** — `react-native-blob-util` slicing, or XHR single-part fallback | **Forks the stage.** The fallback makes `part_size` the whole file, which removes per-part retry — and per-part resume is the entire premise of 8A. If Stage 7 lands on the fallback, 8A must first restore multipart on the backend |
| **[DECIDE 3]**, ETag shape — *"leave the ETag array in a shape 8A can persist, rather than hiding it in a closure"* | 8A's resume state is that shape. Written before Stage 7, this plan can only guess it |
| **[DECIDE 5]** — the local job index (`mobile/src/storage/jobIndex.ts`) | 8A reconciles against it on relaunch. Its schema and storage mechanism are Stage 7's to choose |
| **[DECIDE 2]** — target client | Directly contradicts this stage. Needs an explicit amendment, not a silent divergence |
| Whether the app builds on Android at all | RN 0.87 + New Architecture, never once run here. Unknown, and unbudgeted |

**Nothing in the Implementation Plan below should be started until Stage 7's
happy path has been observed on an Android emulator.** That is Task 1.

---

## Components

| Component | Action |
|-----------|--------|
| `mobile/android/app/src/main/java/com/dayreel/upload/` | Create — the WorkManager worker, the native module, the package |
| `mobile/android/app/src/main/AndroidManifest.xml` | Modify — **four permissions and a service type it does not have** (**[DECIDE 6]**) |
| `mobile/android/app/build.gradle` | Modify — `androidx.work:work-runtime-ktx`, OkHttp |
| `mobile/android/app/src/main/java/com/dayreel/MainApplication.kt` | Modify — register the package manually; local modules are not autolinked |
| `mobile/src/native/` | Create — the TS spec/binding for the module (**[DECIDE 2]**) |
| `mobile/src/upload/uploader.ts` | Modify — delegate to native on Android, keep the Stage 7 path on iOS (**[DECIDE 1]**) |
| `mobile/src/screens/JobListScreen.tsx` | Modify — progress sourced from `WorkInfo`, not from a JS closure |
| `backend/internal/api/` | **Possibly modify** — a re-presign endpoint (**[DECIDE 5]**) |
| `mobile/ios/` | **No change** — deliberately. iOS background upload is `URLSession`, a different stage |
| `backend/internal/storage/s3.go` | **No change** unless **[DECIDE 5]** takes option (b) |

---

## Boundaries

### Inbound: what JS hands the native module

The handoff happens once, at job creation, and then JS is not required again:

```ts
UploadManager.enqueue({
  jobId: '550e8400-...',
  uploadId: 's3-multipart-id',
  sourcePath: '/data/user/0/com.dayreel/files/uploads/550e8400.mp4',
  partSize: 5242880,
  totalBytes: 20971520,
  parts: [{ partNumber: 1, url: 'http://10.0.2.2:4566/...' }, ...],
  completeUrl: 'http://10.0.2.2:8080/jobs/550e8400-.../complete',
})
```

Two things about this shape are load-bearing:

- **`sourcePath`, not a content URI.** Stage 7 **[DECIDE 3]** uses the picker's
  `copyTo: 'cachesDirectory'` → `fileCopyUri`. The cache directory is
  *reclaimable by Android under storage pressure*, and a SAF content URI is not
  durable across process death without `takePersistableUriPermission`. An upload
  that must survive a kill has to own its bytes: copy into `filesDir`, delete on
  success or terminal failure. This is a change to Stage 7's picker handling,
  small but not optional.
- **`completeUrl` is passed in.** See **[DECIDE 3]** — the native worker calls
  `POST /complete` itself.

### Outbound: `POST /jobs/{id}/complete`, from Kotlin

Unchanged wire format, new caller:

```json
{"upload_id": "s3-multipart-id",
 "parts": [{"part_number": 1, "etag": "\"abc123\""}, ...]}
```

Stage 7 established why this cannot be skipped: it is
`CompleteMultipartUpload` that emits the `s3:ObjectCreated:CompleteMultipartUpload`
event wired to `dayreel-validate` in `init-aws.sh`. **Nothing else starts the
pipeline.** If the app is killed after the last part and JS owns this call, the
clip sits in S3 as an incomplete multipart upload until the user happens to
reopen the app — which is exactly the failure 8A exists to remove.

### The resume state

Per part, durably, outside the process:

```json
{
  "job_id": "550e8400-...",
  "upload_id": "s3-multipart-id",
  "source_path": "/data/user/0/com.dayreel/files/uploads/550e8400.mp4",
  "part_size": 5242880,
  "total_bytes": 20971520,
  "created_at": "2026-08-12T14:02:00Z",
  "expires_at": "2026-08-12T15:02:00Z",
  "parts": [
    {"part_number": 1, "url": "http://10.0.2.2:4566/...", "etag": "\"abc\"", "state": "done"},
    {"part_number": 2, "url": "http://10.0.2.2:4566/...", "etag": null,      "state": "pending"}
  ]
}
```

`expires_at` is `created_at + expires_in` (3600s, `handlers.go:22`). It is in the
record because a resume after expiry is a **different failure** from a network
error and must not be retried into the ground — see **[DECIDE 5]**.

### What is not a boundary here

No backend pipeline change, no S3 layout change, no DynamoDB change. Same four
routes (`router.go:19-22`). The only candidate backend change in the whole stage
is a re-presign endpoint, and **[DECIDE 5]** recommends against it for now.

---

### [DECIDE 1] — Android has never been run, and it is a prerequisite, not a task

**The single largest unknown in this stage, and it is not about WorkManager.**

`mobile/` has been built and run exactly never since commit `5ce5a4e` (Stage
2A/2B). Stage 7 flags this for iOS — *"Whether anything in `mobile/` still
builds… has not been run since"* — and chose iOS partly because Android was
unverified. 8A cannot make the same choice.

What is unverified, concretely:

- Android SDK, emulator image, and `ANDROID_HOME` on this machine
- `react-native run-android` under RN 0.87 with `newArchEnabled=true`
  (`gradle.properties`), Kotlin 2.2.0, compileSdk 37 / targetSdk 36
  (`android/build.gradle:4-8`)
- Whether `react-native-document-picker@9.3.1` works under the New Architecture
  on Android — the same pairing risk Stage 7 flags for `react-native-blob-util`
- Whether Stage 7's uploader works on Android at all, given it will have been
  written and verified against iOS

#### Options

**(a) Verify Stage 7's happy path on Android first, as Task 1.** Restart the API
with `S3_PUBLIC_ENDPOINT=http://10.0.2.2:4566`, point the client config at
`http://10.0.2.2:8080`, and run pick → upload → completed → reel URL with the
**JS** uploader, before a line of Kotlin exists.

**(b) Write the Kotlin module first and debug both at once.** Faster on paper.
In practice a failure could be the emulator, the toolchain, the New
Architecture, the network config, or the module — five candidates for one
symptom.

**(c) Amend Stage 7 to target Android instead of iOS**, making 8A's prerequisite
someone else's problem. Cleanest sequencing, but it reopens a decision Stage 7
made for defensible reasons and invalidates its caption-sync work, which depends
on Safari/AVFoundation in the iOS Simulator (**[DECIDE 7]** there).

#### Recommendation: **(a)**, and treat it as a gate, not a checklist item.

This is the same discipline Stage 7 applies to its own blocker: *"Phase 1 is a
`curl` script… If the PUT from the host does not work, every line of React
Native written in the meantime is written against a hypothesis."* The Android
equivalent is: if the JS uploader does not work on the emulator, every line of
Kotlin is written against a hypothesis.

**Budget it as unbudgeted.** Stage 7 estimates 30 minutes for `pod install` and
calls it *"the first plausible place this stage stalls."* A never-run Android
toolchain is the same risk with less prior exposure.

**[WAIT]** — gated on Stage 7 landing at all, and on **[DECIDE 2]** there being
amended to acknowledge that 8A needs Android.

---

### [DECIDE 2] — TurboModule or legacy bridge module

`gradle.properties` sets `newArchEnabled=true`, and RN 0.87 defaults to the New
Architecture. That makes this a real choice rather than a formality.

**(a) TurboModule with codegen.** A `mobile/src/native/NativeUploadManager.ts`
spec, codegen generating the C++/Java interface, Kotlin implementing the
generated spec.
- The idiomatic path for 0.87, type-checked across the boundary, and what the
  project would need anyway if it ever ships.
- Codegen is another build step that has never run in this repo, and its
  failures are opaque.

**(b) Legacy `ReactContextBaseJavaModule` + `ReactPackage`,** relying on the New
Architecture's interop layer.
- Far more documentation and worked examples; smaller first-time failure surface.
- Interop is a compatibility shim. It works, but it is the path being deprecated.

#### Recommendation: **(b) for the first working version, (a) if time allows.**

Chosen on the same grounds Stage 7 chose the simulator: this stage already has
one large unknown (**[DECIDE 1]**), and adding an unrun codegen pipeline to it
means two unknowns sharing one symptom. The module's surface is three methods —
`enqueue`, `getStatus`, `cancel` — so the migration cost later is small and
local.

**Either way, registration is manual.** `MainApplication.kt` builds its package
list from `PackageList(this).packages`, which is autolinking — and a module
inside the app project is not autolinked. The file already carries the comment
naming the fix:

```kotlin
PackageList(this).packages.apply {
  // Packages that cannot be autolinked yet can be added manually here, for example:
  // add(MyReactNativePackage())
}
```

Easy to miss, and the symptom is `UploadManager` being `undefined` in JS with no
build error.

---

### [DECIDE 3] — who calls `POST /jobs/{id}/complete`

**(a) The Kotlin worker, as its final step.** Upload all parts, collect ETags,
POST them, then report success.
- The only option where an upload started and then abandoned actually reaches
  the pipeline. Kill the app after part 3 of 4, never reopen it: WorkManager
  finishes part 4 and completes the upload, and the reel is waiting.
- The native module now knows about the DayReel API, not just "PUT these bytes."
  A generic uploader becomes a pipeline-aware one.

**(b) JS, on next launch, when it observes all parts done.**
- Keeps the native module a dumb byte-mover with a clean boundary.
- **Defeats the stage's purpose.** The upload completes but the job never
  starts, so "the reel is ready when you next open the app" — the product
  claim — becomes "the pipeline starts when you next open the app."

#### Recommendation: **(a).**

The boundary cost is real and worth paying: the aim is *upload survives app
kill*, and an upload that finishes its bytes but never triggers processing has
not survived anything useful.

Mitigation for the coupling: pass `completeUrl` in as a parameter rather than
building it in Kotlin from a base URL. The module then knows *that* it must POST
a JSON body somewhere on success, not *what DayReel's API looks like*.

---

### [DECIDE 4] — where resume state lives, and the 10 KB `Data` cap

**A concrete constraint that will otherwise be discovered as a crash.**

WorkManager's `Data` is capped at `MAX_DATA_BYTES` = **10,240 bytes**, and
exceeding it throws `IllegalStateException` at enqueue time. A presigned S3 URL
is roughly 400–900 characters. So:

| Clip | Parts @ 5 MiB | Rough URL payload | Fits in `Data`? |
|---|---|---|---|
| 20 MB (10s @ 1080p) | 4 | ~3 KB | Yes |
| 50 MB | 10 | ~8 KB | Marginal |
| 100 MB (4K, or ~40s @ 1080p) | 20 | ~16 KB | **No — throws** |

Stage 7 already flags the exposure: *"Real-device video sizes… If clips are 4K
the part count and upload time both change materially."* This is where that
becomes a hard failure rather than a slow upload.

#### Options

**(a) Persist the job record to a JSON file in `filesDir`; pass only `jobId` in
`Data`.** The worker reads the file on every run, rewrites it after each part
completes.
- No new dependency, trivially inspectable via `adb shell run-as`, and the same
  file is the relaunch reconciliation source.
- Hand-rolled durability: partial writes on a kill mid-write must be handled
  (write to a temp file, then rename — `rename` is atomic on the same
  filesystem).

**(b) Room database.** Proper atomic per-part updates, queryable.
- Correct, and heavier: a schema, a DAO, KAPT/KSP in a project with no
  annotation processing today.

**(c) `SharedPreferences`.** Fine for a handful of small values, wrong for a
per-part array with frequent updates.

#### Recommendation: **(a), with atomic temp-file-then-rename.**

Chosen for the same reason 4A chose a manifest object over a schema change: the
data is small, self-describing, written by one writer, and read at exactly two
moments (worker start, app relaunch). Room's guarantees are real but priced for
a problem this stage does not have.

**Backstop worth recording:** S3 already knows which parts landed —
`ListParts` on the `upload_id` is the authoritative answer, and local state is a
cache of it. If local state is ever lost or judged untrustworthy, a
`ListParts`-backed endpoint is the repair path. Not built here; noted so it is
not re-derived later.

---

### [DECIDE 5] — presigned URLs expire in an hour; kills can last longer

`presignExpiry` is `1 * time.Hour` (`handlers.go:22`) and `expires_in: 3600` is
returned to the client. There is **no endpoint that re-issues URLs for an
existing `upload_id`** — `router.go:19-22` has four routes and none of them do
this.

So a resume more than an hour after job creation gets a 403 on every remaining
part, forever. WorkManager's exponential backoff will retry that 403 politely
until it gives up, which is the worst possible presentation: a job that looks
like it is working and never will.

**(a) Accept the window; treat expiry as terminal.** On 403, or when
`now > expires_at`, stop immediately, mark the job `upload_expired`, surface
"upload expired — tap to restart", and do not retry.

**(b) Add `POST /jobs/{id}/upload-urls`** returning fresh presigned URLs for
parts not yet uploaded. The job already stores `upload.upload_id`
(`models.UploadInfo`, `job.go:58`), and `ListParts` gives the server the
authoritative set of completed parts, so the endpoint needs no request body.
- Makes resume genuinely unbounded, which is what "survives app kill" implies at
  full strength.
- A backend change in a mobile stage, and a fifth route on an API that has held
  at four since 2A.

#### Recommendation: **(a) for this stage, (b) recorded as the follow-up.**

The demo is kill-and-relaunch within seconds; an hour is ample. And (a) is not a
throwaway — the expired state must exist regardless, since (b) can also fail.

**The failure mode to avoid is silent infinite retry**, and that is a
classification bug, not a feature gap: 403-on-expiry must be terminal
(`Result.failure()`), while a 5xx or a socket error is retryable
(`Result.retry()`). This is the same error-classification split 3A called *"the
load-bearing decision"* for workers, arriving now on the client.

---

### [DECIDE 6] — foreground service, and four permissions the manifest lacks

`AndroidManifest.xml:3` declares exactly one permission: `INTERNET`. With
`targetSdkVersion = 36` (`android/build.gradle:6`), a long-running upload needs
more than that.

#### The constraint

A plain `Worker` gets roughly **10 minutes** before the system may stop it. A
20 MB upload over loopback to LocalStack takes seconds — but the whole point of
this stage is weak networks, where the same upload can take much longer. There
are two shapes:

**(a) Foreground service via `setForeground()` / expedited work.** A persistent
notification, no 10-minute ceiling, survives backgrounding cleanly.
- Requires, on targetSdk 36:
  - `FOREGROUND_SERVICE`
  - `FOREGROUND_SERVICE_DATA_SYNC` (API 34+)
  - `POST_NOTIFICATIONS` — **a runtime permission on API 33+**, so it must be
    requested and gracefully refused
  - `ACCESS_NETWORK_STATE` for WorkManager's network constraints
  - `foregroundServiceType="dataSync"` on WorkManager's service entry
- Gives the user a visible, cancellable upload — arguably the honest UX for
  something consuming their data.
- `dataSync` foreground services have had tightening runtime quotas across
  recent Android versions; worth verifying on the emulator image actually used
  rather than trusting a doc page.

**(b) Plain background `Worker`, one part per run.** Each run uploads one part
and returns `Result.retry()` until done, staying far inside any single-run
ceiling.
- No notification, no new permissions beyond `ACCESS_NETWORK_STATE`.
- Progress becomes coarse and system-scheduled: the OS decides when the next run
  happens, which can be minutes. "Resumes automatically" becomes "resumes
  eventually," and the demo becomes hard to show.

#### Recommendation: **(a)**, with the notification permission handled as
optional.

If `POST_NOTIFICATIONS` is denied, fall back to non-expedited work rather than
failing the upload — a denied notification should degrade scheduling, not break
the feature.

**Flag:** this is the first Android permission work in the project, on an
emulator that has never been started. The permissions themselves are a
five-line manifest change; verifying the foreground service actually behaves
across a kill is the part that takes time.

---

## Files

| File | Action | Purpose |
|------|--------|---------|
| `mobile/android/.../upload/UploadWorker.kt` | Create | `CoroutineWorker`: read state, PUT remaining parts, collect ETags, POST complete |
| `mobile/android/.../upload/UploadStateStore.kt` | Create | Atomic JSON persistence in `filesDir` (**[DECIDE 4]**) |
| `mobile/android/.../upload/UploadManagerModule.kt` | Create | `enqueue` / `getStatus` / `cancel` (**[DECIDE 2]**) |
| `mobile/android/.../upload/UploadPackage.kt` | Create | `ReactPackage` registration |
| `mobile/android/.../MainApplication.kt` | Modify | `add(UploadPackage())` — not autolinked |
| `mobile/android/app/src/main/AndroidManifest.xml` | Modify | Four permissions + `foregroundServiceType` (**[DECIDE 6]**) |
| `mobile/android/app/build.gradle` | Modify | `androidx.work:work-runtime-ktx`, OkHttp |
| `mobile/src/native/UploadManager.ts` | Create | Typed binding + platform guard |
| `mobile/src/upload/uploader.ts` | Modify | Native on Android, Stage 7's JS path on iOS |
| `mobile/src/upload/reconcile.ts` | Create | On launch, reconcile local job index against `getStatus` |
| `mobile/src/screens/HomeScreen.tsx` | Modify | Copy into `filesDir`, not `cachesDirectory` |
| `mobile/src/screens/JobListScreen.tsx` | Modify | Progress from `WorkInfo` |
| `mobile/CONTEXT.md` | Modify | Platform asymmetry, stated plainly |
| `docs/stage-plans/stage-7-upload-integration.md` | Modify | Record the Android amendment against **[DECIDE 2]** |

---

## Tasks

1. [ ] **GATE: Stage 7's happy path, on an Android emulator, with the JS
       uploader.** API restarted on `10.0.2.2`. Nothing below starts until this
       is observed (**[DECIDE 1]**)
2. [ ] Manifest permissions + `foregroundServiceType`; WorkManager and OkHttp in
       `build.gradle`; confirm the app still builds
3. [ ] `UploadStateStore.kt` + unit tests — atomic write, partial-write recovery.
       Pure logic, fastest loop in the stage
4. [ ] `UploadWorker.kt` — part loop, ETag capture, `POST /complete`, error
       classification (403/expiry terminal, 5xx retryable)
5. [ ] `UploadManagerModule.kt` + `UploadPackage.kt`; register in
       `MainApplication.kt`
6. [ ] `mobile/src/native/UploadManager.ts`; verify the module is defined at all
       before building anything on it
7. [ ] Switch the picker to `filesDir`; wire `uploader.ts` to native on Android
8. [ ] `reconcile.ts` — relaunch reconciliation against the local job index
9. [ ] Progress from `WorkInfo` in `JobListScreen`
10. [ ] **The kill test**, and every failure path in Verification
11. [ ] `mobile/CONTEXT.md`; amend Stage 7's **[DECIDE 2]**; record results here

---

## Test

```bash
# Point everything at the emulator's host alias. NOTE: this breaks Stage 7's
# iOS path for as long as it is set — one signed host per API process.
cd infra
S3_PUBLIC_ENDPOINT=http://10.0.2.2:4566 docker compose up -d --build api

# Gate: the Stage 7 path, on Android, before any Kotlin exists
cd ../mobile && npx react-native run-android
# pick -> upload -> completed -> reel URL. If this fails, stop.

# --- The kill test: the stage in one command sequence ---
# 1. Pick a clip >15 MB so it is genuinely multipart (4+ parts at 5 MiB)
# 2. Start the upload, wait for ~2 parts
adb shell am force-stop com.dayreel        # not "back out" — force stop

# 3. Upload continues with the app dead
adb logcat -s UploadWorker:V | grep -E "part [0-9]+ (uploaded|failed)"

# 4. State is on disk, not in a closure
adb shell run-as com.dayreel cat files/uploads/<job_id>.json

# 5. Reopen; the job is further along, not restarted
adb shell am start -n com.dayreel/.MainActivity

# 6. It reached the pipeline without JS ever running again
curl -s localhost:8080/jobs/<job_id> | jq '{status, stages}'

# --- Airplane mode: the network case ---
adb shell cmd connectivity airplane-mode enable
# ... expect retry with backoff, no failure, no busy loop ...
adb shell cmd connectivity airplane-mode disable
# ... expect resume from the next unsent part ...
```

---

## Verification

_Nothing checked off until observed on a real emulator._

**The gate**

- [ ] App builds and launches on an Android emulator under RN 0.87 + New Arch
- [ ] Stage 7's JS upload path completes end to end on Android, before any Kotlin
- [ ] `POST /jobs` returns URLs whose host is `10.0.2.2:4566`, and a PUT to one
      from the emulator returns 200

**Survives the kill — the stage itself**

- [ ] A >15 MB clip uploads as **4+ parts**, not one (a single-part bug is
      invisible on a small clip — Stage 7 makes the same point for the same reason)
- [ ] `adb shell am force-stop` mid-upload does **not** stop the upload
- [ ] Parts already uploaded are **not** re-uploaded after the kill — verified by
      part numbers in logcat, not by the upload merely finishing
- [ ] The ETag of a part uploaded *before* the kill appears in the
      `POST /complete` body sent *after* it
- [ ] `POST /complete` is sent by the worker with the app never reopened, and the
      pipeline runs to `completed` on its own
- [ ] Relaunching mid-upload shows correct live progress, not a reset bar and not
      a duplicate upload
- [ ] The state file survives a kill *during* a write (temp-then-rename holds)
- [ ] Device reboot mid-upload: WorkManager reschedules and the upload resumes

**Failure paths**

- [ ] **Airplane mode mid-upload:** retries with backoff, no failure, no busy loop
- [ ] **Network restored:** resumes from the next unsent part
- [ ] **Expired presign** (shorten `presignExpiry`, or wait): terminal
      `upload_expired` state with a restart affordance — **not** an infinite
      retry, the specific failure **[DECIDE 5]** exists to prevent
- [ ] **Cancel:** work is cancelled, the source file is deleted, S3 has no
      dangling multipart upload (or one that is explicitly abandoned)
- [ ] **`POST_NOTIFICATIONS` denied:** upload still works, degraded to
      non-expedited (**[DECIDE 6]**)
- [ ] **Duplicate enqueue** for one job does not run two workers —
      `enqueueUniqueWork` with `ExistingWorkPolicy.KEEP`, verified by trying it
- [ ] **LocalStack restarted mid-upload:** per Stage 7's inherited note,
      `PERSISTENCE=1` is silently ignored, so buckets and the multipart upload
      vanish. Expect a clean terminal failure, not a retry loop against an
      upload ID that no longer exists

**Hygiene**

- [ ] Source copies in `filesDir` are deleted on success **and** on terminal
      failure — an upload directory that only grows is a storage leak on a real
      device
- [ ] No `WakeLock` held after work completes
- [ ] iOS still builds. It will not have background upload, and that is expected

---

## Claude Code Implementation Plan

### Recommended Approach: prove the platform, then the persistence, then the worker

Same shape as Stage 7's, one layer down. Stage 7's uncertain element was whether
a signature holds; here it is whether Android runs at all. Both are cheap to
test and expensive to assume.

Order the work by feedback speed:

1. **The emulator** — slowest loop, largest unknown, blocks everything (Task 1)
2. **`UploadStateStore`** — pure Kotlin, unit-testable, no emulator needed
3. **The worker** — needs the emulator but is testable via `adb` without any JS
4. **The bridge and the JS** — fastest to change, least uncertain

### Pre-Flight Check

```
0a. Stage 7 merged, and its [DECIDE 1] and [DECIDE 3] resolved. If either took a
    fallback, STOP and re-plan — the stage forks
0b. echo $ANDROID_HOME; sdkmanager --list | head    # does the SDK exist at all
0c. emulator -list-avds                            # is there an AVD
0d. adb devices                                     # emulator boots
0e. docker system df                                # the 8 GB ceiling, as in 6A/7
0f. curl -s localhost:8080/health
0g. From the emulator: reach http://10.0.2.2:8080/health. If this fails,
    nothing else matters
0h. Confirm the API was restarted with S3_PUBLIC_ENDPOINT=http://10.0.2.2:4566,
    and note that iOS is now broken until it is changed back
```

### Execution Steps

```
Phase 1: The platform gate            <-- DO NOT SKIP AHEAD
1. Emulator boots, app builds, Stage 7's JS upload completes on Android
2. COMMIT nothing; this phase produces knowledge, not code

Phase 2: Manifest + deps
3. Permissions, foregroundServiceType, work-runtime-ktx, OkHttp
4. Rebuild; confirm the app still launches. COMMIT.

Phase 3: Persistence            <-- fast loop, no emulator
5a. UploadStateStore.kt
5b. UploadStateStoreTest.kt      (atomic write, partial-write recovery)
6.  ./gradlew test. COMMIT.

Phase 4: The worker
7.  UploadWorker.kt — part loop, ETags, complete, error classification
8.  Drive it from an instrumented test or a debug entry point, via adb,
    with no JS in the loop at all
9.  COMMIT.

Phase 5: The bridge
10a. UploadManagerModule.kt
10b. UploadPackage.kt
11.  MainApplication.kt registration  <-- the easy-to-miss one
12.  Verify UploadManager is defined in JS before building on it. COMMIT.

Phase 6: JS integration (parallel writes)
13a. src/native/UploadManager.ts
13b. src/upload/uploader.ts        (platform branch)
13c. src/upload/reconcile.ts
13d. HomeScreen (filesDir), JobListScreen (WorkInfo progress)

Phase 7: The kill test and every failure path
14. force-stop mid-upload; reboot; airplane mode; expiry; cancel
15. CONTEXT.md; amend Stage 7's DECIDE 2; record results here
```

### Parallel Opportunities

| Phase | Parallel Files |
|-------|----------------|
| 3 | `UploadStateStore.kt`, its test |
| 5 | `UploadManagerModule.kt`, `UploadPackage.kt` |
| 6 | `UploadManager.ts`, `uploader.ts`, `reconcile.ts`, both screens |

Phases 1–4 are strictly sequential. As in Stage 7, the parallelism is all in the
cheap parts.

### Subagents

The pattern that has paid off across 4A–7 is delegating **empirical research
with a slow, verbose feedback loop** — never authoring.

- **Worth an agent:** the Android toolchain check (0b–0d) and first
  `run-android` build. Slow, extremely verbose, long tail of failures unrelated
  to this plan. Exactly Stage 7's argument for delegating `pod install`.
- **Worth an agent:** whether `react-native-document-picker@9.3.1` and RN 0.87's
  New Architecture actually work together on Android. Yes/no question, expensive
  answer. **Require exact commands and exact output, not conclusions.**
- **Not worth an agent:** the Kotlin worker, the state store, the bridge, the JS.
  Small, and they need to be written by whoever read the Stage 7 uploader.

### Potential Blockers

| Blocker | Resolution |
|---|---|
| **No Android SDK or emulator on this machine** | Stage-ending. Would force **[DECIDE 1](c)** — amend Stage 7 to Android — or defer 8A entirely |
| **Stage 7 took [DECIDE 1](b), the API upload proxy** | Re-plan. The Kotlin worker PUTs to the API and most of ETag persistence dissolves |
| **Stage 7 took [DECIDE 3](b), single-part XHR** | Re-plan. Per-part resume is the premise; it would have to be restored on the backend first |
| **`Data` exceeds 10 KB on a large clip** | **[DECIDE 4]** — this is exactly why state lives in a file and only `jobId` goes in `Data` |
| **Module `undefined` in JS** | Manual registration in `MainApplication.kt`. No build error, easy to miss |
| **Foreground service killed by the emulator's battery policy** | `adb shell dumpsys deviceidle`; test on a non-restricted profile before concluding the code is wrong |
| **Doze / App Standby delays work in a background emulator** | Expedited work; verify with `adb shell cmd deviceidle force-idle` rather than assuming |
| **New Arch + document picker incompatible on Android** | Falls back to a different picker, or `Intent.ACTION_GET_CONTENT` natively — a real cost, flagged early because it lands in phase 1 |
| **Docker 8 GB ceiling** | Only the `api` image rebuilds; lighter than 6A. Prune first anyway |

### Time Estimate

- Phase 1 (Android gate): **30 min to 2+ hours, entirely unknown**
- Phase 2 (manifest, deps): ~15 min
- Phase 3 (state store + tests): ~25 min
- Phase 4 (worker): ~45 min
- Phase 5 (bridge): ~25 min
- Phase 6 (JS integration): ~30 min
- Phase 7 (kill test, failure paths): ~45 min
- **Total: ~3½ hours if the toolchain cooperates**

`PROJECT_PLAN.md` budgets 30 minutes for this stage. That estimate assumed a
verified Android toolchain, an existing uploader to extend, and no permission
work. None of those hold. The estimate is not wrong so much as written before
the shape was known — the same way 6A's 20 minutes became 40.

---

## Notes

### What this stage closes

**The gap Stage 7 explicitly leaves open.** Stage 7's verification includes
*"App backgrounded mid-upload: record what happens. Expected to fail; this is
the exact gap 8A exists to close"* and *"Resume after that drop is NOT expected
to work. Confirm it fails cleanly and record what 8A has to build."* This stage
is the answer to a question Stage 7 writes down rather than solves — a cleaner
handoff than the project has managed between most stages.

It also closes the **product claim**. The repo description leads with resuming
from the exact byte after an app kill. Until this stage, that sentence describes
an intention.

### Risks and inherited tensions

- **Platform asymmetry, introduced deliberately.** After 8A, Android has
  background upload and iOS does not. iOS would need `URLSession` background
  transfer — a comparable amount of work in a different language against a
  different scheduler. `PROJECT_PLAN.md` scopes 8A to Kotlin/WorkManager only,
  so this is scoped, not accidental. `mobile/CONTEXT.md` must say so plainly or
  the next person will read the gap as a bug.
- **One signed host per API process** (Stage 7 **[DECIDE 1]**). Working on 8A
  breaks the iOS path for the duration. Not fixable here; worth a line in the
  README so it is not rediscovered.
- **The native module knows about the API** (**[DECIDE 3]**). A deliberate
  boundary compromise, mitigated by passing `completeUrl` rather than
  constructing it.
- **First permission work, first native module, first emulator run — all in one
  stage.** Any one of them is a normal afternoon. Together they are why the
  estimate is 3½ hours against a planned 30 minutes.
- **Inherited from 1B:** `PERSISTENCE=1` is silently ignored in LocalStack
  Community. A restart wipes the multipart upload out from under a resuming
  worker. It must fail terminally, not retry forever.
- **Inherited from 5A:** `MOCK_TRANSCRIBE=true` is still the default, so any
  reel produced here has synthetic captions. Irrelevant to upload correctness,
  worth knowing before anyone reads the output and worries.

### Deliberately not in scope

- **iOS background upload.** A different mechanism entirely; if wanted, it is
  its own stage.
- **Camera capture.** The picker remains the only input, as in Stage 7.
- **Upload queue across multiple clips.** One clip at a time. `enqueueUniqueWork`
  keyed by `job_id` makes the multi-clip case a scheduling change later rather
  than a redesign.
- **Re-presigning expired URLs** — **[DECIDE 5](b)**, recorded as the follow-up.
- **Auth.** Still none, anywhere.
- **In-app playback** — Stage 8B.

### Uncertain, flagged rather than smoothed over

- **Whether an Android SDK and emulator exist on this machine at all.** Every
  estimate here is conditional on it, and nothing in the repo answers it.
- **Whether `react-native-document-picker@9.3.1` works under RN 0.87's New
  Architecture on Android.** Unverified pairing, same class of risk Stage 7
  flags for `react-native-blob-util`.
- **Whether a `dataSync` foreground service survives aggressive OEM battery
  policy.** It survives on an emulator; that is weaker evidence than it looks,
  and this project has now been caught twice by "it passed locally" (presigned
  URLs, bucket policies).
- **Whether WorkManager's guarantees hold across `force-stop` specifically.**
  `force-stop` is harsher than a swipe-away — on some Android versions it
  cancels pending work until the app is next launched. **If that is what
  happens, the stage's headline verification needs restating, not
  hand-waving:** the honest claim would become "survives being swiped away and
  resumes on relaunch," which is weaker than the README currently promises.
  **This is the single most important thing to establish empirically**, and it
  should be established in phase 1, not discovered in phase 7.
- **Real clip sizes off an actual phone.** Stage 7 flags this; it matters more
  here, because part count drives both the `Data` cap in **[DECIDE 4]** and how
  long the foreground service must live.
