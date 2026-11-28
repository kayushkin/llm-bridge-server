package sessions

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Request types are canonical — defined in llm-bridge/msg/session_meta.go.
type (
	CreateFolderRequest     = msg.CreateFolderRequest
	RenameFolderRequest     = msg.RenameFolderRequest
	SetSessionFolderRequest = msg.SetSessionFolderRequest
	MarkSessionDoneRequest  = msg.MarkSessionDoneRequest
)

func (s *Service) HandleListFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.Store.ListFolders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if folders == nil {
		folders = []string{}
	}
	httpjson.WriteJSON(w, msg.FolderList{FolderOrder: folders})
}

func (s *Service) HandleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req CreateFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := s.Store.CreateFolder(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) HandleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "folder name is required", http.StatusBadRequest)
		return
	}
	if err := s.Store.DeleteFolder(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) HandleRenameFolder(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req RenameFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || name == "" || req.NewName == "" {
		http.Error(w, "old and new folder names are required", http.StatusBadRequest)
		return
	}
	if err := s.Store.RenameFolder(name, req.NewName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) HandleSetSessionFolder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req SetSessionFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || id == "" {
		http.Error(w, "session id and request body are required", http.StatusBadRequest)
		return
	}
	if err := s.Store.SetSessionFolder(id, req.Folder); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleMarkSessionDone toggles a session between "done" and "active". The
// done state atomically (a) emits an EventSessionState through the central
// derivation pipeline so the row + SSE feed agree, and (b) moves the session
// into the canonical Archive folder. Reversing both undoes a manual mark.
//
// New events flowing into the session after a manual mark will overwrite the
// state through derivation (e.g. a tool_call flips back to tool_running) —
// that's intentional, the session re-engaging visibly says "no longer done".
func (s *Service) HandleMarkSessionDone(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req MarkSessionDoneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || id == "" {
		http.Error(w, "session id and request body are required", http.StatusBadRequest)
		return
	}

	sess, err := s.Store.GetSession(id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || sess == nil {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	nextState := msg.SessionIdle
	nextFolder := ""
	if req.Done {
		nextState = msg.SessionCompleted
		nextFolder = store.ArchiveFolder
	}

	// Emit the state transition through BroadcastEvent so derivation owns
	// the row update + SSE fan-out (single source of truth for session.state).
	if _, err := s.Harness.BroadcastEvent(&msg.Event{
		Type:            msg.EventSessionState,
		Harness:         msg.Harness(sess.Harness),
		BridgeSessionID: id,
		Timestamp:       time.Now(),
		State: &msg.StateEvent{
			State:  nextState,
			Reason: string(msg.ReasonMarkDoneUser),
		},
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Folder move is the second half of the atomic action.
	if err := s.Store.SetSessionFolder(id, nextFolder); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
