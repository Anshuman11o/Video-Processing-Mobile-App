package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// requireFFmpeg skips the test when the binaries are absent, so the suite still
// runs on a machine (or CI image) without them. The worker image always has
// them; see backend/Dockerfile.worker.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not installed", bin)
		}
	}
}

// synthVideo generates a small h264/aac clip to probe against.
func synthVideo(t *testing.T, dir string, seconds string) string {
	t.Helper()
	path := filepath.Join(dir, "in.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration="+seconds,
		"-f", "lavfi", "-i", "sine=frequency=440:duration="+seconds,
		"-c:v", "libx264", "-c:a", "aac",
		"-t", seconds,
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not synthesize test video (%v): %s", err, out)
	}
	return path
}

func TestProbe(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := synthVideo(t, dir, "2")

	res, err := Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	if !res.HasVideo {
		t.Error("expected a video stream")
	}
	if !res.HasAudio {
		t.Error("expected an audio stream")
	}
	if res.VideoCodec != "h264" {
		t.Errorf("video codec: got %q, want h264", res.VideoCodec)
	}
	if res.AudioCodec != "aac" {
		t.Errorf("audio codec: got %q, want aac", res.AudioCodec)
	}
	if res.Width != 320 || res.Height != 240 {
		t.Errorf("dimensions: got %dx%d, want 320x240", res.Width, res.Height)
	}
	// Duration is float seconds and rarely lands exactly on the requested value.
	if res.DurationSec < 1.5 || res.DurationSec > 2.5 {
		t.Errorf("duration: got %v, want ~2", res.DurationSec)
	}
}

// A non-media file must fail rather than probe as an empty result — this is the
// signal the validate stage turns into a permanent failure.
func TestProbe_NotMedia(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.mp4")
	if err := os.WriteFile(path, []byte("this is not a video"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Probe(context.Background(), path); err == nil {
		t.Fatal("expected an error probing a text file named .mp4")
	}
}

func TestRemuxFaststart(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := synthVideo(t, dir, "2")
	out := filepath.Join(dir, "out.mp4")

	if err := RemuxFaststart(context.Background(), in, out); err != nil {
		t.Fatalf("remux: %v", err)
	}

	res, err := Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("probe output: %v", err)
	}
	if res.VideoCodec != "h264" {
		t.Errorf("remux should copy the stream, not transcode: got %q", res.VideoCodec)
	}

	// Faststart puts moov ahead of mdat. Checking byte order directly is the
	// only way to know the flag actually took effect.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	moov, mdat := indexOf(data, "moov"), indexOf(data, "mdat")
	if moov < 0 || mdat < 0 {
		t.Fatalf("expected both atoms: moov=%d mdat=%d", moov, mdat)
	}
	if moov > mdat {
		t.Errorf("moov at %d is after mdat at %d; faststart did not apply", moov, mdat)
	}
}

func indexOf(haystack []byte, needle string) int {
	n := []byte(needle)
outer:
	for i := 0; i+len(n) <= len(haystack); i++ {
		for j := range n {
			if haystack[i+j] != n[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
