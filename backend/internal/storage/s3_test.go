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

// localConfig mirrors the compose environment: the API reaches S3 at an
// in-cluster hostname, and clients reach it somewhere else entirely.
func localConfig() *config.Config {
	return &config.Config{
		AWSRegion:        "us-east-1",
		AWSEndpoint:      "http://localstack:4566",
		S3PublicEndpoint: "http://10.0.2.2:4566",
		S3RawBucket:      "dayreel-raw-videos",
		UseLocalStack:    true,
	}
}

// A re-presigned URL must be signed for the host the client can reach, exactly
// as a freshly created one is.
//
// This is the stage 7 regression guard aimed at stage 8A's new code path: the
// resume handler presigns the same parts a second time, and a handler that
// built its own presign call instead of reusing GeneratePresignedUploadURL
// would sign the in-cluster hostname. The URL would look right, upload fine
// from inside the compose network, and be unusable from the device — which is
// the bug stage 7 existed to fix.
func TestPresignedUploadURLSignsPublicEndpoint(t *testing.T) {
	client, err := NewS3Client(context.Background(), localConfig())
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

	if parsed.Host != "10.0.2.2:4566" {
		t.Errorf("presigned host = %q, want the public endpoint 10.0.2.2:4566", parsed.Host)
	}
	if parsed.Host == "localstack:4566" {
		t.Errorf("presigned against the in-cluster endpoint; no client can reach it")
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

// On real AWS both endpoints are the same and the override must disappear
// rather than sign something invented.
func TestPresignedUploadURLWithoutPublicEndpoint(t *testing.T) {
	cfg := localConfig()
	cfg.S3PublicEndpoint = ""

	client, err := NewS3Client(context.Background(), cfg)
	if err != nil {
		t.Fatalf("new s3 client: %v", err)
	}
	if client.presignEndpoint != "" {
		t.Errorf("presignEndpoint = %q, want empty when public and internal agree", client.presignEndpoint)
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
	if parsed.Host != "localstack:4566" {
		t.Errorf("presigned host = %q, want the configured endpoint", parsed.Host)
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
