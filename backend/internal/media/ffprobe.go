// Package media wraps the ffprobe and ffmpeg binaries.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// ProbeResult is the subset of ffprobe output the pipeline cares about.
type ProbeResult struct {
	DurationSec float64
	VideoCodec  string
	AudioCodec  string
	Width       int
	Height      int
	HasVideo    bool
	HasAudio    bool
}

// ffprobeOutput mirrors the ffprobe JSON schema. Numeric fields arrive as
// strings, hence the manual parsing below.
type ffprobeOutput struct {
	Streams []struct {
		CodecType string `json:"codec_type"`
		CodecName string `json:"codec_name"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
		Duration  string `json:"duration"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

// Probe inspects a local media file.
//
// An error here means the file could not be parsed as media at all, which the
// caller should treat as a permanent failure — the bytes will not improve on
// retry.
func Probe(ctx context.Context, path string) (*ProbeResult, error) {
	cmd := exec.CommandContext(ctx, "ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Bare "exit status 1" is unactionable; ffprobe puts the real reason on
		// stderr.
		return nil, fmt.Errorf("ffprobe %s: %w: %s", path, err, stderr.String())
	}

	var out ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("parse ffprobe output: %w", err)
	}

	res := &ProbeResult{}

	if d, err := strconv.ParseFloat(out.Format.Duration, 64); err == nil {
		res.DurationSec = d
	}

	for _, s := range out.Streams {
		switch s.CodecType {
		case "video":
			// Keep the first video stream; extra ones are cover art or
			// thumbnails and should not override the real track.
			if res.HasVideo {
				continue
			}
			res.HasVideo = true
			res.VideoCodec = s.CodecName
			res.Width = s.Width
			res.Height = s.Height
			// Some containers carry duration per-stream but not in format.
			if res.DurationSec == 0 {
				if d, err := strconv.ParseFloat(s.Duration, 64); err == nil {
					res.DurationSec = d
				}
			}
		case "audio":
			if res.HasAudio {
				continue
			}
			res.HasAudio = true
			res.AudioCodec = s.CodecName
		}
	}

	return res, nil
}
