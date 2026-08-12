# Stage 7: Upload Integration

> Status: **draft — not approved.** Seven decisions open, listed below.
> Written 2026-08-12, immediately after 6A landed and the pipeline first
> produced a playable reel.

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
| `backend/internal/config/config.go` | **No change** — `PublicEndpoint()` already exists (6A) |
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
| `mobile/ios/` | Modify — pods have never been installed here; possibly ATS (**[DECIDE 2]**) |
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
|| content_type == ""` is a 400. This matters — `react-native-document-picker`
types `size` as `number | null` and `type` as `string | null`, so the client
must supply fallbacks or it will send a request the API rejects.

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
assumed. The signature then covers `host: localhost:4566`, which is exactly what
the client sends, and LocalStack sees that Host header intact through Docker's
published port.

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

Follows directly from **[DECIDE 1]**: the signed host is fixed per API process,
so this has to be answered, not hedged.

| Target | API host | `S3_PUBLIC_ENDPOINT` | Works today? |
|---|---|---|---|
| iOS Simulator | `http://localhost:8080` | `http://localhost:4566` | Yes — simulator shares host loopback, and `localhost` is an unqualified hostname, which the existing ATS config permits |
| Android Emulator | `http://10.0.2.2:8080` | `http://10.0.2.2:4566` | Yes in principle; `usesCleartextTraffic` is already templated in the manifest. Requires an Android SDK/emulator this machine has not been verified to have |
| Physical iPhone | `http://<lan-ip>:8080` | `http://<lan-ip>:4566` | **Unknown** — gated on the ATS question in **[DECIDE 1](c)** |

#### Recommendation: **iOS Simulator as the committed target; physical device as a stretch verification.**

Chosen because it is the only row that is *known* to work with the app's
existing configuration, because this machine has Xcode tooling available and an
unverified Android toolchain, and because the simulator runs the same AVPlayer
that **[DECIDE 6]**'s caption verification depends on.

The device path costs one env var and one compose restart, so it stays cheap to
attempt — but it should be attempted **after** the simulator path is green, so a
failure there is unambiguously a networking failure and not an upload bug.

Flag: `mobile/ios/` has **no `Podfile.lock` and no `Pods/` directory** — CocoaPods
has never been installed in this repo. The first `pod install` is unbudgeted time
and the first plausible place this stage stalls.

---

### [DECIDE 3] — upload mechanics on the client

#### The constraint

The API always issues a multipart upload with 5 MiB parts. Ten seconds of iPhone
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
- One new native dependency, which means a pod install and an Android rebuild.
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
`pick({type: ['video/*']})` and uses `result.uri`. On iOS that URI is
security-scoped and may not be readable by a native uploader. The picker
supports `copyTo: 'cachesDirectory'`, which populates `fileCopyUri` — **use that
path**, and check `copyError`.

---

### [DECIDE 4] — how the client learns the job is done

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
app. It is **Safari in the iOS Simulator**, which plays HLS through AVFoundation
— the same engine `react-native-video` will use in 8B. It reaches
`http://localhost:4566/...`, needs no dependency, and costs one paste.

Options once the measurement exists:

- **(a) Do nothing.** Legitimate if AVPlayer shows correct sync — that would mean
  the defect is in ffmpeg's *reader*, not in the output, and 6A's observation was
  an artefact of the only tool available to it.
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
| `mobile/ios/Podfile.lock`, `mobile/ios/Pods/` | Create | First pod install in this repo |
| `mobile/CONTEXT.md` | Modify | It currently says "no real API calls yet" |
| `docs/stage-plans/stage-6a-package-worker.md` | Modify | Record the caption-sync measurement against its known-defect section |

## Tasks

1. [ ] **Backend: presign against the public endpoint.** `s3.go` + handler +
       compose env. Unit test asserts the URL host.
2. [ ] Verify from the **host** with `curl` before any app code exists —
       `POST /jobs`, PUT the part to the returned URL, `POST /complete`, watch
       the pipeline run. **If this does not work, nothing downstream can.**
3. [ ] Cache TTL (**separate commit**, backend).
4. [ ] `pod install`; add `react-native-blob-util`; get the app building on the
       simulator. Do this **before** writing client code, not after.
5. [ ] `mobile/src/types/api.ts` regenerated from `models/job.go`.
6. [ ] `config/index.ts`, `api/client.ts` — real calls, mocks deleted.
7. [ ] `upload/uploader.ts` + tests.
8. [ ] `storage/jobIndex.ts`.
9. [ ] `hooks/useJobPolling.ts`.
10. [ ] Screens: Home (pick → upload → navigate), JobList (real + progress),
        Player (reel URL).
11. [ ] End to end on the simulator.
12. [ ] Failure paths — all of them in Verification, none skipped.
13. [ ] **Caption sync in Safari on the simulator** (**[DECIDE 7]**); record the
        measurement in the 6A plan.
14. [ ] Physical device attempt (stretch).
15. [ ] `mobile/CONTEXT.md`.

## Test

```bash
# 0. Prove the endpoint fix from the HOST, before any app exists.
cd infra && docker compose up -d --build api

JOB_JSON=$(curl -s -X POST localhost:8080/jobs \
  -H 'Content-Type: application/json' \
  -d '{"filename":"clip.mp4","size_bytes":1048576,"content_type":"video/mp4"}')

echo "$JOB_JSON" | jq -r '.upload_urls[0].url'
# EXPECT: http://localhost:4566/...   NOT http://localstack:4566/...

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

# 1. Then, and only then, the app.
cd ../mobile && npx react-native run-ios
```

## Verification

_Nothing checked off until observed. Simulator unless stated._

**The blocker itself**

- [ ] `POST /jobs` returns URLs whose host is `localhost:4566`, not
      `localstack:4566`
- [ ] A `curl PUT` **from the host** to that URL returns 200 with an ETag —
      the step that has required a container since Stage 3A
- [ ] With `S3_SKIP_SIGNATURE_VALIDATION=0` set on localstack, the same PUT still
      returns 200 — i.e. the signature is genuinely valid, not merely unchecked
- [ ] The API's own S3 calls (`CreateMultipartUpload`, `CompleteMultipartUpload`)
      still succeed — the internal endpoint was not broken by the override

**Happy path, from the app**

- [ ] App builds and launches on the simulator after `pod install`
- [ ] Picking a video yields a readable `fileCopyUri`, a non-null size, and a
      non-null MIME type — all three, since the API 400s on any of them missing
- [ ] `POST /jobs` succeeds from the app; the job ID appears in the local index
- [ ] Every part PUTs and returns an ETag; **a >5 MB clip uploads as multiple
      parts**, not one (a one-part-only bug is invisible on a small test clip)
- [ ] Upload progress advances monotonically 0 → 100% and is not fabricated
- [ ] `POST /complete` returns 200 and the validate queue receives the S3 event
- [ ] The job list shows the job advancing, and reaches `completed`
- [ ] `GET /jobs/{id}/reel` returns 200 **to the app**, and `PlayerScreen`
      displays the real `hls_url`
- [ ] Killing and relaunching the app still shows the job in the list

**Caption sync — the first real player in this project**

- [ ] `master.m3u8` opens in **Safari inside the iOS Simulator** and plays
- [ ] Captions appear at all (they may not; a player showing no captions looks
      identical to a clip with none — check the track picker, not just the video)
- [ ] **The cue that starts at t=0 is present**, or confirmed dropped
- [ ] Measured offset between spoken/expected cue time and displayed cue time,
      written down as a number, not as "seems fine"
- [ ] Same check with `X-TIMESTAMP-MAP` present, if **[DECIDE 7]** proceeds to a
      fix — the comparison is the entire point
- [ ] Result recorded in `stage-6a-package-worker.md`'s known-defect section

**Failure paths**

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
- [ ] Temp part slices are deleted from the simulator's cache directory after
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
0e. xcodebuild -version && xcrun simctl list devices available | grep iPhone
0f. cd mobile && ls ios/Podfile.lock      # EXPECT MISSING. pod install is unbudgeted
0g. node --version                        # package.json requires >= 22.11
0h. ATS reality check, BEFORE costing the device path: run a trivial
    fetch('http://<lan-ip>:8080/health') from the app and see whether iOS
    blocks it. This is the only unknown in DECIDE 2 and it is a two-minute test
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
3.  docker-compose.yml: S3_PUBLIC_ENDPOINT on the api service
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
11. npm i react-native-blob-util
12. cd ios && pod install       (first ever in this repo)
13. npx react-native run-ios    (an unmodified app, to isolate build failures)

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
19. Simulator: pick -> upload -> stages -> completed -> reel URL
20. EVERY failure path in Verification. The network-drop one especially:
    it is the one that will actually happen to a user
21. Caption sync in Safari on the simulator; record the number in the 6A plan
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

- **Worth an agent:** the `pod install` + first simulator build (phase 3). It is
  slow, extremely verbose, and has a long tail of toolchain failures unrelated to
  anything in this plan. Exactly 2B's argument for delegating `react-native init`.
- **Worth an agent:** determining empirically whether iOS ATS permits cleartext
  to a LAN IP under `NSAllowsLocalNetworking` (0h), and what LocalStack's
  signature-validation default actually is (0i). Both are yes/no questions with
  expensive answers and cheap delegation. **Require the exact commands and exact
  output, not conclusions** — this is where a confident wrong answer costs most.
- **Not worth an agent:** the Go change, the uploader, the screens, the types.
  These are small, and the types in particular need to be written by whoever read
  `models/job.go`.

Give any agent an explicit file boundary and the 8 GB disk warning, as in 4A–6A.

### Potential Blockers

| Blocker | Resolution |
|---|---|
| **Presigned URL still rejected after the endpoint override** | Fall back to **[DECIDE 1](b)**, the API upload proxy. It contradicts a 2A principle, so it is a last resort — but it is a *known-working* last resort |
| **LocalStack does not validate signatures, so local success proves nothing** | Set `S3_SKIP_SIGNATURE_VALIDATION=0` for the verification run. Same trap 6A flagged for bucket policies: local 200 ≠ AWS 200 |
| **`pod install` fails** — never run in this repo | First plausible stall. Delegate it, budget real time, and do it before any client code |
| **RN 0.87 New Architecture vs `react-native-blob-util`** | Unverified pairing. If it breaks, fall back to **[DECIDE 3](b)** (XHR, single part) and accept a backend `part_size` change with the retry loss stated |
| **Pipeline finishes inside the 10s cache, so no stages are ever observed** | **[DECIDE 4](i)**. Without it, `PROJECT_PLAN`'s own verification for this stage cannot be satisfied |
| **Stage-status string mismatch** (`complete` vs `completed`) | **[DECIDE 5]**. Silent: the progress bar reads 0/4 through a perfectly successful job |
| **iOS ATS blocks the LAN IP** | Device path only. Falls back to simulator scope, which **[DECIDE 2]** already commits to |
| **`MOCK_TRANSCRIBE=true` is the default** | Captions will read `[mock transcript] segment N`. Fine for upload verification; **the caption-sync measurement should still use the mock**, since its cue at t=0 is precisely what exposes the defect |
| **Docker 8 GB ceiling** | Only the `api` image rebuilds here, so this stage is lighter than 6A. Prune before starting anyway |

### Time Estimate

- Phase 1 (the blocker, backend + curl proof): ~30 minutes, **high variance** —
  this is the only genuinely unknown part
- Phase 2 (cache TTL): ~10 minutes
- Phase 3 (pod install, first build): ~30 minutes, high variance, delegable
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

- **Inherited from 6A: the caption defect.** ~112 ms early, first cue dropped,
  fix unverifiable server-side. Stage 7 inherits the evidence-gathering, not
  necessarily the fix (**[DECIDE 7]**).
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
- **Android.** Not because it cannot work, but because **[DECIDE 2]** says one
  signed host at a time and the simulator is the one with a verified toolchain.

### Uncertain, flagged rather than smoothed over

- **Whether LocalStack validates presigned signatures by default.** If it does
  not, the central verification of this stage is vacuous unless the setting is
  forced. This is the single most important thing to establish first.
- **Whether iOS ATS permits cleartext to an RFC1918 IP literal** under
  `NSAllowsLocalNetworking: true`. Apple's documentation names unqualified
  hostnames and `.local`; it does not name private-range literals. Untested here.
- **Whether `react-native-blob-util` works under RN 0.87's New Architecture.**
  Unverified pairing; it gates **[DECIDE 3]**.
- **Whether AVPlayer honours `X-TIMESTAMP-MAP` the way the spec says.** It
  should. ffmpeg demonstrably does not, which is why 6A could not find out.
- **Real-device video sizes.** The 5 MiB part size and "4-5 parts" figure come
  from a 1080p bitrate estimate, not from a measured file off this phone. If
  clips are 4K the part count and upload time both change materially.
- **Whether anything in `mobile/` still builds.** The last commit touching it is
  `5ce5a4e` (Stage 2A/2B). It has not been run since, and RN 0.87 with React
  19.2 is a recent stack.
