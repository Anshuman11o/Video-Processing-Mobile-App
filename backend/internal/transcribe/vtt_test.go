package transcribe

import (
	"context"
	"strings"
	"testing"
)

func TestFormatTimestamp(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want string
	}{
		{"zero", 0, "00:00:00.000"},
		{"sub-second", 0.25, "00:00:00.250"},
		{"seconds", 7.5, "00:00:07.500"},
		{"minutes", 90.125, "00:01:30.125"},
		{"over an hour", 3661.007, "01:01:01.007"},
		{"exact minute", 60, "00:01:00.000"},
		{"exact hour", 3600, "01:00:00.000"},
		// Truncating each field independently would render this as 00:00:59.999
		// and could produce a cue that ends before the next one starts.
		{"rounds up across the minute boundary", 59.9996, "00:01:00.000"},
		// A negative cue time makes a document fail to parse outright, so it is
		// clamped rather than emitted.
		{"negative clamps to zero", -1.5, "00:00:00.000"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTimestamp(tc.in); got != tc.want {
				t.Errorf("formatTimestamp(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A silent clip must produce a valid document, not an empty file and not an
// error. This is the 4A silent-clip obligation as it surfaces in output format.
func TestWriteVTTWithNoSegments(t *testing.T) {
	got := WriteVTT(nil)

	if !strings.HasPrefix(got, "WEBVTT") {
		t.Errorf("document does not begin with the mandatory WEBVTT header: %q", got)
	}
	if got != "WEBVTT\n\n" {
		t.Errorf("cue-less document should be the header alone, got %q", got)
	}
}

func TestWriteVTT(t *testing.T) {
	got := WriteVTT([]Segment{
		{Start: 0, End: 2.5, Text: "Hello there."},
		{Start: 2.5, End: 5, Text: "Second cue."},
	})

	want := "WEBVTT\n\n" +
		"00:00:00.000 --> 00:00:02.500\nHello there.\n\n" +
		"00:00:02.500 --> 00:00:05.000\nSecond cue.\n\n"

	if got != want {
		t.Errorf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestWriteVTTSkipsEmptyText(t *testing.T) {
	// Whisper implementations emit whitespace-only segments for pauses. A cue
	// with no payload is treated as malformed by some parsers.
	got := WriteVTT([]Segment{
		{Start: 0, End: 1, Text: "  "},
		{Start: 1, End: 2, Text: "\n"},
		{Start: 2, End: 3, Text: "Real text."},
	})

	if strings.Count(got, "-->") != 1 {
		t.Errorf("expected exactly one cue, got:\n%s", got)
	}
	if !strings.Contains(got, "Real text.") {
		t.Errorf("the non-empty cue was dropped:\n%s", got)
	}
}

func TestWriteVTTTrimsSurroundingWhitespace(t *testing.T) {
	// whisper.cpp pads segment text with a leading space; carrying that into the
	// cue payload shifts every line of rendered subtitles.
	got := WriteVTT([]Segment{{Start: 0, End: 1, Text: "  padded text  "}})

	if !strings.Contains(got, "\npadded text\n") {
		t.Errorf("text was not trimmed:\n%q", got)
	}
}

func TestMockIsDurationAware(t *testing.T) {
	segs, err := Mock{DurationSeconds: 10}.Transcribe(context.Background(), "/nonexistent.wav")
	if err != nil {
		t.Fatalf("mock transcribe: %v", err)
	}

	// 10s at 3s per cue: 0-3, 3-6, 6-9, 9-10.
	if len(segs) != 4 {
		t.Fatalf("got %d cues for a 10s clip, want 4: %+v", len(segs), segs)
	}

	// No cue may extend past the media, or a player renders subtitles after the
	// video has ended.
	last := segs[len(segs)-1]
	if last.End != 10 {
		t.Errorf("final cue ends at %v, want it clipped to the 10s duration", last.End)
	}

	for i, s := range segs {
		if s.End <= s.Start {
			t.Errorf("cue %d has non-positive duration: %+v", i, s)
		}
		if i > 0 && s.Start < segs[i-1].End {
			t.Errorf("cue %d overlaps the previous one: %+v vs %+v", i, s, segs[i-1])
		}
	}
}

func TestMockWithUnknownDuration(t *testing.T) {
	segs, err := Mock{DurationSeconds: 0}.Transcribe(context.Background(), "")
	if err != nil {
		t.Fatalf("mock transcribe: %v", err)
	}
	if len(segs) != 1 {
		t.Errorf("unknown duration should yield exactly one cue, got %d", len(segs))
	}
}

// Mock mode has to work when the audio file does not exist: a silent clip never
// had one written.
func TestMockIgnoresMissingAudio(t *testing.T) {
	if _, err := (Mock{DurationSeconds: 5}).Transcribe(context.Background(), "/definitely/not/here.wav"); err != nil {
		t.Errorf("mock must not require the audio file to exist: %v", err)
	}
}

// The mock must round-trip through the real VTT writer, since that pairing is
// what every downstream stage actually consumes during development.
func TestMockRendersAsValidVTT(t *testing.T) {
	segs, _ := Mock{DurationSeconds: 7}.Transcribe(context.Background(), "")
	doc := WriteVTT(segs)

	if !strings.HasPrefix(doc, "WEBVTT\n\n") {
		t.Errorf("mock output is not a valid VTT document:\n%s", doc)
	}
	if strings.Count(doc, "-->") != len(segs) {
		t.Errorf("expected %d cues in the document, got:\n%s", len(segs), doc)
	}
}
