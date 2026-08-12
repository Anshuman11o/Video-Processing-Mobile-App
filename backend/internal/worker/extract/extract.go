// Package extract implements the second pipeline stage: pull the transcription
// audio and a bounded set of keyframes out of the validated MP4.
package extract

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/media"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/storage"
	"github.com/anshumanagarwal/dayreel/internal/worker"
)

// Options tune frame selection. Grouped in one struct, like validate.Limits, so
// tuning later is a one-line change rather than a restructure.
type Options struct {
	// SceneThreshold is ffmpeg's scene score, 0.0-1.0. Higher means fewer cuts
	// are considered scene changes.
	SceneThreshold float64

	// MaxFrames caps how many keyframes are extracted. Scene detection is
	// unbounded above — hard-cut footage crosses the threshold constantly — and
	// every frame is an S3 PUT plus an ffmpeg seek.
	MaxFrames int

	// JPEGQuality is ffmpeg's -q:v, 1 best to 31 worst.
	JPEGQuality int
}

// DefaultOptions are starting guesses, not measured values. SceneThreshold in
// particular deserves revisiting once there is real footage.
var DefaultOptions = Options{
	SceneThreshold: 0.3,
	MaxFrames:      20,
	JPEGQuality:    3,
}

// Stage extracts audio and keyframes from a validated video.
type Stage struct {
	s3     *storage.S3Client
	bucket string
	opts   Options
}

// New creates an extract Stage reading from and writing to the given bucket.
func New(s3 *storage.S3Client, outputBucket string, opts Options) *Stage {
	return &Stage{s3: s3, bucket: outputBucket, opts: opts}
}

func (s *Stage) Name() models.StageName { return models.StageExtract }

func (s *Stage) OutputBucket() string { return s.bucket }

// OutputKey is the manifest, not the audio. The manifest is written last, so
// its presence is what proves every artifact landed — which is exactly the
// question the runner's idempotency guard asks.
func (s *Stage) OutputKey(jobID string) string { return events.ExtractManifestKey(jobID) }

// Process downloads the validated MP4, extracts audio and keyframes, and writes
// a manifest naming everything it produced.
func (s *Stage) Process(ctx context.Context, msg *events.StageMessage) (string, error) {
	dir, err := os.MkdirTemp("", "dayreel-extract-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "input.mp4")

	if err := s.s3.DownloadToFile(ctx, msg.Input.Bucket, msg.Input.Key, inPath); err != nil {
		return "", fmt.Errorf("download input: %w", err)
	}

	probe, err := media.Probe(ctx, inPath)
	if err != nil {
		// This is validate's own output, so a probe failure means it is damaged
		// or was never what it claimed. Re-reading the same bytes will not help.
		return "", worker.Permanent("validated input is not readable", err)
	}

	manifest := events.ExtractManifest{
		JobID:           msg.JobID,
		CreatedAt:       time.Now().UTC(),
		Source:          msg.Input,
		DurationSeconds: probe.DurationSec,
		Width:           probe.Width,
		Height:          probe.Height,
		Frames:          []events.FrameRef{},
	}

	if manifest.Audio, err = s.extractAudio(ctx, dir, inPath, msg.JobID, probe); err != nil {
		return "", err
	}

	if manifest.Frames, err = s.extractFrames(ctx, dir, inPath, msg.JobID, probe); err != nil {
		return "", err
	}

	// The manifest goes up last, after everything it references. The runner
	// treats this object's existence as proof the stage completed.
	manifestPath := filepath.Join(dir, "extract.json")
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(manifestPath, body, 0o644); err != nil {
		return "", fmt.Errorf("write manifest: %w", err)
	}

	outputKey := s.OutputKey(msg.JobID)
	if err := s.s3.UploadFile(ctx, s.bucket, outputKey, manifestPath, "application/json"); err != nil {
		return "", fmt.Errorf("upload manifest: %w", err)
	}

	return outputKey, nil
}

// extractAudio demuxes the audio track, or records that there was none.
//
// A silent clip is legitimate — validate admits them deliberately — so absence
// is a fact to record, not a failure. No silent WAV is synthesized: that would
// fabricate data and spend transcription compute on manufactured silence.
func (s *Stage) extractAudio(
	ctx context.Context, dir, inPath, jobID string, probe *media.ProbeResult,
) (events.AudioArtifact, error) {
	if !probe.HasAudio {
		return events.AudioArtifact{Present: false}, nil
	}

	audioPath := filepath.Join(dir, "audio.wav")
	if err := media.ExtractAudio(ctx, inPath, audioPath); err != nil {
		return events.AudioArtifact{}, worker.Permanent("audio extraction failed", err)
	}

	key := events.ExtractAudioKey(jobID)
	if err := s.s3.UploadFile(ctx, s.bucket, key, audioPath, "audio/wav"); err != nil {
		return events.AudioArtifact{}, fmt.Errorf("upload audio: %w", err)
	}

	return events.AudioArtifact{
		Present:    true,
		Key:        key,
		SampleRate: media.AudioSampleRate,
		Channels:   media.AudioChannels,
		Codec:      media.AudioCodec,
	}, nil
}

// extractFrames detects scene changes, selects a bounded set, and uploads one
// JPEG per selected timestamp.
func (s *Stage) extractFrames(
	ctx context.Context, dir, inPath, jobID string, probe *media.ProbeResult,
) ([]events.FrameRef, error) {
	scenes, err := media.DetectSceneChanges(ctx, inPath, s.opts.SceneThreshold)
	if err != nil {
		// Detection reads the file the same way probing did. If it fails here,
		// retrying the identical bytes will not change the outcome.
		return nil, worker.Permanent("scene detection failed", err)
	}

	timestamps := selectTimestamps(scenes, probe.DurationSec, s.opts.MaxFrames)

	frames := make([]events.FrameRef, 0, len(timestamps))
	for i, ts := range timestamps {
		framePath := filepath.Join(dir, fmt.Sprintf("frame_%03d.jpg", i+1))
		if err := media.ExtractFrameAt(ctx, inPath, ts, framePath, s.opts.JPEGQuality); err != nil {
			return nil, worker.Permanent(fmt.Sprintf("frame extraction failed at %.3fs", ts), err)
		}

		key := events.ExtractFrameKey(jobID, i+1)
		if err := s.s3.UploadFile(ctx, s.bucket, key, framePath, "image/jpeg"); err != nil {
			return nil, fmt.Errorf("upload frame %d: %w", i+1, err)
		}

		frames = append(frames, events.FrameRef{Key: key, TimestampSeconds: ts})
	}

	return frames, nil
}

// selectTimestamps turns raw scene-change times into the frames actually worth
// extracting. Pure function: no I/O, so the policy is testable on its own.
//
// Two bounds, because scene detection is unbounded at both ends:
//
//   - t=0 is always included, whether or not it scored as a scene change. A
//     static clip crosses no threshold and would otherwise yield no frames at
//     all, and a job with no thumbnail is worse than one with an arbitrary
//     thumbnail.
//   - At most max frames survive. When detection returns more, an evenly spaced
//     subset is kept rather than the first max — the first frames of a fast-cut
//     opening are all the same few seconds of the clip.
func selectTimestamps(scenes []float64, duration float64, max int) []float64 {
	if max <= 0 {
		return []float64{}
	}

	// t=0 first, then detected changes, skipping any that effectively duplicate
	// it or fall outside the clip.
	candidates := []float64{0}
	for _, ts := range scenes {
		if ts <= 0 {
			continue
		}
		if duration > 0 && ts >= duration {
			continue
		}
		candidates = append(candidates, ts)
	}

	if len(candidates) <= max {
		return candidates
	}

	// Keep t=0 and spread the remaining slots across the rest of the candidates.
	selected := make([]float64, 0, max)
	selected = append(selected, candidates[0])

	rest := candidates[1:]
	slots := max - 1

	if slots == 1 {
		// A single remaining slot has no span to spread across; the midpoint is
		// more representative of the clip than either end.
		return append(selected, rest[len(rest)/2])
	}

	for i := range slots {
		// Round to nearest so the spread stays even rather than drifting toward
		// the start through repeated truncation. slots >= 2 here, so the
		// denominator is safe.
		idx := int((float64(i)*float64(len(rest)-1))/float64(slots-1) + 0.5)
		if idx >= len(rest) {
			idx = len(rest) - 1
		}
		selected = append(selected, rest[idx])
	}

	return selected
}
