package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// /bin/cat echoes its stdin, so the order lines reach the process is the
// order they come back out.
func TestStdinBeforeTheProcessExistsIsHeldAndDeliveredInOrder(t *testing.T) {
	outgoing := make(chan *msg.RunnerMessage, 16)
	registry := NewSubprocessRegistry(outgoing, nil, "http://bridge.invalid", nil)
	const session = "br_test"

	if known, _ := registry.DeliverStdin(session, "too early"); known {
		t.Fatal("stdin for a session nobody asked to spawn was accepted")
	}
	registry.BeginSpawn(session)
	for _, line := range []string{"first", "second"} {
		if known, err := registry.DeliverStdin(session, line); !known || err != nil {
			t.Fatalf("DeliverStdin(%q) during the spawn = %v, %v", line, known, err)
		}
	}
	err := registry.Spawn(session, &msg.RunnerSpawn{
		Harness: msg.HarnessClaudeCode, BinaryPath: "/bin/cat", StartParams: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(registry.KillAll)
	if known, err := registry.DeliverStdin(session, "third"); !known || err != nil {
		t.Fatalf("DeliverStdin after the spawn = %v, %v", known, err)
	}

	want := []string{`{"method":"start","params":{}}`, "first", "second", "third"}
	for i, w := range want {
		select {
		case m := <-outgoing:
			if m.Type != msg.RunnerMsgStdout || m.Stdout.Data != w {
				t.Fatalf("line %d = %+v, want %q", i, m, w)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("line %d (%q) never came back", i, w)
		}
	}
}

func TestAnAbandonedSpawnStopsHoldingInput(t *testing.T) {
	registry := NewSubprocessRegistry(make(chan *msg.RunnerMessage, 1), nil, "", nil)
	registry.BeginSpawn("br_failed")
	registry.AbandonSpawn("br_failed")
	if known, _ := registry.DeliverStdin("br_failed", "late"); known {
		t.Error("input for an abandoned spawn was still held")
	}
}
