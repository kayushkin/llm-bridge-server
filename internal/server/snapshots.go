package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	snapshotstore "github.com/kayushkin/snapshot-store"
)

// snapshotRetention is how long captured snapshot rows are kept before the
// background purge removes them. Git's prune-expire window in the blob
// sidecar is set one day wider so a blob can't be reclaimed before its
// last referencing row is deleted.
const snapshotRetention = 30 * 24 * time.Hour

// snapshotMaxBashFiles caps how many file paths a single Bash command may
// snapshot. Most commands touch 1–2 files; the cap keeps a pathological
// `rm -rf $(seq 1 1000)` from spending hours hashing.
const snapshotMaxBashFiles = 32

// handleGetSnapshots returns the captured before/after states for every file
// touched by a single tool call. Most tools snapshot one file; Bash can
// snapshot many. Each entry has its own before/after pair (either may be
// null if that phase didn't capture for that file). Response shape:
//
//	{ "files": [{ "file_path": "...", "before": {...}, "after": {...} }, ...] }
func (s *Server) handleGetSnapshots(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	toolUseID := r.PathValue("tool_use_id")
	snaps, err := s.snapshotStore.Get(sessionID, toolUseID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"files": groupSnapshotsByFile(snaps)})
}

// handleGetSnapshotBlob streams the raw bytes of a stored blob. Content is
// content-addressed by SHA so the response is safe to cache forever.
func (s *Server) handleGetSnapshotBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	content, err := s.snapshotStore.ReadBlob(sha)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
	w.Write(content)
}

// maybeCaptureSnapshot inspects a hook-exec payload and captures file state
// for every file the tool call appears to mutate. Edit/Write/MultiEdit/
// NotebookEdit each carry an explicit file_path in tool_input; Bash has its
// command parsed for known mutating ops (rm, touch, cp, mv, sed -i, tee, dd,
// shell redirects). Caller runs this in a goroutine because git-hash-object
// latency would otherwise extend the hook-exec response time.
func (s *Server) maybeCaptureSnapshot(event string, sessionID string, body []byte) {
	if s.snapshotStore == nil {
		return
	}
	phase, ok := phaseForEvent(event)
	if !ok {
		return
	}

	var payload struct {
		ToolName  string `json:"tool_name"`
		ToolUseID string `json:"tool_use_id"`
		CWD       string `json:"cwd"`
		ToolInput struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
			Command      string `json:"command"`
		} `json:"tool_input"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return
	}
	if payload.ToolUseID == "" || sessionID == "" {
		log.Printf("[snapshot] skip %s %s: tool_use_id or session_id missing",
			event, payload.ToolName)
		return
	}

	paths := pathsForTool(payload.ToolName, payload.ToolInput.FilePath,
		payload.ToolInput.NotebookPath, payload.ToolInput.Command, payload.CWD)
	if len(paths) == 0 {
		return
	}
	if len(paths) > snapshotMaxBashFiles {
		log.Printf("[snapshot] %s tool=%s captured %d/%d files (cap)",
			phase, payload.ToolName, snapshotMaxBashFiles, len(paths))
		paths = paths[:snapshotMaxBashFiles]
	}
	for _, p := range paths {
		if _, err := s.snapshotStore.Capture(sessionID, payload.ToolUseID, phase, p); err != nil {
			log.Printf("[snapshot] capture %s %s %s: %v", phase, payload.ToolName, p, err)
		}
	}
}

// pathsForTool returns the list of files a tool invocation should snapshot.
// Returns nil for tools we don't track. The list is in command order, with
// duplicates removed.
func pathsForTool(toolName, filePath, notebookPath, command, cwd string) []string {
	switch toolName {
	case "Edit", "Write", "MultiEdit":
		if filePath == "" {
			return nil
		}
		return []string{filePath}
	case "NotebookEdit":
		if notebookPath != "" {
			return []string{notebookPath}
		}
		if filePath != "" {
			return []string{filePath}
		}
		return nil
	case "Bash":
		return extractBashFilePaths(command, cwd)
	default:
		return nil
	}
}

// startSnapshotGC launches background retention enforcement. Daily sweep
// deletes metadata rows older than snapshotRetention; weekly git gc reclaims
// disk once blobs have aged past the prune-expire window.
func (s *Server) startSnapshotGC() {
	if s.snapshotStore == nil {
		return
	}
	go func() {
		purge := time.NewTicker(24 * time.Hour)
		gc := time.NewTicker(7 * 24 * time.Hour)
		defer purge.Stop()
		defer gc.Stop()
		for {
			select {
			case <-purge.C:
				n, err := s.snapshotStore.PurgeOlderThan(time.Now().Add(-snapshotRetention))
				if err != nil {
					log.Printf("[snapshot] purge: %v", err)
					continue
				}
				if n > 0 {
					log.Printf("[snapshot] purged %d rows older than %s", n, snapshotRetention)
				}
			case <-gc.C:
				if err := s.snapshotStore.GC(); err != nil {
					log.Printf("[snapshot] gc: %v", err)
				}
			}
		}
	}()
}

func phaseForEvent(event string) (snapshotstore.Phase, bool) {
	switch event {
	case "PreToolUse":
		return snapshotstore.PhaseBefore, true
	case "PostToolUse":
		return snapshotstore.PhaseAfter, true
	default:
		return "", false
	}
}

// fileSnapshots is one entry in the API response: a single file plus its
// before/after captures (either may be nil if that phase didn't fire for
// that file).
type fileSnapshots struct {
	FilePath string `json:"file_path"`
	Before   any    `json:"before"`
	After    any    `json:"after"`
}

// groupSnapshotsByFile collapses the flat snapshot rows for one tool call
// into one entry per file_path with paired before/after metadata. Files are
// returned sorted by path so the API response is deterministic.
func groupSnapshotsByFile(snaps []snapshotstore.Snapshot) []fileSnapshots {
	if len(snaps) == 0 {
		return []fileSnapshots{}
	}
	byPath := make(map[string]*fileSnapshots)
	for i := range snaps {
		snap := &snaps[i]
		entry, ok := byPath[snap.FilePath]
		if !ok {
			entry = &fileSnapshots{FilePath: snap.FilePath}
			byPath[snap.FilePath] = entry
		}
		switch snap.Phase {
		case snapshotstore.PhaseBefore:
			entry.Before = marshalSnapshot(snap)
		case snapshotstore.PhaseAfter:
			entry.After = marshalSnapshot(snap)
		}
	}
	out := make([]fileSnapshots, 0, len(byPath))
	for _, e := range byPath {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FilePath < out[j].FilePath })
	return out
}

func marshalSnapshot(snap *snapshotstore.Snapshot) any {
	if snap == nil {
		return nil
	}
	out := map[string]any{
		"phase":      string(snap.Phase),
		"file_path":  snap.FilePath,
		"size":       snap.Size,
		"created_at": snap.CreatedAt,
	}
	if snap.BlobSHA != "" {
		out["blob_sha"] = snap.BlobSHA
		out["blob_url"] = "/snapshots/blob/" + snap.BlobSHA
	}
	if snap.IsBinary {
		out["is_binary"] = true
	}
	if snap.TooLarge {
		out["too_large"] = true
	}
	if snap.Missing {
		out["missing"] = true
	}
	return out
}

