package agentstore

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func registerPromptSourceHandlers(mux *http.ServeMux, h *handler) {
	mux.HandleFunc("GET /prompt-collections", h.listPromptCollections)
	mux.HandleFunc("POST /prompt-collections", h.createPromptCollection)
	mux.HandleFunc("GET /prompt-collections/{id}", h.getPromptCollection)
	mux.HandleFunc("POST /prompt-collections/{id}/sections", h.createPromptSection)
	mux.HandleFunc("POST /prompt-collections/{id}/render", h.renderPromptCollection)
	mux.HandleFunc("POST /prompt-collections/{id}/outputs", h.addPromptCollectionOutput)
	mux.HandleFunc("PUT /prompt-collections/{id}/root", h.movePromptCollectionRoot)
	mux.HandleFunc("POST /prompt-collections/{id}/retire", h.retirePromptCollection)
	mux.HandleFunc("GET /prompt-collections/{id}/revisions", h.listPromptCollectionRevisions)
	mux.HandleFunc("POST /prompt-collections/import-untracked", h.importUntrackedPromptFiles)
	mux.HandleFunc("POST /prompt-outputs/{id}/enable", h.enablePromptOutput)
	mux.HandleFunc("POST /prompt-outputs/{id}/disable", h.disablePromptOutput)
	mux.HandleFunc("PUT /prompt-sections/{id}", h.updatePromptSection)
	mux.HandleFunc("DELETE /prompt-sections/{id}", h.deletePromptSection)
	mux.HandleFunc("GET /prompt-sections/{id}/revisions", h.listPromptSectionRevisions)

	mux.HandleFunc("GET /prompt-drifts", h.listPromptDrifts)
	mux.HandleFunc("POST /prompt-drifts/reconcile", h.reconcilePromptDrifts)
	mux.HandleFunc("GET /prompt-drifts/{id}", h.getPromptDrift)
	mux.HandleFunc("GET /prompt-drifts/{id}/disk-content", h.getPromptDriftDiskContent)
	mux.HandleFunc("PUT /prompt-drifts/{id}/annotation", h.setPromptDriftAnnotation)
	mux.HandleFunc("POST /prompt-drifts/{id}/apply", h.applyPromptDrift)
	mux.HandleFunc("POST /prompt-drifts/{id}/dismiss", h.dismissPromptDrift)

	mux.HandleFunc("GET /prompt-harness-deliveries", h.listPromptHarnessDeliveries)
	mux.HandleFunc("PUT /prompt-harness-deliveries/{harness}", h.setPromptHarnessDelivery)
	mux.HandleFunc("GET /prompt-delivery-options", h.listPromptDeliveryOptions)

	mux.HandleFunc("GET /tracked-file-ignore-rules", h.listTrackedFileIgnoreRules)
	mux.HandleFunc("POST /tracked-file-ignore-rules", h.createTrackedFileIgnoreRule)
	mux.HandleFunc("POST /tracked-file-ignore-rules/{id}/enable", h.enableTrackedFileIgnoreRule)
	mux.HandleFunc("POST /tracked-file-ignore-rules/{id}/disable", h.disableTrackedFileIgnoreRule)
}

// writeStoreErr maps a store error to a status: a missing row is the caller's
// 404, a rule the input broke is a 400, anything else is ours.
func writeStoreErr(w http.ResponseWriter, err error, inputStatus int) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeErr(w, 404, "not found")
	default:
		writeErr(w, inputStatus, err.Error())
	}
}

func (h *handler) listPromptCollections(w http.ResponseWriter, r *http.Request) {
	views, err := h.s.ListPromptCollectionViews()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, views)
}

func (h *handler) getPromptCollection(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	view, err := h.s.GetPromptCollectionView(id)
	if err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	writeJSON(w, 200, view)
}

func (h *handler) createPromptCollection(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Scope       string `json:"scope"`
		RootPath    string `json:"root_path"`
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c, err := h.s.EnsurePromptCollection(body.Scope, strings.TrimSpace(body.RootPath), strings.TrimSpace(body.Title), strings.TrimSpace(body.Description))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	view, err := h.s.GetPromptCollectionView(c.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, view)
}

// promptSectionWrite is the body of a section create or update. note is the
// reason for the change and lands on the revision. There is no heading field:
// the store composes the heading line from level and title.
type promptSectionWrite struct {
	Level    *int     `json:"level"` // required: 0 is a real level, so an absent one cannot be told from it
	Title    string   `json:"title"`
	Body     string   `json:"body"`
	Tags     []string `json:"tags"`
	Position int      `json:"position"`
	// AfterSectionID places a new section right after an existing one. Create only.
	AfterSectionID int64  `json:"after_section_id"`
	Enabled        *bool  `json:"enabled"`
	Note           string `json:"note"`
}

func (b promptSectionWrite) section() (*PromptSection, error) {
	if b.Level == nil {
		return nil, errors.New("level is required: 0 for text with no heading, 1 for a group, 2 for a section in a group")
	}
	enabled := true
	if b.Enabled != nil {
		enabled = *b.Enabled
	}
	return &PromptSection{Level: *b.Level, Title: b.Title, Body: b.Body, Tags: b.Tags, Position: b.Position, Enabled: enabled}, nil
}

func (h *handler) createPromptSection(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body promptSectionWrite
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	section, err := body.section()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	result, err := h.s.CreatePromptSection(id, section, body.AfterSectionID, body.Note)
	if err != nil {
		writeStoreErr(w, err, 400)
		return
	}
	h.broadcastRenderedFiles(result)
	writeJSON(w, 201, result)
}

func (h *handler) updatePromptSection(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body promptSectionWrite
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if body.AfterSectionID != 0 {
		writeErr(w, 400, "after_section_id places a new section; to move an existing one, set position")
		return
	}
	if body.Position == 0 {
		existing, err := h.s.GetPromptSection(id)
		if err != nil {
			writeStoreErr(w, err, 500)
			return
		}
		body.Position = existing.Position
	}
	section, err := body.section()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	result, err := h.s.UpdatePromptSection(id, section, body.Note)
	if err != nil {
		writeStoreErr(w, err, 400)
		return
	}
	h.broadcastRenderedFiles(result)
	writeJSON(w, 200, result)
}

func (h *handler) deletePromptSection(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	result, err := h.s.DeletePromptSection(id, r.URL.Query().Get("note"))
	if err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	h.broadcastRenderedFiles(result)
	writeJSON(w, 200, result)
}

func (h *handler) renderPromptCollection(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	result, err := h.s.RenderPromptCollection(id)
	if err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	h.broadcastRenderedFiles(result)
	if result.RefusedReason != "" {
		writeJSON(w, 409, result)
		return
	}
	writeJSON(w, 200, result)
}

func (h *handler) addPromptCollectionOutput(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		RelativePath string `json:"relative_path"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	view, err := h.s.AddPromptCollectionOutput(id, body.RelativePath)
	if err != nil {
		writeStoreErr(w, err, 400)
		return
	}
	writeJSON(w, 201, view)
}

// movePromptCollectionRoot points a project collection at a new root, for a
// repo whose directory was renamed. slug is optional; empty keeps it.
func (h *handler) movePromptCollectionRoot(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		RootPath string `json:"root_path"`
		Slug     string `json:"slug"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	move, err := h.s.MovePromptCollectionRoot(id, body.RootPath, body.Slug)
	switch {
	case errors.Is(err, ErrPromptCollectionMoveConflict):
		writeErr(w, 409, err.Error())
		return
	case errors.Is(err, ErrPromptCollectionMoveInvalid):
		writeErr(w, 400, err.Error())
		return
	case err != nil:
		writeStoreErr(w, err, 500)
		return
	}
	writeJSON(w, 200, move)
}

func (h *handler) setPromptOutputEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	view, err := h.s.SetPromptCollectionOutputEnabled(id, enabled)
	if err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	writeJSON(w, 200, view)
}

func (h *handler) enablePromptOutput(w http.ResponseWriter, r *http.Request) {
	h.setPromptOutputEnabled(w, r, true)
}

func (h *handler) disablePromptOutput(w http.ResponseWriter, r *http.Request) {
	h.setPromptOutputEnabled(w, r, false)
}

func (h *handler) listPromptCollectionRevisions(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	revisions, err := h.s.ListPromptSectionRevisions(id, 0, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, revisions)
}

func (h *handler) listPromptSectionRevisions(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var collectionID int64
	if err := h.s.db.QueryRow(`SELECT collection_id FROM prompt_section_revisions WHERE section_id=? ORDER BY id DESC LIMIT 1`, id).Scan(&collectionID); err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	revisions, err := h.s.ListPromptSectionRevisions(collectionID, id, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, revisions)
}

func (h *handler) importUntrackedPromptFiles(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GlobalImportOrder []string `json:"global_import_order"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	reports, err := h.s.ImportUntrackedPromptFiles(body.GlobalImportOrder)
	if err != nil {
		writeJSON(w, 409, map[string]any{"error": err.Error(), "imported_before_failure": reports})
		return
	}
	if reports == nil {
		reports = []PromptImportReport{}
	}
	writeJSON(w, 200, reports)
}

// --- drift

func (h *handler) listPromptDrifts(w http.ResponseWriter, r *http.Request) {
	collectionID, _ := strconv.ParseInt(r.URL.Query().Get("collection_id"), 10, 64)
	drifts, err := h.s.ListPromptDrifts(collectionID, r.URL.Query()["status"])
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, drifts)
}

func (h *handler) reconcilePromptDrifts(w http.ResponseWriter, r *http.Request) {
	reconciliation, err := h.s.ReconcilePromptDrifts()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	h.afterPromptDriftReconciliation(reconciliation)
	writeJSON(w, 200, reconciliation)
}

func (h *handler) afterPromptDriftReconciliation(reconciliation *PromptDriftReconciliation) {
	// A refused render leaves a collection's other files behind the sections
	// until someone settles the drift that blocked it; say so where it shows.
	for _, reason := range reconciliation.Refused {
		log.Printf("prompt drift reconcile: %s", reason)
	}
	if h.onPromptDriftsDetected != nil && len(reconciliation.Detected) > 0 {
		h.onPromptDriftsDetected(reconciliation.Detected)
	}
}

func (h *handler) getPromptDrift(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	drift, err := h.s.GetPromptDrift(id)
	if err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	writeJSON(w, 200, drift)
}

func (h *handler) getPromptDriftDiskContent(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	content, err := h.s.GetPromptDriftDiskContent(id)
	if err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write(content)
}

func (h *handler) setPromptDriftAnnotation(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var annotation PromptDriftAnnotation
	if err := decodeStrict(r, &annotation); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	drift, err := h.s.SetPromptDriftAnnotation(id, annotation)
	if err != nil {
		writeStoreErr(w, err, 400)
		return
	}
	writeJSON(w, 200, drift)
}

// applyPromptDrift is the approval. The body is optional; when present it is
// the annotation to apply with, replacing the stored one.
func (h *handler) applyPromptDrift(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var annotation *PromptDriftAnnotation
	if r.ContentLength != 0 {
		annotation = &PromptDriftAnnotation{}
		if err := decodeStrict(r, annotation); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	if err := h.s.ApplyPromptDrift(id, annotation); err != nil {
		writeStoreErr(w, err, 409)
		return
	}
	drift, err := h.s.GetPromptDrift(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	result, err := h.s.RenderPromptCollection(drift.CollectionID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	h.broadcastRenderedFiles(result)
	writeJSON(w, 200, map[string]any{"drift": drift, "render": result})
}

func (h *handler) dismissPromptDrift(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	drift, err := h.s.DismissPromptDrift(id)
	if err != nil {
		writeStoreErr(w, err, 409)
		return
	}
	writeJSON(w, 200, drift)
}

// --- harness deliveries

func (h *handler) listPromptHarnessDeliveries(w http.ResponseWriter, r *http.Request) {
	deliveries, err := h.s.ListPromptHarnessDeliveries()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, deliveries)
}

func (h *handler) setPromptHarnessDelivery(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Delivery           string `json:"delivery"`
		NativeRelativePath string `json:"native_relative_path"`
		Note               string `json:"note"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	delivery, err := h.s.SetPromptHarnessDelivery(PromptHarnessDelivery{Harness: r.PathValue("harness"), Delivery: body.Delivery, NativeRelativePath: body.NativeRelativePath, Note: body.Note})
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, delivery)
}

// listPromptDeliveryOptions serves the vocabularies a UI needs, so none is
// copied into a front end: the delivery kinds, and the file names that can be
// an output or a native file.
func (h *handler) listPromptDeliveryOptions(w http.ResponseWriter, r *http.Request) {
	fileNames := []string{}
	for name := range agentFileNames {
		if isPromptProseFileName(name) {
			fileNames = append(fileNames, name)
		}
	}
	sort.Strings(fileNames)
	directories := []string{}
	for name := range harnessConfigDirectoryNames {
		directories = append(directories, name)
	}
	sort.Strings(directories)
	writeJSON(w, 200, map[string]any{
		"deliveries":                     []string{PromptDeliveryInject, PromptDeliveryNativeFile},
		"prompt_file_names":              fileNames,
		"harness_config_directory_names": directories,
		"drift_statuses":                 []string{PromptDriftStatusOpen, PromptDriftStatusHeld, PromptDriftStatusApplied, PromptDriftStatusDismissed, PromptDriftStatusSuperseded, PromptDriftStatusAlreadyAccounted},
		"tracked_file_ignore_rule_kinds": []string{TrackedFileIgnoreKindPathPattern, TrackedFileIgnoreKindGitWorktree},
		"deepest_section_heading_level":  promptSectionDeepestHeadingLevel,
	})
}

// --- ignore rules

func (h *handler) listTrackedFileIgnoreRules(w http.ResponseWriter, r *http.Request) {
	rules, err := h.s.ListTrackedFileIgnoreRules()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, rules)
}

func (h *handler) createTrackedFileIgnoreRule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind        string `json:"kind"`
		PathPattern string `json:"path_pattern"`
		Reason      string `json:"reason"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rule, err := h.s.CreateTrackedFileIgnoreRule(body.Kind, body.PathPattern, body.Reason)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 201, rule)
}

func (h *handler) setTrackedFileIgnoreRuleEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := h.s.SetTrackedFileIgnoreRuleEnabled(id, enabled); err != nil {
		writeStoreErr(w, err, 500)
		return
	}
	h.listTrackedFileIgnoreRules(w, r)
}

func (h *handler) enableTrackedFileIgnoreRule(w http.ResponseWriter, r *http.Request) {
	h.setTrackedFileIgnoreRuleEnabled(w, r, true)
}

func (h *handler) disableTrackedFileIgnoreRule(w http.ResponseWriter, r *http.Request) {
	h.setTrackedFileIgnoreRuleEnabled(w, r, false)
}

// broadcastRenderedFiles tells the embedding server which files a render
// wrote, so connected runners pick them up.
func (h *handler) broadcastRenderedFiles(result *PromptRenderResult) {
	if h.onFileSaved == nil || result == nil {
		return
	}
	for _, written := range result.WrittenFiles {
		file := written
		if version, err := h.s.LatestVersion(file.ID); err == nil && version != nil {
			h.onFileSaved(&file, version)
		}
	}
}

// retirePromptCollection takes a collection out of use; note is required.
func (h *handler) retirePromptCollection(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if err := decodeStrict(r, &body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	retirement, err := h.s.RetirePromptCollection(id, body.Note)
	switch {
	case errors.Is(err, ErrPromptCollectionRetireRefused), errors.Is(err, ErrPromptCollectionRetired):
		writeErr(w, 409, err.Error())
		return
	case err != nil:
		writeStoreErr(w, err, 500)
		return
	}
	writeJSON(w, 200, retirement)
}
