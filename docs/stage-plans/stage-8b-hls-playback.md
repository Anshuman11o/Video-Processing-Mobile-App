# Stage 8B: HLS Playback

> Status: **draft — not approved.** Six decisions open: **[DECIDE 1]**–**[DECIDE 6]**.
> Written 2026-08-13 alongside `stage-8a-background-upload.md`.
>
> **[DECIDE 1] is a gate, not a preference.** If `react-native-video` v6 does not
> work under RN 0.87's New Architecture, the stage does not proceed on its
> recommended path and every estimate below is void. Answer it first, cheaply.
>
> The Android toolchain question — **no SDK, 2.8 GB free disk** — is
> **[DECIDE 7] in stage 8A**. It is not restated here. **8B is blocked on it
> completely**, where 8A is blocked on it only from its Phase 5.

## Aim

Play a finished reel — video and captions — inside the DayReel app, from the
`hls_url` the pipeline has been producing since 6A and that Stage 7 currently
only displays as text.

This is the last unproven link in the chain. The pipeline produces HLS; ffmpeg
and `ffprobe` have read it back (6A); no client has ever played it.

## What this stage is actually testing

Not `react-native-video`. That library works; thousands of apps use it.

**It is testing whether 6A's output is correct**, and the two ways this project
has repeatedly been bitten both live here:

1. **A player showing no captions looks exactly like a clip with none.** 6A said
   this in its own subagent notes and it is still true. Every caption check in
   this plan must inspect the *track picker*, not just the video surface.
2. **6A's known caption defect has never been measured by a real player.** The
   candidate fix (`X-TIMESTAMP-MAP`) was unverifiable in 6A because ffmpeg
   ignores that header entirely — four different values produced byte-identical
   output. This stage is where the oracle finally exists. See **[DECIDE 3]**.

## The finding that de-risks this stage: a real-player oracle exists TODAY

Stage 7 **[DECIDE 7]** planned to measure caption sync in **Safari on the iOS
Simulator**. The platform switch to Android took that oracle away, and nothing
replaced it.

But the HLS URL is not like the upload URL, and the difference matters:

| | Upload URL | HLS URL |
|---|---|---|
| Signed? | Yes, SigV4 covers the `Host` header | **No.** Plain URL into a public-read bucket |
| Host rewritable by hand? | **No** — rewriting invalidates the signature | **Yes** |
| Segment references | n/a | **Relative paths** — verified in `backend/internal/media/playlist.go` (`Rendition.PlaylistPath()` emits `480p/playlist.m3u8`; `subs.URI` is `subs/playlist.m3u8`) |

Because segment URIs are relative, they resolve against whatever base the player
loaded the master from. So this should work from the host **right now**, with no
emulator and no SDK:

```bash
JOB=<a completed job id>
open -a Safari "http://localhost:4566/dayreel-hls-output/$JOB/master.m3u8"
```

Safari on macOS plays HLS through AVFoundation and exposes a caption track
picker. `docs/SETUP.md` records that LocalStack serves unsigned GETs regardless
of policy, and `init-aws.sh` additionally grants public read on
`dayreel-hls-output`, so no credentials are involved.

**Status: ASSUMED, not verified.** It has not been run. It costs one command to
find out, and if it works it means:

- Stage 7's caption measurement (**[DECIDE 7]** there) is **unblocked today**,
  before any toolchain exists.
- **[DECIDE 3]** below can be answered with data rather than deferred.
- 8B's own risk drops sharply, because a playlist that plays in AVFoundation is
  very unlikely to be malformed for ExoPlayer.

It does **not** replace the emulator test — AVFoundation and ExoPlayer disagree
about edge cases, which is half of why **[DECIDE 3]** exists — but it converts
the caption question from "blocked on Android" to "answerable now."

## Components

| Component | Action |
|---|---|
| `mobile/package.json` | Modify — `react-native-video` (**[DECIDE 1]**) |
| `mobile/src/screens/PlayerScreen.tsx` | Modify — replace the placeholder with a real player |
| `mobile/src/screens/JobListScreen.tsx` | Modify — a play affordance on completed jobs |
| `mobile/src/api/client.ts` | **No change** — `getReel` already exists |
| `mobile/src/hooks/` | Possibly modify — fetch the reel on focus |
| `mobile/android/app/src/main/res/xml/network_security_config.xml` | Create — **only under [DECIDE 5]**(b) |
| `backend/internal/media/hls.go`, `subtitles` | **Deliberately not touched** — the caption fix belongs to a 6B (**[DECIDE 3]**) |
| `infra/localstack/init-aws.sh` | Possibly modify — **[DECIDE 2]**, only if the access model changes |

## Boundaries

### Inbound: `GET /jobs/{id}/reel`

`handlers.go:242-266`. Returns 200 only when `job.Status == completed` **and**
`job.Output != nil` (`handlers.go:256`); otherwise 409 `NOT_READY`.

```json
{
  "job_id": "550e8400-...",
  "hls_url": "http://10.0.2.2:4566/dayreel-hls-output/550e8400-.../master.m3u8",
  "thumbnail_url": "http://10.0.2.2:4566/dayreel-processed/550e8400-.../frames/frame_001.jpg"
}
```

Two things to note about this response, both verified:

1. **It does not consult the Redis cache** (unlike `GET /jobs/{id}`), so it and
   the job status can disagree for up to the cache TTL. A UI that shows a play
   button on `status == completed` can therefore offer playback slightly before
   `/reel` returns 200. Handle the 409 as "not ready yet, retry," not as an error.
2. **`thumbnail_url` points into `dayreel-processed`, which has no public-read
   policy.** `init-aws.sh` grants `s3:GetObject` to `*` on
   `dayreel-hls-output` **only**. `packager.go:279` builds the thumbnail URL with
   the same `publicURL` helper as the HLS URL (`packager.go:275`), against
   `s.inputBucket`. It works locally purely because LocalStack serves unsigned
   GETs to any bucket. **On real S3 it would 403.** See **[DECIDE 2]**.

### The HLS output being played

Written by 6A. Structure, verified from `media/playlist.go` and 6A's
verification section:

```
dayreel-hls-output/{job_id}/
  master.m3u8            # EXT-X-VERSION:6, EXT-X-INDEPENDENT-SEGMENTS
                         # EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID="subs",
                         #   NAME="English",LANGUAGE="en",
                         #   DEFAULT=YES,AUTOSELECT=YES,FORCED=NO,
                         #   URI="subs/playlist.m3u8"
                         # one EXT-X-STREAM-INF per rendition, each carrying
                         #   SUBTITLES="subs"
  480p/playlist.m3u8 + segment_NNN.ts
  360p/playlist.m3u8 + segment_NNN.ts
  subs/playlist.m3u8 + subs_000.vtt
```

`DEFAULT=YES` and `AUTOSELECT=YES` come from `packager.go:191-197`
(`Default: true`). The playlist therefore *asks* for captions on by default —
whether ExoPlayer honours that is **[DECIDE 4]**.

Rungs are source-dependent: 6A verified a 640×480 source produces **only** 480p
and 360p, no upscaled 720p. So the ladder a test job produces may have two
variants, not three.

### Outbound

Nothing. 8B writes no S3 object, publishes no message, and changes no DynamoDB
item. It is the only stage in this project that is purely a consumer.

---

### [DECIDE 1] — the player library, and the New Architecture gate

**Answer this before anything else in the stage.** It is a build, not a debate.

`mobile/package.json` has **no video library** (verified). `gradle.properties`
has `newArchEnabled=true` and React Native is `0.87.0` with React `19.2.3`.

**The unknown, stated plainly: `react-native-video` v6's support for RN 0.87 with
`newArchEnabled=true` has not been checked in this environment or anywhere else
in this project.** v6 ships a Fabric/TurboModule implementation and is the
library the RN ecosystem standardises on, so the expectation is that it works —
but RN 0.87 is recent, and "expected to work" is exactly the phrasing that has
preceded three separate silent failures in this project.

#### Options

**(a) `react-native-video` v6.** ExoPlayer (Media3) on Android. Handles
`EXT-X-MEDIA:TYPE=SUBTITLES` renditions natively and exposes `selectedTextTrack`
/ `onTextTracks` for picking one.
- The only option that reads the subtitle rendition **through the master
  playlist**, which is the entire reason 6A **[DECIDE 2]** hand-assembled it.
- One native dependency, autolinked, needing a Gradle build that has never run
  here.
- **Gated on the New Architecture question.**

**(b) `expo-video`.** Modern, well-maintained.
- Requires `expo-modules-core` bolted into a bare RN app (`npx install-expo-modules`),
  which rewrites `MainApplication.kt`, `build.gradle` and the Podfile-equivalent
  wiring. On an app that has never built once, adding an entire module system to
  get a video view is a large blast radius for a small feature.

**(c) `react-native-vlc-media-player`.** Tolerant of odd streams — genuinely
useful if 6A's output turns out to be subtly malformed.
- **A very large binary.** libVLC adds tens of MB per ABI, and
  `reactNativeArchitectures` currently lists **four** ABIs
  (`armeabi-v7a,arm64-v8a,x86,x86_64`). Against **2.8 GB free host disk**
  (8A **[DECIDE 7]**) that is not a footnote — it is potentially the difference
  between a build completing and not.
- Caption-track APIs are less aligned with HLS subtitle renditions.

**(d) A `WebView` with `<video>` or hls.js.** Android System WebView's Chromium
supports HLS natively.
- Zero native dependencies and, notably, **not affected by the New Architecture
  question at all**.
- Loses native fullscreen, background audio, and precise track control; adds a
  cleartext-in-WebView problem of its own (`mixed content` rules, and the WebView
  honours the same `usesCleartextTraffic` policy as **[DECIDE 5]**).
- Recorded as the **escape hatch** if (a) fails and neither (b) nor (c) is
  palatable. It would still prove the playlist is playable, which is the actual
  question this stage asks.

#### Recommendation: **(a), behind an explicit build gate as the first task of the stage.**

The gate, concretely:

```
1. npm i react-native-video
2. npx react-native run-android          # unmodified app, no player code yet
3. If it builds and launches: proceed.
   If it fails: capture the exact error and STOP. Do not write player code
   against a dependency that does not compile.
```

Fallback order if the gate fails: **(d) WebView** to prove the stream is playable
and unblock the caption measurement, then reassess (b) versus (c) with the actual
error in hand. (c) is last on disk cost alone.

A second, smaller unknown worth capturing at the same time: `react-native-video`
v6 pulls a **Media3/ExoPlayer** version with its own `compileSdk` and AGP
expectations. This project pins `compileSdk 37`, `buildTools 37.0.0`, Kotlin
2.2.0 and Gradle 9.4.1 — a combination newer than most published compatibility
matrices. If the gate fails, read the error before assuming it is about the New
Architecture.

---

### [DECIDE 2] — how the app is allowed to fetch the segments

6A **[DECIDE 4]** resolved this **locally only** and explicitly left the real-AWS
access model open: *"an open question that Stage 7 or any deployment work must
answer."* Stage 7 did not answer it, correctly, because it only displayed a URL.
**8B is the first stage that actually fetches the bytes, so it is the first stage
that has to look at this.**

The state today, verified:

- `init-aws.sh` applies a public-read `s3:GetObject` policy to
  `dayreel-hls-output` for `Principal: "*"`, and CORS for `GET`/`HEAD`.
- `dayreel-processed` gets **CORS on `dayreel-raw-videos` only** and **no policy
  at all** — yet `thumbnail_url` points into it (`packager.go:279`).
- `docs/SETUP.md` records that LocalStack serves **unsigned requests to any
  bucket** with 200, which means **local success proves nothing about either
  bucket's real access model.**

Presigning is ruled out on technical grounds, not preference: HLS playlists
reference segments by relative path (verified in `playlist.go`), so a presigned
master is followed by 403s on every segment.

#### Options

**(a) Keep public-read on `dayreel-hls-output`, plain URLs; fix the thumbnail
separately.** What the local stack does today.
- Zero work for playback. It is also what CloudFront sits in front of in
  production, so it is not throwaway.
- The **thumbnail is still broken on real AWS** and needs its own answer:
  extend the policy to `dayreel-processed/*/frames/*`, presign the thumbnail
  (a single object, so presigning genuinely works here unlike for HLS), or drop
  the thumbnail from the UI.
- A public bucket on a real account is both a cost (open egress) and an exposure
  (`config/free-tier.md`).

**(b) Presign the thumbnail, leave HLS public.** Splits the two correctly: one
object can be presigned, a playlist tree cannot.
- Requires a small API change (`GET /jobs/{id}/reel` returning a presigned
  `thumbnail_url`), which is arguably a 2A/6A concern rather than 8B's.

**(c) Serve HLS through the API.** A proxy endpoint streaming from S3.
- Avoids public objects entirely, and directly contradicts `stage-2a-go-api.md`'s
  *"No video bytes through API"* — a principle held for six stages.
- Also puts every segment request through a Go handler, and HLS is many small
  requests by design.

**(d) CloudFront + Origin Access Control.** The production answer.
- **Not available in LocalStack Community** (`config/free-tier.md` parity table),
  so it cannot be built or tested here at all. `PROJECT_PLAN.md`'s Stage 8B says
  "Play completed reels from CloudFront/LocalStack" — the CloudFront half of that
  sentence is **not testable in this project as configured**, and should be read
  as aspirational rather than as a deliverable.

**(e) Rewrite every segment URI in the playlists to presigned URLs at packaging
time.** The only scheme that avoids both a public bucket and a proxy.
- Technically possible — the packager already writes the master by hand.
- Bakes an expiry into stored content: the playlist becomes a time bomb, cannot
  be cached, and must be regenerated per viewer. Recorded so it is not
  re-proposed as clever.

#### Recommendation: **(a) for playback + (b) for the thumbnail, and do NOT let 8B become the access-model stage.**

Concretely for 8B:

- Change nothing about `dayreel-hls-output`. It already works locally and it is
  the correct CloudFront origin shape.
- **Write down the thumbnail finding** — it is a real bug that only LocalStack's
  permissiveness is hiding — and decide whether 8B fixes it or defers it. If
  `PlayerScreen` shows a poster image, 8B is *relying* on it and should fix it.
  If it does not, defer with the finding recorded.
- **Record explicitly that the real-AWS access model is still open**, now for the
  third stage running, and that (d) is the intended production answer that cannot
  be tested here.

The tension worth naming: this recommendation defers a question that has now been
deferred by 6A, 7 and 8B. That is defensible — none of those stages deploys
anything — but it should be an *explicit* deferral each time, not an inherited
silence.

---

### [DECIDE 3] — 6A's caption defect: measure here, fix where?

The defect, from `stage-6a-package-worker.md`'s known-defect section: **the first
caption cue is dropped entirely, and every other cue lands ~112 ms early**,
because subtitle timings are offset against the MPEG-TS start PTS. Observed: the
mock's `0→3s` cue is absent and its `3→6s` cue surfaces at `00:02.888`.

`-muxdelay 0 -muxpreload 0` already cut this from ~1.4 s to ~0.07 s. The residual
is small but it reliably eats a cue starting at t=0, and the mock transcript
always starts one there (`MOCK_TRANSCRIBE=true` is still the default).

**Why it is unresolved: ffmpeg ignores `X-TIMESTAMP-MAP` entirely.** 6A tried four
different values and got byte-identical output. Real players honour it. So the
candidate fix could not be evaluated with the only tool 6A had.

#### The four candidate fixes, carried forward unchanged

- **(a) Do nothing.** Legitimate *if* a real player shows correct sync — that
  would mean the defect is in ffmpeg's **reader**, not in the output, and 6A's
  observation was an artefact of its instrument.
- **(b) Emit `X-TIMESTAMP-MAP`** in the VTT with `MPEGTS` derived from the real
  start PTS. The spec-sanctioned mechanism; the reason it was unverifiable
  disappears the moment a real player is in the loop.
- **(c) Shift every cue timestamp by the measured offset.** Player-independent,
  but bakes an environment-specific constant into content.
- **(d) Force the TS start PTS to zero** via `-output_ts_offset` /
  `-avoid_negative_ts`. Removes the offset rather than describing it.

#### The scope question, which is the actual decision

The **measurement** belongs to 8B — it owns the first real player. The **fix**
edits `backend/internal/media/`, which is 6A's code. Stage 7 **[DECIDE 7]** already
flagged this: *"arguably its own follow-up stage (6B), rather than expanding an
integration stage into backend media work."*

- **(i) 8B measures; any fix becomes a Stage 6B.** Keeps 8B a playback stage.
  Costs one more stage document and one more round trip through the pipeline to
  re-verify.
- **(ii) 8B measures and fixes inline.** Fastest to a correct demo. Expands a
  client stage into ffmpeg flag work, into a `media` package with existing tests,
  and mixes two very different failure modes in one commit.
- **(iii) Measure, record, ship the defect.** Honest, and the caption is 112 ms
  early on a mock transcript that reads `[mock transcript] segment N`. Nobody
  watching the demo can perceive 112 ms; the *dropped first cue* is perceptible,
  though, and it is the more damaging half.

#### Recommendation: **(i) — measure in 8B, fix in a 6B, with (iii) as the fallback if the measurement says the impact is cosmetic.**

And critically: **the measurement is not blocked on Android.** Per the finding at
the top of this document, Safari on macOS can play
`http://localhost:4566/dayreel-hls-output/{job}/master.m3u8` today. Do that
**first**, before the toolchain question is even resolved. Two oracles are better
than one, and AVFoundation-vs-ExoPlayer disagreement is itself a finding worth
having.

The measurement protocol, so it produces a number rather than an impression:

1. Run a job with `MOCK_TRANSCRIBE=true` — its cue at t=0 is precisely what
   exposes the defect.
2. `awslocal s3 cp s3://dayreel-hls-output/$JOB/subs/subs_000.vtt -` and record
   the **authored** cue times.
3. Play in Safari (host) and in ExoPlayer (emulator). For each: is the t=0 cue
   present? At what wall-clock position does the second cue appear?
4. **Write the offset down as a number in `stage-6a-package-worker.md`'s
   known-defect section**, per player. "Seems fine" is not a result.
5. Only then choose among (a)–(d).

---

### [DECIDE 4] — captions may simply not appear, and that will look like a broken playlist

The trap: **a player showing no captions is visually identical to a clip with no
captions.** 6A wrote this down; it is worth acting on rather than re-learning.

The specific Android mechanism: ExoPlayer's `DefaultTrackSelector` decides text
track selection from `TrackSelectionParameters`, which by default derive from
the device's system captioning preference (`CaptioningManager`) and
`preferredTextLanguage`. **A fresh Android emulator has captions disabled
system-wide.** `DEFAULT=YES,AUTOSELECT=YES` in the master (which
`packager.go:196` does set) expresses the content's intent, but whether ExoPlayer
auto-selects the track anyway on a device with captions off is **UNKNOWN and must
be tested, not reasoned about.**

If it does not, the observable symptom is: **video plays, no captions, no error,
no warning.** Which is indistinguishable from a malformed subtitle rendition —
i.e. it would look exactly like a 6A bug that isn't there.

#### Options

**(a) Leave `react-native-video` defaults alone.** Whatever ExoPlayer decides.
- Most "correct" in the sense of respecting user accessibility preferences.
- Makes the stage's central verification depend on an emulator setting nobody
  remembered to change.

**(b) Force the text track explicitly**:
`selectedTextTrack={{type: 'index', value: 0}}`.
- Deterministic. Captions are on, always, and a blank caption area is then
  genuinely evidence of a problem.
- Overrides user preference, which is wrong for a shipping app and fine for a
  demo.

**(c) `selectedTextTrack={{type: 'language', value: 'en'}}`.** Matches the
`LANGUAGE="en"` the playlist declares.
- Selects by intent rather than position, and survives a playlist gaining a
  second language.
- Fails silently if the language tag ever drifts from `en`.

**(d) (c) plus a UI toggle** driven by `onTextTracks`, so the track list is
*visible* in the app.
- The version that actually distinguishes the three states this project keeps
  confusing: no track exists / a track exists but is not selected / a track is
  selected but has no cues.

#### Recommendation: **(d) — select by language, and render the track list.**

The track list is the part that matters. It costs a `<Text>` rendering
`onTextTracks`' payload and it turns "captions didn't show up" from a mystery
into a three-way diagnosis. That is worth more in this project than a polished
caption toggle.

Also required, and separately easy to miss: **`react-native-video` on Android
needs `subtitleStyle` configured for captions to be legible** (white on white is
a real outcome), and a **cue-less `WEBVTT`** — which the silent-clip path
produces and 6A verified is emitted — must not crash the player. 6A verified the
file is valid; **nothing has ever verified a client handles it.**

---

### [DECIDE 5] — cleartext HTTP to `10.0.2.2:4566`, and what happens in a release build

This is where an assumption in the stage briefing needs correcting, because the
correction changes what work is needed.

**The claim:** ExoPlayer over cleartext HTTP to `10.0.2.2:4566` will be blocked by
Android's default network security config on API 28+.

**What is actually true here, verified:**

- `mobile/android/app/src/main/AndroidManifest.xml:12` reads
  `android:usesCleartextTraffic="${usesCleartextTraffic}"` — a placeholder, with
  no `manifestPlaceholders` block anywhere in `mobile/android/`.
- The placeholder is supplied by the React Native Gradle Plugin, not by this
  repo. `mobile/node_modules/@react-native/gradle-plugin/react-native-gradle-plugin/src/main/kotlin/com/facebook/react/utils/AgpConfiguratorUtils.kt:34-46`
  sets it to `"true"` for `debug` and `debugOptimized`, and **`"false"` for
  `release`**.

So: **a debug build permits cleartext app-wide, and `npx react-native run-android`
builds debug.** Playback to `http://10.0.2.2:4566` should work out of the box.
**A release build sets it to `false` and every segment fetch fails.**

The blocker is real; it just lands on a build variant this project may never
produce. That is worth deciding deliberately rather than discovering.

#### Options

**(a) Rely on the RN plugin's debug placeholder. Change nothing.**
- Zero work, and correct for every build this project will actually make.
- A release build silently loses all playback **and all API calls and all
  uploads** — the same policy governs the whole app, not just ExoPlayer.

**(b) Add `res/xml/network_security_config.xml`** with a cleartext-permitted
domain entry for `10.0.2.2` (and `localhost`), referenced from the manifest via
`android:networkSecurityConfig`.
- Explicit, narrow, and works in both variants.
- **`networkSecurityConfig` takes precedence over `usesCleartextTraffic`**, so
  adding it silently changes debug behaviour too — anything not listed becomes
  blocked. If the LAN-IP path is ever used (8A **[DECIDE 7]**(c), a physical
  device), the config needs that address as well or it breaks in a way that looks
  like a server problem.
- ~15 lines and one manifest attribute.

**(c) `android:usesCleartextTraffic="true"` hardcoded.** Overrides the
placeholder in both variants.
- One line, and it makes the app permanently cleartext-permissive — the opposite
  of the direction Android is moving.

#### Recommendation: **(a), with the release-build consequence written into `docs/SETUP.md`.**

The gap is real and it is unreachable: nothing in this project builds a release
APK, and `config/free-tier.md` says the project closes after a handful of runs.
Adding (b) means adding a file whose precedence rules can break the working debug
path, to protect a build variant that will not be produced.

**But it must be documented**, next to the existing "one client environment at a
time" section, because "the app works in dev and every network call fails in
release" is exactly the shape of silent failure this project keeps hitting.

If a release build is ever wanted, take (b) at that point — not now.

---

### [DECIDE 6] — what "verified" means for this stage if the emulator never runs

8B is blocked, in full, on 8A **[DECIDE 7]** (no Android SDK; 2.8 GB free disk).
That is a scheduled, plausible outcome, not a remote one — so the stage needs a
position on it in advance rather than an improvised one at the wall.

#### Options

**(a) 8B does not start until the toolchain exists.** Clean, and it means the
stage's evidence is uniformly device-based.
- Leaves the caption question — the genuinely valuable finding — unanswered for
  as long as the disk is full, even though it is answerable today.

**(b) Split 8B into 8B-0 (host-side playback evidence) and 8B-1 (the app).**
8B-0 runs the Safari/VLC/ffplay measurement from the host, answers **[DECIDE 3]**,
and produces a written offset per player. 8B-1 is the `react-native-video`
integration and waits on the toolchain.
- Extracts real value from a blocked stage.
- Host-side evidence is **not** evidence about ExoPlayer, and must not be
  recorded as if it were.

**(c) Ship 8B against the WebView escape hatch (**[DECIDE 1]**(d)).** Still needs
the toolchain — it is an Android app either way. Not actually an unblocking
option; recorded to prevent it being mistaken for one.

**(d) Declare the stage done on host-side evidence.** No. A playback stage whose
player was never run is the same category of green-happy-path failure that 6A and
7 both had to go back and repair.

#### Recommendation: **(b) — split it.**

Do 8B-0 immediately: it is one `open -a Safari` command, one VTT dump, and a
number written into 6A's plan. It costs minutes, it unblocks **[DECIDE 3]**, and
it retires the single largest piece of uncertainty about whether 6A's output is
actually good.

Do 8B-1 when the toolchain exists. **Do not mark 8B complete until ExoPlayer has
played the stream on the emulator** — the whole point of the stage is the client,
and AVFoundation agreeing tells us about the playlist, not about the app.

---

## Files

| File | Action | Purpose |
|---|---|---|
| `mobile/package.json` | Modify | `react-native-video` (**[DECIDE 1]**) |
| `mobile/src/screens/PlayerScreen.tsx` | Modify | Real player; track list; error and loading states |
| `mobile/src/screens/JobListScreen.tsx` | Modify | Play affordance on `completed` jobs only |
| `mobile/src/hooks/useReel.ts` | Create — **or skip** | Stage 7's `PlayerScreen.tsx` already calls `getReel` inline (`:42`). Extract only if the retry-on-409 logic makes it worth a hook |
| `mobile/src/types/api.ts` | Modify | Only if the reel response changes under **[DECIDE 2]**(b) |
| `mobile/android/app/src/main/res/xml/network_security_config.xml` | Create | **Only under [DECIDE 5]**(b). Not recommended |
| `docs/SETUP.md` | Modify | Release-build cleartext consequence; playback caveats |
| `docs/stage-plans/stage-6a-package-worker.md` | Modify | **Record the measured caption offset**, per player, as a number |
| `mobile/CONTEXT.md` | Modify | Still describes the Stage 2B state and calls the player "Stage 6". `PlayerScreen.tsx` was corrected by Stage 7; this file was not |

## Tasks

**8B-0 — host-side, unblocked today**

1. [ ] Run a job to `completed`; capture `hls_url`
2. [ ] Play `http://localhost:4566/dayreel-hls-output/$JOB/master.m3u8` in Safari
       on the host. Record whether it plays at all
3. [ ] Caption measurement per the **[DECIDE 3]** protocol; write the number into
       `stage-6a-package-worker.md`
4. [ ] Repeat with a **silent clip** (empty `WEBVTT`, no cues) — does the player
       survive it?
5. [ ] Decide **[DECIDE 3]** on the evidence

**8B-1 — the app. Blocked on 8A [DECIDE 7]**

6. [ ] **THE GATE**: `npm i react-native-video` → `npx react-native run-android`
       on an **unmodified** app. Build only. Stop here if it fails
7. [ ] `PlayerScreen.tsx`: `<Video>` on the reel URL
8. [ ] Track list rendering + `selectedTextTrack` (**[DECIDE 4]**)
9. [ ] `subtitleStyle` so captions are legible
10. [ ] `JobListScreen.tsx`: play affordance
11. [ ] Loading / error / 409-retry states
12. [ ] Full E2E on the emulator: pick → upload → process → play with captions
13. [ ] Failure paths — all of them
14. [ ] `docs/SETUP.md`, `mobile/CONTEXT.md`, results into this file

## Test

```bash
# ── 8B-0: host-side, no emulator, no SDK ──────────────────────────────────────
cd infra && docker compose up -d
JOB=<run a job, or reuse a completed one>

curl -s localhost:8080/jobs/$JOB/reel | jq
# hls_url will say 10.0.2.2 (the committed Android target). The HLS URL is
# UNSIGNED, so unlike an upload URL the host may be rewritten freely.

docker exec dayreel-localstack awslocal s3 cp \
  s3://dayreel-hls-output/$JOB/master.m3u8 -
# EXPECT: EXT-X-MEDIA ... SUBTITLES group, and SUBTITLES="subs" on EVERY
# EXT-X-STREAM-INF. If one variant lacks it, captions vanish on a bitrate switch.

docker exec dayreel-localstack awslocal s3 cp \
  s3://dayreel-hls-output/$JOB/subs/subs_000.vtt -
# Record the AUTHORED cue times. This is the reference the player is measured
# against — without it, "the captions look right" is unfalsifiable.

open -a Safari "http://localhost:4566/dayreel-hls-output/$JOB/master.m3u8"
# 1. Does it play?
# 2. Open the caption track picker. Is "English" listed?
# 3. Is the cue at t=0 present?
# 4. At what position does the SECOND cue appear? Write the number down.

# Second opinion (host ffmpeg is 8.0; the container's 6.1.1 cannot read
# subtitle renditions at all — 6A established this):
ffplay "http://localhost:4566/dayreel-hls-output/$JOB/master.m3u8"

# ── 8B-1: the app. Blocked on 8A [DECIDE 7]. ──────────────────────────────────
cd ../mobile
npm i react-native-video
npx react-native run-android      # THE GATE. Build BEFORE writing player code.
```

## Verification

_Nothing checked off until observed._

**The gate**

- [ ] The app builds and launches on the emulator with `react-native-video`
      installed and **no player code written yet** — so a later failure is
      attributable to the code, not the dependency

**Playback**

- [ ] A completed job shows a play affordance; a `processing` or `failed` job
      does not
- [ ] Tapping it plays the HLS stream in the app
- [ ] Video renders — not a black surface with audio, which is the usual shape of
      a Fabric/New Architecture view-mounting failure
- [ ] Audio plays
- [ ] Seeking works and does not desync captions
- [ ] `GET /jobs/{id}/reel` returning **409** is handled as "not ready, retry",
      not surfaced as an error — the reel endpoint bypasses the Redis cache
      (`handlers.go:245`) while `GET /jobs/{id}` does not, so the two can disagree

**Captions — the reason this stage matters**

- [ ] The caption **track picker lists "English"** — checked in the picker, not
      inferred from the video surface
- [ ] Captions are actually rendered, and are legible (`subtitleStyle`)
- [ ] **The cue at t=0 is present, or confirmed dropped.** State which
- [ ] **The measured offset, written as a number**, for ExoPlayer and for Safari
      separately. Not "seems fine"
- [ ] The two players agree, or the disagreement is recorded — it is itself a
      finding about **[DECIDE 3]**
- [ ] Result recorded in `stage-6a-package-worker.md`'s known-defect section
- [ ] If a fix is applied (in a 6B), the same measurement is repeated **after** —
      the comparison is the entire point

**Failure and edge paths**

- [ ] **A silent clip** (empty `WEBVTT`, zero cues): the player does not crash,
      the track may or may not be listed, and playback still works. 6A verified
      the VTT is valid; **no client has ever consumed one**
- [ ] **A two-rung ladder.** 6A verified a 640×480 source yields only 480p/360p.
      The player must not assume three variants
- [ ] **A 404 master** (LocalStack restarted — `PERSISTENCE=1` is silently
      ignored in Community, so this happens): a clear error, not an infinite
      spinner
- [ ] **Navigating away mid-playback** releases the player — no audio continuing
      over the job list, no leaked ExoPlayer instance
- [ ] **Backgrounding during playback** and returning
- [ ] **A job that failed** never offers playback

**Access model**

- [ ] Segments load from the app with no credentials — confirming the public-read
      path works end to end
- [ ] **`thumbnail_url` is fetched (or explicitly not).** If `PlayerScreen` shows
      a poster, record that it points at `dayreel-processed`, which has **no**
      public-read policy and works locally only because LocalStack serves
      unsigned GETs. **This is a real bug hidden by LocalStack** (**[DECIDE 2]**)

**Explicitly NOT verified, and why**

- [ ] **Anything about real AWS.** LocalStack serves unsigned GETs to any bucket,
      so local playback success says nothing about whether either bucket would be
      readable in S3. The same trap 6A flagged for bucket policies
- [ ] **CloudFront.** `PROJECT_PLAN.md`'s Stage 8B names it; it is Pro-only in
      LocalStack (`config/free-tier.md`) and cannot be exercised here at all
- [ ] **Release-build networking.** `usesCleartextTraffic` is `false` for release
      (**[DECIDE 5]**) and no release build is produced

## Claude Code Implementation Plan

### Recommended Approach: Answer the Caption Question on the Host, Then Gate on the Build

Two tracks with wildly different costs, and interleaving them wastes the cheap
one:

1. **Host-side playback evidence (8B-0).** Minutes. No SDK, no emulator, no disk.
   Answers **[DECIDE 3]**, which is the highest-value uncertainty in the stage.
2. **The Android integration (8B-1).** Blocked on 8A **[DECIDE 7]**, and itself
   gated on **[DECIDE 1]**'s build.

Do (1) first regardless of when (2) becomes possible. Then, inside (2), **build
before you write**: install `react-native-video` and run the app unmodified. A
player component written against a dependency that will not compile is a pure
loss, and this is the same discipline Stage 7 applied to `pod install`.

### Pre-Flight Check

```
0a. docker compose ps                         # 6 containers healthy
0b. curl -s localhost:8080/health
0c. A job at status "completed" with output populated. If none, run one —
    6A measured the whole pipeline at ~4 seconds.
0d. curl -s localhost:8080/jobs/$JOB/reel     # EXPECT 200, not 409
0e. df -h /System/Volumes/Data                # 2.8GB at plan time. 8A [DECIDE 7]
0f. docker system df                          # 4.344GB images, 2.302GB cache
0g. Confirm 8A [DECIDE 7]. Everything from phase 3 on is blocked on it.
0h. Confirm [DECIDE 1]. Phase 3 IS the confirmation.
0i. Is Stage 7's mobile half MERGED, not just present in the working tree?
    As of 2026-08-13 PlayerScreen.tsx already fetches getReel (:42) and renders
    the hls_url (:105) with "Playback arrives in Stage 8B" (:67) — i.e. Stage 7
    has built exactly the seam 8B replaces, and it was still uncommitted.
    Editing it concurrently with the main session will conflict.
```

### Execution Steps

```
Phase 1: Host-side playback evidence   <-- NO TOOLCHAIN NEEDED. DO THIS FIRST.
1.  Dump master.m3u8 and subs_000.vtt; record authored cue times
2.  Play the master in Safari on the host with the URL rewritten to localhost
3.  Caption measurement per DECIDE 3's protocol. WRITE THE NUMBER DOWN.
4.  Repeat with a silent clip (empty WEBVTT)
5.  Record results in stage-6a-package-worker.md
6.  COMMIT the doc update.

Phase 2: Decide   <-- gate, not work
7.  Answer DECIDE 3 on the evidence from phase 1. If a fix is needed, open a
    6B rather than growing this stage into backend media work.

=== BLOCKED ON 8A [DECIDE 7] BELOW THIS LINE ===

Phase 3: THE BUILD GATE   <-- build before writing
8.  npm i react-native-video
9.  npx react-native run-android      # UNMODIFIED app
10. If it fails: capture the exact error, STOP, and re-open DECIDE 1.
    Do not write player code against a dependency that does not compile.
11. COMMIT the dependency separately, so a later bisect can tell the build
    change from the feature.

Phase 4: The player (parallel writes)
12a. src/hooks/useReel.ts             (409 => retry, not error)
12b. src/screens/PlayerScreen.tsx     (<Video>, track list, subtitleStyle)
12c. src/screens/JobListScreen.tsx    (play affordance on completed only)
13.  npx tsc --noEmit

Phase 5: On the emulator
14. Play. Check the TRACK PICKER, not just the video surface.
15. Caption offset in ExoPlayer; compare against phase 1's Safari number.
16. EVERY failure path in Verification. The silent clip and the 404 master
    especially — both are reachable today and neither has a client test.
17. docs/SETUP.md, mobile/CONTEXT.md, results into this file
```

### Parallel Opportunities

| Phase | Parallel files |
|---|---|
| 4 | `useReel.ts`, `PlayerScreen.tsx`, `JobListScreen.tsx` |

That is the whole list, and it is honest: this is a small stage wrapped around
two sequential gates. Phases 1 → 2 → 3 are strictly ordered and each can end the
stage.

**`PlayerScreen.tsx` and `JobListScreen.tsx` are Stage 7's files.** Do not start
Phase 4 while the main session is still editing them.

### Subagents

- **Worth an agent: the [DECIDE 1] build gate.** `npm i react-native-video` plus a
  first Android build is slow, extremely verbose, and has a long tail of Gradle
  and AGP failures unrelated to anything in this plan. Same argument 2B made for
  `react-native init` and Stage 7 made for `pod install`. **Require the exact
  command and the exact error text, not a conclusion** — "it works" from an agent
  that did not actually launch the app is the most expensive possible wrong
  answer here.
- **Worth an agent: `react-native-video` v6 + RN 0.87 New Architecture
  compatibility research**, run *in parallel* with the build attempt, not instead
  of it. Published matrices, open issues, the Media3 version it pulls. **Research
  informs the fallback; the build decides.**
- **Not worth an agent:** the player component, the hook, the screens. Small, and
  they need to be written by whoever read the reel response shape.

Give any agent an explicit file boundary, the 8 GB Docker ceiling, and the
**2.8 GB host disk** figure — which for this stage is a first-class constraint,
not background (**[DECIDE 1]**(c)'s libVLC option is rejected largely on it).

### Potential Blockers

| Blocker | Resolution |
|---|---|
| **No Android SDK; 2.8 GB free disk** | 8A **[DECIDE 7]**. Blocks 8B-1 completely. **[DECIDE 6]** splits the stage so 8B-0 still delivers |
| **`react-native-video` v6 fails under `newArchEnabled=true`** | **[DECIDE 1]**. Fallback order: WebView (d) → `expo-video` (b) → VLC (c). Read the actual error before assuming it is the New Architecture — `compileSdk 37` / AGP / Kotlin 2.2.0 is an equally plausible culprit |
| **Video plays with no captions** | **[DECIDE 4]**. Indistinguishable from a malformed playlist without the track list. Render `onTextTracks` before debugging the backend |
| **Captions render white-on-white** | `subtitleStyle`. Looks identical to "no captions" |
| **Cleartext blocked** | **[DECIDE 5]**. Debug builds are already permitted by the RN Gradle plugin. If it *does* fail in debug, something else changed and the manifest placeholder is the first thing to inspect |
| **`thumbnail_url` 403s** | **[DECIDE 2]**. Will not happen locally — LocalStack serves unsigned GETs — which is precisely the danger |
| **LocalStack restarted, master is 404** | `PERSISTENCE=1` is silently ignored in Community (`config/free-tier.md`). Re-run the job. The app must show an error, not spin |
| **Merge conflicts with Stage 7's mobile work** | `PlayerScreen.tsx` and `JobListScreen.tsx` are shared. Sequence, do not race |

### Time Estimate

- Phase 1 (host-side playback + caption measurement): **~30 minutes, unblocked
  today**
- Phase 2 (decide): ~10 minutes
- Phase 3 (build gate): ~30 minutes, **very high variance** — a first Android
  build in a repo that has never had one
- Phase 4 (player + screens): ~40 minutes
- Phase 5 (E2E, captions, failure paths): ~40 minutes
- **Total, once unblocked: ~2½ hours**
- **8A [DECIDE 7] (the toolchain): unbounded, and not counted above**

The estimate is small because the stage is small. The risk is not in the size —
it is that two of the five phases are gates that can end the stage, and one of
them is a toolchain that does not exist.

---

## Notes

### Interaction with Stage 8A

Stated in full in `stage-8a-background-upload.md`'s Notes; summarised here so
this document stands alone:

- **8A [DECIDE 7] (Android toolchain) is shared and lives there.** It gates 8B
  entirely.
- **Neither stage depends on the other functionally.** 8B reads `hls_url` from an
  endpoint that has worked since 6A; 8A touches upload only.
- **Both add client code to the same app**, and both are downstream of Stage 7's
  mobile half landing. `PlayerScreen.tsx` is contested between Stage 7 and 8B;
  `mobile/src/upload/` and `mobile/src/storage/` are contested between Stage 7
  and 8A.

### Build order

**Recommended: 8A's backend phases (1–4) → 8B → 8A's client phases (5–7).**

- 8A Phases 1–4 are the only work in either stage that is unblocked today, and
  they close a genuine hole (no way to re-presign an existing upload) that
  nothing else can work around.
- 8B comes next because it is smaller, it is the actual product payoff
  (`PROJECT_PLAN.md`: *"Both 8A and 8B complete = Full E2E demo"*), and it
  produces the first real player in the project — the oracle Stage 7's
  **[DECIDE 7]** needs and that the iOS→Android switch removed.
- 8A's client half is last: most expensive, and its scope is contingent on
  8A **[DECIDE 1]**.

**8B-0 (Phase 1 above) is the single exception and should be done first of all**,
before either stage formally starts. It costs minutes, needs nothing that does
not exist, and answers the oldest open question in the project.

If a strict stage-at-a-time order is mandated: **8A before 8B**, because 8A has
four phases that can start immediately and 8B has one.

### Risks and inherited tensions

- **Inherited from 6A: the caption defect.** ~112 ms early, first cue dropped, fix
  unverifiable server-side. 8B owns the measurement (**[DECIDE 3]**); a 6B should
  own the fix.
- **Inherited from 6A: the real-AWS HLS access model is still open.** Third stage
  running. 8B does not close it and should not pretend to (**[DECIDE 2]**).
- **Inherited from 5A: `MOCK_TRANSCRIBE=true` is the default.** Every caption seen
  in this stage will read `[mock transcript] segment N`. That is *correct* for the
  caption-sync measurement — the mock's cue at t=0 is exactly what exposes the
  defect — and misleading for a demo.
- **Inherited from 2B: the app has never built on Android.** Not once, in this
  repo. Stage 7's mobile code — API client, uploader, job index, polling, and a
  `PlayerScreen` that fetches the reel — has been **written but never executed**.
  8B builds on top of a layer whose every line is unproven.
- **`mobile/CONTEXT.md` still describes the Stage 2B state** and calls the player
  "Stage 6." It is the last place the old numbering survives.
- **A new class of bug this stage can surface and no earlier stage could:**
  playlist-level errors that only a real player rejects. `ffprobe` reading a
  stream is a much weaker statement than ExoPlayer playing it.

### Deliberately not in scope

- **CloudFront**, per `config/free-tier.md`'s parity table — Pro-only in
  LocalStack. `PROJECT_PLAN.md` names it for this stage; it is not buildable here.
- **The caption fix itself.** **[DECIDE 3]**(i) puts it in a 6B.
- **Offline / downloaded playback, DRM, PiP, background audio, casting.**
- **Adaptive-bitrate tuning.** The ladder is 6A's and stays fixed.
- **A release build.** **[DECIDE 5]** turns on this not existing.
- **iOS.** The platform decision is final and Android-only.

### Uncertain, flagged rather than smoothed over

- **Whether `react-native-video` v6 works with RN 0.87 + New Architecture.** The
  stage's gate. Unverified anywhere in this project.
- **Whether Safari on the host can play the rewritten HLS URL.** High confidence,
  never run. One command settles it, and a lot rides on it.
- **Whether ExoPlayer auto-selects a `DEFAULT=YES` subtitle track on a device with
  system captions disabled.** Unknown. If it does not, the symptom is a silently
  caption-free video (**[DECIDE 4]**).
- **Whether AVFoundation and ExoPlayer agree about `X-TIMESTAMP-MAP`.** They
  should; ffmpeg demonstrably ignores it, which is why 6A could not find out.
- **Whether a cue-less `WEBVTT` subtitle rendition is safe in ExoPlayer.** 6A
  verified it is valid WebVTT and valid HLS. Players disagree about edge cases and
  the silent-clip path produces exactly this.
- **Whether anything in `mobile/` builds at all on Android.** The last commit
  touching it before Stage 7 is `5ce5a4e` (Stage 2A/2B). RN 0.87 with React 19.2
  and Gradle 9.4.1 is a recent stack and has never been exercised here.
