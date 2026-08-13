package media

import (
	"strings"
	"testing"
)

// mockVTT is the shape MOCK_TRANSCRIBE produces, and the case that exposed the
// defect: a cue at exactly zero, which an unmapped document pushes to a negative
// item time.
const mockVTT = "WEBVTT\n\n" +
	"00:00:00.000 --> 00:00:03.000\n[mock transcript] segment 1\n\n" +
	"00:00:03.000 --> 00:00:06.000\n[mock transcript] segment 2\n\n"

// measuredStartPTS is what ffmpeg actually produced for a 30fps source with two
// B-frames of reorder delay: 6000 ticks, 66.7 ms. AVFoundation placed cues
// exactly on their authored times when mapped to this value.
const measuredStartPTS = 6000

func TestWithTimestampMapPlacesHeaderInsideTheHeaderBlock(t *testing.T) {
	got := WithTimestampMap(mockVTT, measuredStartPTS)

	want := "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:6000,LOCAL:00:00:00.000\n\n"
	if !strings.HasPrefix(got, want) {
		t.Fatalf("header block wrong.\n got: %q\nwant prefix: %q", got, want)
	}

	// A map after the blank line is ignored by parsers, which looks identical to
	// having no map at all — the exact failure this replaced.
	blank := strings.Index(got, "\n\n")
	if idx := strings.Index(got, "X-TIMESTAMP-MAP"); idx > blank {
		t.Errorf("map at %d is past the end of the header block at %d", idx, blank)
	}

	if !strings.Contains(got, "00:00:00.000 --> 00:00:03.000") {
		t.Errorf("cues were altered; the map must not rewrite them:\n%s", got)
	}
}

func TestWithTimestampMapHandlesCuelessDocument(t *testing.T) {
	// The silent-clip path. 6A verified the cue-less document is valid WebVTT;
	// adding a header must not be what finally breaks it.
	got := WithTimestampMap("WEBVTT\n\n", measuredStartPTS)

	if got != "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:6000,LOCAL:00:00:00.000\n\n" {
		t.Errorf("cue-less document mangled: %q", got)
	}
}

func TestWithTimestampMapIsIdempotent(t *testing.T) {
	once := WithTimestampMap(mockVTT, measuredStartPTS)
	twice := WithTimestampMap(once, measuredStartPTS)

	if once != twice {
		t.Errorf("second application changed the document:\n once: %q\ntwice: %q", once, twice)
	}
}

func TestWithTimestampMapLeavesForeignInputAlone(t *testing.T) {
	// Not WebVTT. Rewriting it would corrupt a document this package did not
	// author, which is worse than leaving the offset in place.
	const srt = "1\n00:00:00,000 --> 00:00:03,000\nhello\n"
	if got := WithTimestampMap(srt, measuredStartPTS); got != srt {
		t.Errorf("non-WebVTT input was rewritten: %q", got)
	}
}

func TestWithTimestampMapEdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		vtt      string
		startPTS int64
		want     string
	}{
		{
			name:     "bare header with no terminator still gets a header block",
			vtt:      "WEBVTT",
			startPTS: 6000,
			want:     "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:6000,LOCAL:00:00:00.000\n\n",
		},
		{
			name:     "zero anchor is written rather than skipped",
			vtt:      "WEBVTT\n\n",
			startPTS: 0,
			want:     "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n",
		},
		{
			name:     "negative anchor clamps; a TS timestamp is unsigned",
			vtt:      "WEBVTT\n\n",
			startPTS: -6000,
			want:     "WEBVTT\nX-TIMESTAMP-MAP=MPEGTS:0,LOCAL:00:00:00.000\n\n",
		},
		{
			name:     "CRLF input keeps CRLF; mixing terminators trips parsers",
			vtt:      "WEBVTT\r\n\r\n",
			startPTS: 6000,
			want:     "WEBVTT\r\nX-TIMESTAMP-MAP=MPEGTS:6000,LOCAL:00:00:00.000\r\n\r\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WithTimestampMap(tt.vtt, tt.startPTS); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSubtitleTimestampAnchor(t *testing.T) {
	ladder := testLadder()

	tests := []struct {
		name    string
		encoded []EncodedRendition
		want    int64
	}{
		{
			name: "takes the measured start PTS",
			encoded: []EncodedRendition{
				{Rendition: ladder[0], StartPTS: 6000},
				{Rendition: ladder[1], StartPTS: 6000},
			},
			want: 6000,
		},
		{
			name:    "no renditions yields the identity mapping",
			encoded: nil,
			want:    0,
		},
		{
			name:    "a negative reading is not propagated into the playlist",
			encoded: []EncodedRendition{{Rendition: ladder[0], StartPTS: -1}},
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SubtitleTimestampAnchor(tt.encoded); got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}
