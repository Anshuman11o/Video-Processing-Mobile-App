package media

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// hlsPreset is the x264 preset for the ladder.
//
// Measured on a 60s 720p source: `fast` and `medium` cost roughly twice the wall
// clock and ~2.8x the CPU of `veryfast` for a 0.35% file-size reduction. At a
// fixed bitrate ladder the slower presets buy quality rather than size, and that
// trade is not worth doubling queue latency here.
const hlsPreset = "veryfast"

// EncodedRendition is a rendition as it actually came out of the encoder.
//
// The requested ladder and the real output differ in ways that matter to the
// master playlist: a 1280x720 source scaled to 480p yields 854x480, not the 852
// a naive aspect calculation gives, and the H.264 level — and therefore the
// CODECS string — moves with both resolution and frame rate.
type EncodedRendition struct {
	Rendition

	// ActualWidth and ActualHeight are measured from the encoded segments.
	ActualWidth  int
	ActualHeight int

	// Codecs is the RFC 6381 string for this rendition, e.g. "avc1.4d401f,mp4a.40.2".
	Codecs string

	// StartPTS is the first video presentation timestamp in the first segment,
	// in 90 kHz ticks. It is never zero in practice — see
	// SubtitleTimestampAnchor for what it is for and why it is measured.
	StartPTS int64
}

// EncodeHLSLadder transcodes inPath into one HLS rendition per entry in
// renditions, writing each into its own subdirectory of outDir along with a
// media playlist. It returns what was actually produced.
//
// One ffmpeg invocation rather than one per rendition: measured at 60s/720p,
// a single pass is ~20-25% faster in wall clock and ~15% cheaper in CPU, because
// it decodes and demuxes once instead of three times. The cost is peak memory
// (~1.7GB vs ~1.1GB), which is why a worker should not run two ladders at once.
//
// It does not write a master playlist. ffmpeg's HLS muxer cannot express a
// subtitle rendition — asking it to segfaults on a multi-rendition ladder — so
// the master is assembled by MasterPlaylist instead.
func EncodeHLSLadder(
	ctx context.Context, inPath, outDir string, renditions []Rendition, segmentSeconds int,
) ([]EncodedRendition, error) {
	if len(renditions) == 0 {
		return nil, errors.New("no renditions selected")
	}
	if segmentSeconds <= 0 {
		return nil, fmt.Errorf("invalid segment duration %d", segmentSeconds)
	}

	args := []string{
		// Without this ffmpeg consumes stdin as interactive commands.
		"-nostdin",
		"-hide_banner",
		"-y",
		"-loglevel", "error",
		"-i", inPath,
		"-filter_complex", buildFilterComplex(renditions),
	}

	for i := range renditions {
		args = append(args, "-map", fmt.Sprintf("[v%d]", i), "-map", "0:a")
	}

	args = append(args,
		"-c:v", "libx264",
		"-preset", hlsPreset,
		// Pins the profile byte of every CODECS string to 4d40. Left to itself
		// x264 picks High and emits avc1.64001e instead.
		"-profile:v", "main",
		"-pix_fmt", "yuv420p",
		// Keyframes by TIME, not by frame count. "-g 180" counts frames, so a
		// 24fps source produced 7.5s segments rather than 6s; this expression
		// gives exactly 6.000000s segments at 24, 30 and 60 fps alike.
		"-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", segmentSeconds),
	)

	for i, r := range renditions {
		args = append(args,
			fmt.Sprintf("-b:v:%d", i), fmt.Sprintf("%dk", r.VideoBitrateKbps),
			fmt.Sprintf("-maxrate:v:%d", i), fmt.Sprintf("%dk", r.VideoBitrateKbps*110/100),
			fmt.Sprintf("-bufsize:v:%d", i), fmt.Sprintf("%dk", r.VideoBitrateKbps*2),
		)
	}

	args = append(args, "-c:a", "aac", "-ar", "48000", "-ac", "2")
	for i, r := range renditions {
		args = append(args, fmt.Sprintf("-b:a:%d", i), fmt.Sprintf("%dk", r.AudioBitrateKbps))
	}

	args = append(args,
		// Drops the first MPEG-TS presentation timestamp from ~1.47s to ~0.07s.
		// Subtitle cues are offset by that start PTS, so without this the whole
		// caption track sits ~1.4s early and a cue at t=0 is dropped entirely.
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-f", "hls",
		"-hls_time", fmt.Sprint(segmentSeconds),
		"-hls_playlist_type", "vod",
		"-hls_list_size", "0",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(outDir, "%v", "segment_%03d.ts"),
		"-var_stream_map", buildVarStreamMap(renditions),
		filepath.Join(outDir, "%v", "playlist.m3u8"),
	)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg hls ladder: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	return measureRenditions(ctx, outDir, renditions)
}

// buildFilterComplex splits the decoded video once and scales each branch.
//
// scale=-2 preserves aspect ratio and rounds to an even dimension. -1 rounds to
// the nearest integer, which produces an odd width for some sources — 1130x720
// to 480p gives 753 — and libx264 rejects odd dimensions outright.
//
// The min() guard clamps to the source height so a rung can never upscale.
// Rungs above the source are already dropped by SelectRenditions; this is a
// backstop, because ffmpeg upscales silently with no warning and exit 0.
func buildFilterComplex(renditions []Rendition) string {
	var b strings.Builder

	if len(renditions) == 1 {
		fmt.Fprintf(&b, "[0:v]scale=-2:'min(%d,trunc(ih/2)*2)'[v0]", renditions[0].Height)
		return b.String()
	}

	fmt.Fprintf(&b, "[0:v]split=%d", len(renditions))
	for i := range renditions {
		fmt.Fprintf(&b, "[s%d]", i)
	}
	for i, r := range renditions {
		fmt.Fprintf(&b, ";[s%d]scale=-2:'min(%d,trunc(ih/2)*2)'[v%d]", i, r.Height, i)
	}
	return b.String()
}

// buildVarStreamMap names each output variant, which becomes the %v directory.
func buildVarStreamMap(renditions []Rendition) string {
	parts := make([]string, len(renditions))
	for i, r := range renditions {
		parts[i] = fmt.Sprintf("v:%d,a:%d,name:%s", i, i, r.Name)
	}
	return strings.Join(parts, " ")
}

// ffprobeStream is the subset of ffprobe's JSON output used here.
type ffprobeStream struct {
	Streams []struct {
		Width    int   `json:"width"`
		Height   int   `json:"height"`
		Level    int   `json:"level"`
		StartPTS int64 `json:"start_pts"`
	} `json:"streams"`
}

// measureRenditions reads back what the encoder actually produced.
//
// The CODECS string cannot be predicted: its final byte is the H.264 level,
// which moves with resolution AND frame rate. The same 720p rung is avc1.4d401f
// from a 30fps source and avc1.4d4020 from a 60fps one, and phone footage is
// routinely 60fps. A hardcoded table would be wrong for a large share of real
// uploads, and the failure is silent — a player rejects a variant whose CODECS
// it cannot satisfy without ever fetching a segment.
func measureRenditions(
	ctx context.Context, outDir string, renditions []Rendition,
) ([]EncodedRendition, error) {
	out := make([]EncodedRendition, 0, len(renditions))

	for _, r := range renditions {
		segment := filepath.Join(outDir, r.Name, "segment_000.ts")

		// JSON rather than csv: with -of csv=p=0 ffprobe emits fields in its own
		// canonical order rather than the requested one, so positional parsing
		// silently mismatches values to names.
		cmd := exec.CommandContext(ctx, "ffprobe",
			"-v", "error",
			"-select_streams", "v:0",
			"-show_entries", "stream=width,height,level,start_pts",
			"-of", "json",
			segment,
		)

		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr

		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("ffprobe rendition %s: %w: %s",
				r.Name, err, strings.TrimSpace(stderr.String()))
		}

		var probe ffprobeStream
		if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
			return nil, fmt.Errorf("parse ffprobe output for %s: %w", r.Name, err)
		}
		if len(probe.Streams) == 0 {
			return nil, fmt.Errorf("rendition %s has no video stream", r.Name)
		}

		s := probe.Streams[0]
		out = append(out, EncodedRendition{
			Rendition:    r,
			ActualWidth:  s.Width,
			ActualHeight: s.Height,
			Codecs:       CodecsString(s.Level),
			StartPTS:     s.StartPTS,
		})
	}

	return out, nil
}

// CodecsString builds the RFC 6381 codecs attribute for an H.264 level.
//
// avc1.PPCCLL — profile 0x4d (Main, pinned by -profile:v main), constraint flags
// 0x40 (constant across every encode measured), and the level as two hex digits.
// Audio is always AAC-LC from ffmpeg's native encoder.
func CodecsString(level int) string {
	return fmt.Sprintf("avc1.4d40%02x,mp4a.40.2", level)
}
