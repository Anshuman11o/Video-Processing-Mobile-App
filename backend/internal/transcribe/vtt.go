package transcribe

import (
	"fmt"
	"strings"
)

// WriteVTT renders segments as a WebVTT document.
//
// A cue-less document is valid WebVTT and is exactly what a silent clip should
// produce: the header alone. That case is not an error and must not be special
// -cased by callers into one — see the silent-clip handling in the transcribe
// stage.
func WriteVTT(segments []Segment) string {
	var b strings.Builder

	// The header, followed by a blank line, is mandatory. A file that begins
	// with anything else is rejected outright by conforming parsers.
	b.WriteString("WEBVTT\n\n")

	for _, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if text == "" {
			// A cue with no payload is not useful and some parsers treat it as
			// malformed, so drop it rather than emit an empty one.
			continue
		}

		b.WriteString(formatTimestamp(seg.Start))
		b.WriteString(" --> ")
		b.WriteString(formatTimestamp(seg.End))
		b.WriteString("\n")
		b.WriteString(text)
		b.WriteString("\n\n")
	}

	return b.String()
}

// formatTimestamp renders seconds as WebVTT's HH:MM:SS.mmm.
//
// The hours field is always written. WebVTT permits MM:SS.mmm, but mixing the
// two forms within a document is a common source of parser disagreement, so this
// commits to the long form throughout.
//
// Negative input is clamped to zero: a negative cue time is meaningless, and
// emitting one produces a document that fails to parse rather than one that
// merely looks odd.
func formatTimestamp(seconds float64) string {
	if seconds < 0 {
		seconds = 0
	}

	// Round to milliseconds first, so 59.9996 becomes 1:00.000 rather than
	// 59.999 — truncating each field independently lets a value sit just below
	// a boundary and produce a cue that ends before it starts.
	totalMillis := int64(seconds*1000 + 0.5)

	millis := totalMillis % 1000
	totalSeconds := totalMillis / 1000
	secs := totalSeconds % 60
	totalMinutes := totalSeconds / 60
	mins := totalMinutes % 60
	hours := totalMinutes / 60

	return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, mins, secs, millis)
}
