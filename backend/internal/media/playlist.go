package media

import (
	"fmt"
	"strings"
)

// Rendition is one video quality tier in an HLS ladder.
type Rendition struct {
	Name   string // directory name, e.g. "480p"
	Width  int
	Height int

	// VideoBitrateKbps and AudioBitrateKbps are what the encoder was asked for.
	VideoBitrateKbps int
	AudioBitrateKbps int
}

// PlaylistPath is the rendition's playlist, relative to the master.
func (r Rendition) PlaylistPath() string { return r.Name + "/playlist.m3u8" }

// bandwidth is the advertised peak bitrate in bits per second.
//
// Video plus audio plus a margin for container overhead. Advertising less than
// the stream actually needs makes a player pick a rendition it cannot sustain,
// which shows up as rebuffering rather than as an error.
func (r Rendition) bandwidth() int {
	return (r.VideoBitrateKbps+r.AudioBitrateKbps)*1000 + 128_000
}

// SubtitleRendition describes the WebVTT track advertised in the master.
type SubtitleRendition struct {
	// GroupID ties EXT-X-MEDIA to the SUBTITLES attribute on each variant. The
	// two must match exactly or players silently show no captions.
	GroupID  string
	Name     string // human-readable, shown in a player's caption menu
	Language string // RFC 5646, e.g. "en"
	URI      string // subtitle playlist, relative to the master
	Default  bool
	Forced   bool
}

// MasterPlaylist renders an HLS master playlist.
//
// ffmpeg can write its own master, but not one that includes a subtitle
// rendition — its HLS muxer has no concept of one. Since the subtitle track is
// the whole point of pairing this stage with the transcript, the master is
// assembled here instead.
//
// subs may be nil, in which case no subtitle rendition is advertised and no
// variant carries a SUBTITLES attribute.
func MasterPlaylist(renditions []Rendition, subs *SubtitleRendition) string {
	var b strings.Builder

	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")

	if subs != nil {
		b.WriteString(fmt.Sprintf(
			"#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=%q,NAME=%q,LANGUAGE=%q,DEFAULT=%s,FORCED=%s,URI=%q\n",
			subs.GroupID, subs.Name, subs.Language,
			yesNo(subs.Default), yesNo(subs.Forced), subs.URI,
		))
	}

	for _, r := range renditions {
		attrs := []string{
			fmt.Sprintf("BANDWIDTH=%d", r.bandwidth()),
			fmt.Sprintf("RESOLUTION=%dx%d", r.Width, r.Height),
			// avc1.4d401f is H.264 Main profile level 3.1, aac-lc is mp4a.40.2.
			// A player uses this to decide whether it can play the variant at
			// all, before fetching a single segment.
			`CODECS="avc1.4d401f,mp4a.40.2"`,
		}
		if subs != nil {
			// Every variant must carry this. A player that switches to a variant
			// without it loses captions mid-playback, which reads as a bug in the
			// video rather than in the playlist.
			attrs = append(attrs, fmt.Sprintf("SUBTITLES=%q", subs.GroupID))
		}

		b.WriteString("#EXT-X-STREAM-INF:" + strings.Join(attrs, ",") + "\n")
		b.WriteString(r.PlaylistPath() + "\n")
	}

	return b.String()
}

// SubtitlePlaylist renders the media playlist for a single WebVTT file.
//
// The whole transcript is one "segment". Segmenting subtitles matters for long
// content, where a player should not fetch an hour of captions to show the first
// line — but clips here are capped at a minute, so a single entry is both valid
// and simpler.
//
// targetDuration must be >= the media duration, or players may stall waiting for
// a segment that never arrives.
func SubtitlePlaylist(vttURI string, durationSeconds float64) string {
	target := int(durationSeconds)
	if float64(target) < durationSeconds {
		target++ // EXT-X-TARGETDURATION is an integer and must not round down
	}
	if target < 1 {
		target = 1
	}

	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	b.WriteString("#EXT-X-VERSION:3\n")
	b.WriteString(fmt.Sprintf("#EXT-X-TARGETDURATION:%d\n", target))
	b.WriteString("#EXT-X-MEDIA-SEQUENCE:0\n")
	// VOD tells the player the whole thing is available now and seeking is safe.
	b.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	b.WriteString(fmt.Sprintf("#EXTINF:%.3f,\n", durationSeconds))
	b.WriteString(vttURI + "\n")
	// Without ENDLIST a player treats this as a live stream and keeps polling.
	b.WriteString("#EXT-X-ENDLIST\n")

	return b.String()
}

// SelectRenditions returns the ladder rungs worth encoding for a source.
//
// Rungs taller than the source are dropped: upscaling produces a larger file
// that looks no better, and every rendition costs encode time and S3 PUTs.
//
// If the source is smaller than every rung, the smallest is kept anyway. A job
// with no playable rendition is worse than one encoded slightly above its source
// resolution, and this is the common case for phone-recorded vertical clips.
func SelectRenditions(ladder []Rendition, sourceHeight int) []Rendition {
	if len(ladder) == 0 {
		return nil
	}

	selected := make([]Rendition, 0, len(ladder))
	smallest := ladder[0]
	for _, r := range ladder {
		if r.Height < smallest.Height {
			smallest = r
		}
		if sourceHeight <= 0 || r.Height <= sourceHeight {
			selected = append(selected, r)
		}
	}

	if len(selected) == 0 {
		return []Rendition{smallest}
	}
	return selected
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}
