package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/conformance"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// conformanceState holds the last run results, prevents concurrent runs, and
// persists the matrix to disk so it survives server restarts.
type conformanceState struct {
	mu      sync.Mutex
	running bool
	matrix  *msg.ConformanceMatrix
	path    string
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

func (s *Server) handleConformanceGet(w http.ResponseWriter, r *http.Request) {
	s.cfState.mu.Lock()
	matrix := s.cfState.matrix
	running := s.cfState.running
	s.cfState.mu.Unlock()

	if matrix == nil {
		writeJSON(w, map[string]any{"running": running, "matrix": nil})
		return
	}
	writeJSON(w, map[string]any{"running": running, "matrix": matrix})
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

		for _, h := range allHarnesses {
			binPath, available := harness.Available(h)
			if !available {
				continue
			}

			log.Printf("[conformance] testing %s (%s)", h, binPath)
			hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Minute)
			result, err := conformance.RunHarness(hctx, binPath)
			hcancel()
			if err != nil {
				log.Printf("[conformance] %s error: %v", h, err)
				continue
			}

			matrix.Harnesses = append(matrix.Harnesses, toMsgResult(result))
		}

		s.cfState.mu.Lock()
		s.cfState.matrix = matrix
		s.cfState.save()
		s.cfState.mu.Unlock()

		log.Printf("[conformance] run complete: %d harnesses tested", len(matrix.Harnesses))
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
