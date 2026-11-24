package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestAggregatesCarryCacheTokensAndAPICallUsagePerQuerySource(t *testing.T) {
	srv, s := newTestServer(t)
	store := func(sessionID, eventType, data string) {
		t.Helper()
		if _, err := s.StoreEvent(context.Background(), sessionID, eventType, []byte(data)); err != nil {
			t.Fatalf("StoreEvent: %v", err)
		}
	}
	store("with-subagents", "result", `{"result":{"usage":{"input_tokens":4,"output_tokens":99,"cache_read_tokens":777170,"cache_write_tokens":527},"model":"test-model"}}`)
	store("with-subagents", "api_call", `{"api_call":{"model":"test-model","input_tokens":4,"output_tokens":99,"cache_read_tokens":777170,"cache_creation_tokens":527,"query_source":"sdk"}}`)
	store("with-subagents", "api_call", `{"api_call":{"model":"test-model","input_tokens":2,"output_tokens":212,"cache_read_tokens":558700,"cache_creation_tokens":270,"query_source":"agent:builtin:general-purpose"}}`)
	store("plain", "result", `{"result":{"usage":{"input_tokens":10,"output_tokens":20},"model":"test-model"}}`)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/aggregates", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var aggregates []msg.SessionAggregate
	if err := json.Unmarshal(rec.Body.Bytes(), &aggregates); err != nil {
		t.Fatalf("decode: %v", err)
	}
	bySession := map[string]msg.SessionAggregate{}
	for _, a := range aggregates {
		bySession[a.SessionID] = a
	}

	a := bySession["with-subagents"]
	if a.InputTokens != 4 || a.OutputTokens != 99 || a.CacheReadTokens != 777170 || a.CacheWriteTokens != 527 {
		t.Errorf("result-event totals = %+v", a)
	}
	want := []msg.SessionAPICallUsage{
		{QuerySource: "agent:builtin:general-purpose", Calls: 1, InputTokens: 2, OutputTokens: 212, CacheReadTokens: 558700, CacheWriteTokens: 270},
		{QuerySource: "sdk", Calls: 1, InputTokens: 4, OutputTokens: 99, CacheReadTokens: 777170, CacheWriteTokens: 527},
	}
	if len(a.APICallUsage) != len(want) {
		t.Fatalf("api_call_usage = %+v, want %+v", a.APICallUsage, want)
	}
	for i := range want {
		if a.APICallUsage[i] != want[i] {
			t.Errorf("api_call_usage[%d] = %+v, want %+v", i, a.APICallUsage[i], want[i])
		}
	}

	// A session with no cache tokens and no api_call events carries neither
	// field: the answer lists every session and most are like this one.
	for _, line := range strings.SplitAfter(rec.Body.String(), "}") {
		if strings.Contains(line, `"session_id":"plain"`) &&
			(strings.Contains(line, "cache_read_tokens") || strings.Contains(line, "api_call_usage")) {
			t.Errorf("the plain session's row carries empty usage fields: %s", line)
		}
	}
	if p := bySession["plain"]; p.InputTokens != 10 || p.OutputTokens != 20 {
		t.Errorf("plain session = %+v, want input 10 output 20", p)
	}
}

func TestAggregatesSinceListWhatASessionDidInTheWindow(t *testing.T) {
	srv, s := newTestServer(t)
	store := func(sessionID, eventType, data string) {
		t.Helper()
		if _, err := s.StoreEvent(context.Background(), sessionID, eventType, []byte(data)); err != nil {
			t.Fatalf("StoreEvent: %v", err)
		}
	}
	store("active", "user_message", `{}`)
	store("active", "api_call", `{"api_call":{"model":"test-model","input_tokens":2,"output_tokens":212,"cache_read_tokens":558700,"cache_creation_tokens":270,"query_source":"agent:builtin:general-purpose"}}`)
	store("active", "session_cost", `{"session_cost":{"total_usd":1.25}}`)

	get := func(query string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/aggregates"+query, nil))
		return rec
	}

	rec := get("?since=" + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var aggregates []msg.SessionAggregate
	if err := json.Unmarshal(rec.Body.Bytes(), &aggregates); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(aggregates) != 1 || aggregates[0].SessionID != "active" {
		t.Fatalf("aggregates = %+v, want the one active session", aggregates)
	}
	a := aggregates[0]
	if a.Turns != 1 || a.SessionCostUSD != 1.25 || len(a.APICallUsage) != 1 || a.APICallUsage[0].CacheReadTokens != 558700 {
		t.Errorf("windowed aggregate = %+v, want 1 turn, cost 1.25 and the subagent's call", a)
	}

	// A window that begins after everything is an empty list, not null.
	rec = get("?since=" + time.Now().Add(3*time.Hour).UTC().Format(time.RFC3339))
	if body := strings.TrimSpace(rec.Body.String()); rec.Code != http.StatusOK || body != "[]" {
		t.Errorf("future window = %d %q, want 200 []", rec.Code, body)
	}

	// The whole-life read carries no cost.
	rec = get("")
	if strings.Contains(rec.Body.String(), "session_cost_usd") {
		t.Errorf("whole-life read carries a cost: %s", rec.Body.String())
	}

	if rec := get("?since=yesterday"); rec.Code != http.StatusBadRequest {
		t.Errorf("since=yesterday answered %d, want 400", rec.Code)
	}
}
