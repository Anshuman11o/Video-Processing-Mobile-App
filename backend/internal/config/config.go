// Package config provides application configuration loaded from environment variables.
package config

import (
	"log"
	"os"
	"strconv"
	"time"
)

// Config holds all configuration values for the API server.
type Config struct {
	Port              string
	AWSRegion         string
	S3RawBucket       string
	S3ProcessedBucket string
	S3HLSBucket       string
	DynamoDBTable     string

	// QueueDriver selects the broker: QueueDriverSQLite (the default, a local
	// file) or QueueDriverSQS (real Amazon SQS). It is validated by
	// queue.FromConfig rather than here, so an unusable value fails at the one
	// place that can name the alternatives.
	QueueDriver string

	// Queue settings. QueueDBPath and QueuePollInterval configure the SQLite
	// driver only — SQS has no file and does its long polling server-side.
	// QueueVisibilityTimeout and QueueMaxDeliveries apply to both, and on SQS
	// they must match what scripts/aws-sqs-setup.sh wrote onto the queues: the
	// queue's own attributes are what AWS enforces, and these are what the
	// workers believe.
	QueueDBPath            string
	QueueVisibilityTimeout time.Duration
	QueueMaxDeliveries     int
	QueuePollInterval      time.Duration

	// JobCacheTTL is how long GET /jobs/{id} may serve a job snapshot from the
	// API's in-process cache before re-reading DynamoDB.
	//
	// It is a knob rather than a constant because the right value is a
	// freshness-versus-reads trade that differs per deployment, and hard-coding
	// it once already produced a visible bug: no worker can invalidate an
	// in-process map, so the TTL is the *only* bound on how far behind a stage
	// transition the API's answer can be. See DefaultJobCacheTTL for the default
	// and why it is what it is.
	JobCacheTTL time.Duration

	// WorkerConcurrency is how many messages one stage process handles at the
	// same time. 1 preserves the original behaviour: claim a message, finish
	// it, claim the next.
	//
	// This is concurrency WITHIN a stage, which is not where the pipeline's
	// parallelism has been coming from. The four stages are already separate
	// processes consuming separate queues, so clip B's validate overlaps clip
	// A's package without this setting doing anything — measured 2026-08-14,
	// job B's validate starting one second after job A's did and running
	// alongside A's remaining three stages.
	//
	// Raising it is therefore not a cure for a pipeline that looks serial.
	// Every stage shells out to ffmpeg, package alone runs a three-rung ladder
	// as three simultaneous x264 encodes, and every stage moves the whole video
	// to and from S3. On one machine the second concurrent job competes for the
	// same cores and the same link: in the run above, B's validate took 218s
	// against A's 67s for the same work, purely from contention. Two at once
	// can finish both later than one at a time would have.
	//
	// It earns its keep where a stage is waiting rather than working — a slow
	// S3 round trip, a stage whose ffmpeg does not saturate the box — or on a
	// host with cores to spare.
	WorkerConcurrency int

	// MockTranscribe skips the speech model and emits synthetic cues. Default
	// true: the budget rules allow only a handful of real runs, so every stage
	// downstream of transcribe is developed against the mock.
	MockTranscribe bool

	// S3PublicEndpoint is the base URL a player or mobile client uses to fetch
	// objects, when that is not the endpoint the SDK resolves on its own.
	//
	// On real AWS it is normally empty and the SDK's own regional endpoint is
	// used for everything. It stays configurable because reels are served
	// straight out of the HLS bucket with no CDN in front, so the host a player
	// must reach is a deployment decision — a bucket website endpoint, a custom
	// domain, or eventually a distribution — and not something the API can
	// infer.
	S3PublicEndpoint string

	// UploadPartSize is the multipart part size, in bytes, that the API slices
	// uploads into and presigns one URL per.
	//
	// It can only be raised, never lowered: S3 requires every part except the
	// last to be at least 5 MiB, so a smaller value is clamped rather than
	// honoured.
	//
	// This was introduced to make short test clips upload as several parts —
	// at 5 MiB a <10s clip is a single part, and one part never iterates the
	// upload loop, sums progress across parts, retries one part while holding
	// another's ETag, or assembles a multi-entry complete. That did not work:
	// see the correction in [DECIDE 8]. Exercising the multipart path needs a
	// test file larger than 5 MiB instead.
	UploadPartSize int64

	// WhisperModelPath is where the ggml model file lives. The model is
	// downloaded at runtime rather than baked into the binary, so this path is
	// expected to be persistent storage — without one it re-downloads on every
	// fresh start.
	WhisperModelPath string
}

// Load reads configuration from environment variables with sensible defaults.
func Load() *Config {
	driver := getEnv("QUEUE_DRIVER", QueueDriverSQLite)

	return &Config{
		Port:              getEnv("API_PORT", "8080"),
		AWSRegion:         getEnv("AWS_REGION", "us-east-1"),
		S3RawBucket:       getEnv("S3_RAW_BUCKET", "dayreel-raw-videos"),
		S3ProcessedBucket: getEnv("S3_PROCESSED_BUCKET", "dayreel-processed"),
		S3HLSBucket:       getEnv("S3_HLS_BUCKET", "dayreel-hls-output"),
		DynamoDBTable:     getEnv("DYNAMODB_TABLE", "dayreel-jobs"),

		// SQLite is the default because it is the one that costs nothing and
		// needs no account: a fresh checkout runs the whole pipeline without an
		// AWS queue existing. SQS is opted into.
		QueueDriver: driver,

		QueueDBPath: getEnv("QUEUE_DB_PATH", "./data/queue.db"),
		// Five minutes is the SQS default and comfortably outlives every stage
		// except transcribe, which heartbeats instead.
		QueueVisibilityTimeout: getEnvDuration("QUEUE_VISIBILITY_TIMEOUT", 5*time.Minute),
		QueueMaxDeliveries:     getEnvInt("QUEUE_MAX_DELIVERIES", 3),
		QueuePollInterval:      getEnvDuration("QUEUE_POLL_INTERVAL", defaultPollInterval(driver)),

		JobCacheTTL: getEnvDuration("JOB_CACHE_TTL", DefaultJobCacheTTL),

		// Clamped rather than validated: 0 or a negative would deadlock the
		// consume loop on a semaphore that can never be acquired, and a worker
		// that silently processes nothing is the worst of the available
		// failures. WORKER_CONCURRENCY sat in .env and .env.example for months
		// with no Go code reading it, so a deployment could believe it had
		// parallelism it did not have; this is the line that makes it true.
		WorkerConcurrency: maxInt(1, getEnvInt("WORKER_CONCURRENCY", 1)),

		MockTranscribe:   getEnv("MOCK_TRANSCRIBE", "true") == "true",
		S3PublicEndpoint: getEnv("S3_PUBLIC_ENDPOINT", ""),
		UploadPartSize:   getEnvBytes("UPLOAD_PART_SIZE", DefaultUploadPartSize),
		// Relative to the working directory, not /models: that absolute path was
		// the worker container's volume mount, and nothing mounts it now that the
		// worker runs as a plain process. Left as the container path it would
		// resolve to a directory the process cannot write and probably cannot read.
		WhisperModelPath: getEnv("WHISPER_MODEL_PATH", "./models/ggml-base.bin"),
	}
}

// The valid values of QUEUE_DRIVER. They are constants rather than bare strings
// so the factory's switch, its error message and this default cannot drift apart.
const (
	// QueueDriverSQLite is the self-hosted broker: one file, no service, no
	// per-request cost, and no way for a worker on another host to reach it.
	QueueDriverSQLite = "sqlite"

	// QueueDriverSQS is Amazon SQS: reachable from anywhere, billed per
	// request, and requiring the queues to exist before anything starts.
	QueueDriverSQS = "sqs"
)

// defaultPollInterval picks the default for QUEUE_POLL_INTERVAL, which is the
// one queue knob whose sensible value depends on the driver.
//
// The runner passes it to Receive as the long-poll duration, and the same number
// means two very different things there. On SQLite it is a local ticker's
// cadence: 250ms costs four queries a second against a file and nothing else.
// On SQS it becomes WaitTimeSeconds, whose granularity is one second — so 250ms
// is a request that returns immediately, and since the consume loop has no delay
// of its own between receives, an idle worker would spin as fast as the network
// allows, billed per request. 20s is SQS's maximum long poll and the only value
// that keeps four idle workers inside the free tier.
//
// An explicit QUEUE_POLL_INTERVAL still wins on both; this is only the default.
func defaultPollInterval(driver string) time.Duration {
	if driver == QueueDriverSQS {
		return 20 * time.Second
	}
	return 250 * time.Millisecond
}

// DefaultJobCacheTTL is the default window GET /jobs/{id} may answer from cache.
//
// One second, not the ten it used to be, because nothing invalidates the cache
// on a stage transition: the cache is a map inside the API process and the four
// workers are separate processes, so a worker cannot reach it even in principle.
// The API invalidates only on POST /jobs/{id}/complete and
// DELETE /jobs/{id}/upload, the two transitions it owns. Every later transition
// is therefore visible to a client only once the entry expires, so the TTL is
// the observation lag, and at 10s an app polling every 2s could get the same
// answer five times and skip whole stages of a ~37s pipeline.
//
// One second still absorbs a poll storm — several job screens polling at 2s
// collapse to roughly one DynamoDB read per job per second — and the reads it
// gives back are worth a fraction of a cent per hour on a ~1.7 KB item. Raise it
// via JOB_CACHE_TTL where reads cost more than freshness does.
const DefaultJobCacheTTL = 1 * time.Second

// DefaultUploadPartSize is S3's minimum size for a non-final part. Anything
// smaller is valid to upload but fails at CompleteMultipartUpload on real S3.
const DefaultUploadPartSize int64 = 5 * 1024 * 1024

// getEnvBytes reads a positive byte count, falling back on anything unusable.
//
// A malformed or non-positive value falls back rather than failing: a bad part
// size would otherwise take the API down at boot, and the fallback is always
// the value real S3 accepts.
func getEnvBytes(key string, fallback int64) int64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		log.Printf("config: ignoring %s=%q, using %d", key, raw, fallback)
		return fallback
	}
	if n < DefaultUploadPartSize {
		// Clamped, not merely warned about, because a smaller part size cannot
		// work anywhere. The 5 MiB floor was verified the hard way — 2026-08-13,
		// EntityTooSmall on CompleteMultipartUpload after every part uploaded
		// cleanly with a 200. Accepting the value would configure a system that
		// fails only at the very last step.
		log.Printf("config: %s=%d is below S3's %d minimum for non-final parts "+
			"and would fail at CompleteMultipartUpload; using %d",
			key, n, DefaultUploadPartSize, DefaultUploadPartSize)
		return fallback
	}
	return n
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// getEnvDuration accepts any Go duration string ("30s", "5m", "250ms").
// A malformed value falls back to the default rather than failing startup:
// a typo in a tuning knob should not take the API down.
func getEnvDuration(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("WARN: %s=%q is not a duration, using %s", key, raw, fallback)
		return fallback
	}
	return d
}

func getEnvInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("WARN: %s=%q is not an integer, using %d", key, raw, fallback)
		return fallback
	}
	return n
}

// maxInt is the floor guard for settings where a non-positive value would not
// merely be wrong but would hang the process — WorkerConcurrency being the case
// it exists for. Go 1.21's builtin `max` would do, but spelling it out keeps
// the clamp visible at the call site.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// PublicEndpoint is the base URL for objects a player must reach.
//
// Empty means "whatever the SDK resolves", which is the right answer on real
// AWS: presigned URLs are then signed against the genuine regional endpoint and
// no override is applied anywhere.
func (c *Config) PublicEndpoint() string {
	return c.S3PublicEndpoint
}
