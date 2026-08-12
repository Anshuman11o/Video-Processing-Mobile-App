package media

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// synthSilentVideo generates a clip with no audio track at all — the case
// validate deliberately admits and extract therefore has to handle.
func synthSilentVideo(t *testing.T, dir string, seconds string) string {
	t.Helper()
	path := filepath.Join(dir, "silent.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration="+seconds,
		"-c:v", "libx264",
		"-t", seconds,
		path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not synthesize silent test video (%v): %s", err, out)
	}
	return path
}

// synthHardCut concatenates two visually different clips so there is a scene
// change at a known point. testsrc and testsrc2 differ enough to cross any
// sensible threshold.
func synthHardCut(t *testing.T, dir string) string {
	t.Helper()

	mk := func(name, src string) string {
		p := filepath.Join(dir, name)
		cmd := exec.Command("ffmpeg", "-v", "error", "-y",
			"-f", "lavfi", "-i", src+"=size=320x240:rate=15:duration=2",
			"-c:v", "libx264", "-t", "2", p,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("could not synthesize %s (%v): %s", name, err, out)
		}
		return p
	}
	a := mk("a.mp4", "testsrc")
	b := mk("b.mp4", "testsrc2")

	list := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(list, []byte("file '"+a+"'\nfile '"+b+"'\n"), 0o644); err != nil {
		t.Fatalf("write concat list: %v", err)
	}

	out := filepath.Join(dir, "cut.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "concat", "-safe", "0", "-i", list, "-c", "copy", out,
	)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not concatenate clips (%v): %s", err, o)
	}
	return out
}

func TestExtractAudio(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := synthVideo(t, dir, "2")
	out := filepath.Join(dir, "audio.wav")

	if err := ExtractAudio(context.Background(), in, out); err != nil {
		t.Fatalf("extract audio: %v", err)
	}

	// Assert the actual stream properties rather than that a file appeared:
	// the whole point of this helper is producing exactly what Whisper wants,
	// and a WAV at the wrong sample rate would still be a plausible-looking file.
	res, err := Probe(context.Background(), out)
	if err != nil {
		t.Fatalf("probe extracted audio: %v", err)
	}
	if res.HasVideo {
		t.Error("extracted audio still carries a video stream")
	}
	if !res.HasAudio {
		t.Fatal("extracted audio has no audio stream")
	}
	if res.AudioCodec != AudioCodec {
		t.Errorf("codec: got %q, want %q", res.AudioCodec, AudioCodec)
	}

	sr := ffprobeField(t, out, "stream=sample_rate")
	if sr != "16000" {
		t.Errorf("sample rate: got %q, want 16000", sr)
	}
	ch := ffprobeField(t, out, "stream=channels")
	if ch != "1" {
		t.Errorf("channels: got %q, want 1 (mono)", ch)
	}
}

func TestExtractAudioFailsOnSilentInput(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := synthSilentVideo(t, dir, "1")

	// Documents why the extract stage checks HasAudio before calling this: a
	// silent clip is a legitimate input, but ffmpeg treats "no audio to extract"
	// as an error, which would otherwise look like a real failure.
	if err := ExtractAudio(context.Background(), in, filepath.Join(dir, "a.wav")); err == nil {
		t.Error("expected an error extracting audio from a clip with no audio track")
	}
}

func TestDetectSceneChanges(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := synthHardCut(t, dir)

	got, err := DetectSceneChanges(context.Background(), in, 0.3)
	if err != nil {
		t.Fatalf("detect scene changes: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("no scene changes detected across a hard cut between two different sources")
	}

	// The cut is at 2s; allow slack for keyframe placement.
	var found bool
	for _, ts := range got {
		if ts > 1.5 && ts < 2.5 {
			found = true
		}
	}
	if !found {
		t.Errorf("no detected change near the 2s cut: %v", got)
	}
}

func TestDetectSceneChangesOnStaticClip(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	// A single flat colour never crosses the threshold. This is the case that
	// makes the extract stage's unconditional t=0 frame necessary.
	path := filepath.Join(dir, "flat.mp4")
	cmd := exec.Command("ffmpeg", "-v", "error", "-y",
		"-f", "lavfi", "-i", "color=c=blue:size=320x240:rate=15:duration=2",
		"-c:v", "libx264", "-t", "2", path,
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("could not synthesize flat clip (%v): %s", err, out)
	}

	got, err := DetectSceneChanges(context.Background(), path, 0.3)
	if err != nil {
		t.Fatalf("detect scene changes: %v", err)
	}
	if len(got) > 1 {
		t.Errorf("static clip produced %d scene changes, expected at most one: %v", len(got), got)
	}
}

func TestExtractFrameAt(t *testing.T) {
	requireFFmpeg(t)
	dir := t.TempDir()
	in := synthVideo(t, dir, "3")
	out := filepath.Join(dir, "frame.jpg")

	if err := ExtractFrameAt(context.Background(), in, 1.5, out, 3); err != nil {
		t.Fatalf("extract frame: %v", err)
	}

	info, err := os.Stat(out)
	if err != nil {
		t.Fatalf("stat frame: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("extracted frame is empty")
	}
	if codec := ffprobeField(t, out, "stream=codec_name"); codec != "mjpeg" {
		t.Errorf("frame codec: got %q, want mjpeg", codec)
	}
}

// ffprobeField reads a single ffprobe entry, e.g. "stream=sample_rate".
func ffprobeField(t *testing.T, path, entry string) string {
	t.Helper()
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_entries", entry, "-of", "default=nw=1:nk=1", path,
	).Output()
	if err != nil {
		t.Fatalf("ffprobe %s on %s: %v", entry, path, err)
	}
	return strings.TrimSpace(string(out))
}
