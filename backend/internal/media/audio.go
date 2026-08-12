package media

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Audio output format constants. These are not arbitrary: Whisper resamples
// everything to 16 kHz mono internally, so producing that here means the
// transcribe stage does no conversion of its own.
const (
	AudioSampleRate = 16000
	AudioChannels   = 1
	AudioCodec      = "pcm_s16le"
)

// ExtractAudio demuxes the audio track to a mono 16 kHz signed 16-bit PCM WAV.
//
// Uncompressed PCM rather than a compressed format because the consumer is a
// speech model, not a listener: every codec in between is a lossy step before
// transcription, and at 32 KB/s the size is bounded by the duration limit
// validate already enforces.
//
// Callers must check that the source actually has an audio stream first. ffmpeg
// fails when asked to extract audio from a silent clip, and that failure would
// be indistinguishable from a real one.
func ExtractAudio(ctx context.Context, inPath, outPath string) error {
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-v", "error",
		"-y",
		"-i", inPath,
		"-vn", // drop video; we only want the audio track
		"-acodec", AudioCodec,
		"-ar", fmt.Sprint(AudioSampleRate),
		"-ac", fmt.Sprint(AudioChannels),
		"-f", "wav",
		outPath,
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Bare "exit status 1" is unactionable, and this error feeds the
		// permanent-vs-transient classification in the worker.
		return fmt.Errorf("ffmpeg extract audio %s: %w: %s", inPath, err, stderr.String())
	}
	return nil
}
