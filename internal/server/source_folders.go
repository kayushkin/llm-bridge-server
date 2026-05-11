package server

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
)

// handleListSourceFolders returns the effective source→folder map: env-var
// defaults overlaid with runtime overrides from the source_folders table.
// Sources present only in the env defaults are reported with default=true;
// sources with a row in the table are default=false (overridden) and carry
// updated_at. Sources present in both are reported once, with the override
// winning and default=false.
func (s *Server) handleListSourceFolders(w http.ResponseWriter, r *http.Request) {
	overrides, err := s.store.ListSourceFolders()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	updatedAts, err := s.store.SourceFolderTimestamps()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	out := make([]msg.SourceFolderMapping, 0, len(overrides)+len(s.cfg.SourceFolders))
	seen := make(map[string]bool)
	for src, folder := range overrides {
		out = append(out, msg.SourceFolderMapping{
			Purpose:    src,
			FolderName: folder,
			Default:    false,
			UpdatedAt:  updatedAts[src],
		})
		seen[src] = true
	}
	for src, folder := range s.cfg.SourceFolders {
		if seen[src] {
			continue
		}
		out = append(out, msg.SourceFolderMapping{
			Purpose:    src,
			FolderName: folder,
			Default:    true,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Purpose < out[j].Purpose })

	writeJSON(w, out)
}

// syncSourceFolderRegistry ensures every folder referenced by either the
// env-backed source defaults or persisted source-folder overrides exists in
// the folder registry. This keeps upgraded installs in sync when new
// LLMBRIDGE_SOURCE_FOLDERS entries are introduced without waiting for the
// first matching session to be created.
func (s *Server) syncSourceFolderRegistry() {
	if s.store == nil {
		return
	}

	wanted := make(map[string]struct{})
	if s.cfg != nil {
		for _, folder := range s.cfg.SourceFolders {
			folder = strings.TrimSpace(folder)
			if folder != "" {
				wanted[folder] = struct{}{}
			}
		}
	}

	overrides, err := s.store.ListSourceFolders()
	if err != nil {
		log.Printf("[source-folders] failed to load overrides for startup sync: %v", err)
	} else {
		for _, folder := range overrides {
			folder = strings.TrimSpace(folder)
			if folder != "" {
				wanted[folder] = struct{}{}
			}
		}
	}

	folders := make([]string, 0, len(wanted))
	for folder := range wanted {
		folders = append(folders, folder)
	}
	sort.Strings(folders)

	for _, folder := range folders {
		if err := s.store.CreateFolder(folder); err != nil {
			log.Printf("[source-folders] failed to ensure folder %q: %v", folder, err)
		}
	}
}

// handlePutSourceFolder upserts a runtime override for the given source.
// If apply_to_existing is true, sessions tagged with this source whose
// folder is empty or matches the previous effective folder are rebucketed
// to the new folder; manual moves to other folders are preserved.
func (s *Server) handlePutSourceFolder(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.PathValue("source"))
	if source == "" {
		http.Error(w, "source is required", http.StatusBadRequest)
		return
	}

	var req msg.PutSourceFolderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	folder := strings.TrimSpace(req.FolderName)
	if folder == "" {
		http.Error(w, "folder_name is required", http.StatusBadRequest)
		return
	}
	if !s.folderExists(folder) {
		http.Error(w, "folder does not exist", http.StatusBadRequest)
		return
	}

	prevFolder := s.folderForSource(source)

	if err := s.store.UpsertSourceFolder(source, folder); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	result := msg.SourceFolderApplyResult{
		Mapping: msg.SourceFolderMapping{
			Purpose:    source,
			FolderName: folder,
			Default:    false,
		},
	}
	if ts, err := s.store.SourceFolderTimestamps(); err == nil {
		result.Mapping.UpdatedAt = ts[source]
	}
	if req.ApplyToExisting {
		updated, err := s.store.ApplySourceFolder(source, prevFolder, folder)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		result.Updated = updated
	}
	writeJSON(w, result)
}

// handleDeleteSourceFolder removes the runtime override; the env default
// (if any) becomes effective again. Backfill is forward-only for delete:
// sessions stay in whatever folder they were in.
func (s *Server) handleDeleteSourceFolder(w http.ResponseWriter, r *http.Request) {
	source := strings.TrimSpace(r.PathValue("source"))
	if source == "" {
		http.Error(w, "source is required", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteSourceFolder(source); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// folderExists reports whether `name` is in the folders table.
func (s *Server) folderExists(name string) bool {
	folders, err := s.store.ListFolders()
	if err != nil {
		return false
	}
	for _, f := range folders {
		if f == name {
			return true
		}
	}
	return false
}
