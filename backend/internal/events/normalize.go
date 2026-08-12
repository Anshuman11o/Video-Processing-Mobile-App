package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/anshumanagarwal/dayreel/internal/models"
)

// ErrNotVideoKey is returned for S3 events whose key does not look like job
// input. It is a permanent condition — retrying an object that will never have
// a job ID in its key changes nothing.
var ErrNotVideoKey = errors.New("s3 key is not a job input key")

// s3EventNotification is the envelope S3 delivers to SQS. Only the fields
// needed to locate the object are modelled.
type s3EventNotification struct {
	Records []struct {
		EventName string `json:"eventName"`
		S3        struct {
			Bucket struct {
				Name string `json:"name"`
			} `json:"bucket"`
			Object struct {
				Key  string `json:"key"`
				Size int64  `json:"size"`
			} `json:"object"`
		} `json:"s3"`
	} `json:"Records"`
}

// NormalizeMessage converts a raw SQS message body into a StageMessage.
//
// The validate queue receives two different shapes. S3 delivers its own event
// notification envelope directly (configured in infra/localstack/init-aws.sh),
// while every downstream stage receives a StageMessage published by the stage
// before it. Normalizing here means stages 4A-6A never learn the S3 shape
// exists.
//
// attempt comes from the SQS ApproximateReceiveCount rather than the body,
// because the S3 envelope carries no attempt field.
func NormalizeMessage(body []byte, defaultStage models.StageName, attempt int) (*StageMessage, error) {
	// A StageMessage always names its stage; an S3 envelope never does. That is
	// the cheapest unambiguous discriminator between the two shapes.
	var probe struct {
		Stage string `json:"stage"`
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(body, &probe); err == nil && probe.Stage != "" && probe.JobID != "" {
		var msg StageMessage
		if err := json.Unmarshal(body, &msg); err != nil {
			return nil, fmt.Errorf("unmarshal stage message: %w", err)
		}
		if attempt > 0 {
			msg.Attempt = attempt
		}
		return &msg, nil
	}

	var evt s3EventNotification
	if err := json.Unmarshal(body, &evt); err != nil {
		return nil, fmt.Errorf("unmarshal s3 event: %w", err)
	}
	if len(evt.Records) == 0 {
		return nil, errors.New("message is neither a stage message nor an s3 event")
	}

	rec := evt.Records[0]

	// S3 percent-encodes keys in event notifications and encodes spaces as "+".
	key, err := decodeS3Key(rec.S3.Object.Key)
	if err != nil {
		return nil, fmt.Errorf("decode s3 key %q: %w", rec.S3.Object.Key, err)
	}

	jobID, err := jobIDFromKey(key)
	if err != nil {
		return nil, err
	}

	return NewStageMessage(
		jobID,
		defaultStage,
		S3Ref{Bucket: rec.S3.Bucket.Name, Key: key},
		attempt,
		uuid.New().String(), // no trace to inherit on the S3-triggered path
	), nil
}

// decodeS3Key reverses the encoding S3 applies to object keys in event
// notifications.
func decodeS3Key(raw string) (string, error) {
	return url.QueryUnescape(strings.ReplaceAll(raw, "+", "%20"))
}

// jobIDFromKey extracts the job ID from a key of the form "<job_id>/<filename>",
// which is the layout the API writes (internal/api/handlers.go).
//
// This is the seam that breaks first if the upload key layout ever changes, so
// it validates that the prefix is actually a UUID rather than taking whatever
// precedes the first slash. Without that check, a stray object written to the
// bucket would be attributed to a plausible-looking job ID and update nothing.
func jobIDFromKey(key string) (string, error) {
	prefix, _, found := strings.Cut(key, "/")
	if !found || prefix == "" {
		return "", fmt.Errorf("%w: %q has no job id prefix", ErrNotVideoKey, key)
	}
	if _, err := uuid.Parse(prefix); err != nil {
		return "", fmt.Errorf("%w: prefix %q is not a uuid", ErrNotVideoKey, prefix)
	}
	return prefix, nil
}
