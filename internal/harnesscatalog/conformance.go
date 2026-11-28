package harnesscatalog

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

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"
	"github.com/kayushkin/llm-bridge-server/internal/textutil"

	"github.com/kayushkin/llm-bridge-server/conformance"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// ConformanceState holds the last run results, prevents concurrent runs, and
// persists the matrix to disk so it survives server restarts.
type ConformanceState struct {
	mu           sync.Mutex
	running      bool
	Matrix       *msg.ConformanceMatrix
	LastRunError string
	Path         string
}

func NewConformanceState(path string) *ConformanceState {
	s := &ConformanceState{Path: path}
	s.Load()
	return s
}

func (s *ConformanceState) Load() {
	if s.Path == "" {
		return
	}
	data, err := os.ReadFile(s.Path)
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
	s.Matrix = &matrix
	log.Printf("[conformance] loaded cached matrix from %s (%d harnesses, generated %s)",
		s.Path, len(matrix.Harnesses), matrix.GeneratedAt.Format(time.RFC3339))
}

func (s *ConformanceState) Save() {
	if s.Path == "" || s.Matrix == nil {
		return
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Printf("conformance mkdir error: %v", err)
		return
	}
	data, err := json.MarshalIndent(s.Matrix, "", "  ")
	if err != nil {
		log.Printf("conformance marshal error: %v", err)
		return
	}
	if err := os.WriteFile(s.Path, data, 0644); err != nil {
		log.Printf("conformance write error: %v", err)
	}
}

// RunOutcome records the two ways a conformance run can cover less than the
// enabled harness set. Both have to reach the caller: a run that tested nothing
// and a run that tested everything cleanly are otherwise indistinguishable at
// the API, which is exactly how an empty matrix once passed for a clean result.
type RunOutcome struct {
	// MissingBinaries names enabled harnesses whose wrapper binary was not on
	// PATH, so the run never attempted them.
	MissingBinaries []msg.Harness
	// HarnessErrors holds "<harness>: <error>" for harnesses that were launched
	// but failed to produce a result.
	HarnessErrors []string
}

// CommitRun replaces the persisted matrix with the results of a finished run.
//
// A run that produced zero harness results is an execution failure (no harness
// binary was launchable), not a finding that every harness is non-conformant.
// Overwriting the last-good matrix with it would destroy real results and
// report a fabricated one, so the previous matrix is kept and the failure is
// surfaced through lastRunError instead.
func (s *ConformanceState) CommitRun(matrix *msg.ConformanceMatrix, outcome RunOutcome) {
	s.mu.Lock()
	defer s.mu.Unlock()

	problems := append([]string(nil), outcome.HarnessErrors...)

	if len(matrix.Harnesses) == 0 {
		if len(outcome.MissingBinaries) > 0 {
			problems = append(problems, fmt.Sprintf("no wrapper binary on PATH for any enabled harness (%s)",
				joinHarnesses(outcome.MissingBinaries)))
		}
		s.LastRunError = strings.Join(problems, "; ")
		log.Printf("[conformance] run produced zero harness results — keeping the last-good matrix (errors: %s)",
			textutil.OrNone(s.LastRunError))
		return
	}

	if lost := s.resultsDiscardedBy(matrix, outcome.MissingBinaries); len(lost) > 0 {
		problems = append(problems, fmt.Sprintf("discarded last-good results for %s: wrapper binary not on PATH",
			joinHarnesses(lost)))
	}

	s.LastRunError = strings.Join(problems, "; ")
	if s.LastRunError != "" {
		log.Printf("[conformance] run completed degraded: %s", s.LastRunError)
	}

	s.Matrix = matrix
	s.Save()
}

// resultsDiscardedBy names the harnesses whose last-good results this run is
// about to throw away: the previous matrix holds a row for them, this run never
// tested them because their wrapper binary was missing, and committing the new
// matrix drops the row. Not testing a harness says nothing about that harness,
// so losing what it last reported must be visible rather than silent.
func (s *ConformanceState) resultsDiscardedBy(fresh *msg.ConformanceMatrix, missingBinaries []msg.Harness) []msg.Harness {
	if s.Matrix == nil || len(missingBinaries) == 0 {
		return nil
	}
	tested := make(map[string]bool, len(fresh.Harnesses))
	for _, h := range fresh.Harnesses {
		tested[h.Harness] = true
	}
	hadResults := make(map[string]bool, len(s.Matrix.Harnesses))
	for _, h := range s.Matrix.Harnesses {
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

func (s *Service) HandleConformanceGet(w http.ResponseWriter, r *http.Request) {
	s.CfState.mu.Lock()
	matrix := s.CfState.Matrix
	running := s.CfState.running
	lastRunError := s.CfState.LastRunError
	s.CfState.mu.Unlock()

	// matrix may be nil (never run, nothing persisted). last_run_error is what
	// distinguishes that from a run that executed but failed outright.
	httpjson.WriteJSON(w, map[string]any{
		"running":        running,
		"matrix":         matrix,
		"last_run_error": lastRunError,
	})
}

func (s *Service) HandleConformanceRun(w http.ResponseWriter, r *http.Request) {
	s.CfState.mu.Lock()
	if s.CfState.running {
		s.CfState.mu.Unlock()
		http.Error(w, "conformance run already in progress", http.StatusConflict)
		return
	}
	s.CfState.running = true
	s.CfState.mu.Unlock()

	// Run in background so the HTTP request returns immediately.
	go func() {
		defer func() {
			s.CfState.mu.Lock()
			s.CfState.running = false
			s.CfState.mu.Unlock()
		}()

		matrix := &msg.ConformanceMatrix{
			GeneratedAt: time.Now(),
		}

		targets, outcome := PlanConformanceRun()

		for _, target := range targets {
			h, binPath := target.Harness, target.Binary

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
				outcome.HarnessErrors = append(outcome.HarnessErrors, fmt.Sprintf("%s: %v", h, err))
				continue
			}

			matrix.Harnesses = append(matrix.Harnesses, *result)
		}

		s.CfState.CommitRun(matrix, outcome)

		log.Printf("[conformance] run complete: %d harnesses tested, %d failed to run, %d had no wrapper binary on PATH",
			len(matrix.Harnesses), len(outcome.HarnessErrors), len(outcome.MissingBinaries))
	}()

	httpjson.WriteJSON(w, map[string]any{"status": "started"})
}

// harnessTarget is an enabled harness whose wrapper binary was found on PATH.
type harnessTarget struct {
	Harness msg.Harness
	Binary  string
}

// PlanConformanceRun splits the enabled harness set into the harnesses this run
// can actually test and the ones whose wrapper binary is not on PATH.
//
// Wrapper binaries resolve through exec.LookPath, so PATH alone decides what a
// run covers, and a wrong PATH silently reduces the run to nothing. Skipping a
// harness is therefore reportable: it is the mechanism behind an empty run, and
// callers cannot tell an empty run from a clean one unless it is recorded.
func PlanConformanceRun() (targets []harnessTarget, outcome RunOutcome) {
	for _, h := range AllHarnesses {
		binPath, available := harness.Available(h)
		if !available {
			outcome.MissingBinaries = append(outcome.MissingBinaries, h)
			continue
		}
		targets = append(targets, harnessTarget{Harness: h, Binary: binPath})
	}
	return targets, outcome
}
