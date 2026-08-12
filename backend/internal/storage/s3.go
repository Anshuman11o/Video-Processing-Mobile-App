// Package storage provides S3 operations for multipart uploads and presigned URLs.
package storage

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/anshumanagarwal/dayreel/internal/config"
)

// CompletedPart represents a completed upload part with its ETag.
type CompletedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

// UploadedPart is a part S3 reports it is already holding for an in-flight
// multipart upload.
//
// This is the authoritative record of upload progress, and it is authoritative
// in a way a client-kept list cannot be: UploadPart is all-or-nothing — the
// request carries a Content-Length and a truncated body is rejected — so a part
// S3 lists is a whole part. The client's note that it uploaded one is a separate
// write on a separate machine, and that machine is the one being killed.
type UploadedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
	Size       int64  `json:"size"`
}

// InFlightUpload is a multipart upload that was created and then neither
// completed nor aborted.
//
// These are not objects. They do not appear in ListObjects, in `aws s3 ls`, or
// in the console's object listing, and they bill as storage from the moment the
// first part lands. ListMultipartUploads is the only thing that can see them.
type InFlightUpload struct {
	Key       string    `json:"key"`
	UploadID  string    `json:"upload_id"`
	Initiated time.Time `json:"initiated"`
}

// S3Client wraps the AWS S3 client for multipart upload operations.
type S3Client struct {
	client        *s3.Client
	presignClient *s3.PresignClient
	bucket        string

	// presignEndpoint is the host presigned URLs are SIGNED against, when that
	// is not the endpoint the API itself talks to.
	//
	// SigV4 covers the Host header, so a presigned URL is only valid for the
	// exact host it was signed for. Signing against a host the device cannot
	// reach produces URLs that are correct, unusable, and fail with
	// SignatureDoesNotMatch the moment anyone rewrites the host to something
	// reachable — which is the stage 7 bug this field exists to prevent.
	//
	// Empty means sign against whatever the SDK resolves, which is the normal
	// case on real AWS: the regional S3 endpoint is reachable from the phone as
	// well as from the server, so nothing needs overriding.
	presignEndpoint string
}

// NewS3Client creates a new S3Client configured for the given bucket.
func NewS3Client(ctx context.Context, cfg *config.Config) (*S3Client, error) {
	// Credentials come from the SDK's default chain: environment, shared config
	// file, or the instance/task role. There is no emulator to special-case any
	// more, so there are no static test credentials either.
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	client := s3.NewFromConfig(awsCfg)
	presignClient := s3.NewPresignClient(client)

	return &S3Client{
		client:          client,
		presignClient:   presignClient,
		bucket:          cfg.S3RawBucket,
		presignEndpoint: cfg.PublicEndpoint(),
	}, nil
}

// CreateMultipartUpload starts a multipart upload and returns the upload ID.
func (s *S3Client) CreateMultipartUpload(ctx context.Context, key, contentType string) (string, error) {
	result, err := s.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		ContentType: aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("create multipart upload: %w", err)
	}
	return *result.UploadId, nil
}

// GeneratePresignedUploadURL creates a presigned URL for uploading a single part.
func (s *S3Client) GeneratePresignedUploadURL(ctx context.Context, key, uploadID string, partNumber int, expiry time.Duration) (string, error) {
	result, err := s.presignClient.PresignUploadPart(ctx, &s3.UploadPartInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(uploadID),
		PartNumber: aws.Int32(int32(partNumber)),
	}, s.presignOptions(expiry)...)
	if err != nil {
		return "", fmt.Errorf("presign upload part %d: %w", partNumber, err)
	}
	return result.URL, nil
}

// CompleteMultipartUpload finalizes a multipart upload with the given ETags.
//
// It is idempotent, and has to be. POST /jobs/{id}/complete now enqueues the
// validate stage as well as assembling the object, and a failed enqueue is
// answered with a 500 the client is expected to retry — see api.CompleteUpload.
// A retry that arrives after S3 has already assembled the object gets
// NoSuchUpload, because the upload ID is gone precisely because it succeeded.
// Reporting that as a failure would strand a video sitting complete in the
// bucket, so a HeadObject settles it: if the object is there, the upload is
// done, whatever this call said.
//
// The fallback is deliberately narrow. Only NoSuchUpload can mean "already
// assembled"; every other rejection — InvalidPart, EntityTooSmall, a transport
// failure — leaves no object behind, and the caller needs the real error to
// choose between telling the client to restart and telling it to retry.
func (s *S3Client) CompleteMultipartUpload(ctx context.Context, key, uploadID string, parts []CompletedPart) error {
	completedParts := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		completedParts[i] = types.CompletedPart{
			PartNumber: aws.Int32(int32(p.PartNumber)),
			ETag:       aws.String(p.ETag),
		}
	}

	_, err := s.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: completedParts,
		},
	})
	if err != nil {
		if IsNoSuchUpload(err) {
			if exists, headErr := s.ObjectExists(ctx, s.bucket, key); headErr == nil && exists {
				return nil
			}
		}
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	return nil
}

// ListParts returns every part S3 currently holds for a multipart upload.
//
// Returns a NoSuchUpload error — see IsNoSuchUpload — when the upload has been
// aborted, reaped, or already completed. Callers must distinguish that from a
// transport failure, because one means "start over" and the other means "retry".
func (s *S3Client) ListParts(ctx context.Context, key, uploadID string) ([]UploadedPart, error) {
	var (
		parts  []UploadedPart
		marker *string
	)

	// Paged because a response caps at 1000 parts. At the 5 MiB minimum part
	// size that is a 5 GB upload, so nothing this project produces reaches a
	// second page: this loop is written and, locally, never executed.
	for {
		out, err := s.client.ListParts(ctx, &s3.ListPartsInput{
			Bucket:           aws.String(s.bucket),
			Key:              aws.String(key),
			UploadId:         aws.String(uploadID),
			PartNumberMarker: marker,
		})
		if err != nil {
			return nil, fmt.Errorf("list parts for %s: %w", key, err)
		}

		for _, p := range out.Parts {
			parts = append(parts, UploadedPart{
				PartNumber: int(aws.ToInt32(p.PartNumber)),
				ETag:       aws.ToString(p.ETag),
				Size:       aws.ToInt64(p.Size),
			})
		}

		if !aws.ToBool(out.IsTruncated) {
			return parts, nil
		}
		marker = out.NextPartNumberMarker
	}
}

// ListMultipartUploads returns every unfinished multipart upload in the bucket,
// optionally restricted to a key prefix.
//
// Nothing in the request path needs this. It exists so abandoned uploads can be
// named, which is what config/free-tier.md's teardown rule requires and what no
// other listing in this project can do.
func (s *S3Client) ListMultipartUploads(ctx context.Context, prefix string) ([]InFlightUpload, error) {
	var (
		uploads   []InFlightUpload
		keyMarker *string
		idMarker  *string
	)

	for {
		in := &s3.ListMultipartUploadsInput{
			Bucket:         aws.String(s.bucket),
			KeyMarker:      keyMarker,
			UploadIdMarker: idMarker,
		}
		if prefix != "" {
			in.Prefix = aws.String(prefix)
		}

		out, err := s.client.ListMultipartUploads(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("list multipart uploads: %w", err)
		}

		for _, u := range out.Uploads {
			uploads = append(uploads, InFlightUpload{
				Key:       aws.ToString(u.Key),
				UploadID:  aws.ToString(u.UploadId),
				Initiated: aws.ToTime(u.Initiated),
			})
		}

		if !aws.ToBool(out.IsTruncated) {
			return uploads, nil
		}
		// Both markers advance together — paging here is keyed on the pair,
		// since one key can carry several concurrent uploads.
		keyMarker = out.NextKeyMarker
		idMarker = out.NextUploadIdMarker
	}
}

// IsNoSuchUpload reports whether err is S3 saying the multipart upload does not
// exist: aborted, reaped by a lifecycle rule, or already completed.
//
// The generic smithy.APIError is checked as well as the typed error because the
// typed shape depends on the error body deserializing into it, which is not
// guaranteed for every response S3 (or an S3-compatible endpoint) can produce.
// A resume that cannot tell "gone" from "broken" retries forever against an
// upload that will never come back.
func IsNoSuchUpload(err error) bool {
	var typed *types.NoSuchUpload
	if errors.As(err, &typed) {
		return true
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "NoSuchUpload"
	}

	return false
}

// IsInvalidPart reports whether S3 rejected a completion because the part list
// does not match what it is holding — a wrong ETag, a part number it has never
// seen, or parts out of order.
//
// Worth distinguishing from a generic failure because it is not retryable: the
// same request will fail identically forever, and the only recovery is a fresh
// upload.
//
// A client that assembles the part list from its own notes is what reaches this:
// a stale ETag, a part number it never actually uploaded, or parts out of order.
// It is the reason POST /jobs/{id}/complete derives the list from ListParts when
// the client does not supply one.
func IsInvalidPart(err error) bool {
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "InvalidPart", "InvalidPartOrder":
			return true
		}
	}
	return false
}

// AbortMultipartUpload cancels an in-progress multipart upload.
func (s *S3Client) AbortMultipartUpload(ctx context.Context, key, uploadID string) error {
	_, err := s.client.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(s.bucket),
		Key:      aws.String(key),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		return fmt.Errorf("abort multipart upload: %w", err)
	}
	return nil
}

// presignOptions returns the presign options, overriding the signed host when a
// separate public endpoint is configured.
//
// The override is applied to the presign call only. The API's own S3 traffic
// keeps using whatever the SDK resolves, so an override that is wrong breaks
// client uploads without also breaking the server. On real AWS with
// S3_PUBLIC_ENDPOINT unset this returns nothing but the expiry, and the SDK
// signs the genuine regional endpoint.
func (s *S3Client) presignOptions(expiry time.Duration) []func(*s3.PresignOptions) {
	opts := []func(*s3.PresignOptions){s3.WithPresignExpires(expiry)}

	if s.presignEndpoint != "" {
		opts = append(opts, s3.WithPresignClientFromClientOptions(func(o *s3.Options) {
			o.BaseEndpoint = aws.String(s.presignEndpoint)
			// Path style keeps the bucket in the path rather than in the
			// hostname. An overridden endpoint is a specific host that was
			// configured because it is reachable; prefixing the bucket onto it
			// invents a hostname that nothing resolves.
			o.UsePathStyle = true
		}))
	}

	return opts
}
