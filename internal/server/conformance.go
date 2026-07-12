package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/conformance"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// conformanceState holds the last run results, prevents concurrent runs, and
// persists the matrix to disk so it survives server restarts.
type conformanceState struct {
	mu           sync.Mutex
	running      bool
	matrix       *msg.ConformanceMatrix
	lastRunError string
	path         string
}

func newConformanceState(path string) *conformanceState {
	s := &conformanceState{path: path}
	s.load()
	return s
}

func (s *conformanceState) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("conformance load error: %v", err)
		}
		return
	}
	var matrix msg.ConformanceMatrix
	if err := json.Unmarshal(data, &matrix); err != nil {
		log.Printf("conformance parse error: %v", err)
		return
	}
	s.matrix = &matrix
	log.Printf("[conformance] loaded cached matrix from %s (%d harnesses, generated %s)",
		s.path, len(matrix.Harnesses), matrix.GeneratedAt.Format(time.RFC3339))
}

func (s *conformanceState) save() {
	if s.path == "" || s.matrix == nil {
		return
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("conformance mkdir error: %v", err)
		return
	}
	data, err := json.MarshalIndent(s.matrix, "", "  ")
	if err != nil {
		log.Printf("conformance marshal error: %v", err)
		return
	}
	if err := os.WriteFile(s.path, data, 0644); err != nil {
		log.Printf("conformance write error: %v", err)
	}
}

// commitRun replaces the persisted matrix with the results of a finished run.
//
// A run that produced zero harness results is an execution failure (no harness
// binary was launchable), not a finding that every harness is non-conformant.
// Overwriting the last-good matrix with it would destroy real results and
// report a fabricated one, so the previous matrix is kept and the failure is
// surfaced through lastRunError instead.
func (s *conformanceState) commitRun(matrix *msg.ConformanceMatrix, harnessRunErrors []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lastRunError = strings.Join(harnessRunErrors, "; ")

	if len(matrix.Harnesses) == 0 {
		log.Printf("[conformance] run produced zero harness results — keeping the last-good matrix (errors: %s)",
			orNone(s.lastRunError))
		return
	}
	if len(harnessRunErrors) > 0 {
		log.Printf("[conformance] run completed with %d harness error(s): %s",
			len(harnessRunErrors), s.lastRunError)
	}

	s.matrix = matrix
	s.save()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (s *Server) handleConformanceGet(w http.ResponseWriter, r *http.Request) {
	s.cfState.mu.Lock()
	matrix := s.cfState.matrix
	running := s.cfState.running
	lastRunError := s.cfState.lastRunError
	s.cfState.mu.Unlock()

	// matrix may be nil (never run, nothing persisted). last_run_error is what
	// distinguishes that from a run that executed but failed outright.
	writeJSON(w, map[string]any{
		"running":        running,
		"matrix":         matrix,
		"last_run_error": lastRunError,
	})
}

func (s *Server) handleConformanceRun(w http.ResponseWriter, r *http.Request) {
	s.cfState.mu.Lock()
	if s.cfState.running {
		s.cfState.mu.Unlock()
		http.Error(w, "conformance run already in progress", http.StatusConflict)
		return
	}
	s.cfState.running = true
	s.cfState.mu.Unlock()

	// Run in background so the HTTP request returns immediately.
	go func() {
		defer func() {
			s.cfState.mu.Lock()
			s.cfState.running = false
			s.cfState.mu.Unlock()
		}()

		matrix := &msg.ConformanceMatrix{
			GeneratedAt: time.Now(),
		}
		var harnessRunErrors []string

		for _, h := range allHarnesses {
			binPath, available := harness.Available(h)
			if !available {
				continue
			}

			log.Printf("[conformance] testing %s (%s)", h, binPath)
			// 22 features at up to 30s each = 11 min worst case; budget
			// 15 min so a slow LLM round-trip near the end of the suite
			// doesn't abort the harness mid-run with a context deadline.
			hctx, hcancel := context.WithTimeout(context.Background(), 15*time.Minute)
			result, err := conformance.RunHarness(hctx, binPath)
			hcancel()
			if err != nil {
				// The harness never ran, so it has no results to report. Keep
				// the error rather than dropping it — a run that quietly tested
				// nothing must not read as a clean run.
				log.Printf("[conformance] %s error: %v", h, err)
				harnessRunErrors = append(harnessRunErrors, fmt.Sprintf("%s: %v", h, err))
				continue
			}

			matrix.Harnesses = append(matrix.Harnesses, toMsgResult(result))
		}

		s.cfState.commitRun(matrix, harnessRunErrors)

		log.Printf("[conformance] run complete: %d harnesses tested, %d failed to run",
			len(matrix.Harnesses), len(harnessRunErrors))
	}()

	writeJSON(w, map[string]any{"status": "started"})
}

func toMsgResult(hr *conformance.HarnessResult) msg.ConformanceHarnessResult {
	var results []msg.ConformanceTestResult
	for _, r := range hr.Results {
		results = append(results, msg.ConformanceTestResult{
			Feature:  msg.ConformanceFeature(r.Feature),
			Passed:   r.Passed,
			Skipped:  r.Skipped,
			Error:    r.Error,
			Duration: r.Duration,
		})
	}
	return msg.ConformanceHarnessResult{
		Harness:  hr.Harness,
		Binary:   hr.Binary,
		TestedAt: hr.TestedAt,
		Results:  results,
		Summary: msg.ConformanceSummary{
			Total:   hr.Summary.Total,
			Passed:  hr.Summary.Passed,
			Failed:  hr.Summary.Failed,
			Skipped: hr.Summary.Skipped,
		},
	}
}
