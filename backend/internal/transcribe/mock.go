package transcribe

import (
	"context"
	"fmt"
)

// mockCueSeconds is how long each synthetic cue lasts. Roughly the length of a
// spoken sentence, so the cue count for a given clip is in the right order of
// magnitude.
const mockCueSeconds = 3.0

// Mock produces synthetic cues without running a model.
//
// This is the default during development. The budget rules in
// config/free-tier.md allow only a handful of real runs, and every stage after
// this one needs *a* transcript to work against — so mock mode is how 6A and the
// mobile client get built, not a testing afterthought.
//
// Cues are duration-aware rather than a single fixed stub, because a one-cue
// transcript would exercise nothing about cue rendering, seeking or overlap in
// the stages that consume it.
type Mock struct {
	// DurationSeconds is the clip length, taken from the extract manifest so no
	// media has to be re-probed. Zero produces a single cue.
	DurationSeconds float64
}

// Transcribe returns synthetic cues spanning the clip.
//
// The audio path is ignored — deliberately. Mock mode must work when the file is
// absent, which is exactly the case for a silent clip whose audio was never
// written.
func (m Mock) Transcribe(_ context.Context, _ string) ([]Segment, error) {
	if m.DurationSeconds <= 0 {
		return []Segment{{
			Start: 0,
			End:   mockCueSeconds,
			Text:  "[mock transcript] duration unknown",
		}}, nil
	}

	var segments []Segment
	for start := 0.0; start < m.DurationSeconds; start += mockCueSeconds {
		end := start + mockCueSeconds
		if end > m.DurationSeconds {
			// The final cue is clipped to the clip, so no cue extends past the
			// media. A transcript claiming speech after the video ends is
			// something a player will render oddly and a reviewer will not spot.
			end = m.DurationSeconds
		}

		segments = append(segments, Segment{
			Start: start,
			End:   end,
			Text:  fmt.Sprintf("[mock transcript] segment %d", len(segments)+1),
		})
	}

	return segments, nil
}
