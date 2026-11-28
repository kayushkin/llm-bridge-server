package sessions

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshots"
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

// HandleGetSnapshots returns the captured before/after states for every file
// touched by a single tool call. Most tools snapshot one file; Bash can
// snapshot many. Each entry has its own before/after pair (either may be
// null if that phase didn't capture for that file). Response shape:
//
//	{ "files": [{ "file_path": "...", "before": {...}, "after": {...} }, ...] }
//
// ⚠️ Two session ids are in play and they are not interchangeable. This route is
// addressed by the BRIDGE session id, like every other /sessions/{id}/… route.
// Snapshot rows are keyed by the HARNESS session id, because capture runs off a
// hook-exec payload and `session_id` in that payload is the id the harness
// reports for itself (MaybeCaptureSnapshot, via HandleExecHook). Reading with
// the id the URL carries therefore matched nothing, ever: measured 2026-08-13,
// 14,690 captured rows across 817 sessions, not one of them keyed by a bridge
// id, and every lookup returning {"files":[]}. It failed silently because an
// empty list is a legitimate answer for a tool that touched no file, so the UI
// drew a tool card with no diff and no error — on both dash surfaces, for as
// long as the feature has existed.
//
// The translation belongs here rather than in the client: this server owns both
// ids and the mapping between them, and moving the route to take a harness id
// would make one /sessions/{id}/ path mean something different from all the
// others. There is deliberately NO fallback to the bridge id — capture never
// writes one, so a fallback could only mask the next regression of this exact
// kind.
func (s *Service) HandleGetSnapshots(w http.ResponseWriter, r *http.Request) {
	bridgeID := r.PathValue("id")
	toolUseID := r.PathValue("tool_use_id")

	sess, err := s.Store.GetSession(bridgeID)
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	// A session that never reached its harness has no harness id and so cannot
	// have captured anything. Empty is the honest answer, not an error.
	if sess.HarnessSessionID == "" {
		httpjson.WriteJSON(w, map[string]any{"files": []FileSnapshots{}})
		return
	}

	snaps, err := s.SnapshotStore().Get(sess.HarnessSessionID, toolUseID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	httpjson.WriteJSON(w, map[string]any{"files": groupSnapshotsByFile(snaps)})
}

// HandleGetSnapshotBlob streams the raw bytes of a stored blob. Content is
// content-addressed by SHA so the response is safe to cache forever.
func (s *Service) HandleGetSnapshotBlob(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	content, err := s.SnapshotStore().ReadBlob(sha)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(content)))
	w.Write(content)
}

// MaybeCaptureSnapshot inspects a hook-exec payload and captures file state
// for every file the tool call appears to mutate. Edit/Write/MultiEdit/
// NotebookEdit each carry an explicit file_path in tool_input; Bash has its
// command parsed for known mutating ops (rm, touch, cp, mv, sed -i, tee, dd,
// shell redirects). Caller runs this in a goroutine because git-hash-object
// latency would otherwise extend the hook-exec response time.
func (s *Service) MaybeCaptureSnapshot(event string, sessionID string, body []byte) {
	if s.SnapshotStore() == nil {
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
		if _, err := s.SnapshotStore().Capture(sessionID, payload.ToolUseID, phase, p); err != nil {
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
		return ExtractBashFilePaths(command, cwd)
	default:
		return nil
	}
}

// StartSnapshotGC launches background retention enforcement. Daily sweep
// deletes metadata rows older than snapshotRetention; weekly git gc reclaims
// disk once blobs have aged past the prune-expire window.
func (s *Service) StartSnapshotGC() {
	if s.SnapshotStore() == nil {
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
				n, err := s.SnapshotStore().PurgeOlderThan(time.Now().Add(-snapshotRetention))
				if err != nil {
					log.Printf("[snapshot] purge: %v", err)
					continue
				}
				if n > 0 {
					log.Printf("[snapshot] purged %d rows older than %s", n, snapshotRetention)
				}
			case <-gc.C:
				if err := s.SnapshotStore().GC(); err != nil {
					log.Printf("[snapshot] gc: %v", err)
				}
			}
		}
	}()
}

func phaseForEvent(event string) (snapshots.Phase, bool) {
	switch event {
	case "PreToolUse":
		return snapshots.PhaseBefore, true
	case "PostToolUse":
		return snapshots.PhaseAfter, true
	default:
		return "", false
	}
}

// FileSnapshots is one entry in the API response: a single file plus its
// before/after captures (either may be nil if that phase didn't fire for
// that file).
type FileSnapshots struct {
	FilePath string `json:"file_path"`
	Before   any    `json:"before"`
	After    any    `json:"after"`
}

// groupSnapshotsByFile collapses the flat snapshot rows for one tool call
// into one entry per file_path with paired before/after metadata. Files are
// returned sorted by path so the API response is deterministic.
func groupSnapshotsByFile(snaps []snapshots.Snapshot) []FileSnapshots {
	if len(snaps) == 0 {
		return []FileSnapshots{}
	}
	byPath := make(map[string]*FileSnapshots)
	for i := range snaps {
		snap := &snaps[i]
		entry, ok := byPath[snap.FilePath]
		if !ok {
			entry = &FileSnapshots{FilePath: snap.FilePath}
			byPath[snap.FilePath] = entry
		}
		switch snap.Phase {
		case snapshots.PhaseBefore:
			entry.Before = marshalSnapshot(snap)
		case snapshots.PhaseAfter:
			entry.After = marshalSnapshot(snap)
		}
	}
	out := make([]FileSnapshots, 0, len(byPath))
	for _, e := range byPath {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FilePath < out[j].FilePath })
	return out
}

func marshalSnapshot(snap *snapshots.Snapshot) any {
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
