package skills

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Handlers answers the store's HTTP routes. The server that mounts the store
// registers each method under its own pattern, behind its own prefix and
// access rules; the paths in the method comments are relative to that prefix.
type Handlers struct {
	s *Store
}

// NewHandlers returns the route handlers of store.
func NewHandlers(store *Store) *Handlers { return &Handlers{s: store} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// Health answers GET /health.
func (h *Handlers) Health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// ---- sources ----

// ListSources answers GET /sources.
func (h *Handlers) ListSources(w http.ResponseWriter, r *http.Request) {
	srcs, err := h.s.ListSources(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if srcs == nil {
		srcs = []Source{}
	}
	writeJSON(w, 200, srcs)
}

// CreateSource answers POST /sources.
func (h *Handlers) CreateSource(w http.ResponseWriter, r *http.Request) {
	var src Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		writeErr(w, 400, "invalid json: "+err.Error())
		return
	}
	if src.Name == "" {
		writeErr(w, 400, "name required")
		return
	}
	if src.Kind == "" {
		src.Kind = SourceKindGit
	}
	switch src.Kind {
	case SourceKindGit:
		if src.RepoURL == "" {
			writeErr(w, 400, "repo_url required for a git source")
			return
		}
	case SourceKindLocal:
		if src.LocalPath == "" {
			writeErr(w, 400, "local_path required for a local source")
			return
		}
	default:
		writeErr(w, 400, fmt.Sprintf("unknown kind %q (want %q or %q)", src.Kind, SourceKindGit, SourceKindLocal))
		return
	}
	src.Enabled = true
	if err := h.s.UpsertSource(r.Context(), &src); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, src)
}

// GetSource answers GET /sources/{id}.
func (h *Handlers) GetSource(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	src, err := h.s.GetSource(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "source not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, src)
}

// DeleteSource answers DELETE /sources/{id}.
func (h *Handlers) DeleteSource(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.DeleteSource(r.Context(), id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// SyncSource answers POST /sources/{id}/sync.
func (h *Handlers) SyncSource(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	res, err := h.s.Sync(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "source not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// ---- skills ----

// ListSkills answers GET /skills.
func (h *Handlers) ListSkills(w http.ResponseWriter, r *http.Request) {
	f := ListSkillsFilter{
		SourceName:    r.URL.Query().Get("source"),
		Query:         r.URL.Query().Get("q"),
		Tag:           r.URL.Query().Get("tag"),
		InstalledOnly: r.URL.Query().Get("installed") == "true",
	}
	if sid := r.URL.Query().Get("source_id"); sid != "" {
		id, err := strconv.ParseInt(sid, 10, 64)
		if err != nil {
			writeErr(w, 400, "invalid source_id")
			return
		}
		f.SourceID = id
	}
	if lim := r.URL.Query().Get("limit"); lim != "" {
		n, err := strconv.Atoi(lim)
		if err != nil {
			writeErr(w, 400, "invalid limit")
			return
		}
		f.Limit = n
	}
	skills, err := h.s.ListSkills(r.Context(), f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if skills == nil {
		skills = []Skill{}
	}
	writeJSON(w, 200, skills)
}

// GetSkill answers GET /skills/{id}.
func (h *Handlers) GetSkill(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sk, err := h.s.GetSkill(r.Context(), id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "skill not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, sk)
}

// DeleteSkill answers DELETE /skills/{id}.
func (h *Handlers) DeleteSkill(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := h.s.DeleteSkill(r.Context(), id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	_ = os.RemoveAll(h.s.SkillDir(id))
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

type skillFileEntry struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// ListSkillFiles answers GET /skills/{id}/files.
func (h *Handlers) ListSkillFiles(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	dir := h.s.SkillDir(id)
	var files []skillFileEntry
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return filepath.SkipAll
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		rel, _ := filepath.Rel(dir, path)
		files = append(files, skillFileEntry{Path: rel, Size: info.Size()})
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, 500, err.Error())
		return
	}
	if files == nil {
		files = []skillFileEntry{}
	}
	writeJSON(w, 200, files)
}

// GetSkillFile answers GET /skills/{id}/files/{path...}.
func (h *Handlers) GetSkillFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rel := r.PathValue("path")
	if rel == "" || strings.Contains(rel, "..") {
		writeErr(w, 400, "invalid path")
		return
	}
	base := h.s.SkillDir(id)
	full := filepath.Join(base, rel)
	// Defense in depth: reject escapes
	if !strings.HasPrefix(filepath.Clean(full), filepath.Clean(base)+string(os.PathSeparator)) {
		writeErr(w, 400, "invalid path")
		return
	}
	info, err := os.Stat(full)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, 404, "file not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if info.IsDir() {
		writeErr(w, 400, "path is a directory")
		return
	}
	ctype := mime.TypeByExtension(filepath.Ext(full))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	http.ServeFile(w, r, full)
}

// InstallSkill answers POST /skills/{id}/install.
func (h *Handlers) InstallSkill(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	force := r.URL.Query().Get("force") == "true"
	inst, err := h.s.Install(r.Context(), id, force)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, 404, "skill not found")
			return
		}
		if errors.Is(err, ErrInstallConflict) {
			writeErr(w, 409, err.Error())
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, inst)
}

// UninstallSkill answers POST /skills/{id}/uninstall.
func (h *Handlers) UninstallSkill(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	inst, err := h.s.Uninstall(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if inst == nil {
		writeJSON(w, 200, map[string]string{"status": "not-installed"})
		return
	}
	writeJSON(w, 200, inst)
}

// ListInstallations answers GET /installations.
func (h *Handlers) ListInstallations(w http.ResponseWriter, r *http.Request) {
	insts, err := h.s.ListInstallations(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if insts == nil {
		insts = []Installation{}
	}
	writeJSON(w, 200, insts)
}

func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("invalid id")
	}
	return id, nil
}

// === seed endpoints ===

// GetSeedProfile answers GET /seed/profile.
func (h *Handlers) GetSeedProfile(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		writeErr(w, 400, "machine_id required")
		return
	}
	prof, err := h.s.EnsureSeedProfile(r.Context(), machineID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, prof)
}

// PutSeedProfile answers PUT /seed/profile.
func (h *Handlers) PutSeedProfile(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		writeErr(w, 400, "machine_id required")
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	prof, err := h.s.SetSeedProfileEnabled(r.Context(), machineID, body.Enabled)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, prof)
}

// GetSeedManifest answers GET /seed/manifest.
func (h *Handlers) GetSeedManifest(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		writeErr(w, 400, "machine_id required")
		return
	}
	mf, err := h.s.BuildSeedManifest(r.Context(), machineID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, mf)
}

// GetVersionContent answers GET /versions/{vid}/content.
func (h *Handlers) GetVersionContent(w http.ResponseWriter, r *http.Request) {
	vid, err := strconv.ParseInt(r.PathValue("vid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad version id")
		return
	}
	v, err := h.s.GetSeedVersion(r.Context(), vid)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	data, err := h.s.ReadSeedVersionContent(r.Context(), vid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"version": v,
		"content": string(data),
	})
}

// PostSeedObserve answers POST /seed/observe.
func (h *Handlers) PostSeedObserve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MachineID string `json:"machine_id"`
		SkillID   int64  `json:"skill_id"`
		Relpath   string `json:"relpath"`
		SHA256    string `json:"sha256"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if body.MachineID == "" || body.SkillID == 0 || body.Relpath == "" {
		writeErr(w, 400, "machine_id, skill_id, relpath required")
		return
	}
	if err := h.s.RecordObservedHash(r.Context(), body.MachineID, body.SkillID, body.Relpath, body.SHA256); err != nil {
		if errors.Is(err, ErrReferencedRowNotFound) {
			writeErr(w, 404, "skill not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// PostSeedDrift answers POST /seed/drift.
func (h *Handlers) PostSeedDrift(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MachineID string `json:"machine_id"`
		SkillID   int64  `json:"skill_id"`
		Relpath   string `json:"relpath"`
		Content   string `json:"content"`
		Note      string `json:"note,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if body.MachineID == "" || body.SkillID == 0 || body.Relpath == "" {
		writeErr(w, 400, "machine_id, skill_id, relpath required")
		return
	}
	v, err := h.s.AppendSeedVersion(r.Context(), body.SkillID, body.Relpath, []byte(body.Content), SeedSourceRunnerDrift, body.MachineID, body.Note)
	if err != nil {
		if errors.Is(err, ErrReferencedRowNotFound) {
			writeErr(w, 404, "skill not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	if err := h.s.RecordObservedHash(r.Context(), body.MachineID, body.SkillID, body.Relpath, v.SHA256); err != nil {
		if errors.Is(err, ErrReferencedRowNotFound) {
			writeErr(w, 404, "skill not found")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}
