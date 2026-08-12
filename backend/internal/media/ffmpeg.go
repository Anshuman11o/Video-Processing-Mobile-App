package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// RemuxFaststart rewrites a media file as an MP4 with the moov atom at the
// front, without re-encoding.
//
// "-c copy" means streams are copied rather than transcoded, so this is fast
// (seconds, not minutes) but only works when the codecs are already
// MP4-compatible. Callers are expected to gate on codec first — see
// worker/validate.
//
// "+faststart" moves the moov atom ahead of the media data, which is what lets
// a player begin playback before the whole file has arrived. Without it,
// progressive playback over HTTP stalls until the very end of the file is
// fetched.
func RemuxFaststart(ctx context.Context, inPath, outPath string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-v", "error",
		"-y",
		"-i", inPath,
		"-c", "copy",
		"-movflags", "+faststart",
		"-f", "mp4",
		outPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ffmpeg remux %s: %w: %s", inPath, err, stderr.String())
	}
	return nil
}
