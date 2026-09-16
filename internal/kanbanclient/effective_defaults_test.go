package kanbanclient

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The body is the one kanban-store's README documents for the route, so a
// drift in either shape shows up here.
const documentedEffectiveDefaults = `{"board_id":"board-1","card_id":"card-1","tags":["cat:product","urgency:high"],
 "matched_rule_ids":["rule-a","rule-b"],
 "defaults":{
   "default_instance_id":{"value":"inst-cc-local","source":{"kind":"tag_rule","rule_id":"rule-a","rule_tags":["cat:product","urgency:high"],"rule_position":0}},
   "default_bundle_id":{"value":"6","source":{"kind":"tag_rule","rule_id":"rule-b","rule_tags":["cat:product"],"rule_position":1}},
   "default_agent_id":{"value":"12","source":{"kind":"board"}}}}`

func effectiveDefaultsServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/boards/board-1/cards/card-1/effective-defaults":
			w.Write([]byte(documentedEffectiveDefaults))
		case "/api/boards/board-1/cards/card-gone/effective-defaults":
			http.Error(w, `{"error":"noteboard item not found: card-gone"}`, http.StatusNotFound)
		case "/api/boards/board-1/cards/card-unreadable/effective-defaults":
			http.Error(w, `{"error":"noteboard read of card card-unreadable failed: connection refused"}`, http.StatusBadGateway)
		case "/api/boards/board-1/cards/card-confused/effective-defaults":
			w.Write([]byte(`{"board_id":"board-1","card_id":"card-other","tags":[],"matched_rule_ids":[],"defaults":{}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func TestACardsEffectiveDefaultsAreReadWithTheirSources(t *testing.T) {
	server := effectiveDefaultsServer(t)
	defer server.Close()

	resolved, err := New(server.URL).GetCardEffectiveDefaults("board-1", "card-1")
	if err != nil {
		t.Fatalf("GetCardEffectiveDefaults: %v", err)
	}
	instance, ok := resolved.Default(DefaultFieldInstance)
	if !ok || instance.Value != "inst-cc-local" || instance.Source.Kind != DefaultSourceKindTagRule || instance.Source.RuleID != "rule-a" {
		t.Errorf("instance = %+v (set %v), want inst-cc-local from rule-a", instance, ok)
	}
	if got := instance.Source.Describe(); got != "tag rule [cat:product + urgency:high] (position 0, id rule-a)" {
		t.Errorf("instance source describes as %q", got)
	}
	agent, ok := resolved.Default(DefaultFieldAgent)
	if !ok || agent.Value != "12" || agent.Source.Describe() != "board" {
		t.Errorf("agent = %+v (set %v), want 12 from the board", agent, ok)
	}
	// Nothing sets the principal, so it is absent — not an empty value.
	if principal, ok := resolved.Default(DefaultFieldPrincipal); ok {
		t.Errorf("an unset default came back as set: %+v", principal)
	}
}

func TestEffectiveDefaultsFailuresAreTold(t *testing.T) {
	server := effectiveDefaultsServer(t)
	defer server.Close()
	client := New(server.URL)

	_, err := client.GetCardEffectiveDefaults("board-1", "card-gone")
	if !errors.Is(err, ErrCardNotFound) || !strings.Contains(err.Error(), "card-gone") {
		t.Errorf("a 404 must be ErrCardNotFound naming the card, got %v", err)
	}
	_, err = client.GetCardEffectiveDefaults("board-1", "card-unreadable")
	if err == nil || errors.Is(err, ErrCardNotFound) || !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("a 502 must be an error carrying the status and body, and not read as a missing card, got %v", err)
	}
	_, err = client.GetCardEffectiveDefaults("board-1", "card-confused")
	if err == nil || !strings.Contains(err.Error(), "card-other") {
		t.Errorf("an answer about another card must be refused, got %v", err)
	}
	if _, err := client.GetCardEffectiveDefaults("", "card-1"); err == nil {
		t.Error("an empty board id must not be requested")
	}
}
