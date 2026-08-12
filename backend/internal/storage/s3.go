// Package storage provides S3 operations for multipart uploads and presigned URLs.
package storage

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/anshumanagarwal/dayreel/internal/config"
)

// CompletedPart represents a completed upload part with its ETag.
type CompletedPart struct {
	PartNumber int    `json:"part_number"`
	ETag       string `json:"etag"`
}

// S3Client wraps the AWS S3 client for multipart upload operations.
type S3Client struct {
	client        *s3.Client
	presignClient *s3.PresignClient
	bucket        string
}

// NewS3Client creates a new S3Client configured for the given bucket.
func NewS3Client(ctx context.Context, cfg *config.Config) (*S3Client, error) {
	var opts []func(*awsconfig.LoadOptions) error

	opts = append(opts, awsconfig.WithRegion(cfg.AWSRegion))

	if cfg.UseLocalStack && cfg.AWSEndpoint != "" {
		opts = append(opts,
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
		)
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.UseLocalStack && cfg.AWSEndpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.AWSEndpoint)
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)
	presignClient := s3.NewPresignClient(client)

	return &S3Client{
		client:        client,
		presignClient: presignClient,
		bucket:        cfg.S3RawBucket,
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
	}, s3.WithPresignExpires(expiry))
	if err != nil {
		return "", fmt.Errorf("presign upload part %d: %w", partNumber, err)
	}
	return result.URL, nil
}

// CompleteMultipartUpload finalizes a multipart upload with the given ETags.
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
		return fmt.Errorf("complete multipart upload: %w", err)
	}
	return nil
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
