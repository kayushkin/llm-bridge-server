package permissions

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newHandlersTestServer serves the routes at the paths the standalone store
// did, on a freshly migrated module.
func newHandlersTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newHandlersTestServerOn(t, newSeededTestStore(t))
}

func newHandlersTestServerOn(t *testing.T, store *Store) *httptest.Server {
	t.Helper()
	h := NewHandlers(store, NewEngine(store))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", h.Health)
	mux.HandleFunc("GET /rules", h.ListRules)
	mux.HandleFunc("POST /rules", h.CreateRule)
	mux.HandleFunc("GET /rules/{id}", h.GetRule)
	mux.HandleFunc("PATCH /rules/{id}", h.UpdateRule)
	mux.HandleFunc("DELETE /rules/{id}", h.DeleteRule)
	mux.HandleFunc("POST /evaluate", h.Evaluate)
	mux.HandleFunc("GET /audit", h.ListAudit)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func call(t *testing.T, server *httptest.Server, method, path string, body any) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequest(method, server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(raw)
}

func TestTheRoutesWriteRulesJudgeACallAndAuditIt(t *testing.T) {
	server := newHandlersTestServer(t)
	if status, body := call(t, server, "GET", "/healthz", nil); status != 200 || strings.TrimSpace(body) != `{"rules":18,"status":"ok"}` {
		t.Fatalf("healthz = %d %s", status, body)
	}
	status, body := call(t, server, "POST", "/rules", map[string]any{"id": "deny-make", "scope": "global", "priority": 10, "tool": "Bash", "pattern": `^make\b`, "outcome": "deny", "message": "no make", "enabled": true})
	if status != 201 || !strings.Contains(body, `"id":"deny-make"`) {
		t.Fatalf("create = %d %s", status, body)
	}
	if status, body := call(t, server, "POST", "/rules", map[string]any{"scope": "global", "tool": "Bash", "outcome": "maybe"}); status != 400 {
		t.Fatalf("a bad outcome = %d %s", status, body)
	}
	status, body = call(t, server, "POST", "/evaluate", map[string]any{"bridge_id": "br_1", "tool": "Bash", "input": map[string]string{"command": "git status && make all"}})
	if status != 200 || !strings.Contains(body, `"outcome":"deny"`) || !strings.Contains(body, `"matched_rule_id":"deny-make"`) {
		t.Fatalf("evaluate = %d %s", status, body)
	}
	status, body = call(t, server, "PATCH", "/rules/deny-make", map[string]any{"enabled": false})
	if status != 200 || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("patch = %d %s", status, body)
	}
	status, body = call(t, server, "POST", "/evaluate", map[string]any{"tool": "Bash", "input": map[string]string{"command": "make all"}})
	if status != 200 || !strings.Contains(body, `"outcome":"ask"`) {
		t.Fatalf("evaluate after disabling = %d %s", status, body)
	}
	status, body = call(t, server, "GET", "/audit?bridge_id=br_1", nil)
	var entries []AuditEntry
	if status != 200 || json.Unmarshal([]byte(body), &entries) != nil || len(entries) != 1 || entries[0].Outcome != "deny" {
		t.Fatalf("audit = %d %s", status, body)
	}
	if status, _ := call(t, server, "GET", "/rules/nobody", nil); status != 404 {
		t.Fatalf("a missing rule = %d", status)
	}
	if status, _ := call(t, server, "PATCH", "/rules/nobody", map[string]any{"enabled": false}); status != 404 {
		t.Fatalf("patching a missing rule = %d", status)
	}
	if status, _ := call(t, server, "DELETE", "/rules/deny-make", nil); status != 204 {
		t.Fatalf("delete = %d", status)
	}
	if status, body := call(t, server, "GET", "/rules?scope=instance:none", nil); status != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("an empty list = %d %s", status, body)
	}
	if status, _ := call(t, server, "POST", "/evaluate", map[string]any{"input": map[string]string{}}); status != 400 {
		t.Fatalf("evaluate without a tool = %d", status)
	}
}
