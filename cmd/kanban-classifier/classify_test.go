package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveAnthropic points classify at a stub returning the given response body.
func serveAnthropic(t *testing.T, body any) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)

	prev := anthropicMessagesURL
	anthropicMessagesURL = srv.URL
	t.Cleanup(func() { anthropicMessagesURL = prev })
}

// A reply cut off at max_tokens still carries a well-formed tool_use block —
// the API drops the unfinished array rather than emitting broken JSON — so it
// decodes without error and is indistinguishable from a model that had no
// opinion on any card. stop_reason is the only evidence, and classify must
// refuse the report on it. Verified against the live API on session
// br_1784416900461396845 (69 cards): max_tokens=1024 returns exactly this
// shape.
func TestClassifyRejectsTruncatedReport(t *testing.T) {
	serveAnthropic(t, map[string]any{
		"stop_reason": "max_tokens",
		"usage":       map[string]any{"output_tokens": 1024},
		"content": []any{
			map[string]any{
				"type": "tool_use",
				"name": reportToolName,
				// Parses cleanly. existing_cards is simply absent.
				"input": map[string]any{
					"trackable":       true,
					"session_summary": "partial",
				},
			},
		},
	})

	rep, err := classify(context.Background(), config{Model: defaultModel}, "digest")
	if !errors.Is(err, errTruncatedReport) {
		t.Fatalf("want errTruncatedReport, got err=%v rep=%+v", err, rep)
	}
	if rep != nil {
		t.Fatalf("a truncated report must not be handed to the caller, got %+v", rep)
	}
}

// The guard must key on stop_reason alone. A complete reply that happens to
// carry no existing_cards is a real classification and has to pass through —
// otherwise every genuinely-empty session becomes an error.
func TestClassifyAcceptsCompleteReportWithNoExistingCards(t *testing.T) {
	serveAnthropic(t, map[string]any{
		"stop_reason": "tool_use",
		"usage":       map[string]any{"output_tokens": 40},
		"content": []any{
			map[string]any{
				"type": "tool_use",
				"name": reportToolName,
				"input": map[string]any{
					"trackable":       true,
					"session_summary": "complete",
					"new_tasks": []any{
						map[string]any{"title": "t", "state": "in_progress", "summary": "s"},
					},
				},
			},
		},
	})

	rep, err := classify(context.Background(), config{Model: defaultModel}, "digest")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rep.ExistingCards) != 0 || len(rep.NewTasks) != 1 {
		t.Fatalf("existing=%d new=%d, want 0/1", len(rep.ExistingCards), len(rep.NewTasks))
	}
}

// A truncated reply that still managed to emit some existing_cards is just as
// unusable as one that emitted none: the cards it left out are silently
// treated as "no opinion". Refuse on stop_reason, not on emptiness.
func TestClassifyRejectsTruncatedReportThatCarriesSomeCards(t *testing.T) {
	serveAnthropic(t, map[string]any{
		"stop_reason": "max_tokens",
		"usage":       map[string]any{"output_tokens": 1024},
		"content": []any{
			map[string]any{
				"type": "tool_use",
				"name": reportToolName,
				"input": map[string]any{
					"trackable": true,
					"existing_cards": []any{
						map[string]any{"card_id": "c1", "state": "done", "summary": "s"},
					},
				},
			},
		},
	})

	if _, err := classify(context.Background(), config{Model: defaultModel}, "digest"); !errors.Is(err, errTruncatedReport) {
		t.Fatalf("want errTruncatedReport, got %v", err)
	}
}
