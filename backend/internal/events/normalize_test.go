package events

import (
	"errors"
	"testing"

	"github.com/anshumanagarwal/dayreel/internal/models"
)

const testJobID = "550e8400-e29b-41d4-a716-446655440000"

func s3Event(key string) string {
	return `{"Records":[{"eventName":"ObjectCreated:CompleteMultipartUpload",
		"s3":{"bucket":{"name":"dayreel-raw-videos"},
		"object":{"key":"` + key + `","size":1024}}}]}`
}

func TestNormalizeMessage_S3Event(t *testing.T) {
	msg, err := NormalizeMessage([]byte(s3Event(testJobID+"/clip.mp4")), models.StageValidate, 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.JobID != testJobID {
		t.Errorf("job id: got %q, want %q", msg.JobID, testJobID)
	}
	if msg.Stage != models.StageValidate {
		t.Errorf("stage: got %q, want %q", msg.Stage, models.StageValidate)
	}
	if msg.Input.Bucket != "dayreel-raw-videos" {
		t.Errorf("bucket: got %q", msg.Input.Bucket)
	}
	if msg.Input.Key != testJobID+"/clip.mp4" {
		t.Errorf("key: got %q", msg.Input.Key)
	}
	if msg.Attempt != 2 {
		t.Errorf("attempt: got %d, want 2 (from receive count)", msg.Attempt)
	}
	if msg.TraceID == "" {
		t.Error("trace id should be synthesized on the S3 path")
	}
}

// S3 percent-encodes keys in notifications and uses "+" for spaces. Failing to
// decode means downloading a key that does not exist.
func TestNormalizeMessage_S3EventEncodedKey(t *testing.T) {
	msg, err := NormalizeMessage([]byte(s3Event(testJobID+"/my+holiday%20clip.mp4")), models.StageValidate, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := testJobID + "/my holiday clip.mp4"; msg.Input.Key != want {
		t.Errorf("key: got %q, want %q", msg.Input.Key, want)
	}
}

// A StageMessage from an upstream stage must pass through unchanged, keeping
// its own trace ID rather than getting a fresh one.
func TestNormalizeMessage_StageMessagePassthrough(t *testing.T) {
	body := `{"job_id":"` + testJobID + `","stage":"extract",
		"input":{"bucket":"dayreel-processed","key":"` + testJobID + `/validated.mp4"},
		"attempt":1,"timestamp":"2026-08-12T10:30:00Z","trace_id":"trace-abc"}`

	msg, err := NormalizeMessage([]byte(body), models.StageValidate, 3)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if msg.Stage != models.StageExtract {
		t.Errorf("stage: got %q, want the body's stage, not the default", msg.Stage)
	}
	if msg.TraceID != "trace-abc" {
		t.Errorf("trace id: got %q, want the inherited trace", msg.TraceID)
	}
	if msg.Attempt != 3 {
		t.Errorf("attempt: got %d, want 3 (receive count overrides body)", msg.Attempt)
	}
}

func TestNormalizeMessage_RejectsNonJobKeys(t *testing.T) {
	cases := map[string]string{
		"no prefix":     "loose-file.mp4",
		"not a uuid":    "some-folder/clip.mp4",
		"empty prefix":  "/clip.mp4",
		"uuid-ish only": "550e8400-not-a-uuid/clip.mp4",
	}

	for name, key := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NormalizeMessage([]byte(s3Event(key)), models.StageValidate, 1)
			if !errors.Is(err, ErrNotVideoKey) {
				t.Errorf("got %v, want ErrNotVideoKey", err)
			}
		})
	}
}

func TestNormalizeMessage_Garbage(t *testing.T) {
	for _, body := range []string{`not json`, `{}`, `{"Records":[]}`} {
		if _, err := NormalizeMessage([]byte(body), models.StageValidate, 1); err == nil {
			t.Errorf("body %q: expected an error", body)
		}
	}
}
