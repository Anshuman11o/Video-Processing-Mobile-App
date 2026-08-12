// Package transcribe implements the third pipeline stage: turn the extracted
// audio into a timed WebVTT transcript.
package transcribe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/storage"
	"github.com/anshumanagarwal/dayreel/internal/transcribe"
	"github.com/anshumanagarwal/dayreel/internal/worker"
)

// TranscriberFor builds the transcriber for one job.
//
// A factory rather than a single shared Transcriber because the mock needs the
// clip duration, which is per-job and only known once the manifest is read. The
// real implementation ignores the duration.
type TranscriberFor func(manifest *events.ExtractManifest) transcribe.Transcriber

// Stage transcribes the audio named by an extract manifest.
type Stage struct {
	s3         *storage.S3Client
	bucket     string
	newDecoder TranscriberFor
}

// New creates a transcribe Stage.
func New(s3 *storage.S3Client, outputBucket string, newDecoder TranscriberFor) *Stage {
	return &Stage{s3: s3, bucket: outputBucket, newDecoder: newDecoder}
}

func (s *Stage) Name() models.StageName { return models.StageTranscribe }

func (s *Stage) OutputBucket() string { return s.bucket }

func (s *Stage) OutputKey(jobID string) string { return jobID + "/transcript.vtt" }

// Process reads the extract manifest, transcribes the audio it names, and writes
// a WebVTT transcript.
func (s *Stage) Process(ctx context.Context, msg *events.StageMessage) (string, error) {
	dir, err := os.MkdirTemp("", "dayreel-transcribe-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// The input is the manifest, not the audio. Stage 4A made extract.json the
	// canonical output precisely so a multi-artifact stage could hand off one
	// key; the audio location is read from inside it.
	manifest, err := s.fetchManifest(ctx, dir, msg)
	if err != nil {
		return "", err
	}

	segments, err := s.transcribeAudio(ctx, dir, manifest)
	if err != nil {
		return "", err
	}

	vttPath := filepath.Join(dir, "transcript.vtt")
	if err := os.WriteFile(vttPath, []byte(transcribe.WriteVTT(segments)), 0o644); err != nil {
		return "", fmt.Errorf("write transcript: %w", err)
	}

	outputKey := s.OutputKey(msg.JobID)
	if err := s.s3.UploadFile(ctx, s.bucket, outputKey, vttPath, "text/vtt"); err != nil {
		return "", fmt.Errorf("upload transcript: %w", err)
	}

	return outputKey, nil
}

// fetchManifest downloads and parses the extract manifest.
func (s *Stage) fetchManifest(
	ctx context.Context, dir string, msg *events.StageMessage,
) (*events.ExtractManifest, error) {
	path := filepath.Join(dir, "extract.json")

	// A missing manifest stays transient. Extract wrote it one step earlier, so
	// absence more likely means a read problem than a permanently broken job.
	if err := s.s3.DownloadToFile(ctx, msg.Input.Bucket, msg.Input.Key, path); err != nil {
		return nil, fmt.Errorf("download manifest: %w", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}

	var manifest events.ExtractManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		// Well-formed JSON is a property of the bytes, not of the attempt.
		// Re-reading the same object cannot produce different JSON.
		return nil, worker.Permanent("extract manifest is not valid JSON", err)
	}

	return &manifest, nil
}

// transcribeAudio runs the model, or short-circuits when there is no audio.
func (s *Stage) transcribeAudio(
	ctx context.Context, dir string, manifest *events.ExtractManifest,
) ([]transcribe.Segment, error) {
	// The silent-clip obligation inherited from stage 4A. Validate deliberately
	// admits clips with no audio track, so extract writes no audio.wav and
	// records audio.present: false. Invoking the model here would fail on a
	// missing S3 key and fail a job the pipeline explicitly accepted.
	//
	// Zero segments render as a valid, cue-less WEBVTT document.
	if !manifest.Audio.Present {
		return nil, nil
	}

	if manifest.Audio.Key == "" {
		// present: true with no key means extract wrote a manifest it should
		// never have written. Retrying reads the same contradiction.
		return nil, worker.Permanent("manifest reports audio present but names no key", nil)
	}

	audioPath := filepath.Join(dir, "audio.wav")
	if err := s.s3.DownloadToFile(ctx, manifest.Source.Bucket, manifest.Audio.Key, audioPath); err != nil {
		return nil, fmt.Errorf("download audio: %w", err)
	}

	segments, err := s.newDecoder(manifest).Transcribe(ctx, audioPath)
	if err != nil {
		// Deliberately transient. Transcription failures are dominated by
		// environmental causes — a model still downloading, memory pressure, a
		// process killed mid-run — which retrying does fix. A genuinely corrupt
		// input would have failed extract's own ffmpeg pass first.
		return nil, fmt.Errorf("transcribe audio: %w", err)
	}

	return segments, nil
}
