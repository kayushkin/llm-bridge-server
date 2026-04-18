package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/kayushkin/llm-bridge/msg"
)

// Request types are canonical — defined in llm-bridge/msg/session_meta.go.
type (
	CreateFolderRequest     = msg.CreateFolderRequest
	RenameFolderRequest     = msg.RenameFolderRequest
	SetSessionFolderRequest = msg.SetSessionFolderRequest
)

func (s *Server) handleListFolders(w http.ResponseWriter, r *http.Request) {
	folders, err := s.store.ListFolders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if folders == nil {
		folders = []string{}
	}
	writeJSON(w, msg.FolderList{FolderOrder: folders})
}

func (s *Server) handleCreateFolder(w http.ResponseWriter, r *http.Request) {
	var req CreateFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Name == "" {
		http.Error(w, "name is required", http.StatusBadRequest)
		return
	}
	if err := s.store.CreateFolder(req.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleDeleteFolder(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "folder name is required", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteFolder(name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRenameFolder(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req RenameFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || name == "" || req.NewName == "" {
		http.Error(w, "old and new folder names are required", http.StatusBadRequest)
		return
	}
	if err := s.store.RenameFolder(name, req.NewName); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleSetSessionFolder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req SetSessionFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || id == "" {
		http.Error(w, "session id and request body are required", http.StatusBadRequest)
		return
	}
	if err := s.store.SetSessionFolder(id, req.Folder); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
