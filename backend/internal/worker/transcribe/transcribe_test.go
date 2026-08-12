package transcribe

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/transcribe"
	"github.com/anshumanagarwal/dayreel/internal/worker"
)

// spyTranscriber records whether it was called. The silent-clip path is defined
// by the model *not* running, so asserting on output alone would pass even if
// the model ran and returned nothing.
type spyTranscriber struct {
	called bool
	segs   []transcribe.Segment
}

func (s *spyTranscriber) Transcribe(context.Context, string) ([]transcribe.Segment, error) {
	s.called = true
	return s.segs, nil
}

// A silent clip must short-circuit before the model, and before any attempt to
// download audio that was never written. This is the obligation stage 4A created
// when it chose to record absence rather than synthesize silence.
func TestSilentClipNeverInvokesTheModel(t *testing.T) {
	spy := &spyTranscriber{}
	stage := New(nil, "dayreel-processed", func(*events.ExtractManifest) transcribe.Transcriber {
		return spy
	})

	manifest := &events.ExtractManifest{
		JobID: "job-1",
		Audio: events.AudioArtifact{Present: false},
	}

	segs, err := stage.transcribeAudio(context.Background(), t.TempDir(), manifest)
	if err != nil {
		t.Fatalf("a silent clip must not be an error: %v", err)
	}
	if spy.called {
		t.Error("the model was invoked for a clip with no audio track")
	}
	if len(segs) != 0 {
		t.Errorf("expected no segments, got %d", len(segs))
	}

	// The end product still has to be a valid document, not an empty file.
	if doc := transcribe.WriteVTT(segs); doc != "WEBVTT\n\n" {
		t.Errorf("silent clip should yield a cue-less WEBVTT document, got %q", doc)
	}
}

// present: true with an empty key is a manifest extract should never have
// written. Retrying re-reads the same contradiction, so it must not be transient.
func TestAudioPresentWithoutKeyIsPermanent(t *testing.T) {
	stage := New(nil, "dayreel-processed", func(*events.ExtractManifest) transcribe.Transcriber {
		return &spyTranscriber{}
	})

	manifest := &events.ExtractManifest{
		JobID: "job-1",
		Audio: events.AudioArtifact{Present: true, Key: ""},
	}

	_, err := stage.transcribeAudio(context.Background(), t.TempDir(), manifest)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !worker.IsPermanent(err) {
		t.Errorf("expected a permanent error, got transient: %v", err)
	}
}

func TestOutputKey(t *testing.T) {
	stage := New(nil, "dayreel-processed", nil)
	if got, want := stage.OutputKey("job-1"), "job-1/transcript.vtt"; got != want {
		t.Errorf("OutputKey = %q, want %q", got, want)
	}
}

// The manifest is 4A's contract with this stage; if the shapes drift apart, the
// break should surface here rather than at runtime.
func TestManifestRoundTripsFromExtractOutput(t *testing.T) {
	raw := []byte(`{
      "job_id": "550e8400",
      "created_at": "2026-08-12T10:32:11Z",
      "source": {"bucket": "dayreel-processed", "key": "550e8400/validated.mp4"},
      "duration_seconds": 6.02,
      "width": 640,
      "height": 480,
      "audio": {
        "present": true,
        "key": "550e8400/audio.wav",
        "sample_rate": 16000,
        "channels": 1,
        "codec": "pcm_s16le"
      },
      "frames": [{"key": "550e8400/frames/frame_001.jpg", "timestamp_seconds": 0}]
    }`)

	var m events.ExtractManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("real extract manifest failed to parse: %v", err)
	}

	if !m.Audio.Present {
		t.Error("audio.present did not survive the round trip")
	}
	if m.Audio.Key != "550e8400/audio.wav" {
		t.Errorf("audio key: got %q", m.Audio.Key)
	}
	// The mock reads duration from here, so a silent failure to parse it would
	// downgrade every mock transcript to a single cue.
	if m.DurationSeconds != 6.02 {
		t.Errorf("duration: got %v, want 6.02", m.DurationSeconds)
	}
}

// The mock is the default development path, so its pairing with a real manifest
// is worth asserting directly.
func TestMockUsesManifestDuration(t *testing.T) {
	manifest := &events.ExtractManifest{
		DurationSeconds: 10,
		Audio:           events.AudioArtifact{Present: true, Key: "j/audio.wav"},
	}

	segs, err := transcribe.Mock{DurationSeconds: manifest.DurationSeconds}.
		Transcribe(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("mock: %v", err)
	}
	if len(segs) < 2 {
		t.Errorf("a 10s clip should produce several cues, got %d", len(segs))
	}
}
