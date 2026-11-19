package hookstore

import (
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "hooks.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreate(t *testing.T, s *Store, id, event, matcher, cmd string, scope msg.HookScope, scopeID string) {
	t.Helper()
	h := &msg.Hook{
		ID:        id,
		Harness:   msg.HarnessClaudeCode,
		Event:     event,
		Matcher:   matcher,
		Command:   cmd,
		ScopeKind: scope,
		ScopeID:   scopeID,
		Enabled:   true,
	}
	if err := s.CreateHook(h); err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
}

func TestCRUDRoundtrip(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "h1", "PreToolUse", "Bash", "cmd-a", msg.HookScopeGlobal, "")

	got, err := s.GetHook("h1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Command != "cmd-a" || got.Event != "PreToolUse" || got.Matcher != "Bash" {
		t.Fatalf("unexpected: %+v", got)
	}

	got.Command = "cmd-b"
	if err := s.UpdateHook(got); err != nil {
		t.Fatalf("update: %v", err)
	}

	got2, _ := s.GetHook("h1")
	if got2.Command != "cmd-b" {
		t.Fatalf("update did not stick: %s", got2.Command)
	}
	if !got2.UpdatedAt.After(got2.CreatedAt) && !got2.UpdatedAt.Equal(got2.CreatedAt) {
		t.Fatalf("updated_at not refreshed: created=%v updated=%v", got2.CreatedAt, got2.UpdatedAt)
	}

	if err := s.DeleteHook("h1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetHook("h1"); err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestListApplicable_ScopeInheritance(t *testing.T) {
	s := newStore(t)
	// Global applies regardless of instance/session.
	mustCreate(t, s, "g-edit", "PreToolUse", "Edit", "cmd-g-edit", msg.HookScopeGlobal, "")
	// Instance applies only when instanceID matches.
	mustCreate(t, s, "i-write", "PreToolUse", "Write", "cmd-i-write", msg.HookScopeInstance, "inst-A")
	// Different instance — should NOT apply for inst-A.
	mustCreate(t, s, "i-other", "PreToolUse", "Read", "cmd-i-other", msg.HookScopeInstance, "inst-B")
	// Session applies only when sessionID matches.
	mustCreate(t, s, "s-bash", "PreToolUse", "Bash", "cmd-s-bash", msg.HookScopeSession, "sess-1")

	got, err := s.ListApplicable(msg.HarnessClaudeCode, "inst-A", "sess-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 applicable hooks, got %d: %+v", len(got), got)
	}
	ids := map[string]bool{}
	for _, h := range got {
		ids[h.ID] = true
	}
	for _, want := range []string{"g-edit", "i-write", "s-bash"} {
		if !ids[want] {
			t.Errorf("missing %s from applicable set", want)
		}
	}
	if ids["i-other"] {
		t.Error("i-other should not apply for inst-A")
	}
}

func TestListApplicable_MostSpecificWins(t *testing.T) {
	s := newStore(t)
	// Three hooks with same (event, matcher), different scopes.
	mustCreate(t, s, "g", "PreToolUse", "Bash", "cmd-global", msg.HookScopeGlobal, "")
	mustCreate(t, s, "i", "PreToolUse", "Bash", "cmd-instance", msg.HookScopeInstance, "inst-A")
	mustCreate(t, s, "sess", "PreToolUse", "Bash", "cmd-session", msg.HookScopeSession, "sess-1")

	got, err := s.ListApplicable(msg.HarnessClaudeCode, "inst-A", "sess-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 hook after dedupe, got %d: %+v", len(got), got)
	}
	if got[0].Command != "cmd-session" {
		t.Errorf("expected session scope to win, got %s (scope=%s)", got[0].Command, got[0].ScopeKind)
	}

	// Without session, instance should win.
	got, _ = s.ListApplicable(msg.HarnessClaudeCode, "inst-A", "")
	if len(got) != 1 || got[0].Command != "cmd-instance" {
		t.Errorf("expected instance to win with no session, got %+v", got)
	}

	// Without instance or session, global should win.
	got, _ = s.ListApplicable(msg.HarnessClaudeCode, "", "")
	if len(got) != 1 || got[0].Command != "cmd-global" {
		t.Errorf("expected global to win with no instance/session, got %+v", got)
	}
}

func TestListApplicable_ExcludesDisabled(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "g", "PreToolUse", "Bash", "cmd", msg.HookScopeGlobal, "")
	if err := s.SetHookEnabled("g", false); err != nil {
		t.Fatal(err)
	}
	got, _ := s.ListApplicable(msg.HarnessClaudeCode, "", "")
	if len(got) != 0 {
		t.Fatalf("disabled hook leaked: %+v", got)
	}
}

func TestListApplicable_ExcludesOtherHarness(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "cc", "PreToolUse", "Bash", "cmd", msg.HookScopeGlobal, "")
	got, _ := s.ListApplicable(msg.HarnessCodex, "", "")
	if len(got) != 0 {
		t.Fatalf("wrong-harness hook leaked: %+v", got)
	}
}

func TestListHooks_Filters(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "a", "PreToolUse", "Bash", "c", msg.HookScopeGlobal, "")
	mustCreate(t, s, "b", "PostToolUse", "Bash", "c", msg.HookScopeGlobal, "")
	mustCreate(t, s, "c", "PreToolUse", "Edit", "c", msg.HookScopeInstance, "inst-A")

	got, _ := s.ListHooks(ListFilter{Event: "PreToolUse"})
	if len(got) != 2 {
		t.Errorf("event filter: want 2, got %d", len(got))
	}

	got, _ = s.ListHooks(ListFilter{ScopeKind: msg.HookScopeInstance})
	if len(got) != 1 || got[0].ID != "c" {
		t.Errorf("scope filter: got %+v", got)
	}
}

func TestCreateHook_SessionWithoutScopeID(t *testing.T) {
	s := newStore(t)
	h := &msg.Hook{
		ID: "bad", Harness: msg.HarnessClaudeCode,
		Event: "PreToolUse", Command: "c",
		ScopeKind: msg.HookScopeSession, ScopeID: "", Enabled: true,
	}
	if err := s.CreateHook(h); err == nil {
		t.Fatal("expected CHECK constraint violation for session scope with empty scope_id")
	}
}
