package storage

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/anshumanagarwal/dayreel/internal/config"
)

// staticCredentials puts credentials in the environment for the duration of a
// test.
//
// Presigning is pure signing arithmetic — no request leaves the process — but
// the SDK still needs a key to sign with, and the default credential chain now
// has no emulator branch to fall back on. Without this the chain would reach for
// IMDS and the test would fail on a machine that simply has no AWS account
// configured.
func staticCredentials(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIAIOSFODNN7EXAMPLE")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY")
	t.Setenv("AWS_SESSION_TOKEN", "")
}

// publicEndpointConfig is a deployment whose clients reach the objects at a host
// the SDK would not resolve on its own — a bucket exposed under a custom domain,
// or an S3-compatible endpoint in front of it.
func publicEndpointConfig() *config.Config {
	return &config.Config{
		AWSRegion:        "us-east-1",
		S3PublicEndpoint: "https://media.dayreel.example",
		S3RawBucket:      "dayreel-raw-videos",
	}
}

// A re-presigned URL must be signed for the host the client can reach, exactly
// as a freshly created one is.
//
// This is the stage 7 regression guard aimed at stage 8A's new code path: the
// resume handler presigns the same parts a second time, and a handler that
// built its own presign call instead of reusing GeneratePresignedUploadURL
// would sign whatever host the SDK resolved. The URL would look right, upload
// fine from the server, and be unusable from the device — which is the bug
// stage 7 existed to fix.
func TestPresignedUploadURLSignsPublicEndpoint(t *testing.T) {
	staticCredentials(t)

	client, err := NewS3Client(context.Background(), publicEndpointConfig())
	if err != nil {
		t.Fatalf("new s3 client: %v", err)
	}

	raw, err := client.GeneratePresignedUploadURL(
		context.Background(), "job-id/clip.mp4", "upload-id", 3, time.Hour)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse presigned url %q: %v", raw, err)
	}

	if parsed.Host != "media.dayreel.example" {
		t.Errorf("presigned host = %q, want the public endpoint media.dayreel.example", parsed.Host)
	}

	// Path style, or the bucket moves into a hostname that does not resolve.
	if got := parsed.Path; got != "/dayreel-raw-videos/job-id/clip.mp4" {
		t.Errorf("presigned path = %q, want path-style with the bucket in the path", got)
	}

	// The signature covers the part number, so a resumed URL is bound to the
	// part it was issued for and cannot be reused for another.
	if got := parsed.Query().Get("partNumber"); got != "3" {
		t.Errorf("partNumber = %q, want 3", got)
	}
	if parsed.Query().Get("X-Amz-Signature") == "" {
		t.Error("no X-Amz-Signature on the presigned URL")
	}
}

// The normal case on real AWS: nothing is configured, so nothing is overridden
// and the SDK signs the genuine regional endpoint. The override mechanism has to
// disappear on this path rather than sign something invented — a URL signed for
// a host that does not exist fails with SignatureDoesNotMatch, which reads as a
// credentials problem and is not one.
func TestPresignedUploadURLWithoutPublicEndpoint(t *testing.T) {
	staticCredentials(t)

	cfg := publicEndpointConfig()
	cfg.S3PublicEndpoint = ""

	client, err := NewS3Client(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new s3 client: %v", err)
	}
	if client.presignEndpoint != "" {
		t.Errorf("presignEndpoint = %q, want empty when no public endpoint is set", client.presignEndpoint)
	}

	raw, err := client.GeneratePresignedUploadURL(
		context.Background(), "job-id/clip.mp4", "upload-id", 1, time.Hour)
	if err != nil {
		t.Fatalf("presign: %v", err)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse presigned url: %v", err)
	}
	// Virtual-hosted style against real S3, which is what the SDK does when no
	// BaseEndpoint is forced on it.
	if parsed.Host != "dayreel-raw-videos.s3.us-east-1.amazonaws.com" {
		t.Errorf("presigned host = %q, want the regional S3 endpoint", parsed.Host)
	}
}

// IsNoSuchUpload decides whether a resume tells the client "start over" or
// "retry", so every way S3 and LocalStack can express the condition has to be
// recognised — and nothing else may be.
func TestIsNoSuchUpload(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "typed error from the SDK",
			err:  &types.NoSuchUpload{Message: aws.String("upload does not exist")},
			want: true,
		},
		{
			name: "typed error wrapped by the storage layer",
			err:  fmt.Errorf("list parts for k: %w", &types.NoSuchUpload{}),
			want: true,
		},
		{
			// The shape LocalStack tends to produce: the code is right, the
			// body never deserializes into the typed error.
			name: "generic api error carrying the code",
			err:  &smithy.GenericAPIError{Code: "NoSuchUpload", Message: "no such upload"},
			want: true,
		},
		{
			name: "generic api error wrapped",
			err:  fmt.Errorf("complete: %w", &smithy.GenericAPIError{Code: "NoSuchUpload"}),
			want: true,
		},
		{
			// The one that matters most. NoSuchKey is a different condition and
			// treating it as a reaped upload would tell a client to re-upload a
			// file that is already there.
			name: "a different api error",
			err:  &smithy.GenericAPIError{Code: "NoSuchKey"},
			want: false,
		},
		{
			name: "entity too small — the part-size failure, not a missing upload",
			err:  &smithy.GenericAPIError{Code: "EntityTooSmall"},
			want: false,
		},
		{
			name: "a transport failure, which must be retried and not abandoned",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "no error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNoSuchUpload(tc.err); got != tc.want {
				t.Errorf("IsNoSuchUpload(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// InvalidPart is the failure a resumed upload hits when the part list and S3
// disagree. It must be told apart from a transient error, because retrying it
// can only ever fail the same way.
func TestIsInvalidPart(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			// Observed for real: LocalStack rewrites a re-uploaded part's ETag
			// to the literal string "None", and completion then fails here.
			name: "invalid part",
			err:  &smithy.GenericAPIError{Code: "InvalidPart", Message: "one or more parts could not be found"},
			want: true,
		},
		{
			name: "invalid part order",
			err:  &smithy.GenericAPIError{Code: "InvalidPartOrder"},
			want: true,
		},
		{
			name: "wrapped",
			err:  fmt.Errorf("complete multipart upload: %w", &smithy.GenericAPIError{Code: "InvalidPart"}),
			want: true,
		},
		{
			name: "no such upload is a different recovery",
			err:  &smithy.GenericAPIError{Code: "NoSuchUpload"},
			want: false,
		},
		{
			name: "entity too small is the part-size failure",
			err:  &smithy.GenericAPIError{Code: "EntityTooSmall"},
			want: false,
		},
		{
			name: "a transport failure must stay retryable",
			err:  errors.New("i/o timeout"),
			want: false,
		},
		{
			name: "no error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsInvalidPart(tc.err); got != tc.want {
				t.Errorf("IsInvalidPart(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
