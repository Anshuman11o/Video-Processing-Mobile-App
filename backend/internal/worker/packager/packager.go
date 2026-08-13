// Package packager implements the terminal pipeline stage: transcode the
// validated video into an HLS ladder with a subtitle rendition, and mark the
// job finished.
//
// Named packager rather than package because "package" is a Go keyword.
package packager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/media"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/storage"
	"github.com/anshumanagarwal/dayreel/internal/worker"
)

// DefaultLadder is the encoding ladder. Rungs taller than the source are
// dropped at runtime by media.SelectRenditions.
//
// Bitrates are starting guesses, unmeasured against real footage.
var DefaultLadder = []media.Rendition{
	{Name: "720p", Width: 1280, Height: 720, VideoBitrateKbps: 2800, AudioBitrateKbps: 128},
	{Name: "480p", Width: 854, Height: 480, VideoBitrateKbps: 1400, AudioBitrateKbps: 128},
	{Name: "360p", Width: 640, Height: 360, VideoBitrateKbps: 800, AudioBitrateKbps: 96},
}

// SegmentSeconds is the HLS target segment duration, per PROJECT_PLAN.
const SegmentSeconds = 6

// Options configure the packager.
type Options struct {
	Ladder         []media.Rendition
	SegmentSeconds int

	// PublicEndpoint is the base URL a player will use to fetch HLS output,
	// when that is not the endpoint S3 itself serves the bucket on.
	//
	// Reels are served straight out of the HLS bucket with no CDN in front, so
	// the host a player must reach — a bucket website endpoint, a custom domain,
	// eventually a distribution — is a deployment decision. It stays
	// configurable for that reason.
	//
	// Empty is the normal setting on real AWS and means "S3's own endpoint for
	// this bucket", which publicURL derives from Region. It used to mean
	// something much worse: the formatted URL began with a bare "/", so the
	// hls_url handed to the client had no scheme and no host at all.
	PublicEndpoint string

	// Region is the bucket's AWS region, used only to derive the endpoint when
	// PublicEndpoint is empty. Ignored when PublicEndpoint is set.
	Region string
}

// DefaultOptions returns options for the given public endpoint and region.
func DefaultOptions(publicEndpoint, region string) Options {
	return Options{
		Ladder:         DefaultLadder,
		SegmentSeconds: SegmentSeconds,
		PublicEndpoint: publicEndpoint,
		Region:         region,
	}
}

// Stage packages a validated video as HLS and completes the job.
type Stage struct {
	s3          *storage.S3Client
	db          *db.DynamoDBClient
	hlsBucket   string
	inputBucket string
	opts        Options
}

// New creates a packager Stage.
//
// It takes a database client, which no other stage does. That is the cost of
// being the terminal stage: it is the only one that finishes the job, via the
// worker.Finalizer hook.
func New(
	s3 *storage.S3Client, database *db.DynamoDBClient,
	hlsBucket, inputBucket string, opts Options,
) *Stage {
	return &Stage{s3: s3, db: database, hlsBucket: hlsBucket, inputBucket: inputBucket, opts: opts}
}

func (s *Stage) Name() models.StageName { return models.StagePackage }

func (s *Stage) OutputBucket() string { return s.hlsBucket }

// OutputKey is the master playlist.
//
// No separate manifest is needed here, unlike extract: master.m3u8 already
// references every rendition playlist, which reference every segment. It is
// uploaded last, so its existence means the whole tree landed.
func (s *Stage) OutputKey(jobID string) string { return jobID + "/master.m3u8" }

// Process transcodes the validated video into an HLS ladder and uploads it.
func (s *Stage) Process(ctx context.Context, msg *events.StageMessage) (string, error) {
	dir, err := os.MkdirTemp("", "dayreel-package-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// This stage needs two inputs. The message names the transcript — what the
	// previous stage produced — and the video is derived from the job ID, per
	// the convention the pipeline has settled into.
	videoKey := msg.JobID + "/validated.mp4"
	videoPath := filepath.Join(dir, "input.mp4")
	if err := s.s3.DownloadToFile(ctx, s.inputBucket, videoKey, videoPath); err != nil {
		return "", fmt.Errorf("download video: %w", err)
	}

	probe, err := media.Probe(ctx, videoPath)
	if err != nil {
		return "", worker.Permanent("validated video is not readable", err)
	}

	renditions := media.SelectRenditions(s.opts.Ladder, probe.Height)

	outDir := filepath.Join(dir, "hls")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return "", fmt.Errorf("create output dir: %w", err)
	}

	// The encoded renditions carry measured resolution and codec strings, which
	// the master playlist needs. They cannot be predicted from the ladder: the
	// H.264 level shifts with frame rate, and aspect-preserving scaling rounds to
	// even dimensions.
	encoded, err := media.EncodeHLSLadder(ctx, videoPath, outDir, renditions, s.opts.SegmentSeconds)
	if err != nil {
		// The input already passed validate and extract, so a transcode failure
		// is a property of the file rather than of the attempt.
		return "", worker.Permanent("hls transcode failed", err)
	}

	// The anchor comes from the encode, so subtitles cannot be written until it
	// has run: cue times are meaningless until the stream's own zero is known.
	anchor := media.SubtitleTimestampAnchor(encoded)

	subs, err := s.writeSubtitles(ctx, dir, outDir, msg, probe.DurationSec, anchor)
	if err != nil {
		return "", err
	}

	master := media.MasterPlaylist(encoded, subs)
	masterPath := filepath.Join(outDir, "master.m3u8")
	if err := os.WriteFile(masterPath, []byte(master), 0o644); err != nil {
		return "", fmt.Errorf("write master playlist: %w", err)
	}

	// Everything except the master goes up first. The master is the declared
	// output, so it must not exist until the things it references do.
	if err := s.uploadTree(ctx, outDir, msg.JobID, masterPath); err != nil {
		return "", err
	}

	outputKey := s.OutputKey(msg.JobID)
	if err := s.s3.UploadFile(ctx, s.hlsBucket, outputKey, masterPath, "application/vnd.apple.mpegurl"); err != nil {
		return "", fmt.Errorf("upload master playlist: %w", err)
	}

	return outputKey, nil
}

// writeSubtitles fetches the transcript and builds the subtitle rendition.
//
// Returns nil when there is no transcript, in which case the master advertises
// no subtitle track and the video still plays.
func (s *Stage) writeSubtitles(
	ctx context.Context, dir, outDir string, msg *events.StageMessage,
	duration float64, anchor int64,
) (*media.SubtitleRendition, error) {
	vttPath := filepath.Join(dir, "transcript.vtt")
	if err := s.s3.DownloadToFile(ctx, msg.Input.Bucket, msg.Input.Key, vttPath); err != nil {
		return nil, fmt.Errorf("download transcript: %w", err)
	}

	subsDir := filepath.Join(outDir, "subs")
	if err := os.MkdirAll(subsDir, 0o755); err != nil {
		return nil, fmt.Errorf("create subs dir: %w", err)
	}

	// A cue-less WEBVTT is the expected output for a silent clip, and is still a
	// valid subtitle rendition. It is carried through rather than dropped so the
	// caption track exists and is simply empty.
	body, err := os.ReadFile(vttPath)
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}

	// The transcript's cue times are relative to the media; the MPEG-TS segments
	// are not, so the two need tying together explicitly. Measured through
	// AVFoundation: without this every cue plays 66.7 ms early on a 30fps source.
	doc := media.WithTimestampMap(string(body), anchor)

	if err := os.WriteFile(filepath.Join(subsDir, "subs_000.vtt"), []byte(doc), 0o644); err != nil {
		return nil, fmt.Errorf("write subtitle segment: %w", err)
	}

	playlist := media.SubtitlePlaylist("subs_000.vtt", duration)
	if err := os.WriteFile(filepath.Join(subsDir, "playlist.m3u8"), []byte(playlist), 0o644); err != nil {
		return nil, fmt.Errorf("write subtitle playlist: %w", err)
	}

	return &media.SubtitleRendition{
		GroupID:  "subs",
		Name:     "English",
		Language: "en",
		URI:      "subs/playlist.m3u8",
		Default:  true,
	}, nil
}

// upload is one file to send to S3.
type upload struct {
	Key  string
	Path string
}

// collectUploads walks root and returns every file except skipPath, keyed under
// jobID.
//
// Separated from the uploading so the two things that are easy to get wrong —
// which files are included, and how keys are built — can be tested without S3.
func collectUploads(root, jobID, skipPath string) ([]upload, error) {
	var uploads []upload

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		// The master playlist is excluded here and uploaded last by the caller:
		// it is the declared output, so it must not appear before the objects it
		// references.
		if info.IsDir() || path == skipPath {
			return nil
		}

		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relative path for %s: %w", path, err)
		}

		// ToSlash because S3 keys are slash-separated regardless of host OS.
		uploads = append(uploads, upload{
			Key:  jobID + "/" + filepath.ToSlash(rel),
			Path: path,
		})
		return nil
	})

	return uploads, err
}

// uploadTree uploads everything under root except skipPath.
func (s *Stage) uploadTree(ctx context.Context, root, jobID, skipPath string) error {
	uploads, err := collectUploads(root, jobID, skipPath)
	if err != nil {
		return fmt.Errorf("collect hls output: %w", err)
	}

	for _, u := range uploads {
		if err := s.s3.UploadFile(ctx, s.hlsBucket, u.Key, u.Path, contentTypeFor(u.Path)); err != nil {
			return fmt.Errorf("upload %s: %w", u.Key, err)
		}
	}
	return nil
}

// Finalize marks the job completed and records where its output lives.
//
// This satisfies worker.Finalizer, and it is the only implementation in the
// project. The runner calls it after the stage's own row is marked completed,
// so the job can never claim completion ahead of its final stage.
func (s *Stage) Finalize(ctx context.Context, msg *events.StageMessage, outputKey string) error {
	job, err := s.db.GetJob(ctx, msg.JobID)
	if err != nil {
		return fmt.Errorf("load job for completion: %w", err)
	}
	if job == nil {
		return fmt.Errorf("job %s not found", msg.JobID)
	}

	// Duration and thumbnail come from the extract manifest rather than a fresh
	// probe: extract already recorded both.
	duration, thumbnailKey := s.outputDetails(ctx, msg.JobID)

	output := &models.OutputInfo{
		HLSURL:          s.publicURL(s.hlsBucket, outputKey),
		DurationSeconds: duration,
	}
	if thumbnailKey != "" {
		output.ThumbnailURL = s.publicURL(s.inputBucket, thumbnailKey)
	}

	var totalMs int64
	if !job.CreatedAt.IsZero() {
		totalMs = time.Since(job.CreatedAt).Milliseconds()
	}

	return s.db.CompleteJob(ctx, msg.JobID, output, totalMs)
}

// outputDetails reads duration and the first frame from the extract manifest.
//
// Best-effort: a missing or unreadable manifest costs metadata, not the job. The
// HLS output is already written and playable by this point, so failing here
// would strand a finished job over a thumbnail.
func (s *Stage) outputDetails(ctx context.Context, jobID string) (float64, string) {
	dir, err := os.MkdirTemp("", "dayreel-package-meta-*")
	if err != nil {
		return 0, ""
	}
	defer os.RemoveAll(dir)

	path := filepath.Join(dir, "extract.json")
	if err := s.s3.DownloadToFile(ctx, s.inputBucket, events.ExtractManifestKey(jobID), path); err != nil {
		return 0, ""
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}

	var manifest events.ExtractManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return 0, ""
	}

	var thumbnail string
	if len(manifest.Frames) > 0 {
		thumbnail = manifest.Frames[0].Key
	}
	return manifest.DurationSeconds, thumbnail
}

// publicURL builds a browser-reachable URL for an object.
//
// An explicit PublicEndpoint is treated as a prefix in front of the bucket,
// because that is what a proxy or an emulator needs: one host serving many
// buckets, addressed path-style.
//
// Empty means real S3, and there the bucket is not a path segment — it is part
// of the host. Formatting it path-style against an empty base produced
// "/dayreel-hls-output/<job>/master.m3u8": no scheme, no host, and no way for a
// player to resolve it. Since S3_PUBLIC_ENDPOINT is documented as empty on real
// AWS, that was the URL every completed job actually handed back.
//
// Virtual-hosted style is the form S3 wants: path-style addressing is
// deprecated for buckets created after September 2020, so deriving the
// path-style URL instead would have been a second, slower bug.
func (s *Stage) publicURL(bucket, key string) string {
	if s.opts.PublicEndpoint == "" {
		region := s.opts.Region
		if region == "" {
			// Only reachable if a caller built Options by hand. us-east-1 is
			// wrong for a bucket elsewhere, but it is a resolvable host that
			// fails loudly on a redirect, rather than a path that resolves to
			// nothing anywhere.
			region = "us-east-1"
		}
		return fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", bucket, region, key)
	}
	base := strings.TrimSuffix(s.opts.PublicEndpoint, "/")
	return fmt.Sprintf("%s/%s/%s", base, bucket, key)
}

// contentTypeFor maps HLS artifacts to their media types. Players are tolerant
// of a wrong type on segments but not on playlists.
func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".m3u8":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".vtt":
		return "text/vtt"
	default:
		return "application/octet-stream"
	}
}
