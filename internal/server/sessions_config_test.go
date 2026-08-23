package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

// Configuring a session that has been created but not yet STARTED.
//
// The reported symptom: "when I start a new session I get some error, but my
// message still sends and the session works." The error was a 500 on
// `POST /sessions/{id}/config`, and the damage was invisible — the model the user
// picked was dropped and the session ran on the harness default while the picker
// still showed their choice.
//
// The shape that produces it is the normal one. A new chat is created, configured
// and sent in that order (chat-core `hooks.ts` `send`), and the process is spawned
// by the SEND — so the config lands in the window where the row exists and no
// harness does.
//
// It is not true that harness config "needs a harness to carry it to", which is
// what the old code said: `startParams` merges the stored `harness_config` into
// the params it spawns with, which is how `permission_mode` already reaches a
// session that has never run. So the config is persisted and the spawn applies it.
func TestConfigSession_NoLiveProcess_PersistsInsteadOfFailing(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "fresh")

	resp := doJSON(t, srv, "POST", "/sessions/fresh/config", map[string]any{"model": "claude-fable-5"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a created-but-unstarted session must accept config", resp.StatusCode)
	}

	sess, err := st.GetSession("fresh")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("harness_config is not an object: %v (raw %q)", err, string(sess.HarnessConfig))
	}
	if cfg["model"] != "claude-fable-5" {
		t.Errorf("model = %v, want claude-fable-5 — the pick was dropped", cfg["model"])
	}
}

// ⚠️ MERGED, never replaced. The create path already writes `permission_mode`
// into this blob, so a bare overwrite would silently un-set it — the exact class
// of bug this endpoint's sibling in hooks_resolve.go merges to avoid.
func TestConfigSession_NoLiveProcess_MergesRatherThanReplaces(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "fresh")
	if err := st.UpdateSessionHarnessConfig("fresh", json.RawMessage(`{"permission_mode":"bypass"}`)); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	resp := doJSON(t, srv, "POST", "/sessions/fresh/config", map[string]any{"model": "m1", "effort": "high"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	sess, _ := st.GetSession("fresh")
	var cfg map[string]any
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg["permission_mode"] != "bypass" {
		t.Errorf("permission_mode = %v, want bypass — the merge dropped what was already there", cfg["permission_mode"])
	}
	if cfg["model"] != "m1" || cfg["effort"] != "high" {
		t.Errorf("model/effort = %v/%v, want m1/high", cfg["model"], cfg["effort"])
	}
}

// An unset field means "leave it alone", not "clear it". Otherwise configuring
// the effort alone would wipe a model chosen a moment earlier.
func TestConfigSession_NoLiveProcess_LeavesUnsetFieldsAlone(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "fresh")

	if resp := doJSON(t, srv, "POST", "/sessions/fresh/config", map[string]any{"model": "m1"}); resp.StatusCode != 200 {
		t.Fatalf("first config: %d", resp.StatusCode)
	}
	if resp := doJSON(t, srv, "POST", "/sessions/fresh/config", map[string]any{"effort": "low"}); resp.StatusCode != 200 {
		t.Fatalf("second config: %d", resp.StatusCode)
	}

	sess, _ := st.GetSession("fresh")
	var cfg map[string]any
	json.Unmarshal(sess.HarnessConfig, &cfg)
	if cfg["model"] != "m1" {
		t.Errorf("model = %v, want m1 — setting effort cleared it", cfg["model"])
	}
	if cfg["effort"] != "low" {
		t.Errorf("effort = %v, want low", cfg["effort"])
	}
}

// A budget-only request on a session with no process keeps its own path: the
// ceiling is server state, persisted separately, and there is no harness config
// to store.
func TestConfigSession_NoLiveProcess_BudgetOnlyStillWorks(t *testing.T) {
	srv, st := testServer(t)
	seedSession(t, st, "fresh")

	resp := doJSON(t, srv, "POST", "/sessions/fresh/config", map[string]any{"max_budget": 5.0})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	sess, _ := st.GetSession("fresh")
	if sess.MaxBudgetUSD != 5.0 {
		t.Errorf("max_budget = %v, want 5", sess.MaxBudgetUSD)
	}
	if len(sess.HarnessConfig) > 0 {
		var cfg map[string]any
		json.Unmarshal(sess.HarnessConfig, &cfg)
		if _, unwanted := cfg["max_budget"]; unwanted {
			t.Error("max_budget leaked into harness_config; it is server state")
		}
	}
}
