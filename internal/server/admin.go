package server

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// handleFoldInactive moves every unfiled session whose updated_at is older
// than InactiveDays into Folder. Intended to be called by the scheduler as a
// periodic housekeeping job.
func (s *Server) handleFoldInactive(w http.ResponseWriter, r *http.Request) {
	var req msg.FoldInactiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.InactiveDays <= 0 {
		http.Error(w, "inactive_days must be > 0", http.StatusBadRequest)
		return
	}
	if req.Folder == "" {
		http.Error(w, "folder is required", http.StatusBadRequest)
		return
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -req.InactiveDays)
	ids, err := s.store.FoldInactive(cutoff, req.Folder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[admin] fold-inactive: moved %d sessions (older than %dd) into %q", len(ids), req.InactiveDays, req.Folder)
	writeJSON(w, msg.FoldInactiveResponse{
		Moved:  len(ids),
		Folder: req.Folder,
		IDs:    ids,
	})
}
