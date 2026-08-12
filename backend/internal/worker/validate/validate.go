// Package validate implements the first pipeline stage: probe the upload,
// reject what cannot be processed, and remux the rest to a faststart MP4.
package validate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/media"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/storage"
	"github.com/anshumanagarwal/dayreel/internal/worker"
)

// Limits are the gates a file must pass to enter the pipeline.
//
// Everything outside these is rejected rather than transcoded: "-c copy"
// remuxing is a ~1s operation, while transcoding runs for minutes and would
// need visibility-timeout heartbeating in the runner. Rejecting is also the
// honest behaviour for a stage named "validate". Loosening this later, or
// adding a transcode fallback, means changing this struct and the gate below.
type Limits struct {
	MaxDuration        time.Duration
	AllowedVideoCodecs []string
	AllowedAudioCodecs []string
}

// DefaultLimits are starting values, not measured ones; real input may argue
// for different bounds.
var DefaultLimits = Limits{
	// 60s, lowered from 10 minutes. This is the only guard between a
	// mis-selected large file and a long, billed processing run, and
	// config/free-tier.md flags it as a cost lever against a $20 total budget.
	// Test clips are <=10s, so this leaves generous headroom while making an
	// accidental upload fail fast instead of transcribing for minutes.
	MaxDuration:        60 * time.Second,
	AllowedVideoCodecs: []string{"h264", "hevc"},
	AllowedAudioCodecs: []string{"aac", "mp3"},
}

// Stage validates and normalizes an uploaded video.
type Stage struct {
	s3     *storage.S3Client
	bucket string
	limits Limits
}

// New creates a validate Stage writing to the given output bucket.
func New(s3 *storage.S3Client, outputBucket string, limits Limits) *Stage {
	return &Stage{s3: s3, bucket: outputBucket, limits: limits}
}

func (s *Stage) Name() models.StageName { return models.StageValidate }

func (s *Stage) OutputBucket() string { return s.bucket }

func (s *Stage) OutputKey(jobID string) string { return jobID + "/validated.mp4" }

// Process downloads the upload, probes it, gates on the limits, and remuxes it
// to a faststart MP4 in the processed bucket.
func (s *Stage) Process(ctx context.Context, msg *events.StageMessage) (string, error) {
	// Work in a temp dir so a crash cannot accumulate files, and so concurrent
	// jobs cannot collide on a fixed path.
	dir, err := os.MkdirTemp("", "dayreel-validate-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	inPath := filepath.Join(dir, "input"+filepath.Ext(msg.Input.Key))
	outPath := filepath.Join(dir, "validated.mp4")

	if err := s.s3.DownloadToFile(ctx, msg.Input.Bucket, msg.Input.Key, inPath); err != nil {
		return "", fmt.Errorf("download input: %w", err)
	}

	probe, err := media.Probe(ctx, inPath)
	if err != nil {
		// ffprobe could not parse it at all. The bytes will not improve.
		return "", worker.Permanent("input is not a readable media file", err)
	}

	if err := s.gate(probe); err != nil {
		return "", err
	}

	if err := media.RemuxFaststart(ctx, inPath, outPath); err != nil {
		// Codecs passed the gate, so a remux failure means the container is
		// damaged in a way ffprobe did not surface. Still not retryable.
		return "", worker.Permanent("remux failed", err)
	}

	outputKey := s.OutputKey(msg.JobID)
	if err := s.s3.UploadFile(ctx, s.bucket, outputKey, outPath, "video/mp4"); err != nil {
		return "", fmt.Errorf("upload output: %w", err)
	}

	return outputKey, nil
}

// gate rejects anything the pipeline cannot process. Every rejection here is
// permanent by construction — these are properties of the file, not of the
// attempt.
func (s *Stage) gate(p *media.ProbeResult) error {
	if !p.HasVideo {
		return worker.Permanent("file contains no video stream", nil)
	}

	if !slices.Contains(s.limits.AllowedVideoCodecs, p.VideoCodec) {
		return worker.Permanent(fmt.Sprintf(
			"video codec %q is not supported (allowed: %v)",
			p.VideoCodec, s.limits.AllowedVideoCodecs), nil)
	}

	// Audio is optional — a silent clip is legitimate — but if present it must
	// survive the remux into MP4.
	if p.HasAudio && !slices.Contains(s.limits.AllowedAudioCodecs, p.AudioCodec) {
		return worker.Permanent(fmt.Sprintf(
			"audio codec %q is not supported (allowed: %v)",
			p.AudioCodec, s.limits.AllowedAudioCodecs), nil)
	}

	if p.DurationSec <= 0 {
		return worker.Permanent("could not determine duration", nil)
	}

	if d := time.Duration(p.DurationSec * float64(time.Second)); d > s.limits.MaxDuration {
		return worker.Permanent(fmt.Sprintf(
			"duration %s exceeds the %s limit", d.Round(time.Second), s.limits.MaxDuration), nil)
	}

	return nil
}
