# internal/worker/extract

The second pipeline stage. Consumes `dayreel-extract`, pulls transcription audio
and a bounded set of keyframes out of `{job_id}/validated.mp4`, and hands off to
`dayreel-transcribe`.

## Files

| File | Purpose |
|------|---------|
| `extract.go` | The `worker.Stage` implementation, plus the pure frame-selection policy |
| `extract_test.go` | Frame selection and key layout; no ffmpeg, no I/O |

## Why the output is a manifest

This is the first stage producing **more than one artifact** — one WAV plus N
JPEGs — while `worker.Stage` returns a single output key.

`extract.json` resolves that. It is the one object declared as the stage's
output, and it names everything else written. **It is uploaded last**, after
every artifact it references, because the runner treats the presence of the
output key as proof the stage finished (`runner.go`). A manifest that existed
before its contents would make the idempotency guard lie.

The alternative — declaring `audio.wav` canonical — breaks precisely where it
matters: a silent clip produces no `audio.wav` at all, so the guard would have
no object to find.

## Non-Obvious Decisions

- **`t=0` is always extracted**, whether or not it scores as a scene change. A
  static clip crosses no threshold and would otherwise produce no frames, and a
  job with no thumbnail is worse than one with an arbitrary thumbnail.
- **Frames are capped at `MaxFrames`** and, when detection overruns, an evenly
  spaced subset is kept rather than the first N. The first 19 frames of a
  fast-cut opening are all the same three seconds of footage.
- **Silent clips are not failures.** Validate deliberately admits them, so
  extract records `audio.present: false` and writes no WAV. No silent audio is
  synthesized: that would fabricate data and spend transcription compute on
  manufactured silence. **This obliges the transcribe stage to short-circuit to
  an empty `WEBVTT` when `audio.present` is false.**
- **Scene detection and frame extraction are two passes, deliberately.**
  Timestamps are collected first so the cap applies before any JPEG is encoded,
  and so each frame carries a real timestamp rather than a positional index.
- **Reads and writes the same bucket.** Its input is validate's output.
  `OutputKey` derives from the job ID and never from `msg.Input.Key`, so a bad
  input cannot cause a write outside its own prefix.

## Traps

- **`pkt_pts_time` does not exist in ffmpeg 6.x.** It was removed after
  deprecation in 5.x, and asking for a missing field is *not* an error — ffprobe
  exits 0 and prints empty values. Using it makes every video report zero scene
  changes with nothing in stderr. Use `pts_time`. See `media/frames.go`.
- **`csv=p=0` leaves a trailing comma** on single-field rows; `nk=1` does not
  remove it. Trim before parsing.
- **A colon in the input path silently truncates it** inside a filter graph
  rather than erroring. `media.DetectSceneChanges` rejects such paths outright
  instead of trying to escape them.
- **`-ss` before `-i` seeks to the nearest preceding keyframe.** Extracted images
  may sit slightly earlier than the timestamp recorded beside them. That is the
  price of the fast seek, and it is fine for thumbnails.
