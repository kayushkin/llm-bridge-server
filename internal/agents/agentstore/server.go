package agentstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// RegisterHandlers registers agent-store's domain handlers on the given mux.
// Memory handlers have moved to memory-store.
//
// Every pattern registered here lives under a path segment agent-store owns
// (/agents, /configs, /reconcile, /files, /versions, /seed, /context). That is
// a hard requirement, not a convention: agent-store is embedded as a library
// into host muxes -- llm-bridge-server mounts it into its *root* mux -- and
// Go 1.22+ ServeMux panics at registration on a conflicting pattern. A generic
// top-level route added here therefore takes the host process down at boot,
// not at build time.
//
// Health is the process's concern, not the store's, so it is deliberately NOT
// registered here. A standalone agent-store calls RegisterHealthHandler; an
// embedding host already serves its own.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	registerWithHandler(mux, &handler{s: s})
}

// RegisterHealthHandler registers "GET /health" on the given mux.
//
// Only a process that owns its mux should call this -- i.e. a standalone
// agent-store server (cmd/server). Do NOT call it from a host that embeds
// agent-store via RegisterHandlers: the host serves its own /health, and
// registering a second one panics the host at boot.
func RegisterHealthHandler(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
}

func registerWithHandler(mux *http.ServeMux, h *handler) {
	mux.HandleFunc("GET /agents", h.listAgents)
	mux.HandleFunc("GET /agents/{slug}", h.getAgent)
	mux.HandleFunc("POST /agents", h.createAgent)
	mux.HandleFunc("PUT /agents/{slug}", h.updateAgent)
	mux.HandleFunc("DELETE /agents/{slug}", h.deleteAgent)

	mux.HandleFunc("GET /agents/{slug}/harnesses", h.getHarnesses)
	mux.HandleFunc("POST /agents/{slug}/harnesses", h.createHarness)

	mux.HandleFunc("GET /agents/{slug}/config", h.getAgentConfig)
	mux.HandleFunc("GET /configs", h.getAllConfigs)

	mux.HandleFunc("GET /reconcile", h.reconcile)

	mux.HandleFunc("GET /files", h.listFiles)
	mux.HandleFunc("GET /files/{id}", h.getFile)
	mux.HandleFunc("GET /files/{id}/content", h.getFileContent)
	mux.HandleFunc("PUT /files/{id}/content", h.putFileContent)
	mux.HandleFunc("POST /files/{id}/enable", h.enableFile)
	mux.HandleFunc("POST /files/{id}/disable", h.disableFile)
	mux.HandleFunc("POST /files/scan", h.scanFiles)
	registerPromptSourceHandlers(mux, h)

	// History (every UI save / drift / runner-detected change is a version row).
	mux.HandleFunc("GET /files/{id}/versions", h.listFileVersions)
	mux.HandleFunc("GET /versions/{vid}/content", h.getVersionContent)

	// Per-machine seeding for remote runners.
	mux.HandleFunc("GET /seed/profiles", h.listSeedProfiles)
	mux.HandleFunc("GET /seed/profile", h.getSeedProfile)
	mux.HandleFunc("PUT /seed/profile", h.putSeedProfile)
	mux.HandleFunc("GET /seed/manifest", h.getSeedManifest)
	mux.HandleFunc("POST /seed/observe", h.postSeedObserve)
	mux.HandleFunc("POST /seed/drift", h.postSeedDrift)
	mux.HandleFunc("GET /seed/state", h.getSeedState)

	mux.HandleFunc("GET /context/resolve", h.resolveContext)
}

type handler struct {
	s *Store
	// onFileSaved is invoked after a successful PUT /files/{id}/content. Wired
	// by llm-bridge-server (which embeds agent-store) to broadcast seed deltas
	// to every connected runner. nil = no broadcast (standalone agent-store
	// invocation, or a deployment without runners).
	onFileSaved     func(*TrackedFile, *TrackedFileVersion)
	onScanCompleted func(*ScanResult)
	// onPromptDriftsDetected is invoked with the drifts a reconcile pass first
	// saw, in their state after the pass. The bridge uses it to ask a tagging
	// agent to label the sections a held drift would add.
	onPromptDriftsDetected func([]PromptDrift)
}

// HandlerHooks are the callbacks an embedding server can hang on the routes.
// Every field is optional.
type HandlerHooks struct {
	OnFileSaved            func(*TrackedFile, *TrackedFileVersion)
	OnScanCompleted        func(*ScanResult)
	OnPromptDriftsDetected func([]PromptDrift)
}

// RegisterHandlersWithHookSet is RegisterHandlers plus the embedding server's hooks.
func RegisterHandlersWithHookSet(mux *http.ServeMux, s *Store, hooks HandlerHooks) {
	registerWithHandler(mux, &handler{s: s, onFileSaved: hooks.OnFileSaved, onScanCompleted: hooks.OnScanCompleted, onPromptDriftsDetected: hooks.OnPromptDriftsDetected})
}

// RegisterHandlersWithHooks is RegisterHandlers plus seed-broadcast hooks.
// Bridge-server wraps with hooks so saves and scans propagate to runners;
// standalone agent-store uses RegisterHandlers and the hooks stay nil.
func RegisterHandlersWithHooks(mux *http.ServeMux, s *Store, onSave func(*TrackedFile, *TrackedFileVersion), onScan func(*ScanResult)) {
	registerWithHandler(mux, &handler{s: s, onFileSaved: onSave, onScanCompleted: onScan})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// === Agents ===

func (h *handler) listAgents(w http.ResponseWriter, r *http.Request) {
	// Fetch statuses for merging
	statuses, _ := h.s.ListStatuses()
	statusMap := make(map[string]AgentStatus)
	for _, st := range statuses {
		if existing, ok := statusMap[st.AgentSlug]; !ok || (existing.Status == "idle" && st.Status != "idle") {
			statusMap[st.AgentSlug] = st
		}
	}

	if r.URL.Query().Get("expanded") == "true" {
		expanded, err := h.s.ListAgentsExpanded()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		type expandedResponse struct {
			Name            string  `json:"name"`
			DisplayName     string  `json:"display_name"`
			Harness         string  `json:"harness"`
			Emoji           string  `json:"emoji"`
			Project         string  `json:"project"`
			Enabled         bool    `json:"enabled"`
			IsDefault       bool    `json:"is_default"`
			HarnessEmoji    string  `json:"harness_emoji"`
			Status          string  `json:"status"`
			StatusTask      *string `json:"status_task,omitempty"`
			StatusSessionID *string `json:"status_session_id,omitempty"`
			StatusSince     *int64  `json:"status_since,omitempty"`
		}
		out := make([]expandedResponse, len(expanded))
		for i, a := range expanded {
			name := a.HarnessName
			if name == "" {
				name = a.Slug
			}
			out[i] = expandedResponse{
				Name:         name,
				DisplayName:  a.DisplayName,
				Harness:      a.Harness,
				Emoji:        a.Emoji,
				Project:      a.Projects,
				Enabled:      a.Enabled,
				IsDefault:    a.IsDefault,
				HarnessEmoji: a.HarnessEmoji,
				Status:       "idle",
			}
			if st, ok := statusMap[a.Slug]; ok {
				out[i].Status = st.Status
				out[i].StatusTask = st.Task
				out[i].StatusSessionID = st.SessionID
				out[i].StatusSince = st.StartedAt
			}
		}
		writeJSON(w, 200, out)
		return
	}

	agents, err := h.s.ListAgents()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type agentResponse struct {
		Agent
		Status          string  `json:"status"`
		StatusTask      *string `json:"status_task,omitempty"`
		StatusSessionID *string `json:"status_session_id,omitempty"`
		StatusSince     *int64  `json:"status_since,omitempty"`
	}
	out := make([]agentResponse, len(agents))
	for i, a := range agents {
		out[i] = agentResponse{Agent: a, Status: "idle"}
		if st, ok := statusMap[a.Slug]; ok {
			out[i].Status = st.Status
			out[i].StatusTask = st.Task
			out[i].StatusSessionID = st.SessionID
			out[i].StatusSince = st.StartedAt
		}
	}
	writeJSON(w, 200, out)
}

func (h *handler) getAgent(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	a, err := h.s.GetAgentBySlug(slug)
	if err != nil {
		writeErr(w, 404, "agent not found: "+slug)
		return
	}
	writeJSON(w, 200, a)
}

// decodeStrict decodes a request body and REFUSES any field it does not know,
// rather than dropping it on the floor. An unmapped field is a client that
// believes it wrote something the store never stored; answering 201 to that is
// silent data loss. This is the guard that would have caught the untagged-Agent
// defect the day it was introduced instead of after it had wiped every display
// name, so it is deliberately applied to every write path, not just that one.
func decodeStrict(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func (h *handler) createAgent(w http.ResponseWriter, r *http.Request) {
	var a Agent
	if err := decodeStrict(r, &a); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if a.Slug == "" {
		writeErr(w, 400, "slug required")
		return
	}
	if err := h.s.UpsertAgent(&a); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, a)
}

func (h *handler) updateAgent(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	existing, err := h.s.GetAgentBySlug(slug)
	if err != nil {
		writeErr(w, 404, "agent not found: "+slug)
		return
	}
	// Decode ONTO the existing agent, so a field the client omits keeps its
	// stored value instead of being zeroed. UpsertAgent writes every column, so
	// decoding into a blank Agent made PUT a full replace: bridge-ui's Agents
	// page never sends `role` at all, and sends `description` only from the edit
	// form -- so toggling an agent's Enabled checkbox erased both. Omission now
	// means "leave it alone"; a client that genuinely wants to clear a field
	// still can, by sending it explicitly as "".
	a := *existing
	if err := decodeStrict(r, &a); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.ID = existing.ID
	if a.Slug == "" {
		a.Slug = slug
	}
	if err := h.s.UpsertAgent(&a); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, a)
}

func (h *handler) deleteAgent(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	a, err := h.s.GetAgentBySlug(slug)
	if err != nil {
		writeErr(w, 404, "agent not found: "+slug)
		return
	}
	if err := h.s.DeleteAgent(a.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// === Harnesses ===

func (h *handler) getHarnesses(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	a, err := h.s.GetAgentBySlug(slug)
	if err != nil {
		writeErr(w, 404, "agent not found: "+slug)
		return
	}
	harnesses, err := h.s.GetAgentHarnesses(a.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, harnesses)
}

func (h *handler) createHarness(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	a, err := h.s.GetAgentBySlug(slug)
	if err != nil {
		writeErr(w, 404, "agent not found: "+slug)
		return
	}
	var ao AgentHarness
	if err := decodeStrict(r, &ao); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ao.AgentID = a.ID
	if err := h.s.UpsertAgentHarness(&ao); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, ao)
}

// === Config ===

func (h *handler) getAgentConfig(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("slug")
	harnessID := r.URL.Query().Get("harness")
	if harnessID == "" {
		harnessID = "inber"
	}
	cfg, err := h.s.GetFullAgentConfig(slug, harnessID)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, cfg)
}

func (h *handler) getAllConfigs(w http.ResponseWriter, r *http.Request) {
	harnessID := r.URL.Query().Get("harness")
	if harnessID == "" {
		harnessID = "inber"
	}
	configs, defaultSlug, err := h.s.GetAllAgentConfigs(harnessID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"default": defaultSlug, "agents": configs})
}

// === Reconciliation ===

func (h *handler) reconcile(w http.ResponseWriter, r *http.Request) {
	agents, err := h.s.ListAgents()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}

	type AgentDiff struct {
		Slug      string   `json:"slug"`
		Harnesses []string `json:"harnesses"`
		Issues    []string `json:"issues"`
	}

	var diffs []AgentDiff
	allHarnesses, _ := h.s.ListHarnesses()

	for _, a := range agents {
		harnesses, err := h.s.GetAgentHarnesses(a.ID)
		if err != nil {
			continue
		}
		registered := make(map[string]bool)
		var harnessNames []string
		for _, o := range harnesses {
			registered[o.HarnessID] = true
			harnessNames = append(harnessNames, o.HarnessID)
		}
		var issues []string
		for _, o := range allHarnesses {
			if !registered[o.ID] && a.Enabled {
				issues = append(issues, fmt.Sprintf("not registered in %s", o.ID))
			}
		}
		if len(issues) > 0 {
			diffs = append(diffs, AgentDiff{Slug: a.Slug, Harnesses: harnessNames, Issues: issues})
		}
	}

	modified, _ := h.s.GetModifiedFiles()
	var modifiedPaths []string
	for _, m := range modified {
		modifiedPaths = append(modifiedPaths, fmt.Sprintf("scan %d: %s", m.ID, m.DiffSummary))
	}

	writeJSON(w, 200, map[string]any{"agent_diffs": diffs, "modified_files": modifiedPaths})
}

var _ = strings.Join // keep import

// === Tracked files ===

func parseID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

func (h *handler) listFiles(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	agentSlug := r.URL.Query().Get("agent_slug")
	list := h.s.ListTrackedFiles
	if r.URL.Query().Get("include_ignored") == "true" {
		list = h.s.ListTrackedFilesIncludingIgnored
	}
	files, err := list(scope, agentSlug)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, files)
}

func (h *handler) getFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	f, err := h.s.GetTrackedFile(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, f)
}

func (h *handler) getFileContent(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	f, data, err := h.s.ReadTrackedFile(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"id":      f.ID,
		"path":    f.Path,
		"enabled": f.Enabled,
		"content": string(data),
	})
}

func (h *handler) putFileContent(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		Content string `json:"content"`
		Note    string `json:"note,omitempty"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	f, v, err := h.s.WriteTrackedFileVersioned(id, []byte(body.Content), VersionSourceUISave, "", body.Note)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if h.onFileSaved != nil && v != nil {
		h.onFileSaved(f, v)
	}
	writeJSON(w, 200, f)
}

func (h *handler) enableFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	f, err := h.s.SetTrackedFileEnabled(id, true)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, f)
}

func (h *handler) disableFile(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	f, err := h.s.SetTrackedFileEnabled(id, false)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, f)
}

func (h *handler) scanFiles(w http.ResponseWriter, r *http.Request) {
	res, err := h.s.Scan()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if h.onScanCompleted != nil {
		h.onScanCompleted(res)
	}
	// A scan is when out-of-band edits are noticed, so it is also when edits
	// to a rendered prompt file are carried back up to the sections.
	reconciliation, err := h.s.ReconcilePromptDrifts()
	if err != nil {
		writeErr(w, 500, "scan finished, prompt drift reconcile failed: "+err.Error())
		return
	}
	h.afterPromptDriftReconciliation(reconciliation)
	writeJSON(w, 200, scanResponse{ScanResult: res, PromptDriftReconciliation: reconciliation})
}

type scanResponse struct {
	*ScanResult
	PromptDriftReconciliation *PromptDriftReconciliation `json:"prompt_drift_reconciliation"`
}

// === Context resolver ===

func (h *handler) resolveContext(w http.ResponseWriter, r *http.Request) {
	harness := r.URL.Query().Get("harness")
	workDir := r.URL.Query().Get("work_dir")
	if harness == "" {
		writeErr(w, 400, "harness query param required")
		return
	}
	// Each tag parameter is one context tag, as a card's tags would be.
	res, err := h.s.ResolveContext(harness, workDir, r.URL.Query()["tag"])
	if errors.Is(err, ErrPromptHarnessDeliveryUnknown) {
		writeErr(w, 404, err.Error())
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

// === File versions (full history) ===

func (h *handler) listFileVersions(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	versions, err := h.s.ListVersions(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if versions == nil {
		versions = []TrackedFileVersion{}
	}
	writeJSON(w, 200, versions)
}

func (h *handler) getVersionContent(w http.ResponseWriter, r *http.Request) {
	vid, err := strconv.ParseInt(r.PathValue("vid"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad version id")
		return
	}
	v, err := h.s.GetVersion(vid)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	data, err := h.s.ReadVersionContent(vid)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"version": v,
		"content": string(data),
	})
}

// === Per-machine seed profiles ===

func (h *handler) listSeedProfiles(w http.ResponseWriter, r *http.Request) {
	profiles, err := h.s.ListSeedProfiles()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if profiles == nil {
		profiles = []MachineSeedProfile{}
	}
	writeJSON(w, 200, profiles)
}

func (h *handler) getSeedProfile(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		writeErr(w, 400, "machine_id required")
		return
	}
	prof, err := h.s.EnsureSeedProfile(machineID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, prof)
}

func (h *handler) putSeedProfile(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		writeErr(w, 400, "machine_id required")
		return
	}
	var body struct {
		Scopes []string `json:"scopes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	prof, err := h.s.SetSeedProfileScopes(machineID, body.Scopes)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, prof)
}

func (h *handler) getSeedManifest(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		writeErr(w, 400, "machine_id required")
		return
	}
	mf, err := h.s.BuildSeedManifest(machineID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, mf)
}

func (h *handler) postSeedObserve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MachineID     string `json:"machine_id"`
		TrackedFileID int64  `json:"tracked_file_id"`
		SHA256        string `json:"sha256"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if body.MachineID == "" || body.TrackedFileID == 0 {
		writeErr(w, 400, "machine_id and tracked_file_id required")
		return
	}
	if err := h.s.RecordObservedHash(body.MachineID, body.TrackedFileID, body.SHA256); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

// postSeedDrift is the runner's pre-overwrite safety hook. The runner POSTs
// the local on-disk content it found that doesn't match what the bridge most
// recently pushed. The bridge appends it as a runner-drift version (so it
// joins the file's history forever) and returns the version row.
//
// Only after this call returns success should the runner proceed with the
// overwrite.
func (h *handler) postSeedDrift(w http.ResponseWriter, r *http.Request) {
	var body struct {
		MachineID     string `json:"machine_id"`
		TrackedFileID int64  `json:"tracked_file_id"`
		Content       string `json:"content"`
		Note          string `json:"note,omitempty"`
	}
	raw, _ := io.ReadAll(r.Body)
	if err := json.Unmarshal(raw, &body); err != nil {
		writeErr(w, 400, "invalid json")
		return
	}
	if body.MachineID == "" || body.TrackedFileID == 0 {
		writeErr(w, 400, "machine_id and tracked_file_id required")
		return
	}
	v, err := h.s.AppendVersion(body.TrackedFileID, []byte(body.Content), VersionSourceRunnerDrift, body.MachineID, body.Note)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := h.s.RecordObservedHash(body.MachineID, body.TrackedFileID, v.SHA256); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, v)
}

func (h *handler) getSeedState(w http.ResponseWriter, r *http.Request) {
	machineID := r.URL.Query().Get("machine_id")
	if machineID == "" {
		writeErr(w, 400, "machine_id required")
		return
	}
	state, err := h.s.ListSeedStateForMachine(machineID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if state == nil {
		state = []MachineSeedState{}
	}
	writeJSON(w, 200, state)
}
