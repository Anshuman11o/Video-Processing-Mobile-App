package events

import (
	"fmt"
	"time"
)

// ExtractManifest is the canonical output of the extract stage.
//
// Extract is the first stage to produce more than one artifact — an audio track
// plus N keyframes — while worker.Stage returns a single output key. The
// manifest resolves that: it is the one object the stage declares as its output,
// and it names everything else that was written.
//
// It is uploaded last, after every artifact it references. That ordering is
// load-bearing: the runner's idempotency guard treats the presence of the output
// key as proof the stage finished, so the manifest must not exist until the
// things it promises do.
type ExtractManifest struct {
	JobID           string        `json:"job_id"`
	CreatedAt       time.Time     `json:"created_at"`
	Source          S3Ref         `json:"source"`
	DurationSeconds float64       `json:"duration_seconds"`
	Width           int           `json:"width,omitempty"`
	Height          int           `json:"height,omitempty"`
	Audio           AudioArtifact `json:"audio"`
	Frames          []FrameRef    `json:"frames"`
}

// AudioArtifact describes the extracted audio, or records its absence.
//
// Present is explicit rather than implied by an empty Key because a silent clip
// is a legitimate input that validate deliberately admits. Downstream needs to
// tell "no audio in the source" apart from "audio extraction has not run",
// without inferring it from a missing field.
type AudioArtifact struct {
	Present    bool   `json:"present"`
	Key        string `json:"key,omitempty"`
	SampleRate int    `json:"sample_rate,omitempty"`
	Channels   int    `json:"channels,omitempty"`
	Codec      string `json:"codec,omitempty"`
}

// FrameRef is one extracted keyframe and the point in the clip it came from.
//
// The timestamp is carried rather than inferred from the filename index so a
// consumer can position a thumbnail on a timeline without re-probing.
type FrameRef struct {
	Key              string  `json:"key"`
	TimestampSeconds float64 `json:"timestamp_seconds"`
}

// ExtractManifestKey is the key of the manifest itself — the extract stage's
// declared output.
func ExtractManifestKey(jobID string) string {
	return jobID + "/extract.json"
}

// ExtractAudioKey is the key of the extracted audio track.
func ExtractAudioKey(jobID string) string {
	return jobID + "/audio.wav"
}

// ExtractFrameKey is the key of the nth keyframe, 1-based.
//
// Zero padding keeps lexicographic order matching numeric order, so a plain
// S3 listing of the frames prefix comes back in clip order. Three digits is
// enough for any capped frame count; a hypothetical 1000th frame would sort
// wrongly but would also mean the cap was removed.
func ExtractFrameKey(jobID string, n int) string {
	return fmt.Sprintf("%s/frames/frame_%03d.jpg", jobID, n)
}
