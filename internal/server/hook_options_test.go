package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

func TestHookOptionsServeTheWiredHarnessesAndOneEventList(t *testing.T) {
	srv, _ := testServer(t)
	w := httptest.NewRecorder()
	srv.handleHookOptions(w, httptest.NewRequest("GET", "/hook-options", nil))
	var options msg.HookOptions
	if err := json.Unmarshal(w.Body.Bytes(), &options); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if len(options.Harnesses) != len(hookConfigKeyByHarness) {
		t.Fatalf("served %d harnesses, the spawn wires %d", len(options.Harnesses), len(hookConfigKeyByHarness))
	}
	for _, option := range options.Harnesses {
		if option.ConfigKey != hookConfigKeyByHarness[option.Harness] {
			t.Errorf("%s config key = %q, want %q", option.Harness, option.ConfigKey, hookConfigKeyByHarness[option.Harness])
		}
		// One list: what this route offers is what GET /harnesses serves as hook_events.
		if len(option.KnownEvents) != len(harnessHookEvents[option.Harness]) {
			t.Errorf("%s known events %v differ from harnessHookEvents %v", option.Harness, option.KnownEvents, harnessHookEvents[option.Harness])
		}
		if option.KnownEvents == nil {
			t.Errorf("%s known_events must be [] on the wire, never null", option.Harness)
		}
		if option.MatcherHelp == "" {
			t.Errorf("%s has no matcher help", option.Harness)
		}
	}
	want := []msg.HookScope{msg.HookScopeSession, msg.HookScopeInstance, msg.HookScopeGlobal}
	if len(options.ScopeKinds) != len(want) {
		t.Fatalf("scope kinds = %v", options.ScopeKinds)
	}
	for i := range want {
		if options.ScopeKinds[i] != want[i] {
			t.Fatalf("scope kinds = %v, want narrowest first %v", options.ScopeKinds, want)
		}
	}
}

// A harness the editor offers must be one the spawn wires: injectHookSettings
// writes the block under the served config key for every listed harness.
func TestEveryHarnessInTheHookOptionsIsWired(t *testing.T) {
	srv, _ := testServer(t)
	for harness, configKey := range hookConfigKeyByHarness {
		sess := &store.Session{SessionID: "br_hook_options_test", Harness: harness}
		if err := srv.injectHookSettings(sess); err != nil {
			t.Fatal(err)
		}
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			t.Fatalf("%s: harness_config after injectHookSettings: %v (%s)", harness, err, string(sess.HarnessConfig))
		}
		if _, ok := cfg[configKey]; !ok {
			t.Errorf("%s: injectHookSettings wrote no %q block; keys %v", harness, configKey, keysOf(cfg))
		}
	}
}
