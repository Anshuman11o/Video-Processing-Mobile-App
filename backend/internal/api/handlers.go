// Package api provides HTTP handlers for the DayReel API.
package api

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"time"

	"github.com/gorilla/mux"

	"github.com/anshumanagarwal/dayreel/internal/cache"
	"github.com/anshumanagarwal/dayreel/internal/config"
	"github.com/anshumanagarwal/dayreel/internal/db"
	"github.com/anshumanagarwal/dayreel/internal/models"
	"github.com/anshumanagarwal/dayreel/internal/storage"
)

const (
	partSize       = 5 * 1024 * 1024 // 5MB per part
	presignExpiry  = 1 * time.Hour
)

// Handler holds dependencies for all HTTP handlers.
type Handler struct {
	s3     *storage.S3Client
	db     *db.DynamoDBClient
	cache  *cache.RedisClient
	config *config.Config
}

// NewHandler creates a new Handler with the given dependencies.
func NewHandler(s3 *storage.S3Client, db *db.DynamoDBClient, cache *cache.RedisClient, cfg *config.Config) *Handler {
	return &Handler{
		s3:     s3,
		db:     db,
		cache:  cache,
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
		PartSize:   int64(partSize),
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
		PartSize:   int64(partSize),
		ExpiresIn:  int(presignExpiry.Seconds()),
	})
}

// CompleteUpload handles POST /jobs/{id}/complete — completes the multipart upload.
func (h *Handler) CompleteUpload(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["id"]

	var req completeUploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body", "BAD_REQUEST")
		return
	}

	// Get the job to find the S3 key
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
	if job.Upload == nil {
		writeError(w, http.StatusBadRequest, "no upload in progress for this job", "BAD_REQUEST")
		return
	}

	// Complete the S3 multipart upload
	if err := h.s3.CompleteMultipartUpload(r.Context(), job.Upload.Key, req.UploadID, req.Parts); err != nil {
		log.Printf("ERROR: complete multipart upload: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to complete upload", "S3_ERROR")
		return
	}

	// Update job status to processing
	now := time.Now().UTC()
	job.Upload.CompletedAt = &now
	if err := h.db.UpdateJobStatus(r.Context(), jobID, models.JobStatusProcessing); err != nil {
		log.Printf("ERROR: update job status: %v", err)
	}

	// Invalidate cache
	_ = h.cache.InvalidateJob(r.Context(), jobID)

	writeJSON(w, http.StatusOK, completeUploadResponse{
		JobID:   jobID,
		Status:  string(models.JobStatusProcessing),
		Message: "Upload complete, processing started",
	})
}

// GetJobStatus handles GET /jobs/{id} — returns job status with all stages.
func (h *Handler) GetJobStatus(w http.ResponseWriter, r *http.Request) {
	jobID := mux.Vars(r)["id"]

	// Check Redis cache first
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

// --- Helpers ---

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message, code string) {
	writeJSON(w, status, errorResponse{Error: message, Code: code})
}
