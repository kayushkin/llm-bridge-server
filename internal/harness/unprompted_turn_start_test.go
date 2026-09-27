package harness

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// A pty session's rollout tailer re-reads the conversation from the start on
// every resume. Its user_messages are history, not turns opening now, and
// counting them would close open questions on a plain resume.
func TestARolloutReplayIsNotATurnStartingNow(t *testing.T) {
	replayed := &msg.Event{Type: msg.EventUserMessage, Extensions: map[string]json.RawMessage{"source": json.RawMessage(`"rollout"`)}}
	if !isRolloutReplay(replayed) {
		t.Error("a rollout user_message was not recognised as a replay")
	}
	for _, live := range []*msg.Event{
		{Type: msg.EventUserMessage},
		{Type: msg.EventUserMessage, Extensions: map[string]json.RawMessage{"source": json.RawMessage(`"otel"`)}},
	} {
		if isRolloutReplay(live) {
			t.Errorf("a live user_message (%v) was taken for a replay", live.Extensions)
		}
	}
}
