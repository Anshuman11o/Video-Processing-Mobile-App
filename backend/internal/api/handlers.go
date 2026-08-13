// Package api provides HTTP handlers for the DayReel API.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/anshumanagarwal/dayreel/internal/cache"
	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/events"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/queue"
	"github.com/anshumanagarwal/dayreel/internal/storage"
)

// presignExpiry bounds how long an upload URL stays usable.
//
// An hour is short for an upload that has to survive the app being killed, and
// deliberately so: POST /jobs/{id}/upload-urls re-issues URLs for whatever is
// still missing, so expiry is now a thing a client recovers from rather than
// the end of the upload.
const presignExpiry = 1 * time.Hour

// partSize is the multipart part size for this request.
//
// Read from config rather than fixed, so a small clip can still be uploaded as
// several parts locally — see config.UploadPartSize for why that matters.
func (h *Handler) partSize() int64 {
	if h.config.UploadPartSize > 0 {
		return h.config.UploadPartSize
	}
	return config.DefaultUploadPartSize
}

// Handler holds dependencies for all HTTP handlers.
type Handler struct {
	s3     *storage.S3Client
	db     *db.DynamoDBClient
	cache  *cache.Cache
	queue  queue.Queue
	config *config.Config
}

// NewHandler creates a new Handler with the given dependencies.
func NewHandler(
	s3 *storage.S3Client, db *db.DynamoDBClient,
	jobCache *cache.Cache, q queue.Queue, cfg *config.Config,
) *Handler {
	return &Handler{
		s3:     s3,
		db:     db,
		cache:  jobCache,
		queue:  q,
		config: cfg,
	}
}

// --- Request/Response types ---

type createJobRequest struct {
	Filename    string `json:"filename"`
	SizeBytes   int64  `json:"size_bytes"`
	ContentType string `json:"content_type"`
}

type uploadPartInfo struct {
	PartNumber int    `json:"part_number"`
	URL        string `json:"url"`
}

type createJobResponse struct {
	JobID      string           `json:"job_id"`
	UploadID   string           `json:"upload_id"`
	UploadURLs []uploadPartInfo `json:"upload_urls"`
	PartSize   int64            `json:"part_size"`
	ExpiresIn  int              `json:"expires_in"`
}

type completeUploadRequest struct {
	UploadID string                  `json:"upload_id"`
	Parts    []storage.CompletedPart `json:"parts"`
}

type completeUploadResponse struct {
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// resumeUploadResponse is everything a client needs to finish an upload it can
// no longer reason about on its own.
//
// uploaded_parts comes straight from S3 rather than from anything the client
// told us, and upload_urls contains ONLY the parts still missing. Both are
// deliberate: the client persists identifiers, never progress.
type resumeUploadResponse struct {
	JobID         string                 `json:"job_id"`
	UploadID      string                 `json:"upload_id"`
	PartSize      int64                  `json:"part_size"`
	TotalParts    int                    `json:"total_parts"`
	UploadedParts []storage.UploadedPart `json:"uploaded_parts"`
	UploadURLs    []uploadPartInfo       `json:"upload_urls"`
	ExpiresIn     int                    `json:"expires_in"`
}

type abortUploadResponse struct {
	JobID   string `json:"job_id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type reelResponse struct {
	JobID        string `json:"job_id"`
	HLSURL       string `json:"hls_url"`
	ThumbnailURL string `json:"thumbnail_url"`
}

type errorResponse struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}

// --- Handlers ---

// CreateJob handles POST /jobs — creates a new job and returns presigned upload URLs.
func (h *Handler) CreateJob(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "BAD_REQUEST")
		return
	}

	if req.Filename == "" || req.SizeBytes <= 0 || req.ContentType == "" {
		writeError(w, http.StatusBadRequest, "filename, size_bytes, and content_type are required", "VALIDATION_ERROR")
		return
	}

	// Create the job
	job := models.NewJob(req.Filename, req.SizeBytes, req.ContentType)
	s3Key := job.JobID + "/" + req.Filename

	// Start multipart upload in S3
	uploadID, err := h.s3.CreateMultipartUpload(r.Context(), s3Key, req.ContentType)
	if err != nil {
		log.Printf("ERROR: create multipart upload: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to initiate upload", "S3_ERROR")
		return
	}

	// Calculate parts
	partSize := h.partSize()
	numParts := int(math.Ceil(float64(req.SizeBytes) / float64(partSize)))
	if numParts == 0 {
		numParts = 1
	}

	// Generate presigned URLs for each part
	uploadURLs := make([]uploadPartInfo, 0, numParts)
	for i := 1; i <= numParts; i++ {
		url, err := h.s3.GeneratePresignedUploadURL(r.Context(), s3Key, uploadID, i, presignExpiry)
		if err != nil {
			log.Printf("ERROR: presign part %d: %v", i, err)
			_ = h.s3.AbortMultipartUpload(r.Context(), s3Key, uploadID)
			writeError(w, http.StatusInternalServerError, "failed to generate upload URLs", "S3_ERROR")
			return
		}
		uploadURLs = append(uploadURLs, uploadPartInfo{PartNumber: i, URL: url})
	}

	// Set upload info on job
	job.Status = models.JobStatusUploading
	job.Upload = &models.UploadInfo{
		UploadID:   uploadID,
		Bucket:     h.config.S3RawBucket,
		Key:        s3Key,
		PartSize:   partSize,
		TotalParts: numParts,
	}

	// Save to DynamoDB
	if err := h.db.CreateJob(r.Context(), job); err != nil {
		log.Printf("ERROR: create job in db: %v", err)
		_ = h.s3.AbortMultipartUpload(r.Context(), s3Key, uploadID)
		writeError(w, http.StatusInternalServerError, "failed to create job", "DB_ERROR")
		return
	}

	writeJSON(w, http.StatusCreated, createJobResponse{
		JobID:      job.JobID,
		UploadID:   uploadID,
		UploadURLs: uploadURLs,
		PartSize:   partSize,
		ExpiresIn:  int(presignExpiry.Seconds()),
	})
}

// ResumeUpload handles POST /jobs/{id}/upload-urls — re-issues presigned URLs
// for the parts S3 does not yet hold.
//
// This is the endpoint the stage exists for. Presigned URLs expire after
// presignExpiry and, until this route existed, nothing could mint new ones for
// an upload already in progress — so every form of resume died an hour after
// the app closed, regardless of what the client had persisted.
//
// It changes no state, so it is safe to call repeatedly: the client is expected
// to call it on every relaunch, before it knows whether anything is missing.
func (h *Handler) ResumeUpload(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["id"]

	job, ok := h.loadResumableJob(w, r, jobID)
	if !ok {
		return
	}
	upload := job.Upload

	uploaded, err := h.s3.ListParts(r.Context(), upload.Key, upload.UploadID)
	if err != nil {
		if storage.IsNoSuchUpload(err) {
			writeError(w, http.StatusGone,
				"the upload no longer exists; create a new job", "UPLOAD_GONE")
			return
		}
		log.Printf("ERROR: list parts for job %s: %v", jobID, err)
		writeError(w, http.StatusInternalServerError, "failed to inspect upload", "S3_ERROR")
		return
	}

	missing := missingParts(upload.TotalParts, uploaded)

	uploadURLs := make([]uploadPartInfo, 0, len(missing))
	for _, partNumber := range missing {
		// Reused rather than reimplemented on purpose: this method is what
		// routes presigning through S3_PUBLIC_ENDPOINT. A fresh presign call
		// here would sign the in-cluster host, look entirely correct, and hand
		// the device URLs it cannot reach.
		url, err := h.s3.GeneratePresignedUploadURL(
			r.Context(), upload.Key, upload.UploadID, partNumber, presignExpiry)
		if err != nil {
			// Deliberately no abort on this path, unlike CreateJob's. That
			// upload had nothing in it; this one is holding the client's bytes,
			// and throwing them away because one presign failed would turn a
			// retryable error into a full re-upload.
			log.Printf("ERROR: re-presign part %d for job %s: %v", partNumber, jobID, err)
			writeError(w, http.StatusInternalServerError, "failed to generate upload URLs", "S3_ERROR")
			return
		}
		uploadURLs = append(uploadURLs, uploadPartInfo{PartNumber: partNumber, URL: url})
	}

	// Never null. The client is plain HTTP from Kotlin, and a null where an
	// array was promised is a crash rather than an empty loop.
	if uploaded == nil {
		uploaded = []storage.UploadedPart{}
	}

	writeJSON(w, http.StatusOK, resumeUploadResponse{
		JobID:         jobID,
		UploadID:      upload.UploadID,
		PartSize:      upload.PartSize,
		TotalParts:    upload.TotalParts,
		UploadedParts: uploaded,
		UploadURLs:    uploadURLs,
		ExpiresIn:     int(presignExpiry.Seconds()),
	})
}

// CompleteUpload handles POST /jobs/{id}/complete — completes the multipart
// upload and hands the job to the pipeline.
//
// The parts array is optional. When it is absent or empty the list is derived
// from ListParts, which gets ascending order and matching ETags for free and
// removes the last reason for a client to persist an ETag at all. A supplied
// array still works: stage 7's uploader sends one.
//
// This is now the only thing that starts processing. An S3 ObjectCreated
// notification used to trigger the validate queue, but real S3 cannot notify a
// SQLite file, so a job that is not enqueued here is a video that silently never
// gets processed — which is why a failed enqueue is a 500 the client retries and
// not a logged warning.
func (h *Handler) CompleteUpload(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["id"]

	// An empty body is a valid request now, so EOF is not a client error.
	var req completeUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body", "BAD_REQUEST")
		return
	}

	job, ok := h.loadJobWithUpload(w, r, jobID)
	if !ok {
		return
	}
	upload := job.Upload

	// A retry whose only failure was the enqueue must reach the enqueue again,
	// so "already complete" is a resumption point here rather than the 409 the
	// other two upload routes answer with.
	//
	// Every S3 step is skipped on this path, not merely tolerated: ListParts and
	// CompleteMultipartUpload both report NoSuchUpload for an upload that
	// finished, and a second trip through them could only turn a retry that must
	// succeed into a 410 telling the client to upload the whole video again.
	if uploadIsComplete(job) {
		h.startPipeline(w, r, job)
		return
	}

	// The client may name the upload it believes it is completing, but it does
	// not get to choose one. This was previously passed straight through to S3
	// without ever being compared to the job's own upload ID.
	if req.UploadID != "" && req.UploadID != upload.UploadID {
		writeError(w, http.StatusConflict,
			"upload_id does not match this job's upload", "UPLOAD_MISMATCH")
		return
	}

	parts := req.Parts
	if len(parts) == 0 {
		uploaded, err := h.s3.ListParts(r.Context(), upload.Key, upload.UploadID)
		if err != nil {
			if storage.IsNoSuchUpload(err) {
				// The upload is gone, and it is gone for one of two opposite
				// reasons: it was aborted or reaped, or it completed. The job
				// record normally settles that — see uploadIsComplete — but it is
				// written after the S3 call and that write can fail, which is
				// exactly the state a retry lands in. Ask the bucket, because it
				// is the only answer that cannot be stale.
				if h.uploadAlreadyAssembled(r, job) {
					log.Printf("job %s: upload already assembled, resuming at the enqueue", jobID)
					h.startPipeline(w, r, job)
					return
				}
				writeError(w, http.StatusGone,
					"the upload no longer exists; create a new job", "UPLOAD_GONE")
				return
			}
			log.Printf("ERROR: list parts for job %s: %v", jobID, err)
			writeError(w, http.StatusInternalServerError, "failed to inspect upload", "S3_ERROR")
			return
		}
		parts = completionParts(uploaded)
	}

	// S3 will happily assemble an object from a subset of its parts, so a
	// client that completes early gets a 200 and a truncated video — which then
	// fails several stages downstream as an obscure ffprobe error. Counting is
	// cheap and turns that into an answer at the point of the mistake.
	if len(parts) != upload.TotalParts {
		log.Printf("WARN: job %s completing with %d of %d parts", jobID, len(parts), upload.TotalParts)
		writeError(w, http.StatusConflict,
			"upload is not finished; some parts are still missing", "INCOMPLETE_UPLOAD")
		return
	}

	if err := h.s3.CompleteMultipartUpload(r.Context(), upload.Key, upload.UploadID, parts); err != nil {
		if storage.IsNoSuchUpload(err) {
			writeError(w, http.StatusGone,
				"the upload no longer exists; create a new job", "UPLOAD_GONE")
			return
		}
		if storage.IsInvalidPart(err) {
			// Not retryable — the same request fails identically forever — so
			// it must not look like the 500 a client backs off and retries.
			log.Printf("ERROR: job %s completing with a part list S3 rejects: %v", jobID, err)
			writeError(w, http.StatusConflict,
				"S3 rejected the part list; the upload must be restarted", "INVALID_PARTS")
			return
		}
		log.Printf("ERROR: complete multipart upload: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to complete upload", "S3_ERROR")
		return
	}

	// Persisted, not just set in memory. Without this the next read cannot tell
	// a finished upload from an abandoned one — see db.MarkUploadComplete.
	//
	// Written before the enqueue on purpose: it is what makes the retry above
	// find its way back here. If the enqueue fails and this write had not
	// happened, the retry would go looking for a multipart upload that S3 has
	// already assembled and consumed.
	if err := h.db.MarkUploadComplete(r.Context(), jobID, time.Now().UTC()); err != nil {
		log.Printf("ERROR: mark upload complete: %v", err)
	}

	// Invalidate cache
	_ = h.cache.InvalidateJob(r.Context(), jobID)

	h.startPipeline(w, r, job)
}

// uploadAlreadyAssembled reports whether the finished object is in the bucket.
//
// Deliberately fails closed: an error asking the question is not evidence that
// the object is there, and answering "yes" on a HeadObject that never succeeded
// would enqueue a job whose input does not exist.
func (h *Handler) uploadAlreadyAssembled(r *http.Request, job *models.Job) bool {
	exists, err := h.s3.ObjectExists(r.Context(), h.config.S3RawBucket, job.Upload.Key)
	if err != nil {
		log.Printf("WARN: head %s for job %s: %v", job.Upload.Key, job.JobID, err)
		return false
	}
	return exists
}

// startPipeline publishes the validate message and writes the response.
//
// Split out because two paths reach it: a fresh completion, and a retry of one
// whose S3 work already succeeded. Both must end with a message on the validate
// queue, because nothing else in the system will ever put one there.
func (h *Handler) startPipeline(w http.ResponseWriter, r *http.Request, job *models.Job) {
	msg := events.NewStageMessage(
		job.JobID,
		models.StageValidate,
		events.S3Ref{Bucket: h.config.S3RawBucket, Key: job.Upload.Key},
		1,
		traceID(r),
	)

	if err := h.queue.Send(r.Context(), events.QueueValidate, msg, 0); err != nil {
		log.Printf("ERROR: enqueue validate for job %s: %v", job.JobID, err)
		// A 500 rather than a 200 with a warning: the bytes are in S3 and
		// nothing but another call to this endpoint will start the pipeline, so
		// the client has to be told to come back.
		writeError(w, http.StatusInternalServerError,
			"failed to queue job for processing", "QUEUE_ERROR")
		return
	}

	writeJSON(w, http.StatusOK, completeUploadResponse{
		JobID:   job.JobID,
		Status:  string(models.JobStatusProcessing),
		Message: "Upload complete, processing started",
	})
}

// AbortUpload handles DELETE /jobs/{id}/upload — aborts the multipart upload
// and releases the parts S3 is holding.
//
// Parts of an incomplete multipart upload bill as storage and appear in no
// object listing, so an upload nobody aborts is both invisible and permanent.
// Until this route existed the only aborts in the system were two server-side
// error paths in CreateJob; nothing could cancel an upload a client walked away
// from.
func (h *Handler) AbortUpload(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["id"]

	job, ok := h.loadResumableJob(w, r, jobID)
	if !ok {
		return
	}

	err := h.s3.AbortMultipartUpload(r.Context(), job.Upload.Key, job.Upload.UploadID)
	// NoSuchUpload is success: the upload is gone, which is what was asked for.
	// The caller here is often a client that has just crashed and does not know
	// what it already did, so a second abort must not be an error.
	if err != nil && !storage.IsNoSuchUpload(err) {
		log.Printf("ERROR: abort multipart upload for job %s: %v", jobID, err)
		writeError(w, http.StatusInternalServerError, "failed to abort upload", "S3_ERROR")
		return
	}

	// failed, because there is no cancelled status and inventing one would
	// change a contract the client already reads. What matters is that it is
	// terminal: the job stops being polled, and a later resume attempt gets 410
	// from S3 rather than looping against an upload that no longer exists.
	if dbErr := h.db.UpdateJobStatus(r.Context(), jobID, models.JobStatusFailed); dbErr != nil {
		log.Printf("ERROR: update job status after abort: %v", dbErr)
	}
	_ = h.cache.InvalidateJob(r.Context(), jobID)

	writeJSON(w, http.StatusOK, abortUploadResponse{
		JobID:   jobID,
		Status:  string(models.JobStatusFailed),
		Message: "Upload aborted, parts released",
	})
}

// GetJobStatus handles GET /jobs/{id} — returns job status with all stages.
func (h *Handler) GetJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["id"]

	// Check the in-process cache first. It exists for exactly this handler: the
	// app polls it every second or two for the whole length of a job, and a 10s
	// TTL turns that into one DynamoDB read per job per ten seconds.
	job, err := h.cache.GetJob(r.Context(), jobID)
	if err != nil {
		log.Printf("WARN: cache get: %v", err)
	}

	if job == nil {
		// Cache miss — fetch from DynamoDB
		job, err = h.db.GetJob(r.Context(), jobID)
		if err != nil {
			log.Printf("ERROR: get job: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to retrieve job", "DB_ERROR")
			return
		}
		if job == nil {
			writeError(w, http.StatusNotFound, "job not found", "NOT_FOUND")
			return
		}

		// Cache the result
		if cacheErr := h.cache.SetJob(r.Context(), job); cacheErr != nil {
			log.Printf("WARN: cache set: %v", cacheErr)
		}
	}

	writeJSON(w, http.StatusOK, job)
}

// GetReel handles GET /jobs/{id}/reel — returns HLS playback URL for completed jobs.
func (h *Handler) GetReel(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["id"]

	job, err := h.db.GetJob(r.Context(), jobID)
	if err != nil {
		log.Printf("ERROR: get job: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to retrieve job", "DB_ERROR")
		return
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "job not found", "NOT_FOUND")
		return
	}

	if job.Status != models.JobStatusCompleted || job.Output == nil {
		writeError(w, http.StatusConflict, "job not yet complete", "NOT_READY")
		return
	}

	writeJSON(w, http.StatusOK, reelResponse{
		JobID:        jobID,
		HLSURL:       job.Output.HLSURL,
		ThumbnailURL: job.Output.ThumbnailURL,
	})
}

// --- Upload reconciliation ---

// missingParts returns the part numbers in 1..totalParts that S3 does not hold.
//
// Pure, and separate from the presigning, so the arithmetic that decides what a
// resume re-uploads can be tested without S3 at all.
//
// A listed part is treated as complete without looking at its size, because
// UploadPart is all-or-nothing: the request carries a Content-Length and a
// short body is rejected outright. There is no half-part state to detect.
//
// Part numbers outside the range are ignored rather than trusted. totalParts
// comes from the job record and the listing comes from S3; if they disagree,
// re-presigning a part number S3 invented would fail anyway.
func missingParts(totalParts int, uploaded []storage.UploadedPart) []int {
	present := make(map[int]bool, len(uploaded))
	for _, p := range uploaded {
		present[p.PartNumber] = true
	}

	missing := make([]int, 0, totalParts)
	for partNumber := 1; partNumber <= totalParts; partNumber++ {
		if !present[partNumber] {
			missing = append(missing, partNumber)
		}
	}
	return missing
}

// completionParts turns S3's part listing into the list
// CompleteMultipartUpload requires: ascending by part number, with the ETags S3
// itself reported.
//
// Both properties come free from deriving server-side, which is the point —
// they are the two things a client assembling this array from its own notes
// gets wrong.
func completionParts(uploaded []storage.UploadedPart) []storage.CompletedPart {
	parts := make([]storage.CompletedPart, len(uploaded))
	for i, p := range uploaded {
		parts[i] = storage.CompletedPart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].PartNumber < parts[j].PartNumber })
	return parts
}

// uploadIsComplete reports whether the multipart upload has already finished.
//
// Both signals are consulted because each can be absent on its own:
// completed_at is written when the upload completes, and status moves on when
// the pipeline picks the job up. Getting this wrong is not cosmetic — a
// completed upload's ListParts fails with NoSuchUpload exactly as a reaped one
// does, so without this check a client that finished successfully would be told
// 410 UPLOAD_GONE and start the whole upload again.
func uploadIsComplete(job *models.Job) bool {
	if job.Upload != nil && job.Upload.CompletedAt != nil {
		return true
	}
	return job.Status == models.JobStatusProcessing || job.Status == models.JobStatusCompleted
}

// loadJobWithUpload fetches a job and rejects the states in which there is no
// upload to act on at all, writing the error response itself.
//
// Shared by all four upload routes so they cannot drift into disagreeing about
// what "no upload" means. The client's recovery differs per code, which is why
// they are distinct and not one 400.
//
// Reads DynamoDB directly rather than through the cache: this decides whether a
// client re-uploads a video, and a stale cached job is exactly the input that
// makes that decision wrong.
func (h *Handler) loadJobWithUpload(w http.ResponseWriter, r *http.Request, jobID string) (*models.Job, bool) {
	job, err := h.db.GetJob(r.Context(), jobID)
	if err != nil {
		log.Printf("ERROR: get job: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to retrieve job", "DB_ERROR")
		return nil, false
	}
	if job == nil {
		writeError(w, http.StatusNotFound, "job not found", "NOT_FOUND")
		return nil, false
	}
	if job.Upload == nil {
		writeError(w, http.StatusConflict, "this job has no upload", "NO_UPLOAD")
		return nil, false
	}
	return job, true
}

// loadResumableJob additionally rejects an upload that has already finished.
//
// Used by the routes that can only act on an upload still in flight: re-issuing
// URLs for a completed upload, or aborting it, are both meaningless. Completion
// itself deliberately does not use this — see CompleteUpload, where an
// already-complete upload is a retry to be finished rather than an error.
func (h *Handler) loadResumableJob(w http.ResponseWriter, r *http.Request, jobID string) (*models.Job, bool) {
	job, ok := h.loadJobWithUpload(w, r, jobID)
	if !ok {
		return nil, false
	}
	if uploadIsComplete(job) {
		writeError(w, http.StatusConflict, "the upload is already complete", "ALREADY_COMPLETE")
		return nil, false
	}
	return job, true
}

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, errorResponse{Error: message, Code: code})
}

// traceID returns the caller's trace header, or a fresh ID. It rides on the
// StageMessage through every stage, so a single video's whole journey — API,
// queue, worker logs — can be grepped with one value.
func traceID(r *http.Request) string {
	for _, header := range []string{"X-Trace-Id", "X-Request-Id"} {
		if v := r.Header.Get(header); v != "" {
			return v
		}
	}
	return uuid.NewString()
}
