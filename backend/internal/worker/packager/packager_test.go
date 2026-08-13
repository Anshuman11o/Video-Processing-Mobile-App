package packager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anshumanagarwal/dayreel/internal/media"
	"github.com/anshumanagarwal/dayreel/internal/worker"
)

// The runner discovers job completion by type assertion. If this stops being
// true the job silently never completes — no error, no failed stage, just a
// reel endpoint that keeps returning 409 exactly as it did before 6A.
func TestStageSatisfiesFinalizer(t *testing.T) {
	var s worker.Stage = &Stage{}
	if _, ok := s.(worker.Finalizer); !ok {
		t.Fatal("packager.Stage must satisfy worker.Finalizer, or jobs never complete")
	}
}

func TestOutputKeyIsTheMasterPlaylist(t *testing.T) {
	s := &Stage{}
	if got, want := s.OutputKey("job-1"), "job-1/master.m3u8"; got != want {
		t.Errorf("OutputKey = %q, want %q", got, want)
	}
}

func TestPublicURL(t *testing.T) {
	got := publicURL("http://localhost:4566", "dayreel-hls-output", "job-1/master.m3u8")
	want := "http://localhost:4566/dayreel-hls-output/job-1/master.m3u8"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A trailing slash on the configured endpoint would otherwise produce a double
// slash, which some players reject and others silently resolve differently.
func TestPublicURLTrimsTrailingSlash(t *testing.T) {
	if got := publicURL("http://localhost:4566/", "b", "k"); got != "http://localhost:4566/b/k" {
		t.Errorf("trailing slash not handled: %q", got)
	}
}

// The bug this guards against: thumbnail_url was built against the processed
// bucket, which has no public-read policy and no CORS, and which stage 1A puts
// on a 7-day delete. It resolved anyway in every local run, because LocalStack
// Community serves unsigned GETs to any bucket regardless of policy (see
// docs/SETUP.md). That is why this is asserted here rather than end to end:
// **the 403 is not reproducible against LocalStack**, so no integration test in
// this project can catch a URL pointing at an unreadable bucket. Only the
// construction can be pinned down, and this pins it down negatively — any
// bucket other than the HLS one is a failure, not just the one that was wrong.
func TestBuildOutputNeverNamesABucketOtherThanHLS(t *testing.T) {
	out := buildOutput("http://10.0.2.2:4566", "dayreel-hls-output", "job-1", "job-1/master.m3u8", 6.02, true)

	for name, got := range map[string]string{
		"hls_url":       out.HLSURL,
		"thumbnail_url": out.ThumbnailURL,
	} {
		if got == "" {
			t.Fatalf("%s is empty", name)
		}
		if !strings.HasPrefix(got, "http://10.0.2.2:4566/dayreel-hls-output/") {
			t.Errorf("%s = %q, which is not in dayreel-hls-output — "+
				"a client cannot read any other bucket on real S3", name, got)
		}
	}

	if want := "http://10.0.2.2:4566/dayreel-hls-output/job-1/thumbnail.jpg"; out.ThumbnailURL != want {
		t.Errorf("thumbnail_url = %q, want %q", out.ThumbnailURL, want)
	}
}

// The copy into the HLS bucket is best-effort, so the URL must be withheld when
// it did not happen. Advertising a thumbnail that was never published is worse
// than advertising none: the field is optional and the client already handles
// its absence, but it has no way to tell a 404 from a job still settling.
func TestBuildOutputOmitsThumbnailWhenItWasNotPublished(t *testing.T) {
	out := buildOutput("http://localhost:4566", "dayreel-hls-output", "job-1", "job-1/master.m3u8", 6.02, false)

	if out.ThumbnailURL != "" {
		t.Errorf("thumbnail_url = %q, want empty when the copy did not land", out.ThumbnailURL)
	}
	if out.HLSURL == "" {
		t.Error("hls_url must survive a missing thumbnail; the reel is still playable")
	}
}

func TestThumbnailKey(t *testing.T) {
	if got, want := thumbnailKey("job-1"), "job-1/thumbnail.jpg"; got != want {
		t.Errorf("thumbnailKey = %q, want %q", got, want)
	}
}

// Players tolerate a wrong content type on segments but not on playlists.
func TestContentTypeFor(t *testing.T) {
	tests := map[string]string{
		"master.m3u8":    "application/vnd.apple.mpegurl",
		"playlist.m3u8":  "application/vnd.apple.mpegurl",
		"segment_000.ts": "video/mp2t",
		"subs_000.vtt":   "text/vtt",
		"something.bin":  "application/octet-stream",
		"UPPERCASE.M3U8": "application/vnd.apple.mpegurl",
	}

	for path, want := range tests {
		if got := contentTypeFor(path); got != want {
			t.Errorf("contentTypeFor(%q) = %q, want %q", path, got, want)
		}
	}
}

// The default ladder must survive rendition selection for the resolutions this
// project actually sees, including the 640x480 test clips used throughout.
func TestDefaultLadderSelection(t *testing.T) {
	tests := []struct {
		sourceHeight int
		wantCount    int
	}{
		{1080, 3},
		{720, 3},
		{480, 2}, // the local test-clip case: no upscale to 720p
		{360, 1},
		{240, 1}, // below every rung, still one playable rendition
	}

	for _, tc := range tests {
		got := media.SelectRenditions(DefaultLadder, tc.sourceHeight)
		if len(got) != tc.wantCount {
			t.Errorf("source height %d: got %d renditions, want %d",
				tc.sourceHeight, len(got), tc.wantCount)
		}
		if len(got) == 0 {
			t.Errorf("source height %d produced nothing playable", tc.sourceHeight)
		}
	}
}

// The master must be excluded from the bulk upload and sent last, because its
// presence is what tells the runner the stage finished. Including it here would
// publish the output key before the segments it references existed.
func TestCollectUploadsExcludesTheMaster(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "master.m3u8"), "#EXTM3U")
	mustWrite(t, filepath.Join(root, "480p", "playlist.m3u8"), "#EXTM3U")
	mustWrite(t, filepath.Join(root, "480p", "segment_000.ts"), "ts")
	mustWrite(t, filepath.Join(root, "subs", "subs_000.vtt"), "WEBVTT")

	got, err := collectUploads(root, "job-1", filepath.Join(root, "master.m3u8"))
	if err != nil {
		t.Fatalf("collectUploads: %v", err)
	}

	keys := make(map[string]bool, len(got))
	for _, u := range got {
		keys[u.Key] = true
	}

	if keys["job-1/master.m3u8"] {
		t.Error("master.m3u8 was included in the bulk upload; it must be uploaded last")
	}
	for _, want := range []string{
		"job-1/480p/playlist.m3u8",
		"job-1/480p/segment_000.ts",
		"job-1/subs/subs_000.vtt",
	} {
		if !keys[want] {
			t.Errorf("missing %q from uploads: %v", want, keys)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d uploads, want 3: %v", len(got), keys)
	}
}

// S3 keys are slash-separated whatever the host filesystem uses.
func TestCollectUploadsUsesForwardSlashes(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "720p", "segment_001.ts"), "ts")

	got, err := collectUploads(root, "job-1", "")
	if err != nil {
		t.Fatalf("collectUploads: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d uploads, want 1", len(got))
	}
	if got[0].Key != "job-1/720p/segment_001.ts" {
		t.Errorf("key = %q, want forward-slash separated", got[0].Key)
	}
	if strings.Contains(got[0].Key, "\\") {
		t.Errorf("key contains a backslash: %q", got[0].Key)
	}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
