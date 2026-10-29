package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func historyRecord(text string) json.RawMessage {
	ev := msg.Event{Type: msg.EventUserMessage, Harness: msg.HarnessCodex, HarnessSessionID: "native", Raw: json.RawMessage(`{"type":"response_item","payload":{"text":"` + text + `"}}`)}
	data, _ := json.Marshal(ev)
	return data
}
func TestCodexHistorySuffix(t *testing.T) {
	a, b := historyRecord("a"), historyRecord("b")
	for _, tc := range []struct {
		name   string
		stored []json.RawMessage
		out    []byte
		want   int
		fails  bool
	}{
		{"append", []json.RawMessage{a}, bytes.Join([][]byte{a, b}, []byte("\n")), 1, false},
		{"retry", []json.RawMessage{a, b}, bytes.Join([][]byte{a, b}, []byte("\n")), 0, false},
		{"identical repeated records", []json.RawMessage{a}, bytes.Join([][]byte{a, a}, []byte("\n")), 1, false},
		{"edited", []json.RawMessage{a}, b, 0, true},
		{"truncated", []json.RawMessage{a, b}, a, 0, true},
		{"invalid", nil, []byte("broken"), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := codexHistorySuffix(tc.stored, tc.out, "native")
			if (err != nil) != tc.fails || len(got) != tc.want {
				t.Fatalf("got %d, %v", len(got), err)
			}
		})
	}
}

func TestCodexHistorySyncRestartAndLiveFanout(t *testing.T) {
	m := newTestManager(t)
	if err := m.store.CreateSession(&store.Session{SessionID: "br_sync", Harness: msg.HarnessCodex, HarnessSessionID: "native", Origin: msg.OriginDiscovery}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n-history-revision) cat '" + dir + "/revision';;\n-import-history) cat '" + dir + "/events';;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "llm-bridge-codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	write := func(name string, b []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	a, b := historyRecord("a"), historyRecord("b")
	write("events", a)
	write("revision", []byte("one"))
	ch := m.Subscribe("br_sync")
	defer m.Unsubscribe("br_sync", ch)
	if n, err := m.SyncCodexHistory(context.Background(), "br_sync"); err != nil || n != 1 {
		t.Fatalf("first sync: %d %v", n, err)
	}
	select {
	case ev := <-ch:
		if ev.RowID == 0 {
			t.Fatal("missing SSE row ID")
		}
	default:
		t.Fatal("no live event")
	}
	// A new manager has no revision cache; durability must prevent duplicates.
	write("events", bytes.Join([][]byte{a, b}, []byte("\n")))
	write("revision", []byte("two"))
	m.codexHistorySyncs.Delete("br_sync")
	if n, err := m.SyncCodexHistory(context.Background(), "br_sync"); err != nil || n != 1 {
		t.Fatalf("restart: %d %v", n, err)
	}
	if n, err := m.SyncCodexHistory(context.Background(), "br_sync"); err != nil || n != 0 {
		t.Fatalf("unchanged: %d %v", n, err)
	}
	rows, err := m.logStore.ListEvents("br_sync", 0, nil)
	if err != nil || len(rows) != 2 {
		t.Fatalf("durable records: %d %v", len(rows), err)
	}
}
