# Stage 7: Upload Integration

> Status: **VERIFIED end to end on an Android emulator, 2026-08-13.** The stage's
> observable outcome — *"Mobile uploads work, jobs process"* — is demonstrated,
> not argued. Evidence in the Verification section below.
>
> Decisions 1–7 settled 2026-08-12; **[DECIDE 8]** added 2026-08-13 while
> planning 8A. One went against the recommendation and is worth reading before
> the code: the Redis cache is deliberately left unfixed (**[DECIDE 4]**).
> Playback stays in **8B** (**[DECIDE 6]**, re-settled 2026-08-13 — see the
> correction note there). **The client target is the Android Emulator**
> (**[DECIDE 2]**, also re-settled 2026-08-13 on verified toolchain evidence —
> see the correction note there; it was previously iOS Simulator).
>
> **Scope of that verification, stated so it is not read as more than it is.**
> Everything below ran against **LocalStack**, on **one** emulator, with
> `MOCK_TRANSCRIBE=true`. **Nothing has ever been provisioned on real AWS** — no
> bucket, no queue, no table — so every real-AWS claim in this document remains
> an assumption. The failure-path matrix is **not** fully worked through; the
> unchecked boxes below are unchecked because they were not run, not because
> they failed.

> **Substrate superseded — re-read the open decisions against this.** The plan
> assumes Docker Compose, LocalStack, SQS and Redis. All four are gone: S3 and
> DynamoDB are real AWS, the queue is a local SQLite file
> (`stage-3b-local-queue.md`), and the status cache is in-process. See
> `infra/CONTEXT.md`.
>
> This matters more here than in the worker stages, because several open items
> are *about* the emulator. Presigned-URL host binding — SigV4 covering the
> `Host` header, the emulator's in-cluster hostname being unreachable from a
> device, `S3_PUBLIC_ENDPOINT=http://10.0.2.2:4566` for the Android emulator —
> is no longer a problem to solve: real S3 signs and serves on the same public
> endpoint from every client. The multipart 5 MiB floor, ETag persistence and
> resume semantics are unaffected and remain the substance of the stage.

## Aim

Make the React Native app upload a real video to the real backend, watch the
real job move through the real pipeline, and end holding a real `hls_url` —
with **no step run from inside a container.**

**That last clause is the whole stage.** Every end-to-end test this project has
run so far was executed from inside the Docker network, because the presigned
upload URLs the API hands out are only reachable from inside it. A mobile client
is outside by definition. This is the first stage where that stops being a
footnote and starts being the work.

It is also the sync point `PROJECT_PLAN.md` names: Track A ran to 6A, Track B
stopped at 2B, and the two have never met. The mobile app has never made a
single HTTP request to the API. `getMockJobs()` is still what the job list
renders.

## Components

| Component | Action |
|-----------|--------|
| `backend/internal/storage/s3.go` | Modify — presign against the **public** endpoint (**[DECIDE 1]**) |
| `backend/internal/config/config.go` | Modify — `PublicEndpoint()` already exists (6A), but `UPLOAD_PART_SIZE` does not (**[DECIDE 8]**) |
| `backend/internal/api/handlers.go` | Modify — `partSize` is the hardcoded `5 * 1024 * 1024` on line 21 (**[DECIDE 8]**) |
| `infra/docker-compose.yml` | Modify — `S3_PUBLIC_ENDPOINT` on the **api** service; it is currently only on `worker-package` |
| `backend/internal/cache/redis.go` | Modify — the 10s job cache is a floor under polling (**[DECIDE 4]**) |
| `mobile/src/api/client.ts` | Modify — real base URL, drop the mock data |
| `mobile/src/types/api.ts` | Modify — **the types do not match the Go models** (**[DECIDE 5]**) |
| `mobile/src/upload/` | Create — the multipart uploader (**[DECIDE 3]**) |
| `mobile/src/storage/` | Create — a local job index; there is no `GET /jobs` (**[DECIDE 5]**) |
| `mobile/src/hooks/` | Create — job polling |
| `mobile/src/screens/HomeScreen.tsx` | Modify — the `// TODO: Stage 7` is literally in the file |
| `mobile/src/screens/JobListScreen.tsx` | Modify — real jobs, real progress |
| `mobile/src/screens/PlayerScreen.tsx` | Modify — show the real reel URL, **not** play it (**[DECIDE 6]**) |
| `mobile/android/` | Build only — SDK/NDK now installed and the build is green (**[DECIDE 2]**; the "never installed" note there is corrected) |
| `backend/internal/media/` | **Deliberately not touched** — the caption fix is decided here, applied later (**[DECIDE 7]**) |

## Boundaries

### The API, exactly as it is today

Read from `backend/internal/api/handlers.go` and `router.go`. Four routes, plus
`/health`. There is **no** `GET /jobs`, no SSE, no WebSocket, no push.

**`POST /jobs`** — `handlers.go:89`

```json
{"filename": "clip.mp4", "size_bytes": 20971520, "content_type": "video/mp4"}
```

All three fields are required and validated: `filename == "" || size_bytes <= 0
|| content_type == ""` is a 400. This matters — the picker types `size` as
`number | null` and `type` as `string | null`, so the client must supply
fallbacks or it will send a request the API rejects.

> **Correction, 2026-08-13.** This paragraph named
> **`react-native-document-picker`**, which is no longer the picker.
> **It does not compile against RN 0.87** — it extends `GuardedResultAsyncTask`,
> which RN 0.87 removed — and it is deprecated with no further versions, so the
> Android build could not be made to succeed with it at all. Replaced by
> `@react-native-documents/picker`. The nullable-field hazard above survives the
> swap unchanged; a second one does not: **`copyTo` is gone**, replaced by a
> separate `keepLocalCopy()` which **resolves on failure rather than throwing**,
> reporting the failure in a `status` field. The `copyTo` references elsewhere in
> this plan predate the swap.

```json
{
  "job_id": "uuid",
  "upload_id": "s3-multipart-id",
  "upload_urls": [{"part_number": 1, "url": "http://localstack:4566/..."}],
  "part_size": 5242880,
  "expires_in": 3600
}
```

`part_size` is a hard-coded 5 MiB (`handlers.go:21`) and `numParts =
ceil(size_bytes / part_size)`. **A multipart upload is created unconditionally**,
even for a one-part file. There is no single-PUT path.

The `url` field is the blocker. See **[DECIDE 1]**.

**`POST /jobs/{id}/complete`** — `{"upload_id": "...", "parts": [{"part_number":
1, "etag": "\"abc\""}]}`. This is **not optional bookkeeping**: it calls
`CompleteMultipartUpload`, which is what emits the
`s3:ObjectCreated:CompleteMultipartUpload` event that `init-aws.sh` wires to
`dayreel-validate`. If the client skips it, the parts sit in S3 as an incomplete
upload and **the pipeline never starts**. Nothing else in the system triggers it.

**`GET /jobs/{id}`** — the full `models.Job`. Served from Redis when warm.

**`GET /jobs/{id}/reel`** — `{job_id, hls_url, thumbnail_url}`, or 409
`NOT_READY`. Returns 200 as of 6A, for the first time in the project's life.
Note it reads DynamoDB directly and **does not** consult the cache, unlike
`GET /jobs/{id}` — so the two endpoints can disagree for up to 10 seconds.

### The upload boundary — the one thing that has never worked

```
mobile app  ──PUT part──▶  ???  ──▶  S3 (LocalStack)
             the URL says http://localstack:4566/...
```

`localstack` is a Docker Compose service name. It resolves inside
`dayreel-network` and nowhere else. The URL cannot be string-replaced with
`localhost` on the client, because SigV4 signs the `host` header — rewriting the
host invalidates the signature. This was written down in
`stage-3a-validate-worker.md` as a **Stage 7 blocker**, restated in 4A, and
restated again in 6A's blockers table. It is now due.

### What is *not* a boundary in this stage

No new S3 objects, no new SQS messages, no DynamoDB schema change. The pipeline
is finished; Stage 7 only changes who talks to it.

---

### [DECIDE 1] — how a client outside Docker gets a usable presigned URL

**RESOLVED: (a), sign against the public endpoint.** Chosen partly because it is
a no-op on real AWS, so it constrains a later deploy least.

**This is the stage.** Everything else is a React Native app talking to an HTTP
API, which is ordinary work. This is not.

#### The mechanism, precisely

`storage.NewS3Client` (`s3.go:32`) builds one `s3.Client` with
`o.BaseEndpoint = cfg.AWSEndpoint` and derives the presign client from it
(`s3.go:56-57`). `cfg.AWSEndpoint` comes from `LOCALSTACK_ENDPOINT`, which
compose sets to `http://localstack:4566` for the `api` service. So the presigner
signs `host: localstack:4566` and returns a URL nobody outside the compose
network can use.

6A already built half the answer: `S3_PUBLIC_ENDPOINT` and `Config.PublicEndpoint()`
(`config.go`), used for HLS URLs. But that setting is only *displayed* — it is
string-formatted into `hls_url`, never signed against. Compose sets it on
`worker-package` **only**; the `api` service does not have it at all.

The distinction is the crux: **for HLS the endpoint only has to be printed
correctly; for an upload it has to be signed correctly.** Reusing the setting is
right. Reusing it the same way is not.

#### Options

**(a) Sign against the public endpoint.** Override `BaseEndpoint` on the presign
call only, leaving the API's own S3 calls on the internal endpoint:

```go
s.presignClient.PresignUploadPart(ctx, input,
    s3.WithPresignExpires(expiry),
    s3.WithPresignClientFromClientOptions(func(o *s3.Options) {
        o.BaseEndpoint = aws.String(publicEndpoint)
        o.UsePathStyle = true
    }),
)
```

`WithPresignClientFromClientOptions` exists in the pinned SDK
(`aws-sdk-go-v2/service/s3 v1.107.1`, `api_client.go:1159`) — verified, not
assumed. The signature then covers `host: 10.0.2.2:4566` (the committed value —
see **[DECIDE 2]**), which is exactly what the client sends, and LocalStack sees
that Host header intact through Docker's published port.

- Signature is valid by construction, because we sign what the client will send.
- Costs one env var on the `api` service and a few lines in `s3.go`.
- **On real AWS it disappears.** With `S3_PUBLIC_ENDPOINT` and
  `LOCALSTACK_ENDPOINT` both unset, `PublicEndpoint()` returns `""`,
  `BaseEndpoint` is never set, and the SDK signs the genuine S3 endpoint. This
  option constrains a later deploy **less** than any other here.
- The sharp edge: **the signed host is fixed per API process.** One value serves
  one client environment. An iOS simulator wants `localhost:4566`; an Android
  emulator wants `10.0.2.2:4566`; a physical device wants the LAN IP. They cannot
  be served simultaneously without restarting the API. That is a real
  constraint, and it is the reason **[DECIDE 2]** has to pick a target rather
  than hedge.

**(b) Proxy the upload through the API.** `PUT /jobs/{id}/parts/{n}` streaming
into S3 server-side. No signature problem at all, no CORS, works from any
network, one endpoint the client always trusts.
- Directly contradicts `stage-2a-go-api.md`: *"No video bytes through API.
  Clients upload directly to S3 via presigned URLs."* — a principle the API was
  designed around and has held for five stages.
- Puts a 20 MB body through a Go handler, and on real AWS through Fargate
  bandwidth and a much longer-lived request.
- Kept as the **documented fallback**: if (a) turns out to fail against real S3
  for a reason LocalStack hides, this is what it falls back to.

**(c) Sign against the host's LAN IP** (`http://192.168.x.x:4566`). The only
option that a physical device, an Android emulator and an iOS simulator can all
reach with the same value.
- Fragile: DHCP reassignment silently breaks every issued URL, and the value has
  to be discovered and injected at compose-up.
- **iOS App Transport Security is an open question here.** The app currently has
  `NSAllowsArbitraryLoads: false` with `NSAllowsLocalNetworking: true`
  (`mobile/ios/DayReel/Info.plist`). Apple documents that flag as covering
  unqualified hostnames and `.local` domains; it does **not** name RFC1918
  literals. Whether cleartext to `192.168.x.x` is permitted under that config is
  **unverified and must be tested empirically before this option is costed** —
  see the pre-flight check.
- This is a mechanically identical variant of (a) — same code, different value.
  It is a **deployment choice, not a design choice**, which is why it is folded
  into **[DECIDE 2]** rather than competing with (a).

**(d) An ngrok / cloudflared tunnel.** Gives a real HTTPS hostname reachable from
a device on any network, sidestepping ATS entirely.
- A third-party dependency in the upload path, and an ephemeral hostname: each
  new tunnel changes the host, which must be re-signed, which means restarting
  the API every session.
- Recorded so it is not re-proposed. Not worth it for a project with a handful of
  runs left.

**(e) Scope Stage 7 to the simulator only.** Not really an endpoint strategy —
it is (a) with `localhost`, plus a decision to stop there.

#### Recommendation: **(a), with `S3_PUBLIC_ENDPOINT` added to the `api` service.**

Chosen over (b) because it preserves the "no video bytes through the API"
principle that 2A set deliberately, and over (c)/(d) because those are values to
put *into* (a), not alternatives to it.

The important property is that (a) is the only option that is **less** work on
real AWS than locally. (b) grows a proxy endpoint that would then need to be
maintained or removed; (c) and (d) are local-only scaffolding. (a) is a config
override that evaluates to a no-op the moment the endpoint is genuinely public.

Two consequences to carry:

1. **The API and S3 hosts are configured independently and must agree about who
   the client is.** `mobile/src/api/client.ts` currently hard-codes
   `http://localhost:8080` for both `__DEV__` and production. Whatever host the
   client uses for the API, the *same* host family has to be what
   `S3_PUBLIC_ENDPOINT` signs. A single `mobile/src/config/index.ts` should be
   the one place either is written.
2. **LocalStack may not validate presigned signatures at all.** LocalStack has a
   `S3_SKIP_SIGNATURE_VALIDATION` setting which, as far as I can determine,
   defaults to skipping. If so, a locally successful upload is **not evidence the
   signature is correct** — the same trap 6A flagged for bucket policies. The
   verification run should set `S3_SKIP_SIGNATURE_VALIDATION=0` on the localstack
   service so a 200 means something. **Confirm the default before trusting the
   result either way.**

---

### [DECIDE 2] — which client is the target: simulator, emulator, or device

**RESOLVED: Android Emulator.** Device is not committed. iOS is not committed.

> **Correction, 2026-08-13.** This decision was first recorded here as
> "RESOLVED: iOS Simulator," and the stated reason was that the simulator was
> *"the one with a verified toolchain"* — a claim repeated in the *Deliberately
> not in scope* section as the justification for excluding Android. **That claim
> was false, and nothing had verified it.** Checked on this machine today:
>
> - Xcode 16.1 is installed, but `xcrun simctl list runtimes` returns **zero
>   runtimes** and `xcrun simctl list devices available` returns **zero
>   devices**. There is no simulator to run.
> - `pod --version` reports `command not found`. CocoaPods was never installed,
>   so the `pod install` this plan budgets for could not have run either.
> - Android is equally bare: no `~/Library/Android/sdk`, no Android Studio, no
>   `adb`, no `emulator` binary, `ANDROID_HOME` unset.
>
> **Neither toolchain was ever verified.** The original decision rested on a
> comparison that did not exist. With both sides at zero the platform choice was
> free, and on a free choice Android wins on a reason that is actually written
> down: `PROJECT_PLAN.md`'s **Stage 8A specifies a Kotlin WorkManager native
> module**, and 8B specifies ExoPlayer. Committing to iOS here would have meant
> standing up one toolchain for Stage 7 and the other for Stage 8A.
>
> Caught before any client code depended on it. Re-settled on that basis.

Follows directly from **[DECIDE 1]**: the signed host is fixed per API process,
so this has to be answered, not hedged.

| Target | API host | `S3_PUBLIC_ENDPOINT` | Works today? |
|---|---|---|---|
| **Android Emulator** | `http://10.0.2.2:8080` | `http://10.0.2.2:4566` | **YES — verified 2026-08-13.** Was "not yet, blocked on disk space" when written; the SDK, NDK and system image were installed the same day and a real 14.9 MB upload ran end to end. See `docs/SETUP.md` |
| iOS Simulator | `http://localhost:8080` | `http://localhost:4566` | **No.** Zero simulator runtimes, zero devices, no CocoaPods. Would need the whole toolchain installed first |
| Physical iPhone | `http://<lan-ip>:8080` | `http://<lan-ip>:4566` | **Unknown** — gated on the ATS question in **[DECIDE 1](c)**, and moot while iOS is uncommitted |

#### Resolution: **Android Emulator as the committed target.**

Chosen because with both toolchains at zero the cost of standing one up is the
same either way, and Android is the one the rest of the plan already needs:
Stage 8A is a Kotlin WorkManager module and Stage 8B is ExoPlayer. Picking iOS
would front-load a toolchain the project abandons one stage later.

Java 21 is installed, which is Android's one satisfied prerequisite. Everything
else — SDK platform 37/36, build-tools 37.0.0, NDK 27.1.12297006, an emulator
system image — has to be installed, and **cannot be until host disk is cleared;
there is 2.8 GB free.** That is now the first plausible place this stage stalls,
replacing `pod install` in that role. `mobile/android/` pins Gradle 9.4.1,
Kotlin 2.2.0, `minSdk` 24 and `newArchEnabled=true`.

> **Outcome, 2026-08-13.** The install happened and the toolchain works. Disk was
> reclaimed, the full set including the NDK was installed (8.8 GiB), and
> `:app:assembleDebug` produced a ~147 MiB APK that runs on an AVD named
> `dayreel-avd`. Three things this paragraph could not have known, recorded in
> `docs/SETUP.md` in full:
>
> - **The SDK platform is `android-37.0`, not `android-37`.** Google moved to
>   minor-versioned platforms and there is no plain `android-37` to install.
> - **Node had to be upgraded.** RN 0.87 requires `^22.13.0 || ^24.3.0 || >= 26`;
>   this machine had v20.11.0. Node 22.23.2 via nvm, pinned in `mobile/.nvmrc`.
>   `nvm use` fails here because `~/.npmrc` sets a `prefix` —
>   `nvm use --delete-prefix 22` is the way through.
> - **The build did not merely stall, it failed.** `react-native-document-picker`
>   does not compile against RN 0.87 and had to be replaced. The toolchain was
>   not the only thing standing between this repo and a running app.
>
> The disk figure predicted the right *risk* and the wrong *stall*: disk was
> never what stopped the build.

Two consequences to carry:

1. **`scripts/verify-presign.sh` can no longer run from the host unmodified.**
   It exercises the presigned-URL matrix from the host, and URLs signed for
   `10.0.2.2` are not host-routable. Documented in `docs/SETUP.md` with both
   workarounds (flip `api` back to `localhost` for the run, or alias `10.0.2.2`
   onto `lo0`). The script is not changed.
2. **The caption oracle in [DECIDE 7] changes engine**, from AVFoundation to
   ExoPlayer. See the note there — it is not a like-for-like substitution.

The device path still costs one env var and one compose restart, so it stays
cheap to attempt — but only **after** the emulator path is green, so a failure
there is unambiguously a networking failure and not an upload bug.

---

### [DECIDE 3] — upload mechanics on the client

**RESOLVED: (a), `react-native-blob-util`.** One native dependency serving both
the part slicing and the job-index persistence in **[DECIDE 5]**.

#### The constraint

The API always issues a multipart upload with 5 MiB parts. Ten seconds of phone
1080p is roughly 20 MB, so **the ordinary case is 4-5 parts, not one.** A design
that only handles a single part will work against a synthetic test clip and fail
against the first real recording — the worst possible failure ordering.

The client must therefore: slice the file, PUT each part to its own URL, capture
each response's `ETag` header, and post all of them back. React Native's `fetch`
cannot do this — it has no way to send a byte range of a file on disk.

#### Options

**(a) `react-native-blob-util`.** `fs.slice(src, dst, start, end)` writes each
part to a temp file; `fetch('PUT', url, RNFetchBlob.wrap(partPath))` streams it
natively with an `uploadProgress` callback; `res.info().headers.ETag` gives the
ETag back.
- One new native dependency, which means an Android rebuild through Gradle.
- Temp slices are real files on a real device and **must be deleted in a
  `finally`**, or a few uploads fill the cache directory.
- The same library's `fs.writeFile` also solves **[DECIDE 5]**'s persistence
  problem, so this is **one** native dependency, not two.

**(b) Zero-dependency `XMLHttpRequest`.** RN's networking layer accepts
`{uri, type, name}` as a request body and streams the file natively;
`upload.onprogress` gives progress and `getResponseHeader('ETag')` gives the tag.
- No new dependency at all, which on an app with no pods installed is worth
  something.
- **Cannot slice.** It only works when `numParts == 1`, which requires making the
  API's `part_size` the whole file size — a backend change that forfeits per-part
  retry and pushes the entire file into one unretryable request.

**(c) Read chunks into JS and `fetch` them.** Any filesystem module can read a
byte range; base64 inflates it 33% and drags every megabyte across the bridge. A
20 MB clip becomes ~27 MB of JavaScript string. Recorded to be rejected.

#### Recommendation: **(a), with per-part retry and no cross-launch resume.**

- **Retry:** per part, 3 attempts, backoff 1s/2s/4s. Parts are independent, so a
  retry re-PUTs only the failed part and keeps the ETags already collected. This
  is the same shape as the SQS receive backoff `config/free-tier.md` mandates,
  for the same reason.
- **Progress:** `(bytes in completed parts + current part's uploaded bytes) /
  size_bytes`. Reported at most ~10×/sec into a single state value.
- **No resume across app kill, deliberately.** `PROJECT_PLAN.md` gives that its
  own stage: *"Stage 8A: Background Upload — **Aim:** Upload survives app kill."*
  Stage 7 doing it would be building 8A. What Stage 7 *should* do is leave the
  ETag array in a shape 8A can persist, rather than hiding it in a closure.
- **Expiry:** `expires_in` is 3600. A 20 MB upload over loopback takes under a
  second, so expiry is not a practical risk here — but the client should surface
  a 403 as "upload expired, start over" rather than as an opaque failure.

Also required, and easy to miss: `HomeScreen.tsx` currently calls
`pick({type: ['video/*']})` and uses `result.uri`. On Android that URI is a
`content://` document reference backed by a provider, not a filesystem path, and
a native uploader that expects a real file cannot open it. The picker supports
`copyTo: 'cachesDirectory'`, which materialises a real file and populates
`fileCopyUri` — **use that path**, and check `copyError`. (The same fix applies
on iOS, where the URI is security-scoped instead.)

---

### [DECIDE 4] — how the client learns the job is done

**RESOLVED: (a), poll every 2s — and deliberately DO NOT fix the cache.**

Not the recommendation, and the consequence should be stated plainly rather than
discovered: with a 10s cache TTL, no worker invalidation, and a ~4s pipeline, the
UI will usually jump from `uploading` to `completed` having shown no intermediate
stage at all. **The stage-progress display is therefore structurally unable to
show progress most of the time.**

That means `PROJECT_PLAN.md`'s stated verification for this stage — *"See job
move through pipeline stages"* — cannot be satisfied as written. It is recorded
as a known limitation rather than quietly reworded, and the cache-coherence gap
stays open for a later stage to close.

#### What the API supports today

Polling `GET /jobs/{id}`. That is the entire list. There is no SSE endpoint, no
WebSocket, no long-poll, no push registration. Anything else in this decision is
new backend infrastructure, not a client choice.

#### The finding that changes the answer

`GET /jobs/{id}` is served from Redis with a **10-second TTL**
(`cache/redis.go:18`), and **nothing invalidates it except `POST /complete`**.
The workers never touch the cache. Meanwhile 6A measured the full pipeline at
**~4 seconds end to end.**

So the entire pipeline can run and finish *inside a single cached response.*
Polling at 1s or 2s does not make transitions visible; it re-reads the same
cached job. `PROJECT_PLAN.md`'s stated verification for this stage is *"See job
move through pipeline stages"* — and with the current cache and the current
pipeline speed, the honest expectation is that the UI jumps from `uploading`
straight to `completed` having shown no stage detail at all.

That is not a polling-interval problem. It is a cache-coherence problem, and it
would have been discovered as a "the app doesn't work" bug three hours into
implementation if this plan had not gone looking.

#### Options

**(a) Poll `GET /jobs/{id}` every 2s while the screen is focused.** Stop on
`completed`/`failed`, stop on background (`AppState`), hard cap the loop.
**(b) Add an SSE endpoint** to the API that streams stage transitions.
**(c) WebSocket.** **(d) APNs push.** — (b), (c) and (d) all require new backend
work, new failure modes, and in (d)'s case an Apple developer account, for a
pipeline that finishes in four seconds.

Independently, for the staleness:

- **(i) Lower `defaultTTL`** from 10s to ~1s for non-terminal jobs. One line,
  own commit. The cache exists to protect DynamoDB from a polling client; at 1s
  it still collapses a 2s poll from N clients and stops hiding the pipeline.
- **(ii) Have the worker runner invalidate on each stage transition.** Correct,
  and a **fifth** shared-runner change. 6A already named the accumulation of
  those as a risk. No.
- **(iii) Use a longer clip** so the stages take long enough to see regardless.

#### Recommendation: **(a) + (i), plus (iii) for the demo run.**

Poll at 2s, cancel on unfocus and on background, hard-cap at 2 minutes (30×
the observed pipeline time) so a stuck job cannot leave a phone polling forever.
Drop the cache TTL to 1s in its own commit. Record the tension plainly: (i) is a
backend change made purely so a UI can observe something, and it should be
reverted or reconsidered if the cache ever has a real load-bearing job.

(b)–(d) are chosen against on cost, not on merit. Push is the right answer for a
pipeline that takes minutes; this one takes four seconds.

---

### [DECIDE 5] — the job list has no endpoint, and the types are already wrong

**RESOLVED: (a), a local job index on the device**, plus regenerating the types
from the Go structs by hand with a guard against future drift.

**Carrying limitation, to be documented in the setup guide:** the job index lives
only on the device. Reinstalling the app, clearing its data, or switching
emulators **loses the entire job history**, even though those jobs still exist
server-side and are still reachable by ID. This is a direct consequence of there
being no `GET /jobs` endpoint, and it is a known trade rather than an oversight.

Two problems that have to be solved together, because both are about the list
screen.

#### Problem 1: there is no `GET /jobs`

`router.go` exposes `/jobs` for POST only. The client can fetch a job it knows
the ID of, and cannot discover one it does not. `JobListScreen` renders
`getMockJobs()` today, so this has never come up.

- **(a) Persist a local job index on the client** — `{job_id, filename,
  created_at, size_bytes, local_uri}` written at job creation, then poll each
  entry. `react-native-blob-util`'s `fs.writeFile` (already being added in
  **[DECIDE 3]**) does this without a second dependency; AsyncStorage would be a
  second native module for the same job.
- **(b) Add `GET /jobs` to the API.** DynamoDB single-table with `PK = JOB#uuid`
  has no access pattern for "all jobs" — this is a `Scan`, which is exactly what
  `stage-1a-data-schemas.md`'s design was avoiding, or a new GSI.
- **(c) Keep the list in React state.** Wiped by every Fast Refresh, which during
  RN development is constantly.

**Recommendation: (a).** Chosen over (b) because a list endpoint means adding a
table scan to satisfy a client convenience, and over (c) because a list that
does not survive a reload cannot be demonstrated.

#### Problem 2: `mobile/src/types/api.ts` does not match `models/job.go`

Written in 2B from the plan document rather than from the Go source, and the two
have drifted. Every one of these is a silent runtime bug, not a compile error,
because JSON does not typecheck itself:

| Field | Go (`models/job.go`) | Mobile (`types/api.ts`) | Consequence |
|---|---|---|---|
| stage status values | `pending` / `running` / `completed` / `failed` | `pending` / `processing` / `complete` / `failed` / `skipped` | **`JobListScreen` counts `s.status === 'complete'` — which never matches.** The progress bar would sit at 0/4 through a successful job |
| stage attempt count | `attempts` | `retry_count` | Always `undefined` |
| output duration | `duration_seconds` (float) | `duration_ms` (required) | Always `undefined`; any ms-based formatting renders `NaN` |
| `output.caption_url` | does not exist | declared | Always `undefined` |
| job status | includes `pending` | omits `pending` | A job can be in a state the union does not allow |

**Recommendation: regenerate the types from the Go structs by hand, and add a
smoke assertion** — one test that parses a real captured `GET /jobs/{id}` body
against the types. The drift happened because the types were written from a plan;
the fix is to write them from the wire.

---

### [DECIDE 6] — does Stage 7 include playback?

**RESOLVED: NO — playback stays in Stage 8B.**

> **Correction, 2026-08-13.** This decision was first recorded here as
> "RESOLVED: YES — playback IS in scope," and that resolution was never
> propagated: the components table, the *Deliberately not in scope* section,
> the **[DECIDE 7]** note and the body of this very section all continued to
> say 8B. Five passages against two. The contradiction was caught while
> planning 8A, before any playback code existed — no `react-native-video` in
> `package.json`, no player on `PlayerScreen`. Re-settled on that basis:
> because nothing had been built, the cheaper and better-supported reading
> won. Stage 7 stops at "jobs process."

`PROJECT_PLAN.md` settles this itself. Stage 7, verbatim:

> **Deliverables:**
> - API client calls real backend
> - Foreground upload using presigned URLs
> - Job status polling and display
>
> **Verification:**
> - Pick video in app
> - See upload progress
> - See job move through pipeline stages
>
> **Observable outcome:** Mobile uploads work, jobs process.

And immediately after it:

> **Stage 8B: HLS Playback** — **Aim:** Play reels in app.
> **Deliverables:** react-native-video with ExoPlayer

**Settled by the plan: playback is Stage 8B. Stage 7 stops at "jobs process."**

What Stage 7 *should* still do, because it costs nothing and proves the last
link: on completion, call `GET /jobs/{id}/reel` and **display the returned
`hls_url`** on `PlayerScreen` — selectable, so it can be pasted into a real
player. That converts the 409-since-2A endpoint from "verified by curl" to
"verified from the client that exists to consume it," without pulling a video
library into this stage.

**The player choice, recorded for 8B rather than adopted now:**
`react-native-video` v6 — AVPlayer on iOS, ExoPlayer on Android, both of which
handle `EXT-X-MEDIA:TYPE=SUBTITLES` renditions natively, with `selectedTextTrack`
to pick one. Alternatives: `expo-video` (needs expo-modules bolted into a bare RN
app), `react-native-vlc-media-player` (tolerant of odd streams, and a very large
binary). **Uncertain and worth checking before 8B commits:** RN 0.87 defaults to
the New Architecture, and `react-native-video` v6's support for it under this
exact RN version is unverified here.

---

### [DECIDE 7] — the caption defect, and what evidence settles it

**RESOLVED: measure first, then fix if the measurement says so.** With playback
back in 8B, this stage still gets a real player in front of the output for the
first time, and that is the whole reason the measurement belongs here rather
than in 8B.

> **Knock-on from [DECIDE 2], 2026-08-13.** This section was written around a
> single oracle — **Safari in the iOS Simulator**, chosen because it plays HLS
> through AVFoundation, the same engine `react-native-video` uses on iOS. With
> the target switched to the Android emulator, **that oracle is not available**
> (there are no simulator runtimes on this machine) and its replacement is not
> equivalent: the emulator's browser and ExoPlayer are a different HLS
> implementation, and **whether ExoPlayer honours `X-TIMESTAMP-MAP` the same way
> AVFoundation does is exactly the kind of thing that differs between players.**
> Since 8B ships ExoPlayer, measuring against ExoPlayer is now the more relevant
> measurement — but it is a *different* measurement, not the one this section
> originally specified. Record which player produced any number.

6A shipped a known defect: **the first caption cue is dropped, and every other
cue lands ~112 ms early**, because subtitle timings are offset against the
MPEG-TS start PTS. `-muxdelay 0 -muxpreload 0` already cut this from ~1.4s to
~0.07s; the residual is small but reliably eats a cue starting at t=0, and the
mock transcript always starts one there.

The candidate fix is `X-TIMESTAMP-MAP` in the VTT. **It could not be verified in
6A because ffmpeg ignores that header entirely** — four different values produced
byte-identical output. Real players honour it. 6A's own words: *"It needs a real
player test, which is Stage 7 territory."*

**Stage 7 is where a real player finally enters, so this is where the evidence
gets collected — even though playback is out of scope.** The oracle is not the
app. It is **Chrome in the Android emulator**, which reaches
`http://10.0.2.2:4566/...`, needs no dependency, and costs one paste. It is not
the same engine `react-native-video` will use in 8B, so if the numbers matter,
re-measure once ExoPlayer is actually in the app.

Options once the measurement exists:

- **(a) Do nothing.** Legitimate if the player shows correct sync — that would
  mean the defect is in ffmpeg's *reader*, not in the output, and 6A's
  observation was an artefact of the only tool available to it.
- **(b) Emit `X-TIMESTAMP-MAP`** with `MPEGTS` derived from the real start PTS.
  The spec-sanctioned mechanism, and the reason it was unverifiable is removed
  the moment a real player is in the loop.
- **(c) Shift every cue timestamp by the measured offset.** Player-independent,
  but bakes an environment-specific constant into content.
- **(d) Force the TS start PTS to zero** (`-output_ts_offset` /
  `-avoid_negative_ts`), removing the offset rather than describing it.

**Recommendation: measure first, then (b) if a fix is needed, (c) as fallback.**
Same discipline as 6A's **[DECIDE 5]** — the number precedes the decision.

**Scope caveat, stated rather than smuggled:** the *decision* belongs in Stage 7
because Stage 7 owns the evidence. The *fix* edits
`backend/internal/media/`, which is 6A's code. If the measurement says a fix is
needed, it should be its own commit, and arguably its own follow-up stage (6B),
rather than expanding an integration stage into backend media work.

---

### [DECIDE 8] — the uploader is barely exercised at the budgeted clip size

**RESOLVED 2026-08-13: make the part size configurable — and use a test clip
larger than 5 MiB.** Added after the stage was approved, while planning 8A.

> **Correction, 2026-08-13, after implementation.** This section originally
> resolved to "set roughly **256 KiB** on the local `api` service". **That does
> not work, and the reasoning below about why it is safe locally was wrong.**
>
> It was tried. All four parts uploaded with 200s, and the job then died at the
> last step:
>
> ```
> api error EntityTooSmall: Your proposed upload is smaller than the minimum allowed size
> ```
>
> **LocalStack enforces S3's 5 MiB floor exactly as S3 does.** The claim below
> that a small part size "works on LocalStack but real S3 will reject it" was
> asserted from the AWS docs and never tested — the same mistake this project
> has now made repeatedly, and the reason it keeps insisting on measurement.
>
> `UPLOAD_PART_SIZE` still exists and is still worth having, but it can only be
> raised: a smaller value is **clamped** to 5 MiB with a log line, so a broken
> part size cannot be configured at all. The way to exercise the multipart path
> is option **(b)** below — a local test clip over 5 MiB, which costs nothing
> because LocalStack is free.

`handlers.go:21` hardcodes `partSize = 5 * 1024 * 1024`. A test clip under ten
seconds is 1–5 MB, so **every local upload is a single part**. That is not only
8A's problem — it is this stage's. At one part, the uploader's loop never
iterates twice, the progress aggregation across completed parts never sums
anything, per-part retry never re-PUTs while holding earlier ETags, and
`POST /complete` never assembles more than one entry. The parts of
**[DECIDE 3]** most likely to be wrong are exactly the parts one part cannot
reach.

Move it to `UPLOAD_PART_SIZE` in config, default 5 MiB, and set roughly
**256 KiB** on the local `api` service. A 3 MB clip then produces about a dozen
parts, and the multipart path is genuinely exercised.

**The ≤10s limit in `config/free-tier.md` is a cost constraint on real AWS, not
a local one.** LocalStack is free, so a larger local fixture is also available
and costs nothing — but a smaller part size is preferable because it exercises
the same code paths without a fixture to generate, store, or keep out of git.

S3's own floor is 5 MiB for every part except the last, so a 256 KiB part size
is **local-only and must not reach a real bucket** — real AWS would reject it
with `EntityTooSmall` on complete. The default stays 5 MiB for that reason, and
this is the second setting after `S3_PUBLIC_ENDPOINT` whose value depends on
where the stack points. Both belong in the same paragraph of `docs/SETUP.md`.

---

## Files

| File | Action | Purpose |
|------|--------|---------|
| `backend/internal/storage/s3.go` | Modify | Presign with `BaseEndpoint` overridden to the public endpoint |
| `backend/internal/storage/s3_test.go` | Create | The presigned URL's host is the public endpoint, and the API's own calls still use the internal one |
| `backend/internal/api/handlers.go` | Modify | Pass `cfg.PublicEndpoint()` into presigning |
| `backend/internal/cache/redis.go` | Modify | TTL (**[DECIDE 4]**) — separate commit |
| `infra/docker-compose.yml` | Modify | `S3_PUBLIC_ENDPOINT` on `api`; `S3_SKIP_SIGNATURE_VALIDATION=0` on localstack for the verification run |
| `mobile/src/config/index.ts` | Create | The single place the API host is written |
| `mobile/src/types/api.ts` | Modify | Regenerate from the Go structs (**[DECIDE 5]**) |
| `mobile/src/api/client.ts` | Modify | Real base URL, typed errors, delete `MOCK_JOBS` |
| `mobile/src/upload/uploader.ts` | Create | Slice → PUT parts → collect ETags → complete |
| `mobile/src/upload/uploader.test.ts` | Create | Part maths and retry policy, against a faked transport |
| `mobile/src/storage/jobIndex.ts` | Create | Local job list persistence (**[DECIDE 5]**) |
| `mobile/src/hooks/useJobPolling.ts` | Create | Focus-aware, background-aware polling with a hard cap |
| `mobile/src/screens/HomeScreen.tsx` | Modify | `copyTo`, real upload, navigate to the job |
| `mobile/src/screens/JobListScreen.tsx` | Modify | Real jobs, upload progress, stage progress |
| `mobile/src/screens/PlayerScreen.tsx` | Modify | Show the real `hls_url`; **no player** (**[DECIDE 6]**) |
| `mobile/package.json` | Modify | `react-native-blob-util` |
| `mobile/android/local.properties` | Create | `sdk.dir`, once an Android SDK exists on this machine |
| `mobile/CONTEXT.md` | Modify | It currently says "no real API calls yet" |
| `docs/stage-plans/stage-6a-package-worker.md` | Modify | Record the caption-sync measurement against its known-defect section |

## Tasks

1. [ ] **Backend: presign against the public endpoint.** `s3.go` + handler +
       compose env. Unit test asserts the URL host.
2. [ ] Verify from the **host** with `curl` before any app code exists —
       `POST /jobs`, PUT the part to the returned URL, `POST /complete`, watch
       the pipeline run. **If this does not work, nothing downstream can.**
3. [ ] Cache TTL (**separate commit**, backend).
4. [x] Install the Android SDK/NDK and an emulator image (~~blocked on disk~~ —
       **done 2026-08-13**); add `react-native-blob-util`; get the app building
       on the emulator. Do this **before** writing client code, not after.
       *Was not done before, and the cost showed: the picker replacement was
       discovered by the first build, after the client code existed.*
5. [ ] `mobile/src/types/api.ts` regenerated from `models/job.go`.
6. [ ] `config/index.ts`, `api/client.ts` — real calls, mocks deleted.
7. [ ] `upload/uploader.ts` + tests.
8. [ ] `storage/jobIndex.ts`.
9. [ ] `hooks/useJobPolling.ts`.
10. [ ] Screens: Home (pick → upload → navigate), JobList (real + progress),
        Player (reel URL).
11. [ ] End to end on the emulator.
12. [ ] Failure paths — all of them in Verification, none skipped.
13. [ ] **Caption sync in the emulator's browser** (**[DECIDE 7]**); record the
        measurement in the 6A plan, naming the player that produced it.
14. [ ] Physical device attempt (stretch).
15. [ ] `mobile/CONTEXT.md`.

## Test

```bash
# 0. Prove the endpoint fix from the HOST, before any app exists.
#
# NOTE: the committed S3_PUBLIC_ENDPOINT is http://10.0.2.2:4566, which the host
# cannot route to. To run this proof from the host, either flip the api service
# to http://localhost:4566 for the duration, or alias the address onto loopback
# (`sudo ifconfig lo0 alias 10.0.2.2`). See docs/SETUP.md.
cd infra && docker compose up -d --build api

JOB_JSON=$(curl -s -X POST localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{"filename":"clip.mp4","size_bytes":1048576,"content_type":"video/mp4"}')

echo "$JOB_JSON" | jq -r '.upload_urls[0].url'
# EXPECT: the configured public host, NOT http://localstack:4566/...
#   committed value  -> http://10.0.2.2:4566/...
#   host-test value  -> http://localhost:4566/...

JOB=$(echo "$JOB_JSON" | jq -r .job_id)
URL=$(echo "$JOB_JSON" | jq -r '.upload_urls[0].url')

# The PUT that has never once been runnable from outside a container
ETAG=$(curl -s -X PUT --upload-file clip.mp4 -D - "$URL" | grep -i '^etag:' | tr -d '\r' | cut -d' ' -f2)

curl -s -X POST localhost:8080/jobs/$JOB/complete \
  -H 'Content-Type: application/json' \
  -d "{\"upload_id\":\"$(echo "$JOB_JSON" | jq -r .upload_id)\",\"parts\":[{\"part_number\":1,\"etag\":$ETAG}]}"

sleep 8
curl -s localhost:8080/jobs/$JOB | jq '{status, stages}'
curl -s localhost:8080/jobs/$JOB/reel | jq

# 1. Then, and only then, the app. (Restore 10.0.2.2 on api first if you flipped it.)
cd ../mobile && npx react-native run-android
```

## Verification

_Nothing checked off until observed. Android emulator unless stated._

> **Results, 2026-08-13.** The reference run is job
> `4bd59394-a104-453b-90d0-fdd363ad1dba` — `18.mp4`, **14,947,952 bytes**,
> uploaded **from the app** as **3 parts** at the 5 MiB default, `completed` in
> 36.7 s across all four stages, `hls_url` returned and played. Everything below
> that is checked was observed on that run or on the build that produced it.
> Everything unchecked was **not run**; none of it failed.

**The blocker itself**

- [x] `POST /jobs` returns URLs whose host is the configured public endpoint
      (`10.0.2.2:4566` as committed), not `localstack:4566` — proven by the
      emulator upload succeeding, which is only possible if the signed host is
      the one the client sends
- [ ] A `curl PUT` **from the host** to that URL returns 200 with an ETag —
      the step that has required a container since Stage 3A. **Requires the
      host-routable value**, so run this with `api` temporarily on
      `localhost:4566` or with `10.0.2.2` aliased onto `lo0`. **Not run in this
      form.** The emulator upload is strictly stronger evidence for the same
      claim — an out-of-Docker client PUT that lands — so this box is now a
      convenience check, not the stage's gate
- [x] With `S3_SKIP_SIGNATURE_VALIDATION=0` set on localstack, the same PUT still
      returns 200 — i.e. the signature is genuinely valid, not merely unchecked.
      The setting is committed in compose and was in force for the run
- [x] The API's own S3 calls (`CreateMultipartUpload`, `CompleteMultipartUpload`)
      still succeed — the internal endpoint was not broken by the override

**Happy path, from the app**

- [x] App builds and launches on the emulator once the Android SDK/NDK exist —
      `:app:assembleDebug`, ~147 MiB debug APK, installed and launched on
      `dayreel-avd`, reaching the API at `10.0.2.2:8080`
- [x] Picking a video yields a readable local path, a non-null size, and a
      non-null MIME type — all three, since the API 400s on any of them missing.
      The job record carries `size_bytes: 14947952` and
      `content_type: "video/mp4"`, so all three arrived. **Note the picker is no
      longer `react-native-document-picker`** — see the correction below
- [x] `POST /jobs` succeeds from the app; the job ID appears in the local index
- [x] Every part PUTs and returns an ETag; **a >5 MB clip uploads as multiple
      parts**, not one — 14.9 MB at a 5 MiB part size gave `total_parts: 3`, and
      `CompleteMultipartUpload` accepted them. This is the check that would have
      been vacuous on a small synthetic clip, and it was run on a real one
- [ ] Upload progress advances monotonically 0 → 100% and is not fabricated —
      not systematically observed
- [x] `POST /complete` returns 200 and the validate queue receives the S3 event —
      `upload.completed_at` is set and `validate` started 0.1 s later
- [x] The job list shows the job advancing, and reaches `completed`
- [x] `GET /jobs/{id}/reel` returns 200 **to the app**, and `PlayerScreen`
      displays the real `hls_url` — and, per 8B, plays it
- [ ] Killing and relaunching the app still shows the job in the list — not run

**Caption sync — the first real player in this project**

> **Correction, 2026-08-13.** This block was written expecting **Chrome inside
> the Android emulator** to be the oracle, after **[DECIDE 2]**'s platform switch
> took Safari/AVFoundation away. That is not what happened, and the substitution
> was unnecessary: the HLS URL is unsigned and its segment references are
> relative, so the master can be played from the **host** with the emulator out
> of the loop entirely. 8B found this and used a headless AVFoundation probe,
> which reports the cue presentation time as a number rather than requiring
> someone to eyeball a browser. Both halves — measurement and fix — moved to 8B
> (**8B [DECIDE 3]**), so these boxes were satisfied there rather than here.

- [x] `master.m3u8` plays, and the player is recorded — **AVFoundation** on the
      host for the measurement, **ExoPlayer** in the app for playback. Not Chrome
      in the emulator
- [x] Captions appear at all — the track picker lists `#0 en English`, checked
      rather than inferred from the video surface
- [x] **The cue that starts at t=0 is present.** It was never dropped; 6A's
      report of a missing first cue was an artefact of ffmpeg's reader
- [x] Measured offset written down as a number — **66.667 ms early** before the
      fix, **0.333 ms late** after, on AVFoundation, on a 30 fps source
- [x] Same check with `X-TIMESTAMP-MAP` present — the before/after was run on
      identical media, which is what makes the number mean anything
- [x] Result recorded in `stage-6a-package-worker.md`'s known-defect section
- [ ] **The same measurement on ExoPlayer.** UNKNOWN, and not interchangeable
      with the AVFoundation figure: the fix anchors on the video start PTS, and a
      player seeding from the container start would read ~21.3 ms early instead

**Failure paths** — _none of these were run. The stage's happy path is verified;
its failure behaviour is not, and should not be assumed from the happy path._

- [ ] **Network drop mid-upload:** kill LocalStack (or disable the host network)
      between parts. Expect: the in-flight part retries 3× with backoff, then a
      clear "upload failed" state — **not** a spinner that never resolves, and
      **not** a `POST /complete` sent with missing parts
- [ ] **Resume after that drop is NOT expected to work.** Confirm it fails
      cleanly and record what 8A has to build
- [ ] **A job that fails:** upload a non-video (rename a `.txt` to `.mp4`).
      Expect validate to fail loudly (per 3A **[DECIDE 2]**, ffprobe is the gate)
      and the app to show `failed` with the stage error, not a stuck `processing`
- [ ] **A silent clip:** no speech → an empty `WEBVTT`. Expect the job to reach
      `completed`, the reel URL to resolve, and the app not to crash on an
      output with no cues. 6A verified the VTT is valid; nothing has verified a
      client handles it
- [ ] **Expired presign:** issue URLs, wait past `expires_in` (or shorten it),
      PUT. Expect a 403 surfaced as "upload expired", not an opaque error
- [ ] **App backgrounded mid-upload:** record what happens. Expected to fail;
      this is the exact gap 8A exists to close
- [ ] **Polling stops** on `completed`, on `failed`, on unfocus, and on
      background — verified in the API logs, which log every request
      (`middleware.go`). A poll loop left running is the client-side version of
      the hot-loop failure `config/free-tier.md` warns about
- [ ] **Duplicate `POST /complete`** does not corrupt the job

**Cost and disk**

- [ ] `docker system df` before and after; the 8 GB ceiling holds
- [ ] Temp part slices are deleted from the emulator's cache directory after
      each upload
- [ ] Nothing was provisioned on real AWS. If anything was, name it and tear it
      down per `config/free-tier.md`

## Claude Code Implementation Plan

### Recommended Approach: Prove the Blocker from `curl` Before Writing Any App Code

The stage has one genuinely uncertain element — whether a presigned URL signed
against a public endpoint is accepted — and a large amount of ordinary client
work stacked on top of it. Those must not be interleaved.

**Phase 1 is a `curl` script.** If the PUT from the host does not work, every
line of React Native written in the meantime is written against a hypothesis. The
same discipline 6A used for the playlist (pure and instantly testable first)
applies, with the roles reversed: here the *backend* is the cheap fast loop and
the *client* is the slow one.

### Pre-Flight Check

```
0a. docker system df                      # 8GB ceiling
0b. docker builder prune -f
0c. docker compose ps                     # 6 containers healthy after 6A
0d. curl -s localhost:8080/health         # API up
0e. df -h /System/Volumes/Data            # STALE as written ("EXPECT ~2.8Gi").
                                          # The SDK is installed now and the
                                          # volume sits around 6 GiB free
0f. echo $ANDROID_HOME && which adb emulator && java -version
    # STALE as written ("EXPECT: unset, both missing"). The SDK now exists at
    # ~/Library/Android/sdk; ANDROID_HOME still has to be exported per shell
0g. node --version   # EXPECT v22.13+. package.json says >= 22.11, which is
                     # LOOSER than RN 0.87's own ^22.13.0 — trust RN's, not ours
0h. Cleartext reality check, BEFORE costing the device path: run a trivial
    fetch('http://<lan-ip>:8080/health') from the app. usesCleartextTraffic is
    templated in the manifest, but confirm it survives the release/debug config
    actually used. Two-minute test
0i. Confirm LocalStack's S3_SKIP_SIGNATURE_VALIDATION default. If it skips,
    every local upload result is uninformative about signature validity
0j. Confirm DECIDE 1 is settled — phases 2 onward are meaningless without it
```

### Execution Steps

```
Phase 1: The blocker, backend only, verified by curl   <-- DO NOT SKIP AHEAD
1.  storage/s3.go: presign with BaseEndpoint overridden via
    s3.WithPresignClientFromClientOptions
2.  handlers.go: pass cfg.PublicEndpoint()
3.  docker-compose.yml: S3_PUBLIC_ENDPOINT on the api service (10.0.2.2; flip to
    localhost only while running the host-side curl proof)
4.  s3_test.go: presigned host is public; internal calls unchanged
5.  go build ./... && go vet ./... && go test ./internal/storage/
6.  docker compose up -d --build api
7.  RUN THE CURL SCRIPT. Upload from the HOST, end to end, to a 200 on
    GET /jobs/{id}/reel. Nothing proceeds until this passes.
8.  COMMIT.

Phase 2: Cache TTL   <-- SEPARATE COMMIT
9.  cache/redis.go TTL; re-run the curl script and watch stages actually change
10. COMMIT separately.

Phase 3: Mobile toolchain   <-- slow, do it before writing code
11. Clear disk, then install Android SDK 37/36, build-tools 37.0.0,
    NDK 27.1.12297006 and an emulator image; set ANDROID_HOME
12. npm i react-native-blob-util
13. npx react-native run-android  (an unmodified app, to isolate build failures)

Phase 4: Client foundations (parallel writes)
14a. src/config/index.ts
14b. src/types/api.ts        (regenerated from models/job.go)
14c. src/api/client.ts       (mocks deleted)
15.  npx tsc --noEmit

Phase 5: Upload + persistence (parallel writes)
16a. src/upload/uploader.ts
16b. src/upload/uploader.test.ts
16c. src/storage/jobIndex.ts
17.  npm test                    <-- fast loop; part maths and retry are pure

Phase 6: Screens (parallel writes)
18a. src/hooks/useJobPolling.ts
18b. src/screens/HomeScreen.tsx
18c. src/screens/JobListScreen.tsx
18d. src/screens/PlayerScreen.tsx

Phase 7: End to end and failure paths
19. Emulator: pick -> upload -> stages -> completed -> reel URL
20. EVERY failure path in Verification. The network-drop one especially:
    it is the one that will actually happen to a user
21. Caption sync in the emulator's browser; record the number AND the player
    that produced it, in the 6A plan
22. Physical device attempt (stretch, gated on 0h)
23. CONTEXT.md; record results in this file
```

### Parallel Opportunities

| Phase | Parallel Files |
|-------|----------------|
| 1 | `s3.go`, `s3_test.go`, `docker-compose.yml` |
| 4 | `config/index.ts`, `types/api.ts`, `api/client.ts` |
| 5 | `uploader.ts`, `uploader.test.ts`, `jobIndex.ts` |
| 6 | `useJobPolling.ts`, `HomeScreen.tsx`, `JobListScreen.tsx`, `PlayerScreen.tsx` |

Phases 1–3 are strictly sequential and each gates the next. The parallelism here
is all inside phases 4–6, which are the cheap parts.

### Subagents

The pattern that has paid off — `pkt_pts_time` in 4A, the exit-0-on-corrupt-input
trap in 5A, the HLS ladder in 6A — is delegating **empirical research with a
verbose, slow feedback loop**, never authoring.

- **Worth an agent:** the Android SDK install + first Gradle build (phase 3). It
  is slow, extremely verbose, and has a long tail of toolchain failures unrelated
  to anything in this plan. Exactly 2B's argument for delegating
  `react-native init`.
- **Worth an agent:** determining empirically whether the app's manifest config
  actually permits cleartext to a LAN IP in the build variant used (0h), and what
  LocalStack's signature-validation default actually is (0i). Both are yes/no
  questions with expensive answers and cheap delegation. **Require the exact
  commands and exact output, not conclusions** — this is where a confident wrong
  answer costs most, and **[DECIDE 2]**'s correction note is the standing example
  of what an unverified toolchain claim costs.
- **Not worth an agent:** the Go change, the uploader, the screens, the types.
  These are small, and the types in particular need to be written by whoever read
  `models/job.go`.

Give any agent an explicit file boundary and the 8 GB disk warning, as in 4A–6A.

### Potential Blockers

| Blocker | Resolution |
|---|---|
| **Presigned URL still rejected after the endpoint override** | Fall back to **[DECIDE 1](b)**, the API upload proxy. It contradicts a 2A principle, so it is a last resort — but it is a *known-working* last resort |
| **LocalStack does not validate signatures, so local success proves nothing** | Set `S3_SKIP_SIGNATURE_VALIDATION=0` for the verification run. Same trap 6A flagged for bucket policies: local 200 ≠ AWS 200 |
| ~~**The Android SDK does not fit on disk**~~ — **CLEARED 2026-08-13.** Disk was reclaimed, the SDK/NDK/system image installed (8.8 GiB), and the build is green | The real stall was elsewhere: `react-native-document-picker` does not compile against RN 0.87, and Node had to go from v20.11 to 22.23. Neither was on this table |
| **`scripts/verify-presign.sh` cannot run from the host** | Expected, not a bug: `10.0.2.2` is not host-routable. Flip `api` to `localhost:4566` for the run, or `sudo ifconfig lo0 alias 10.0.2.2`. Documented in `docs/SETUP.md` |
| ~~**RN 0.87 New Architecture vs `react-native-blob-util`**~~ | **RESOLVED 2026-08-13: it works.** So does `react-native-video` v6. The fallback to **[DECIDE 3](b)** was not needed |
| **Pipeline finishes inside the 10s cache, so no stages are ever observed** | **[DECIDE 4](i)**. Without it, `PROJECT_PLAN`'s own verification for this stage cannot be satisfied |
| **Stage-status string mismatch** (`complete` vs `completed`) | **[DECIDE 5]**. Silent: the progress bar reads 0/4 through a perfectly successful job |
| **Cleartext to the LAN IP is blocked on the device** | Device path only. Falls back to emulator scope, which **[DECIDE 2]** already commits to |
| **`MOCK_TRANSCRIBE=true` is the default** | Captions will read `[mock transcript] segment N`. Fine for upload verification; **the caption-sync measurement should still use the mock**, since its cue at t=0 is precisely what exposes the defect |
| **Docker 8 GB ceiling** | Only the `api` image rebuilds here, so this stage is lighter than 6A. Prune before starting anyway |

### Time Estimate

- Phase 1 (the blocker, backend + curl proof): ~30 minutes, **high variance** —
  this is the only genuinely unknown part
- Phase 2 (cache TTL): ~10 minutes
- Phase 3 (Android SDK install, first build): ~30 minutes at best, **very high
  variance and currently blocked on disk space**, delegable
- Phase 4 (client foundations): ~20 minutes
- Phase 5 (uploader + tests): ~35 minutes
- Phase 6 (screens): ~30 minutes
- Phase 7 (E2E, failure paths, caption measurement): ~45 minutes
- **Total: ~3¼ hours**, longer than 6A

It is longer because it is three stages wearing one name: an infrastructure fix
that has been deferred since 3A, a client feature set that does not exist yet,
and the first real-player verification in the project. Phases 1 and 3 are where
the estimate will move, and they move in opposite directions — phase 1 is either
twenty minutes or two hours depending on whether the signature holds.

---

## Notes

### What this stage closes

**The presigned-URL blocker, open since Stage 3A.** Flagged in 3A's findings,
restated in 4A, restated again in 6A's blockers table, and worked around every
time by running the upload inside a container. There is no container to hide in
here.

Worth noting the shape of it, because it is the same shape as 6A's finding about
`GET /jobs/{id}/reel`: every stage verified its own outputs and passed, and the
one thing nobody could verify was the thing an actual client would do. The
project has now hit that twice. The lesson both times is that a component test
passing says nothing about whether the seam holds.

### Risks and inherited tensions

- **~~Inherited from 6A: the caption defect.~~ CLOSED in 8B, 2026-08-13.** The
  inherited description — *"~112 ms early, first cue dropped"* — was wrong on
  both counts. Measured: 66.667 ms, varying as `2/fps`; the first cue was never
  dropped. `X-TIMESTAMP-MAP` is emitted and the residual is 0.333 ms on
  AVFoundation. The evidence-gathering this stage was to inherit happened in 8B
  instead, because the oracle turned out not to need the emulator at all.
- **Inherited from 6A: the public-read HLS bucket is local-only, and the
  real-AWS access model is an explicit open question.** Stage 7 does not answer
  it and should not pretend to — the app displays a URL rather than fetching
  segments, so nothing here forces the question.
- **Inherited from 5A: `MOCK_TRANSCRIBE=true` is the default.** Every caption
  observed in this stage will be synthetic unless a run deliberately opts in.
- **Inherited from 2B: the app has never made an HTTP request.** Not one line of
  the client's networking has ever executed against the real API. Treat every
  part of it as unproven, including the parts that look trivial.
- **Inherited from 1B: `PERSISTENCE=1` is silently ignored** in LocalStack
  Community. A LocalStack restart wipes buckets, queues and job rows — and the
  client's local job index will then reference jobs that no longer exist. The
  list screen must survive a 404 from `GET /jobs/{id}` without breaking.
- **A backend change made for a UI's benefit** (**[DECIDE 4](i)**, the cache
  TTL). Small and defensible; still worth naming, because it is the kind of
  change that later looks arbitrary.
- **Budget.** Everything here is LocalStack. No AWS resource should be created by
  this stage at all; if one is, `config/free-tier.md` requires naming it and
  tearing it down explicitly.

### Deliberately not in scope

- **Playback in the app** — Stage 8B, per `PROJECT_PLAN.md` quoted in
  **[DECIDE 6]**.
- **Background / resumable upload** — Stage 8A, explicitly.
- **Camera capture.** The picker is the only input.
- **Auth.** There is none, anywhere, and adding it here would be inventing a
  requirement.
- **A `GET /jobs` list endpoint** — **[DECIDE 5]**, solved client-side instead.
- **iOS.** Not because it cannot work, but because **[DECIDE 2]** says one signed
  host at a time and the committed target is the Android emulator. The iOS
  toolchain is absent on this machine anyway — no simulator runtimes, no
  CocoaPods — so `mobile/ios/` is untouched by this stage.

### Uncertain, flagged rather than smoothed over

_Updated 2026-08-13. Most of this list is now answered; the answers are kept with
the questions so the change is visible._

- **RESOLVED — whether LocalStack validates presigned signatures by default.** It
  is forced off-by-default risk rather than a default: `S3_SKIP_SIGNATURE_VALIDATION=0`
  is committed in compose and was in force for the verified run, so the 200s mean
  something. LocalStack still does **not** check whether a request is
  authenticated *at all* — see `docs/SETUP.md`. That gap is untouched.
- **RESOLVED — whether the app's cleartext config holds for `10.0.2.2`.** It
  does, in the debug variant: the app reached `10.0.2.2:8080` and PUT parts to
  `10.0.2.2:4566`. **The LAN-IP half is still UNKNOWN** — no physical device was
  attempted — and so is the release variant.
- **RESOLVED — `react-native-blob-util` under RN 0.87's New Architecture.** It
  builds and runs with `newArchEnabled=true`. So does `react-native-video` v6,
  which was 8B **[DECIDE 1]**'s open gate.
- **STILL UNKNOWN — whether ExoPlayer honours `X-TIMESTAMP-MAP` the way the spec
  says.** ExoPlayer renders the caption track, so it is not ignoring the
  subtitle rendition, but **no offset has been measured on it.** AVFoundation's
  figures (66.667 ms early → 0.333 ms late) are AVFoundation's alone, and may not
  transfer: the fix anchors on the video start PTS rather than the container's.
- **PARTLY ANSWERED — real-device video sizes.** The "4-5 parts" figure was an
  estimate. The first real clip, 10 s at 1280x720, was **14.9 MB → 3 parts**. The
  multipart path is genuinely exercised, which was the point; the estimate was
  simply high. 4K clips remain unmeasured.
- **RESOLVED, badly, then fixed — whether anything in `mobile/` still builds.**
  It did **not**. `react-native-document-picker@9.3.1` fails to compile against
  RN 0.87 and is deprecated with no successor version, so the first Android build
  in the project's life failed outright and the dependency had to be replaced
  (see the correction under `POST /jobs`). This line was the right thing to
  flag, and the answer was worse than "probably fine".
