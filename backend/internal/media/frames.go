package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// DetectSceneChanges returns the timestamps, in seconds, where the video crosses
// the given scene-change score (0.0-1.0; higher means fewer changes).
//
// This only reads timestamps — it decodes once and writes no images. Extracting
// frames is a separate step so that any cap on frame count can be applied before
// a single JPEG is encoded, and so each frame carries a real timestamp rather
// than a positional index.
//
// The alternative — one pass with "-vf select=...,showinfo" — requires scraping
// pts values out of ffmpeg's stderr, which is brittle in a way that does not
// announce itself when the log format shifts.
func DetectSceneChanges(ctx context.Context, path string, threshold float64) ([]float64, error) {
	if err := checkFilterPath(path); err != nil {
		return nil, err
	}

	// The comma inside gt() is escaped for ffmpeg's filter parser, which would
	// otherwise read it as a filter separator. This is libavfilter escaping,
	// not shell escaping — exec passes argv directly, with no shell involved.
	filter := fmt.Sprintf("movie=%s,select=gt(scene\\,%g)", path, threshold)

	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-f", "lavfi",
		"-i", filter,
		// pts_time, NOT pkt_pts_time. pkt_pts_time was removed in ffmpeg 6.0
		// (the worker image runs 6.1.1), and asking for a field that no longer
		// exists is not an error: ffprobe exits 0 and prints empty values. Using
		// the old name reports "no scene changes" for every video, forever, with
		// nothing in stderr to say why.
		"-show_entries", "frame=pts_time",
		"-of", "csv=p=0",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffprobe scene detect %s: %w: %s", path, err, stderr.String())
	}

	var timestamps []float64
	for _, line := range strings.Split(stdout.String(), "\n") {
		// csv=p=0 emits a bare value per frame, but a trailing separator appears
		// for entries ffprobe could not fill, so trim before parsing.
		field := strings.TrimSuffix(strings.TrimSpace(line), ",")
		if field == "" || field == "N/A" {
			continue
		}

		ts, err := strconv.ParseFloat(field, 64)
		if err != nil {
			// One unreadable timestamp is not worth failing the stage over; the
			// caller always has t=0 and whatever else parsed.
			continue
		}
		timestamps = append(timestamps, ts)
	}

	return timestamps, nil
}

// ExtractFrameAt writes a single JPEG from the given timestamp.
//
// "-ss" is placed before "-i" so ffmpeg seeks using the demuxer's index instead
// of decoding from the start, which keeps this cheap enough to call once per
// selected frame. The cost is precision: the seek lands on the nearest preceding
// keyframe, so the image may be slightly earlier than the timestamp recorded
// alongside it.
func ExtractFrameAt(ctx context.Context, inPath string, timestamp float64, outPath string, quality int) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-v", "error",
		"-y",
		"-ss", strconv.FormatFloat(timestamp, 'f', 3, 64),
		"-i", inPath,
		"-frames:v", "1",
		"-q:v", strconv.Itoa(quality),
		outPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg extract frame at %.3fs from %s: %w: %s",
			timestamp, inPath, err, stderr.String())
	}
	return nil
}

// filterUnsafe are characters that are syntax inside a filter graph, where the
// "movie=" source argument is unescaped twice. Escaping them correctly needs
// doubled backslashes and is easy to get subtly wrong; a colon, for instance,
// silently truncates the path at that point rather than erroring.
const filterUnsafe = `:,'\[]`

// checkFilterPath rejects paths that cannot be passed safely to a filter graph.
//
// Rejecting rather than escaping is deliberate. Callers control these paths —
// they are temp files this package's callers create — so this should never fire.
// If it ever does, the layout changed, and a named error is far better than the
// failure it replaces: a mis-escaped path makes ffprobe emit nothing at all and
// exit 0, which reads as "this video has no scene changes".
func checkFilterPath(path string) error {
	if i := strings.IndexAny(path, filterUnsafe); i >= 0 {
		return fmt.Errorf(
			"path %q contains %q, which is filter-graph syntax; scene detection needs a path free of %s",
			path, path[i], filterUnsafe)
	}
	return nil
}
