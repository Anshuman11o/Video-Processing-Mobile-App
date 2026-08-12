package api

import (
	"net/http"

	"github.com/gorilla/mux"
)

// NewRouter creates a new mux.Router with all API routes configured.
func NewRouter(h *Handler) *mux.Router {
	r := mux.NewRouter()

	// Health check
	r.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	}).Methods("GET")

	// Job endpoints
	r.HandleFunc("/jobs", h.CreateJob).Methods("POST")
	r.HandleFunc("/jobs/{id}", h.GetJobStatus).Methods("GET")
	r.HandleFunc("/jobs/{id}/complete", h.CompleteUpload).Methods("POST")
	r.HandleFunc("/jobs/{id}/reel", h.GetReel).Methods("GET")

	// Apply middleware
	r.Use(LoggingMiddleware)
	r.Use(CORSMiddleware)

	return r
}
