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

// testEncoded mirrors what ffmpeg actually reported for a 720p30 source: note
// the 360p rung lands on a different H.264 level than the two above it.
func testEncoded() []EncodedRendition {
	ladder := testLadder()
	return []EncodedRendition{
		{Rendition: ladder[0], ActualWidth: 1280, ActualHeight: 720, Codecs: "avc1.4d401f,mp4a.40.2"},
		{Rendition: ladder[1], ActualWidth: 854, ActualHeight: 480, Codecs: "avc1.4d401f,mp4a.40.2"},
		{Rendition: ladder[2], ActualWidth: 640, ActualHeight: 360, Codecs: "avc1.4d401e,mp4a.40.2"},
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
	got := MasterPlaylist(testEncoded(), testSubs())

	if !strings.HasPrefix(got, "#EXTM3U\n") {
		t.Errorf("playlist must begin with #EXTM3U:\n%s", got)
	}
	// Version 6 matches what ffmpeg writes into the media playlists it generates
	// for these segments; a mismatch between master and media is a spec smell.
	if !strings.Contains(got, "#EXT-X-VERSION:6") {
		t.Errorf("expected version 6 to match the generated media playlists:\n%s", got)
	}
	if !strings.Contains(got, "#EXT-X-INDEPENDENT-SEGMENTS") {
		t.Errorf("missing EXT-X-INDEPENDENT-SEGMENTS:\n%s", got)
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
	got := MasterPlaylist(testEncoded(), testSubs())

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
	// AUTOSELECT is what makes a player enable the track for a matching system
	// language without the user choosing it explicitly.
	if !strings.Contains(got, "AUTOSELECT=YES") {
		t.Errorf("missing AUTOSELECT on the subtitle rendition:\n%s", got)
	}
}

// The measured resolution must win over the ladder's nominal one. A 1280x720
// source scaled to the 480p rung yields 854x480, and advertising the ladder's
// number instead would describe a stream that does not exist.
func TestMasterPlaylistUsesMeasuredResolution(t *testing.T) {
	encoded := testEncoded()
	encoded[1].ActualWidth = 754 // e.g. a 1130x720 source
	encoded[1].ActualHeight = 480

	got := MasterPlaylist(encoded, nil)

	if !strings.Contains(got, "RESOLUTION=754x480") {
		t.Errorf("measured resolution not used:\n%s", got)
	}
	if strings.Contains(got, "RESOLUTION=854x480") {
		t.Errorf("ladder resolution leaked into the master:\n%s", got)
	}
}

func TestCodecsString(t *testing.T) {
	// Level 31 (0x1f) is 720p30; level 32 (0x20) is what 720p60 produces. A
	// hardcoded table would be wrong for every 60fps upload.
	tests := map[int]string{
		31: "avc1.4d401f,mp4a.40.2",
		30: "avc1.4d401e,mp4a.40.2",
		32: "avc1.4d4020,mp4a.40.2",
		20: "avc1.4d4014,mp4a.40.2",
	}
	for level, want := range tests {
		if got := CodecsString(level); got != want {
			t.Errorf("CodecsString(%d) = %q, want %q", level, got, want)
		}
	}
}

func TestMasterPlaylistWithoutSubtitles(t *testing.T) {
	got := MasterPlaylist(testEncoded(), nil)

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
		if r.peakBandwidth() <= r.averageBandwidth() {
			t.Errorf("%s: BANDWIDTH %d does not exceed AVERAGE-BANDWIDTH %d",
				r.Name, r.peakBandwidth(), r.averageBandwidth())
		}
	}

	// These are the exact values ffmpeg computes for its own master playlists;
	// matching them keeps our hand-written master consistent with the segments.
	want := map[string]int{"720p": 3220800, "480p": 1680800, "360p": 985600}
	for _, r := range testLadder() {
		if got := r.peakBandwidth(); got != want[r.Name] {
			t.Errorf("%s: BANDWIDTH = %d, want %d (ffmpeg's own value)", r.Name, got, want[r.Name])
		}
	}
}

func TestMasterPlaylistDeclaresResolutionAndCodecs(t *testing.T) {
	got := MasterPlaylist(testEncoded(), nil)

	for _, want := range []string{"RESOLUTION=1280x720", "RESOLUTION=854x480", "RESOLUTION=640x360"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
	// CODECS comes from measurement, not from the ladder: the level byte differs
	// per rendition and moves with frame rate. Asserting all three are identical
	// would bake in the very assumption that is wrong.
	if n := strings.Count(got, "CODECS="); n != 3 {
		t.Errorf("expected a CODECS attribute on each of 3 variants, got %d:\n%s", n, got)
	}
	if !strings.Contains(got, `CODECS="avc1.4d401e,mp4a.40.2"`) {
		t.Errorf("the 360p rendition's distinct level was not carried through:\n%s", got)
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
