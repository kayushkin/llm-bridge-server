package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// serveOneshot points classify at a stub llm-bridge-server whose oneshot
// endpoint returns the given msg.OneShotResponse-shaped body.
func serveOneshot(t *testing.T, body any) config {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return config{ModelRole: "efficient", BridgeURL: srv.URL}
}

// A reply that stopped at max_tokens is incomplete however cleanly it parses:
// the cards it left out are indistinguishable from "no opinion". stop_reason
// is the only evidence, and classify must refuse the report on it. The check
// lives in the oneshot client now, but the behavior is this command's
// contract, so it is pinned here.
func TestClassifyRejectsTruncatedReport(t *testing.T) {
	cfg := serveOneshot(t, map[string]any{
		"stop_reason": "max_tokens",
		"usage":       map[string]any{"output_tokens": maxOutputTokens},
		"parsed": map[string]any{
			"trackable":       true,
			"session_summary": "partial",
		},
	})

	rep, err := classify(context.Background(), cfg, "digest")
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
	cfg := serveOneshot(t, map[string]any{
		"stop_reason": "end_turn",
		"usage":       map[string]any{"output_tokens": 40},
		"parsed": map[string]any{
			"trackable":       true,
			"session_summary": "complete",
			"new_tasks": []any{
				map[string]any{"title": "t", "state": "in_progress", "summary": "s"},
			},
		},
	})

	rep, err := classify(context.Background(), cfg, "digest")
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
	cfg := serveOneshot(t, map[string]any{
		"stop_reason": "max_tokens",
		"usage":       map[string]any{"output_tokens": maxOutputTokens},
		"parsed": map[string]any{
			"trackable": true,
			"existing_cards": []any{
				map[string]any{"card_id": "c1", "state": "done", "summary": "s"},
			},
		},
	})

	if _, err := classify(context.Background(), cfg, "digest"); !errors.Is(err, errTruncatedReport) {
		t.Fatalf("want errTruncatedReport, got %v", err)
	}
}
