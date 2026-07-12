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

// runOutcome records the two ways a conformance run can cover less than the
// enabled harness set. Both have to reach the caller: a run that tested nothing
// and a run that tested everything cleanly are otherwise indistinguishable at
// the API, which is exactly how an empty matrix once passed for a clean result.
type runOutcome struct {
	// missingBinaries names enabled harnesses whose wrapper binary was not on
	// PATH, so the run never attempted them.
	missingBinaries []msg.Harness
	// harnessErrors holds "<harness>: <error>" for harnesses that were launched
	// but failed to produce a result.
	harnessErrors []string
}

// commitRun replaces the persisted matrix with the results of a finished run.
//
// A run that produced zero harness results is an execution failure (no harness
// binary was launchable), not a finding that every harness is non-conformant.
// Overwriting the last-good matrix with it would destroy real results and
// report a fabricated one, so the previous matrix is kept and the failure is
// surfaced through lastRunError instead.
func (s *conformanceState) commitRun(matrix *msg.ConformanceMatrix, outcome runOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()

	problems := append([]string(nil), outcome.harnessErrors...)

	if len(matrix.Harnesses) == 0 {
		if len(outcome.missingBinaries) > 0 {
			problems = append(problems, fmt.Sprintf("no wrapper binary on PATH for any enabled harness (%s)",
				joinHarnesses(outcome.missingBinaries)))
		}
		s.lastRunError = strings.Join(problems, "; ")
		log.Printf("[conformance] run produced zero harness results — keeping the last-good matrix (errors: %s)",
			orNone(s.lastRunError))
		return
	}

	if lost := s.resultsDiscardedBy(matrix, outcome.missingBinaries); len(lost) > 0 {
		problems = append(problems, fmt.Sprintf("discarded last-good results for %s: wrapper binary not on PATH",
			joinHarnesses(lost)))
	}

	s.lastRunError = strings.Join(problems, "; ")
	if s.lastRunError != "" {
		log.Printf("[conformance] run completed degraded: %s", s.lastRunError)
	}

	s.matrix = matrix
	s.save()
}

// resultsDiscardedBy names the harnesses whose last-good results this run is
// about to throw away: the previous matrix holds a row for them, this run never
// tested them because their wrapper binary was missing, and committing the new
// matrix drops the row. Not testing a harness says nothing about that harness,
// so losing what it last reported must be visible rather than silent.
func (s *conformanceState) resultsDiscardedBy(fresh *msg.ConformanceMatrix, missingBinaries []msg.Harness) []msg.Harness {
	if s.matrix == nil || len(missingBinaries) == 0 {
		return nil
	}
	tested := make(map[string]bool, len(fresh.Harnesses))
	for _, h := range fresh.Harnesses {
		tested[h.Harness] = true
	}
	hadResults := make(map[string]bool, len(s.matrix.Harnesses))
	for _, h := range s.matrix.Harnesses {
		hadResults[h.Harness] = true
	}

	var discarded []msg.Harness
	for _, h := range missingBinaries {
		// Matrix rows are keyed by the wrapper-binary suffix ("claudecode"),
		// not the canonical harness id ("claude_code").
		key := msg.HarnessShortName(h)
		if hadResults[key] && !tested[key] {
			discarded = append(discarded, h)
		}
	}
	return discarded
}

func joinHarnesses(harnesses []msg.Harness) string {
	names := make([]string, len(harnesses))
	for i, h := range harnesses {
		names[i] = string(h)
	}
	return strings.Join(names, ", ")
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

		targets, outcome := planConformanceRun()

		for _, target := range targets {
			h, binPath := target.harness, target.binary

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
				outcome.harnessErrors = append(outcome.harnessErrors, fmt.Sprintf("%s: %v", h, err))
				continue
			}

			matrix.Harnesses = append(matrix.Harnesses, toMsgResult(result))
		}

		s.cfState.commitRun(matrix, outcome)

		log.Printf("[conformance] run complete: %d harnesses tested, %d failed to run, %d had no wrapper binary on PATH",
			len(matrix.Harnesses), len(outcome.harnessErrors), len(outcome.missingBinaries))
	}()

	writeJSON(w, map[string]any{"status": "started"})
}

// harnessTarget is an enabled harness whose wrapper binary was found on PATH.
type harnessTarget struct {
	harness msg.Harness
	binary  string
}

// planConformanceRun splits the enabled harness set into the harnesses this run
// can actually test and the ones whose wrapper binary is not on PATH.
//
// Wrapper binaries resolve through exec.LookPath, so PATH alone decides what a
// run covers, and a wrong PATH silently reduces the run to nothing. Skipping a
// harness is therefore reportable: it is the mechanism behind an empty run, and
// callers cannot tell an empty run from a clean one unless it is recorded.
func planConformanceRun() (targets []harnessTarget, outcome runOutcome) {
	for _, h := range allHarnesses {
		binPath, available := harness.Available(h)
		if !available {
			outcome.missingBinaries = append(outcome.missingBinaries, h)
			continue
		}
		targets = append(targets, harnessTarget{harness: h, binary: binPath})
	}
	return targets, outcome
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
