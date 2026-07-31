package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// GET /sessions/{id}/messages and /history proxy straight to log-store,
// bypassing the manager, so the handler drains the session's log-store
// write queue itself before proxying. Every archived session — the
// common case — has no queue at all, and that drain must be a no-op
// rather than a hang or an error on the endpoint the chat UI loads a
// conversation from.
func TestSessionHistoryProxyServesSessionsWithNoWriteQueue(t *testing.T) {
	const body = `{"messages":[{"role":"user","text":"hello"}]}`

	logStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/sessions/br-archived/messages" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	t.Cleanup(logStore.Close)

	srv, _, _ := testServerWithInstanceAndLogStore(t, msg.HarnessClaudeCode, logStore.URL)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(ts.URL + "/sessions/br-archived/messages")
	if err != nil {
		t.Fatalf("GET messages: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d; want 200", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	if string(got) != body {
		t.Fatalf("body = %q; want log-store's response passed through unchanged", string(got))
	}
}
