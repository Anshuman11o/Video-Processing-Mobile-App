package media

import (
	"fmt"
	"strings"
)

// timestampMapLocal is the WebVTT side of the X-TIMESTAMP-MAP pairing.
//
// Cue times in the transcript are already relative to the start of the media, so
// local zero is what gets pinned to the stream's first presentation timestamp.
// Pinning any other local time would work equally well arithmetically and be
// harder to read.
const timestampMapLocal = "00:00:00.000"

// SubtitleTimestampAnchor returns the MPEG-TS tick (90 kHz) that WebVTT time
// zero must be mapped to for cues to land on the media timeline.
//
// The problem it solves, measured rather than reasoned about: ffmpeg shifts the
// whole mux forward so no DTS is negative, and the shift is the video reorder
// delay — 2 B-frames at 30fps gave a first video PTS of 6000 ticks (66.7 ms).
// Without a mapping a player takes the RFC 8216 default of MPEGTS:0 <-> LOCAL:0
// and every cue lands that much early, with the cue at t=0 pushed to a negative
// item time.
//
// The anchor is the VIDEO start PTS, not the container's. Those differ here —
// AAC priming puts the first audio PTS 1920 ticks earlier — and the difference
// is not cosmetic: measured through AVFoundation on 2026-08-13, MPEGTS:6000
// placed every cue exactly on its authored time while MPEGTS:4080, the
// container start, left them 21.3 ms early.
//
// Taking the first rendition's value is safe because the ladder comes out of a
// single ffmpeg invocation over a single decode, so every variant shares one
// timeline — verified across all three rungs of a 3-rung job, all 6000. It is
// also the only thing that could be done: one VTT serves every variant, so a
// genuine disagreement between rungs would not be expressible.
func SubtitleTimestampAnchor(encoded []EncodedRendition) int64 {
	if len(encoded) == 0 {
		return 0
	}
	if encoded[0].StartPTS < 0 {
		return 0
	}
	return encoded[0].StartPTS
}

// WithTimestampMap inserts an X-TIMESTAMP-MAP header into a WebVTT document,
// mapping local time zero onto startPTS.
//
// The header belongs in the header block: after the WEBVTT line and before the
// blank line that ends the block. A parser stops reading headers at that blank
// line, so a map placed after it is silently ignored — which looks exactly like
// the bug it was added to fix.
//
// The header is written even when startPTS is zero. That case is identical to
// the default a player assumes in its absence, so it costs nothing, and one
// code path that always runs is worth more here than a conditional one that
// only runs on some sources.
//
// Input that does not look like WebVTT is returned untouched rather than
// rewritten: the caller is passing through a transcript it did not author, and
// corrupting an unrecognised document is worse than leaving the offset in.
func WithTimestampMap(vtt string, startPTS int64) string {
	if !strings.HasPrefix(vtt, "WEBVTT") {
		return vtt
	}
	// Re-running the packager over its own output must not stack headers; the
	// crash-resume path makes that reachable.
	if strings.Contains(vtt, "X-TIMESTAMP-MAP") {
		return vtt
	}
	if startPTS < 0 {
		startPTS = 0
	}

	header := fmt.Sprintf("X-TIMESTAMP-MAP=MPEGTS:%d,LOCAL:%s", startPTS, timestampMapLocal)

	nl := strings.IndexByte(vtt, '\n')
	if nl < 0 {
		// A bare "WEBVTT" with no terminator. Still valid input; give it a
		// header block and the blank line the format requires.
		return vtt + "\n" + header + "\n\n"
	}

	// CRLF is legal WebVTT even though nothing here emits it, and mixing
	// terminators inside one header block trips strict parsers.
	eol := "\n"
	if nl > 0 && vtt[nl-1] == '\r' {
		eol = "\r\n"
		nl--
	}

	return vtt[:nl] + eol + header + vtt[nl:]
}
