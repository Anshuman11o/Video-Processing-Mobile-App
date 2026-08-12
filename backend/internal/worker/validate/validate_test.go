package validate

import (
	"testing"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/media"
	"github.com/anshumanagarwal/dayreel/internal/worker"
)

func TestGate(t *testing.T) {
	s := New(nil, "dayreel-processed", DefaultLimits)

	cases := []struct {
		name    string
		probe   media.ProbeResult
		wantErr bool
	}{
		{
			name:  "h264 with aac passes",
			probe: media.ProbeResult{HasVideo: true, VideoCodec: "h264", HasAudio: true, AudioCodec: "aac", DurationSec: 30},
		},
		{
			name:  "hevc passes",
			probe: media.ProbeResult{HasVideo: true, VideoCodec: "hevc", HasAudio: true, AudioCodec: "mp3", DurationSec: 30},
		},
		{
			// A silent clip is legitimate input, not a validation failure.
			name:  "no audio passes",
			probe: media.ProbeResult{HasVideo: true, VideoCodec: "h264", DurationSec: 30},
		},
		{
			name:    "audio-only rejected",
			probe:   media.ProbeResult{HasAudio: true, AudioCodec: "aac", DurationSec: 30},
			wantErr: true,
		},
		{
			name:    "vp9 rejected",
			probe:   media.ProbeResult{HasVideo: true, VideoCodec: "vp9", DurationSec: 30},
			wantErr: true,
		},
		{
			name:    "unsupported audio rejected",
			probe:   media.ProbeResult{HasVideo: true, VideoCodec: "h264", HasAudio: true, AudioCodec: "opus", DurationSec: 30},
			wantErr: true,
		},
		{
			// Derived from DefaultLimits rather than hardcoded: MaxDuration is a
			// cost lever that is expected to be tuned, and a test that pins the
			// number fails on tuning instead of on a real regression.
			name:    "over duration rejected",
			probe:   media.ProbeResult{HasVideo: true, VideoCodec: "h264", DurationSec: limitSeconds() + 1},
			wantErr: true,
		},
		{
			name:  "exactly at the limit passes",
			probe: media.ProbeResult{HasVideo: true, VideoCodec: "h264", DurationSec: limitSeconds()},
		},
		{
			name:    "unknown duration rejected",
			probe:   media.ProbeResult{HasVideo: true, VideoCodec: "h264", DurationSec: 0},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.gate(&tc.probe)

			if tc.wantErr && err == nil {
				t.Fatal("expected rejection, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("expected pass, got %v", err)
			}

			// Every gate rejection describes the file, not the attempt, so it
			// must be permanent. A transient classification here would retry
			// three times and then reach the DLQ having learned nothing.
			if err != nil && !worker.IsPermanent(err) {
				t.Errorf("rejection must be permanent, got %T: %v", err, err)
			}
		})
	}
}

func TestOutputKey(t *testing.T) {
	s := New(nil, "dayreel-processed", DefaultLimits)
	const jobID = "550e8400-e29b-41d4-a716-446655440000"

	if got, want := s.OutputKey(jobID), jobID+"/validated.mp4"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestCustomLimits(t *testing.T) {
	s := New(nil, "dayreel-processed", Limits{
		MaxDuration:        5 * time.Second,
		AllowedVideoCodecs: []string{"vp9"},
		AllowedAudioCodecs: []string{"opus"},
	})

	if err := s.gate(&media.ProbeResult{HasVideo: true, VideoCodec: "vp9", DurationSec: 3}); err != nil {
		t.Errorf("vp9 should pass under custom limits: %v", err)
	}
	if err := s.gate(&media.ProbeResult{HasVideo: true, VideoCodec: "h264", DurationSec: 3}); err == nil {
		t.Error("h264 should be rejected under custom limits")
	}
}

// limitSeconds is DefaultLimits.MaxDuration in seconds, so the duration cases
// track the configured limit instead of a literal.
func limitSeconds() float64 {
	return DefaultLimits.MaxDuration.Seconds()
}
