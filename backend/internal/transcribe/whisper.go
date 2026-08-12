package transcribe

import (
	"context"
	"errors"
	"fmt"
	"os"
)

// WhisperCPP runs whisper.cpp as a subprocess, the same way the media package
// runs ffmpeg.
//
// The model is downloaded at runtime rather than baked into the image, so
// ModelPath is expected to point into a persistent volume. Without one the model
// re-downloads on every fresh container.
type WhisperCPP struct {
	ModelPath string
}

// NewWhisperCPP creates a transcriber backed by the whisper.cpp binary.
func NewWhisperCPP(modelPath string) *WhisperCPP {
	return &WhisperCPP{ModelPath: modelPath}
}

// errNotImplemented is deliberately explicit. Stage 5A is built mock-first: the
// entire pipeline is proven with MOCK_TRANSCRIBE=true before any model enters
// the image. Running with MOCK_TRANSCRIBE=false before this is finished should
// fail immediately and say why, rather than produce an empty transcript that
// looks like a clip with no speech in it.
var errNotImplemented = errors.New(
	"whisper.cpp transcriber is not implemented yet: run with MOCK_TRANSCRIBE=true, " +
		"or complete phase 6 of docs/stage-plans/stage-5a-transcribe-worker.md")

// Transcribe runs the model over the given audio file.
func (w *WhisperCPP) Transcribe(_ context.Context, audioPath string) ([]Segment, error) {
	if _, err := os.Stat(w.ModelPath); err != nil {
		// A missing model is transient by design: it downloads at runtime, so a
		// container that starts before the download finishes should retry rather
		// than fail the job permanently.
		return nil, fmt.Errorf("whisper model not available at %s: %w", w.ModelPath, err)
	}

	_ = audioPath
	return nil, errNotImplemented
}
