package extract

import (
	"testing"

	"github.com/anshumanagarwal/dayreel/internal/events"
)

func TestSelectTimestamps(t *testing.T) {
	tests := []struct {
		name     string
		scenes   []float64
		duration float64
		max      int
		want     []float64
	}{
		{
			// The static-clip case: nothing crosses the scene threshold. Without
			// the t=0 guarantee this job would have no thumbnail at all.
			name:     "no scene changes still yields the first frame",
			scenes:   nil,
			duration: 10,
			max:      20,
			want:     []float64{0},
		},
		{
			name:     "scene changes are kept in order after t=0",
			scenes:   []float64{2.5, 5, 7.5},
			duration: 10,
			max:      20,
			want:     []float64{0, 2.5, 5, 7.5},
		},
		{
			// ffmpeg reporting a change at t=0 must not produce a duplicate
			// frame, since t=0 is already included unconditionally.
			name:     "a detected change at zero is not duplicated",
			scenes:   []float64{0, 4},
			duration: 10,
			max:      20,
			want:     []float64{0, 4},
		},
		{
			// Seeking at or past the end produces either a duplicate of the last
			// frame or an ffmpeg error, neither of which is worth a PUT.
			name:     "timestamps at or beyond the duration are dropped",
			scenes:   []float64{3, 10, 12},
			duration: 10,
			max:      20,
			want:     []float64{0, 3},
		},
		{
			name:     "unknown duration keeps every positive timestamp",
			scenes:   []float64{3, 12},
			duration: 0,
			max:      20,
			want:     []float64{0, 3, 12},
		},
		{
			name:     "a single slot yields only the first frame",
			scenes:   []float64{1, 2, 3},
			duration: 10,
			max:      1,
			want:     []float64{0},
		},
		{
			name:     "two slots take the first frame and a middle one",
			scenes:   []float64{1, 2, 3},
			duration: 10,
			max:      2,
			want:     []float64{0, 2},
		},
		{
			name:     "a non-positive cap extracts nothing",
			scenes:   []float64{1, 2},
			duration: 10,
			max:      0,
			want:     []float64{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectTimestamps(tc.scenes, tc.duration, tc.max)

			if len(got) != len(tc.want) {
				t.Fatalf("got %v (len %d), want %v (len %d)", got, len(got), tc.want, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("index %d: got %v, want %v (full: %v)", i, got[i], tc.want[i], got)
				}
			}
		})
	}
}

// The cap exists because scene detection is unbounded above: hard-cut footage
// crosses the threshold constantly, and each surviving frame costs an ffmpeg
// seek and an S3 PUT.
func TestSelectTimestampsEnforcesCap(t *testing.T) {
	scenes := make([]float64, 0, 50)
	for i := 1; i <= 50; i++ {
		scenes = append(scenes, float64(i))
	}

	got := selectTimestamps(scenes, 60, 20)

	if len(got) != 20 {
		t.Fatalf("got %d frames, want exactly the cap of 20: %v", len(got), got)
	}
	if got[0] != 0 {
		t.Errorf("first frame is %v, want t=0 to survive capping", got[0])
	}

	// Keeping the first 19 of a fast-cut opening would return the same few
	// seconds repeatedly, so the selection must reach the end of the clip.
	last := got[len(got)-1]
	if last < 40 {
		t.Errorf("last selected timestamp is %v; selection is bunched at the start rather than spread", last)
	}

	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Errorf("timestamps not strictly increasing at index %d: %v", i, got)
			break
		}
	}
}

func TestExtractKeyHelpers(t *testing.T) {
	const jobID = "550e8400-e29b-41d4-a716-446655440000"

	if got, want := events.ExtractManifestKey(jobID), jobID+"/extract.json"; got != want {
		t.Errorf("manifest key: got %q, want %q", got, want)
	}
	if got, want := events.ExtractAudioKey(jobID), jobID+"/audio.wav"; got != want {
		t.Errorf("audio key: got %q, want %q", got, want)
	}

	// Zero padding is what keeps a plain S3 listing in clip order.
	if got, want := events.ExtractFrameKey(jobID, 7), jobID+"/frames/frame_007.jpg"; got != want {
		t.Errorf("frame key: got %q, want %q", got, want)
	}
	if got, want := events.ExtractFrameKey(jobID, 20), jobID+"/frames/frame_020.jpg"; got != want {
		t.Errorf("frame key: got %q, want %q", got, want)
	}
}
