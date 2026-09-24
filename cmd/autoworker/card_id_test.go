package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// A worker dispatched for a queue card names that card, so llm-bridge-server
// can read its tags and inject the context sections they select. The field
// must survive the JSON the adapter forwards, not only the Go struct.
func TestAWorkerForAQueueCardNamesTheCard(t *testing.T) {
	p := providerSpec{Name: "anthropic", Harness: msg.HarnessClaudeCode, InstanceID: "inst-test"}
	create, err := newWorkerSessionRequest(p, time.Now(), 0, "", "", "card-queued")
	if err != nil {
		t.Fatalf("newWorkerSessionRequest: %v", err)
	}
	encoded, err := json.Marshal(create)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if wire["card_id"] != "card-queued" {
		t.Errorf("card_id on the wire = %v, want card-queued", wire["card_id"])
	}
}

// With an empty queue the worker runs on the default prompt and is for no
// card. Sending a card id then would make the bridge refuse the session.
func TestAWorkerWithNoQueueCardNamesNoCard(t *testing.T) {
	p := providerSpec{Name: "anthropic", Harness: msg.HarnessClaudeCode, InstanceID: "inst-test"}
	create, err := newWorkerSessionRequest(p, time.Now(), 0, "", "", "")
	if err != nil {
		t.Fatalf("newWorkerSessionRequest: %v", err)
	}
	if create.CardID != "" {
		t.Errorf("CardID = %q, want empty", create.CardID)
	}
}
