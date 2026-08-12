# internal/worker/transcribe

The third pipeline stage. Consumes `dayreel-transcribe`, turns the extracted
audio into a timed WebVTT transcript, and hands off to `dayreel-package`.

## Files

| File | Purpose |
|------|---------|
| `transcribe.go` | The `worker.Stage`: manifest → audio → segments → VTT |
| `transcribe_test.go` | Silent-clip short-circuit and manifest parsing; no model, no I/O |

The model-facing code lives in `internal/transcribe` — the `Transcriber`
interface, the mock, whisper.cpp, and VTT rendering. This package is only the
pipeline stage.

## The input is the manifest, not the audio

`stage-1a-data-schemas.md` originally said this stage reads `audio.wav`. Stage 4A
made `extract.json` its canonical output, so what actually arrives is the
manifest, and the audio key is read from inside it.

The indirection pays for itself: the manifest carries `duration_seconds`, sample
rate and channel count, so nothing here re-probes media to learn what extract
already recorded.

## Non-Obvious Decisions

- **A silent clip must not reach the model.** When `audio.present` is false there
  is no `audio.wav` object at all — extract deliberately writes none. The stage
  short-circuits before downloading anything and renders a cue-less `WEBVTT`,
  which is a valid document. This is an obligation stage 4A created on purpose,
  discharged here rather than discovered as a crash on a missing key.
- **The test asserts this with a spy, not with output.** The defining property is
  that the model does not *run*; an output-only assertion would pass even if it
  ran and returned nothing.
- **Transcription failures are transient.** They are dominated by environmental
  causes — a model still downloading, memory pressure, a killed process — which
  retrying does fix. Genuinely corrupt audio would have failed extract's ffmpeg
  pass one stage earlier.
- **A malformed manifest is permanent.** Valid JSON is a property of the bytes,
  and re-reading the same object cannot produce different JSON.
- **`audio.present: true` with an empty key is permanent too.** That manifest
  should never have been written, and retrying re-reads the same contradiction.
- **The transcriber is built per job**, not once, because the mock needs the clip
  duration and only the manifest knows it.

## Mock mode is the default

`MOCK_TRANSCRIBE=true` is the default in `docker-compose.yml`, and that is
deliberate rather than a testing convenience. `config/free-tier.md` allows only a
handful of real runs before the project closes, and every stage after this one
needs *a* transcript to develop against.

Mock cues are duration-aware — one roughly every three seconds, clipped to the
clip — because a single fixed cue would exercise nothing about cue rendering,
seeking, or overlap in the stages that consume it.

## Traps

- **The whisper.cpp path is a stub that fails loudly.** It is completed in phase 6
  of the stage plan. It fails rather than returning nothing, because an empty
  transcript is indistinguishable from a clip with no speech — the same silent
  failure mode that made `pkt_pts_time` expensive to find in 4A.
- **The model is downloaded at runtime**, so `WHISPER_MODEL_PATH` must point into
  a persistent volume. Without the mount it re-downloads on every fresh
  container. A missing model is transient for the same reason.
- **This stage can outlive the queue's visibility timeout.** The broker has one
  timeout for every queue (`QUEUE_VISIBILITY_TIMEOUT`, 5m) rather than a
  per-queue attribute, so there is nowhere to give this stage a longer lease than
  the others; the runner's heartbeat, which extends the lease every 30s while
  `Process` runs, is the only thing keeping a long transcription's message from
  being re-claimed. It is not optional: the idempotency check cannot catch a
  mid-flight redelivery, because it asks whether the output exists and it does
  not exist until the work finishes.
