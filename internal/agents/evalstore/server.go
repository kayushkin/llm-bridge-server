package evalstore

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
)

// Bridge is what the server needs from llm-bridge-server besides model
// calls: reading operations to import and to count. Nil when no bridge is
// configured; the routes that need it then say so.
type Bridge interface {
	OperationSource
	OperationLister
}

// Server serves the HTTP API. Routes are rooted at /, as the other stores'.
type Server struct {
	store  *Store
	runner *Runner
	bridge Bridge
}

// RegisterHandlers mounts the API on mux.
func RegisterHandlers(mux *http.ServeMux, store *Store, runner *Runner, bridge Bridge) {
	s := &Server{store: store, runner: runner, bridge: bridge}
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /vocabulary", s.vocabulary)

	mux.HandleFunc("GET /suites", s.listSuites)
	mux.HandleFunc("POST /suites", s.createSuite)
	mux.HandleFunc("GET /suites/{id}", s.getSuite)
	mux.HandleFunc("PATCH /suites/{id}", s.patchSuite)
	mux.HandleFunc("DELETE /suites/{id}", s.deleteSuite)
	mux.HandleFunc("GET /suites/{id}/cases", s.listCases)
	mux.HandleFunc("POST /suites/{id}/cases", s.createCase)
	mux.HandleFunc("POST /suites/{id}/cases/import-operations", s.importOperations)
	mux.HandleFunc("GET /cases/{id}", s.getCase)
	mux.HandleFunc("PATCH /cases/{id}", s.patchCase)

	mux.HandleFunc("GET /runs", s.listRuns)
	mux.HandleFunc("POST /runs", s.createRun)
	mux.HandleFunc("GET /runs/{id}", s.getRun)
	mux.HandleFunc("PATCH /runs/{id}", s.patchRun)
	mux.HandleFunc("DELETE /runs/{id}", s.deleteRun)
	mux.HandleFunc("POST /runs/{id}/start", s.startRun)
	mux.HandleFunc("POST /runs/{id}/resume", s.resumeRun)
	mux.HandleFunc("POST /runs/{id}/stop", s.stopRun)
	mux.HandleFunc("GET /runs/{id}/answers", s.listAnswers)
	mux.HandleFunc("GET /runs/{id}/verdicts", s.listVerdicts)
	mux.HandleFunc("GET /runs/{id}/checks", s.listChecks)
	mux.HandleFunc("POST /runs/{id}/checks", s.recheck)
	mux.HandleFunc("GET /runs/{id}/report", s.report)
	mux.HandleFunc("GET /runs/{id}/cost-projection", s.costProjection)
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	counts := map[string]int{}
	for _, table := range []string{"suites", "cases", "runs", "answers", "verdicts"} {
		var n int
		if err := s.store.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			writeStoreError(w, err)
			return
		}
		counts[table] = n
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "counts": counts})
}

func (s *Server) vocabulary(w http.ResponseWriter, r *http.Request) {
	checkerDescriptions := map[string]string{}
	for name, checker := range checkers {
		checkerDescriptions[name] = checker.Description()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"run_statuses":           RunStatuses,
		"source_kinds":           sourceKinds,
		"checkers":               checkerDescriptions,
		"case_selection_modes":   []string{CaseSelectionAll, CaseSelectionSample},
		"answer_origins":         []string{AnswerOriginReplayed, AnswerOriginCaseOriginal, AnswerOriginLegacyImport},
		"weekly_calls_sources":   []string{WeeklyCallsSourceFixed, WeeklyCallsSourceLLMBridgeOperations},
		"required_failure_kinds": []string{FailureKindNone, FailureKindNoAnswer},
	})
}

func (s *Server) listSuites(w http.ResponseWriter, r *http.Request) {
	suites, err := s.store.ListSuites()
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"suites": suites})
}

func (s *Server) createSuite(w http.ResponseWriter, r *http.Request) {
	var suite Suite
	if !decode(w, r, &suite) {
		return
	}
	created, err := s.store.CreateSuite(&suite)
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) getSuite(w http.ResponseWriter, r *http.Request) {
	suite, err := s.store.GetSuite(r.PathValue("id"))
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, suite)
}

func (s *Server) patchSuite(w http.ResponseWriter, r *http.Request) {
	var patch SuitePatch
	if !decode(w, r, &patch) {
		return
	}
	suite, err := s.store.PatchSuite(r.PathValue("id"), patch)
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, suite)
}

func (s *Server) deleteSuite(w http.ResponseWriter, r *http.Request) {
	if writeStoreError(w, s.store.DeleteSuite(r.PathValue("id"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("id")})
}

func (s *Server) listCases(w http.ResponseWriter, r *http.Request) {
	cases, err := s.store.ListCases(r.PathValue("id"))
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"cases": cases})
}

func (s *Server) createCase(w http.ResponseWriter, r *http.Request) {
	var c Case
	if !decode(w, r, &c) {
		return
	}
	created, err := s.store.CreateCase(r.PathValue("id"), &c)
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

func (s *Server) importOperations(w http.ResponseWriter, r *http.Request) {
	var request ImportOperationsRequest
	if !decode(w, r, &request) {
		return
	}
	var source OperationSource
	if s.bridge != nil {
		source = s.bridge
	}
	result, err := s.store.ImportOperations(r.Context(), r.PathValue("id"), request, source)
	if errors.Is(err, ErrIntentRouteMissing) {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) patchCase(w http.ResponseWriter, r *http.Request) {
	var patch CasePatch
	if !decode(w, r, &patch) {
		return
	}
	c, err := s.store.PatchCase(r.PathValue("id"), patch)
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) getCase(w http.ResponseWriter, r *http.Request) {
	c, err := s.store.GetCase(r.PathValue("id"))
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := s.store.ListRuns(r.URL.Query().Get("suite_id"))
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var request NewRun
	if !decode(w, r, &request) {
		return
	}
	run, err := s.runner.CreateRun(r.Context(), request)
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// runWithProgress is a run and how far it has got.
type runWithProgress struct {
	*Run
	Progress RunProgress `json:"progress"`
}

func (s *Server) writeRun(w http.ResponseWriter, status int, run *Run) {
	progress, err := s.store.RunProgress(run.ID)
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, status, runWithProgress{Run: run, Progress: progress})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.PathValue("id"))
	if writeStoreError(w, err) {
		return
	}
	s.writeRun(w, http.StatusOK, run)
}

func (s *Server) patchRun(w http.ResponseWriter, r *http.Request) {
	var patch RunPatch
	if !decode(w, r, &patch) {
		return
	}
	run, err := s.store.PatchRun(r.PathValue("id"), patch)
	if writeStoreError(w, err) {
		return
	}
	s.writeRun(w, http.StatusOK, run)
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	if writeStoreError(w, s.store.DeleteRun(r.PathValue("id"))) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": r.PathValue("id")})
}

func (s *Server) startRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.store.GetRun(r.PathValue("id"))
	if writeStoreError(w, err) {
		return
	}
	if run.Status != RunStatusCreated {
		writeError(w, http.StatusConflict, "run "+run.ID+" is "+run.Status+"; use POST /runs/{id}/resume")
		return
	}
	run, err = s.runner.StartRun(run.ID, false)
	if writeStoreError(w, err) {
		return
	}
	s.writeRun(w, http.StatusAccepted, run)
}

func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RetryFailed bool `json:"retry_failed"`
	}
	if r.ContentLength != 0 && !decode(w, r, &body) {
		return
	}
	run, err := s.runner.StartRun(r.PathValue("id"), body.RetryFailed)
	if writeStoreError(w, err) {
		return
	}
	s.writeRun(w, http.StatusAccepted, run)
}

func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.runner.StopRun(r.PathValue("id"))
	if writeStoreError(w, err) {
		return
	}
	s.writeRun(w, http.StatusAccepted, run)
}

// includeBodies says whether a list should carry answer bodies. Answers to
// real prompts quote real mail, so they come only when asked for.
func includeBodies(r *http.Request) bool {
	return r.URL.Query().Get("include_bodies") == "true"
}

func (s *Server) listAnswers(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetRun(r.PathValue("id")); writeStoreError(w, err) {
		return
	}
	answers, err := s.store.ListAnswers(r.PathValue("id"), AnswerFilter{CaseID: r.URL.Query().Get("case_id"), ModelID: r.URL.Query().Get("model_id")})
	if writeStoreError(w, err) {
		return
	}
	if !includeBodies(r) {
		for _, a := range answers {
			a.Parsed = nil
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"answers": answers})
}

func (s *Server) listVerdicts(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetRun(r.PathValue("id")); writeStoreError(w, err) {
		return
	}
	verdicts, err := s.store.ListVerdicts(r.PathValue("id"), VerdictFilter{CaseID: r.URL.Query().Get("case_id"),
		JudgeModelID: r.URL.Query().Get("judge_model_id")})
	if writeStoreError(w, err) {
		return
	}
	if !includeBodies(r) {
		for _, v := range verdicts {
			v.Parsed = nil
			for i := range v.Candidates {
				v.Candidates[i].Reason = ""
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"verdicts": verdicts})
}

func (s *Server) listChecks(w http.ResponseWriter, r *http.Request) {
	if _, err := s.store.GetRun(r.PathValue("id")); writeStoreError(w, err) {
		return
	}
	checks, err := s.store.ListChecks(r.PathValue("id"))
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": checks})
}

func (s *Server) recheck(w http.ResponseWriter, r *http.Request) {
	if writeStoreError(w, s.store.CheckRun(r.PathValue("id"))) {
		return
	}
	s.listChecks(w, r)
}

func (s *Server) report(w http.ResponseWriter, r *http.Request) {
	options := ReportOptions{}
	if raw := r.URL.Query().Get("calls_per_week"); raw != "" {
		request, err := parseProjectionRequest(map[string][]string{"calls_per_week": {raw}}, CostModel{})
		if writeStoreError(w, err) {
			return
		}
		options.CallsPerWeek = request.CallsPerWeek
	}
	var lister OperationLister
	if s.bridge != nil {
		lister = s.bridge
	}
	report, err := s.store.BuildReport(r.Context(), r.PathValue("id"), options, lister)
	if writeStoreError(w, err) {
		return
	}
	switch r.URL.Query().Get("format") {
	case "", "json":
		writeJSON(w, http.StatusOK, report)
	case "markdown":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Write([]byte(report.Markdown()))
	default:
		writeError(w, http.StatusBadRequest, "format must be json or markdown")
	}
}

func (s *Server) costProjection(w http.ResponseWriter, r *http.Request) {
	var lister OperationLister
	if s.bridge != nil {
		lister = s.bridge
	}
	projection, err := s.store.BuildCostProjection(r.Context(), r.PathValue("id"), r.URL.Query(), lister)
	if writeStoreError(w, err) {
		return
	}
	writeJSON(w, http.StatusOK, projection)
}

// decode reads a JSON body and refuses unknown fields, so a misspelled key
// is a 400 rather than a write that drops it.
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "bad json: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("encode response: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(message)})
}

func writeStoreError(w http.ResponseWriter, err error) bool {
	if err == nil {
		return false
	}
	switch {
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrInvalid):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	default:
		log.Printf("request failed: %v", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
	return true
}
