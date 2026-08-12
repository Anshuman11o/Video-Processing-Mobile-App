// Package transcribe turns extracted audio into timed text.
//
// The Transcriber interface is the seam between the pipeline and whatever
// actually runs the model. Stage 5A ships two implementations: a mock used by
// default during development, and whisper.cpp invoked as a subprocess. Keeping
// the stage dependent on the interface rather than on either implementation is
// what makes the packaging decision reversible — swapping whisper.cpp for a
// sidecar later changes only what is behind this interface.
package transcribe

import "context"

// Segment is one span of transcribed speech.
//
// Times are seconds from the start of the audio. This mirrors what every Whisper
// implementation produces, so no implementation has to invent structure the
// model did not give it.
type Segment struct {
	Start float64
	End   float64
	Text  string
}

// Transcriber converts an audio file into timed segments.
//
// Implementations receive a path rather than bytes: the audio is already a file
// on disk by the time it gets here, models read from disk anyway, and a 10
// minute clip is ~19 MB that nothing gains from holding in memory.
//
// Returning zero segments is a valid, non-error result. Silence and speechless
// audio both legitimately produce nothing.
type Transcriber interface {
	Transcribe(ctx context.Context, audioPath string) ([]Segment, error)
}
