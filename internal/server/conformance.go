package server

import (
	"context"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/conformance"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// conformanceState holds the last run results and prevents concurrent runs.
type conformanceState struct {
	mu      sync.Mutex
	running bool
	matrix  *msg.ConformanceMatrix
}

var cfState conformanceState

func (s *Server) handleConformanceGet(w http.ResponseWriter, r *http.Request) {
	cfState.mu.Lock()
	matrix := cfState.matrix
	running := cfState.running
	cfState.mu.Unlock()

	if matrix == nil {
		writeJSON(w, map[string]any{"running": running, "matrix": nil})
		return
	}
	writeJSON(w, map[string]any{"running": running, "matrix": matrix})
}

func (s *Server) handleConformanceRun(w http.ResponseWriter, r *http.Request) {
	cfState.mu.Lock()
	if cfState.running {
		cfState.mu.Unlock()
		http.Error(w, "conformance run already in progress", http.StatusConflict)
		return
	}
	cfState.running = true
	cfState.mu.Unlock()

	// Run in background so the HTTP request returns immediately.
	go func() {
		defer func() {
			cfState.mu.Lock()
			cfState.running = false
			cfState.mu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		matrix := &msg.ConformanceMatrix{
			GeneratedAt: time.Now(),
		}

		for _, h := range allHarnesses {
			binPath, available := harness.Available(h)
			if !available {
				continue
			}

			log.Printf("[conformance] testing %s (%s)", h, binPath)
			result, err := conformance.RunHarness(ctx, binPath)
			if err != nil {
				log.Printf("[conformance] %s error: %v", h, err)
				continue
			}

			matrix.Harnesses = append(matrix.Harnesses, toMsgResult(result))
		}

		cfState.mu.Lock()
		cfState.matrix = matrix
		cfState.mu.Unlock()

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
