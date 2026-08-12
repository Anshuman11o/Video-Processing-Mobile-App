package media

import (
	"strings"
	"testing"
)

func testLadder() []Rendition {
	return []Rendition{
		{Name: "720p", Width: 1280, Height: 720, VideoBitrateKbps: 2800, AudioBitrateKbps: 128},
		{Name: "480p", Width: 854, Height: 480, VideoBitrateKbps: 1400, AudioBitrateKbps: 128},
		{Name: "360p", Width: 640, Height: 360, VideoBitrateKbps: 800, AudioBitrateKbps: 96},
	}
}

func testSubs() *SubtitleRendition {
	return &SubtitleRendition{
		GroupID:  "subs",
		Name:     "English",
		Language: "en",
		URI:      "subs/playlist.m3u8",
		Default:  true,
	}
}

func TestMasterPlaylistStructure(t *testing.T) {
	got := MasterPlaylist(testLadder(), testSubs())

	if !strings.HasPrefix(got, "#EXTM3U\n") {
		t.Errorf("playlist must begin with #EXTM3U:\n%s", got)
	}
	if !strings.Contains(got, "#EXT-X-VERSION:3") {
		t.Error("missing version tag")
	}
	if n := strings.Count(got, "#EXT-X-STREAM-INF:"); n != 3 {
		t.Errorf("got %d variants, want 3:\n%s", n, got)
	}
	for _, want := range []string{"720p/playlist.m3u8", "480p/playlist.m3u8", "360p/playlist.m3u8"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing rendition URI %q:\n%s", want, got)
		}
	}
}

// A variant missing the SUBTITLES attribute loses captions the moment the player
// switches to it — which looks like a video bug, not a playlist bug, and only
// shows up under bandwidth changes.
func TestMasterPlaylistPutsSubtitlesOnEveryVariant(t *testing.T) {
	got := MasterPlaylist(testLadder(), testSubs())

	variants := strings.Count(got, "#EXT-X-STREAM-INF:")
	tagged := strings.Count(got, `SUBTITLES="subs"`)

	if tagged != variants {
		t.Errorf("%d of %d variants carry SUBTITLES:\n%s", tagged, variants, got)
	}

	if !strings.Contains(got, "#EXT-X-MEDIA:TYPE=SUBTITLES") {
		t.Errorf("missing EXT-X-MEDIA subtitle declaration:\n%s", got)
	}
	// The GROUP-ID and the SUBTITLES attribute must match exactly or players
	// silently show nothing.
	if !strings.Contains(got, `GROUP-ID="subs"`) {
		t.Errorf("EXT-X-MEDIA GROUP-ID does not match the variant attribute:\n%s", got)
	}
	if !strings.Contains(got, `URI="subs/playlist.m3u8"`) {
		t.Errorf("subtitle URI missing or unquoted:\n%s", got)
	}
}

func TestMasterPlaylistWithoutSubtitles(t *testing.T) {
	got := MasterPlaylist(testLadder(), nil)

	if strings.Contains(got, "SUBTITLES") {
		t.Errorf("no subtitle rendition was given, but the master advertises one:\n%s", got)
	}
	if strings.Contains(got, "EXT-X-MEDIA") {
		t.Errorf("unexpected EXT-X-MEDIA:\n%s", got)
	}
	if n := strings.Count(got, "#EXT-X-STREAM-INF:"); n != 3 {
		t.Errorf("variants should be unaffected, got %d", n)
	}
}

// BANDWIDTH under-advertising makes a player choose a rendition it cannot
// sustain; the symptom is rebuffering rather than an error.
func TestBandwidthExceedsCombinedBitrate(t *testing.T) {
	for _, r := range testLadder() {
		combined := (r.VideoBitrateKbps + r.AudioBitrateKbps) * 1000
		if r.bandwidth() <= combined {
			t.Errorf("%s: BANDWIDTH %d does not exceed combined bitrate %d",
				r.Name, r.bandwidth(), combined)
		}
	}
}

func TestMasterPlaylistDeclaresResolutionAndCodecs(t *testing.T) {
	got := MasterPlaylist(testLadder(), nil)

	for _, want := range []string{"RESOLUTION=1280x720", "RESOLUTION=854x480", "RESOLUTION=640x360"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// A wrong CODECS string is a silent playback failure: the player rejects the
	// variant before fetching a segment.
	if n := strings.Count(got, `CODECS="avc1.4d401f,mp4a.40.2"`); n != 3 {
		t.Errorf("expected a CODECS attribute on each of 3 variants, got %d:\n%s", n, got)
	}
}

func TestSubtitlePlaylist(t *testing.T) {
	got := SubtitlePlaylist("subs_000.vtt", 6.02)

	for _, want := range []string{
		"#EXTM3U",
		"#EXT-X-PLAYLIST-TYPE:VOD",
		"#EXT-X-ENDLIST", // without this a player treats it as live and keeps polling
		"subs_000.vtt",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}

	// TARGETDURATION is an integer and must not round below the real duration,
	// or a player can stall waiting for a segment that never comes.
	if !strings.Contains(got, "#EXT-X-TARGETDURATION:7") {
		t.Errorf("TARGETDURATION should round 6.02 up to 7:\n%s", got)
	}
}

func TestSubtitlePlaylistClampsTinyDurations(t *testing.T) {
	// The silent-clip path can produce a near-zero duration. TARGETDURATION:0 is
	// invalid.
	got := SubtitlePlaylist("subs_000.vtt", 0)
	if !strings.Contains(got, "#EXT-X-TARGETDURATION:1") {
		t.Errorf("zero duration must clamp to 1:\n%s", got)
	}
}

func TestSelectRenditions(t *testing.T) {
	tests := []struct {
		name         string
		sourceHeight int
		want         []string
	}{
		{"1080p source takes the whole ladder", 1080, []string{"720p", "480p", "360p"}},
		{"720p source takes all three", 720, []string{"720p", "480p", "360p"}},
		// The common local case: a 640x480 test clip must not be upscaled to 720p.
		{"480p source drops the 720p rung", 480, []string{"480p", "360p"}},
		{"360p source keeps only the smallest", 360, []string{"360p"}},
		// Below every rung: one playable rendition beats none.
		{"240p source still yields one rendition", 240, []string{"360p"}},
		{"unknown height takes the whole ladder", 0, []string{"720p", "480p", "360p"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SelectRenditions(testLadder(), tc.sourceHeight)

			if len(got) != len(tc.want) {
				t.Fatalf("got %d renditions, want %d: %+v", len(got), len(tc.want), names(got))
			}
			for i := range got {
				if got[i].Name != tc.want[i] {
					t.Errorf("index %d: got %q, want %q (full: %v)", i, got[i].Name, tc.want[i], names(got))
				}
			}
		})
	}
}

func TestSelectRenditionsNeverReturnsEmpty(t *testing.T) {
	for _, h := range []int{1, 100, 359} {
		if got := SelectRenditions(testLadder(), h); len(got) == 0 {
			t.Errorf("source height %d produced no renditions; a job with nothing playable is worse than one slightly upscaled", h)
		}
	}
}

func names(rs []Rendition) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
	}
	return out
}
