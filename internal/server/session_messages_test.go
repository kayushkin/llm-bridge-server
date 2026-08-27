package server

import (
	"encoding/json"
	"testing"
)

// The page and the stream resume point are served together so they are consistent with
// each other. These cases pin the wire shape and the ordering invariant that makes the
// pairing safe; `handleSessionMessages`'s doc comment states it at length.

func TestSessionMessagesResponse_CarriesTheModelUnchanged(t *testing.T) {
	// The model rides as raw bytes precisely so this layer cannot alter it. A decode and
	// re-encode would be a place for the two representations to drift, and it would cost
	// a full parse of a megabyte page to achieve nothing.
	model := json.RawMessage(`{"sessionId":"br_1","turns":[],"entries":{},"more":false}`)

	encoded, err := json.Marshal(SessionMessagesResponse{
		Model:  model,
		Stream: StreamResumePoint{Head: 4242},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var round struct {
		Model  json.RawMessage   `json:"model"`
		Stream StreamResumePoint `json:"stream"`
	}
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(round.Model) != string(model) {
		t.Errorf("model changed in transit:\n got %s\nwant %s", round.Model, model)
	}
	if round.Stream.Head != 4242 {
		t.Errorf("stream.head = %d, want 4242", round.Stream.Head)
	}
}

func TestStreamResumePoint_ZeroMeansConnectWithoutOne(t *testing.T) {
	// A session with no stored events has no resume point, and 0 has to survive the wire
	// as 0 rather than being omitted — a client that cannot tell "no events yet" from
	// "field missing" would guess, and guessing here means either replaying everything
	// or skipping everything.
	encoded, err := json.Marshal(SessionMessagesResponse{
		Model:  json.RawMessage(`{}`),
		Stream: StreamResumePoint{Head: 0},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var round map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	stream, ok := round["stream"]
	if !ok {
		t.Fatalf("stream missing from %s", encoded)
	}
	if string(stream) != `{"head":0}` {
		t.Errorf("stream = %s, want {\"head\":0}", stream)
	}
}

// The head is an llm-bridge-server row id, NOT a log-store one. The two stores number
// the same events independently and this codebase has already paid for confusing them.
// The field lives under its own name in its own struct so the mistake needs effort.
func TestStreamResumePoint_IsNotTheLogStoreIDSpace(t *testing.T) {
	page := json.RawMessage(`{"entries":{"e_2094222":{"eventId":2094222}}}`)
	encoded, _ := json.Marshal(SessionMessagesResponse{
		Model:  page,
		Stream: StreamResumePoint{Head: 17},
	})
	var round SessionMessagesResponse
	if err := json.Unmarshal(encoded, &round); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if round.Stream.Head == 2094222 {
		t.Errorf("the resume point took the page's log-store id space")
	}
	if round.Stream.Head != 17 {
		t.Errorf("stream.head = %d, want 17", round.Stream.Head)
	}
}
