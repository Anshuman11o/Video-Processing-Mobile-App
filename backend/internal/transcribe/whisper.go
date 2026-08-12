package transcribe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// whisperBinary is the compiled whisper.cpp CLI, built into the worker image.
	whisperBinary = "whisper-cli"

	// blankAudioMarker is the literal text whisper.cpp emits for a stretch it
	// judges to contain no speech. It is a deterministic sentinel rather than a
	// hallucination, and must be filtered — otherwise a quiet clip produces a
	// transcript that reads "[BLANK_AUDIO]" to a viewer.
	blankAudioMarker = "[BLANK_AUDIO]"

	// modelURL and modelMinBytes describe the ggml base model. The model is
	// downloaded at runtime rather than baked into the image, to keep the image
	// small against a constrained Docker disk allowance.
	modelURL      = "https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-base.bin"
	modelMinBytes = 100 << 20 // the real file is ~141 MiB; anything far below is a truncated download

	// stderrLimit caps how much of whisper.cpp's stderr is carried into an
	// error. A missing input file makes it dump its entire help text, which is
	// ~80 lines of noise per occurrence.
	stderrLimit = 600
)

// ErrUnreadableAudio reports that whisper.cpp could not decode the input.
//
// Exported as a sentinel so the pipeline stage can classify it as permanent
// without this package having to know about the worker's error types.
var ErrUnreadableAudio = errors.New("whisper could not read the audio file")

// WhisperCPP runs whisper.cpp as a subprocess, the same way the media package
// runs ffmpeg.
type WhisperCPP struct {
	ModelPath string

	// downloadOnce guards the model fetch so concurrent jobs in one process do
	// not race to write the same file.
	downloadOnce sync.Once
	downloadErr  error
}

// NewWhisperCPP creates a transcriber backed by the whisper.cpp binary.
func NewWhisperCPP(modelPath string) *WhisperCPP {
	return &WhisperCPP{ModelPath: modelPath}
}

// whisperJSON is whisper.cpp's -oj output. Only the fields used here are
// declared; the file also carries model hyperparameters and system info.
type whisperJSON struct {
	Transcription []struct {
		// Offsets are integer milliseconds. The sibling "timestamps" field holds
		// the same information as SRT-style strings with a comma decimal
		// separator, which would have to be parsed — these do not.
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text string `json:"text"`
	} `json:"transcription"`
}

// Transcribe runs the model over the given audio file.
func (w *WhisperCPP) Transcribe(ctx context.Context, audioPath string) ([]Segment, error) {
	if err := w.ensureModel(ctx); err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "dayreel-whisper-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// -of takes a path with no extension; whisper appends .json itself. Without
	// it, output lands beside the input as "audio.wav.json".
	outBase := filepath.Join(dir, "out")
	jsonPath := outBase + ".json"

	cmd := exec.CommandContext(ctx, whisperBinary,
		"-m", w.ModelPath,
		"-f", audioPath,
		"-oj",          // structured segments, so cue formatting stays ours
		"-of", outBase, // output base path, extension appended
		"-np", // no progress: trims ~40 lines of hyperparameter dump from stderr
	)

	var stderr strings.Builder
	cmd.Stderr = &stderr
	// Segments are also echoed to stdout; the JSON file is the source of truth.
	cmd.Stdout = io.Discard

	runErr := cmd.Run()

	// The exit code alone is not trustworthy. whisper.cpp exits 0 on a corrupt
	// or empty input file, writing no output at all — so a transcript that
	// silently came back empty would be indistinguishable from a clip with no
	// speech. The output file's existence is the real success signal.
	body, readErr := os.ReadFile(jsonPath)
	if readErr != nil {
		if runErr != nil {
			return nil, fmt.Errorf("whisper-cli failed: %w: %s", runErr, truncate(stderr.String()))
		}
		// Exited cleanly but produced nothing: the decode failed.
		return nil, fmt.Errorf("%w: %s", ErrUnreadableAudio, truncate(stderr.String()))
	}
	if runErr != nil {
		return nil, fmt.Errorf("whisper-cli failed: %w: %s", runErr, truncate(stderr.String()))
	}

	var parsed whisperJSON
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse whisper output: %w", err)
	}

	segments := make([]Segment, 0, len(parsed.Transcription))
	for _, t := range parsed.Transcription {
		// whisper.cpp pads every segment's text with a leading space.
		text := strings.TrimSpace(t.Text)
		if text == "" || text == blankAudioMarker {
			continue
		}

		segments = append(segments, Segment{
			Start: float64(t.Offsets.From) / 1000,
			End:   float64(t.Offsets.To) / 1000,
			Text:  text,
		})
	}

	return segments, nil
}

// ensureModel downloads the model if it is not already present.
//
// Failures here are returned as ordinary errors, which the pipeline treats as
// transient — a download that failed can succeed on retry, and a worker that
// starts before the model has landed should wait rather than fail the job.
func (w *WhisperCPP) ensureModel(ctx context.Context) error {
	w.downloadOnce.Do(func() {
		w.downloadErr = w.downloadModel(ctx)
	})
	return w.downloadErr
}

func (w *WhisperCPP) downloadModel(ctx context.Context) error {
	if info, err := os.Stat(w.ModelPath); err == nil {
		if info.Size() >= modelMinBytes {
			return nil
		}
		// A file too small to be the model is a previous download that was
		// interrupted. Left alone it would be reused forever, producing failures
		// that look like a broken model rather than a broken download.
		if err := os.Remove(w.ModelPath); err != nil {
			return fmt.Errorf("remove truncated model %s: %w", w.ModelPath, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(w.ModelPath), 0o755); err != nil {
		return fmt.Errorf("create model dir: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, modelURL, nil)
	if err != nil {
		return fmt.Errorf("build model request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download model: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download model: unexpected status %s", resp.Status)
	}

	// Download to a temporary name and rename into place. A rename is atomic, so
	// an interrupted download can never leave a partial file at the real path
	// for the next run to pick up and trust.
	tmpPath := w.ModelPath + ".tmp"
	tmp, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create model temp file: %w", err)
	}
	defer os.Remove(tmpPath)

	written, err := io.Copy(tmp, resp.Body)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write model: %w", err)
	}
	if written < modelMinBytes {
		return fmt.Errorf("model download truncated: got %d bytes, expected at least %d", written, modelMinBytes)
	}

	if err := os.Rename(tmpPath, w.ModelPath); err != nil {
		return fmt.Errorf("install model: %w", err)
	}
	return nil
}

// truncate bounds captured stderr. whisper.cpp prints its full help text when
// the input file is missing.
func truncate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) <= stderrLimit {
		return s
	}
	return s[:stderrLimit] + "… (truncated)"
}
