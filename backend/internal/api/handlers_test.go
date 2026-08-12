package api

import (
	"reflect"
	"testing"
	"time"

	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/storage"
)

// part is a shorthand for the listing S3 returns. Size is carried but never
// consulted — see missingParts.
func part(n int, etag string) storage.UploadedPart {
	return storage.UploadedPart{PartNumber: n, ETag: etag, Size: 5 * 1024 * 1024}
}

// missingParts decides which bytes a resumed upload sends again. Sending too
// few silently truncates the video; sending too many is wasted transfer on a
// metered connection.
func TestMissingParts(t *testing.T) {
	tests := []struct {
		name       string
		totalParts int
		uploaded   []storage.UploadedPart
		want       []int
	}{
		{
			name:       "a fresh upload is entirely missing",
			totalParts: 3,
			uploaded:   nil,
			want:       []int{1, 2, 3},
		},
		{
			name:       "a contiguous prefix leaves the tail",
			totalParts: 5,
			uploaded:   []storage.UploadedPart{part(1, `"a"`), part(2, `"b"`)},
			want:       []int{3, 4, 5},
		},
		{
			// The case a client-kept ETag array gets wrong. Parts can land in
			// any order — retries, parallel PUTs — and the gap is not a suffix.
			name:       "out-of-order parts reconcile as a set, not a prefix",
			totalParts: 9,
			uploaded:   []storage.UploadedPart{part(5, `"e"`), part(2, `"b"`), part(9, `"i"`)},
			want:       []int{1, 3, 4, 6, 7, 8},
		},
		{
			name:       "a finished upload is missing nothing",
			totalParts: 3,
			uploaded:   []storage.UploadedPart{part(3, `"c"`), part(1, `"a"`), part(2, `"b"`)},
			want:       []int{},
		},
		{
			name:       "a single-part upload",
			totalParts: 1,
			uploaded:   nil,
			want:       []int{1},
		},
		{
			// S3 listing a part the job record does not know about means one of
			// the two is wrong; re-presigning an invented part number would
			// only fail later and further away.
			name:       "part numbers beyond total_parts are ignored",
			totalParts: 2,
			uploaded:   []storage.UploadedPart{part(1, `"a"`), part(2, `"b"`), part(7, `"g"`)},
			want:       []int{},
		},
		{
			name:       "a zero-size part still counts as present",
			totalParts: 2,
			uploaded:   []storage.UploadedPart{{PartNumber: 1, ETag: `"a"`, Size: 0}},
			want:       []int{2},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := missingParts(tc.totalParts, tc.uploaded)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("missingParts(%d, %v) = %v, want %v",
					tc.totalParts, tc.uploaded, got, tc.want)
			}
		})
	}
}

// missingParts must never return nil, because the response marshals it into an
// array the client iterates.
func TestMissingPartsIsNeverNil(t *testing.T) {
	got := missingParts(0, nil)
	if got == nil {
		t.Fatal("missingParts returned nil; the client would see JSON null")
	}
	if len(got) != 0 {
		t.Errorf("missingParts(0, nil) = %v, want empty", got)
	}
}

// CompleteMultipartUpload requires parts in ascending part-number order. S3's
// listing is already sorted, but the ordering is a requirement of the API and
// not an accident of the listing, so it is asserted here rather than assumed.
func TestCompletionPartsSortsAscending(t *testing.T) {
	uploaded := []storage.UploadedPart{
		part(3, `"c"`), part(1, `"a"`), part(10, `"j"`), part(2, `"b"`),
	}

	got := completionParts(uploaded)

	want := []storage.CompletedPart{
		{PartNumber: 1, ETag: `"a"`},
		{PartNumber: 2, ETag: `"b"`},
		{PartNumber: 3, ETag: `"c"`},
		{PartNumber: 10, ETag: `"j"`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("completionParts = %v, want %v", got, want)
	}
}

// The ETag must be carried through untouched — quotes included. S3 returns
// them quoted and rejects the completion if they are stripped.
func TestCompletionPartsPreservesQuotedETags(t *testing.T) {
	got := completionParts([]storage.UploadedPart{part(1, `"9bb58f26192e4ba00f01e2e7b136bbd8"`)})
	if len(got) != 1 {
		t.Fatalf("got %d parts, want 1", len(got))
	}
	if got[0].ETag != `"9bb58f26192e4ba00f01e2e7b136bbd8"` {
		t.Errorf("ETag = %s, want the quoted value S3 reported", got[0].ETag)
	}
}

func TestCompletionPartsEmpty(t *testing.T) {
	if got := completionParts(nil); len(got) != 0 {
		t.Errorf("completionParts(nil) = %v, want empty", got)
	}
}

// uploadIsComplete separates "already done, go poll" from "gone, start over".
// Both look identical to S3 — ListParts returns NoSuchUpload either way — so a
// wrong answer here makes a successful client re-upload the whole video.
func TestUploadIsComplete(t *testing.T) {
	now := time.Now().UTC()

	tests := []struct {
		name string
		job  *models.Job
		want bool
	}{
		{
			name: "uploading, nothing recorded",
			job: &models.Job{
				Status: models.JobStatusUploading,
				Upload: &models.UploadInfo{UploadID: "u"},
			},
			want: false,
		},
		{
			name: "completed_at recorded",
			job: &models.Job{
				Status: models.JobStatusUploading,
				Upload: &models.UploadInfo{UploadID: "u", CompletedAt: &now},
			},
			want: true,
		},
		{
			// The pipeline has the job. completed_at may predate
			// db.MarkUploadComplete, so status has to be enough on its own.
			name: "processing without completed_at",
			job: &models.Job{
				Status: models.JobStatusProcessing,
				Upload: &models.UploadInfo{UploadID: "u"},
			},
			want: true,
		},
		{
			name: "completed",
			job: &models.Job{
				Status: models.JobStatusCompleted,
				Upload: &models.UploadInfo{UploadID: "u"},
			},
			want: true,
		},
		{
			// An aborted upload lands here. It must NOT read as complete, or
			// the client is told 409 ALREADY_COMPLETE and polls forever for a
			// job whose bytes were thrown away; it should fall through to S3
			// and get 410 UPLOAD_GONE.
			name: "failed — aborted, not complete",
			job: &models.Job{
				Status: models.JobStatusFailed,
				Upload: &models.UploadInfo{UploadID: "u"},
			},
			want: false,
		},
		{
			name: "pending",
			job: &models.Job{
				Status: models.JobStatusPending,
				Upload: &models.UploadInfo{UploadID: "u"},
			},
			want: false,
		},
		{
			name: "no upload at all",
			job:  &models.Job{Status: models.JobStatusUploading},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := uploadIsComplete(tc.job); got != tc.want {
				t.Errorf("uploadIsComplete = %v, want %v", got, tc.want)
			}
		})
	}
}
