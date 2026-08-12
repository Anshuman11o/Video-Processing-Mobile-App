package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Whole-object operations for the worker pipeline.
//
// These take an explicit bucket, unlike the multipart methods above, which
// operate on the client's configured raw-videos bucket. Workers read from
// dayreel-raw-videos and write to dayreel-processed, so a single bound bucket
// does not work for them.

// DownloadToFile streams an object to a local path.
//
// The worker downloads rather than streams because ffmpeg and ffprobe need a
// real seekable file — probing a container requires reading the moov atom,
// which may live at either end of the file.
func (s *S3Client) DownloadToFile(ctx context.Context, bucket, key, destPath string) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("get object %s/%s: %w", bucket, key, err)
	}
	defer out.Body.Close()

	f, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", destPath, err)
	}
	defer f.Close()

	if _, err := io.Copy(f, out.Body); err != nil {
		return fmt.Errorf("write %s: %w", destPath, err)
	}
	return nil
}

// UploadFile uploads a local file to the given bucket and key.
func (s *S3Client) UploadFile(ctx context.Context, bucket, key, srcPath, contentType string) error {
	f, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", srcPath, err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", srcPath, err)
	}

	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(bucket),
		Key:           aws.String(key),
		Body:          f,
		ContentType:   aws.String(contentType),
		ContentLength: aws.Int64(stat.Size()),
	})
	if err != nil {
		return fmt.Errorf("put object %s/%s: %w", bucket, key, err)
	}
	return nil
}

// ObjectExists reports whether an object is present.
//
// A missing object is (false, nil), not an error — the distinction matters
// because this backs the workers' idempotency guard. Collapsing "absent" into
// the error path makes the guard fail closed (every message looks like a
// transient failure and retries forever); collapsing a real error into
// "absent" makes it fail open (work is redone, outputs overwritten).
func (s *S3Client) ObjectExists(ctx context.Context, bucket, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true, nil
	}

	// HeadObject has an empty response body, so the SDK cannot always model the
	// error precisely. Check the typed shape first, then fall back to the
	// wire-level code.
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return false, nil
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return false, nil
		}
	}

	return false, fmt.Errorf("head object %s/%s: %w", bucket, key, err)
}
