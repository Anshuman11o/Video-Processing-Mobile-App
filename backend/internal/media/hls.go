package media

import (
	"context"
	"errors"
)

// errHLSNotImplemented is deliberately explicit.
//
// The ffmpeg ladder invocation is being determined empirically inside the worker
// image rather than written from documentation. Two stages running have now been
// bitten by command lines that looked right and failed silently: pkt_pts_time,
// removed in ffmpeg 6 and returning empty output with exit 0; and whisper.cpp
// exiting 0 on a corrupt input while writing nothing. A guessed HLS invocation
// would fail the same way — an empty or malformed ladder that still exits 0.
var errHLSNotImplemented = errors.New(
	"hls ladder encoding is not implemented yet: see phase 2 of " +
		"docs/stage-plans/stage-6a-package-worker.md")

// EncodeHLSLadder transcodes inPath into one HLS rendition per entry in
// renditions, writing each into its own subdirectory of outDir along with a
// media playlist.
//
// It does not write a master playlist: ffmpeg's HLS muxer cannot express a
// subtitle rendition, so the master is assembled by MasterPlaylist instead.
func EncodeHLSLadder(
	ctx context.Context, inPath, outDir string, renditions []Rendition, segmentSeconds int,
) error {
	if len(renditions) == 0 {
		return errors.New("no renditions selected")
	}
	return errHLSNotImplemented
}
