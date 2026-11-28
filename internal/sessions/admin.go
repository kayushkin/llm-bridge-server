package sessions

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge/msg"
)

// HandleFileInactive moves every unfiled session whose updated_at is older
// than InactiveDays into Folder. Intended to be called by the scheduler as a
// periodic housekeeping job.
func (s *Service) HandleFileInactive(w http.ResponseWriter, r *http.Request) {
	var req msg.FileInactiveRequest
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
	ids, err := s.Store.FileInactive(cutoff, req.Folder)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[admin] file-inactive: moved %d sessions (older than %dd) into %q", len(ids), req.InactiveDays, req.Folder)
	httpjson.WriteJSON(w, msg.FileInactiveResponse{
		Moved:  len(ids),
		Folder: req.Folder,
		IDs:    ids,
	})
}

// HandleArchiveOld moves every non-running session older than InactiveDays
// into the Archive folder, regardless of the session's current folder.
// Intended to be called by the scheduler as a periodic housekeeping job.
func (s *Service) HandleArchiveOld(w http.ResponseWriter, r *http.Request) {
	var req msg.ArchiveOldRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if req.InactiveDays <= 0 {
		http.Error(w, "inactive_days must be > 0", http.StatusBadRequest)
		return
	}

	cutoff := time.Now().UTC().AddDate(0, 0, -req.InactiveDays)
	ids, err := s.Store.ArchiveOld(cutoff)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[admin] archive-old: archived %d sessions (older than %dd)", len(ids), req.InactiveDays)
	httpjson.WriteJSON(w, msg.ArchiveOldResponse{
		Moved: len(ids),
		IDs:   ids,
	})
}
