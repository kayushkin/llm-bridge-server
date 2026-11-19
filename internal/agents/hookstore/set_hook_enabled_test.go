package hookstore

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// SetHookEnabled had exactly one call site in the whole repo —
// hook_test.go:137, passing false — so the enable direction had never run.
//
// The store spells "enabled" three different ways along one round trip: the
// column is INTEGER, the writes bind a Go bool, and scanHook reads into an int
// and compares `enabled != 0`. Two of those three conversions are the driver's
// and nothing asserted either of them. Every existing test creates hooks with
// Enabled: true and the one disable is only ever observed as an absence from a
// filtered list, which a broken write would produce just as convincingly as a
// working one.
//
// Filed by the 201st nightly pass, 2026-08-15, working step 2 of noteboard card
// 25ac6a72-a5a0-4ada-b4a9-aeeb4e688177.

// TestSetHookEnabledRoundTripsThroughTheIntegerColumn is the round trip
// itself, read back through GetHook rather than inferred from a filtered list.
func TestSetHookEnabledRoundTripsThroughTheIntegerColumn(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "h", "PreToolUse", "Bash", "cmd", msg.HookScopeGlobal, "")

	if err := s.SetHookEnabled("h", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	off, err := s.GetHook("h")
	if err != nil {
		t.Fatalf("get after disable: %v", err)
	}
	if off.Enabled {
		t.Fatal("hook still reads as enabled after SetHookEnabled(id, false)")
	}

	if err := s.SetHookEnabled("h", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	on, err := s.GetHook("h")
	if err != nil {
		t.Fatalf("get after re-enable: %v", err)
	}
	if !on.Enabled {
		t.Error("hook still reads as disabled after SetHookEnabled(id, true) — a Go true did not survive the trip through an INTEGER column")
	}
}

// TestSetHookEnabledTrueReturnsAHookToListApplicable is what the existing
// disable test half-asserts. It shows the hook leaves the applicable set and
// comes back, so the disable is a state and not a deletion.
func TestSetHookEnabledTrueReturnsAHookToListApplicable(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "g", "PreToolUse", "Bash", "cmd", msg.HookScopeGlobal, "")

	if err := s.SetHookEnabled("g", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if got, _ := s.ListApplicable(msg.HarnessClaudeCode, "", ""); len(got) != 0 {
		t.Fatalf("disabled hook leaked: %+v", got)
	}

	// The row must still be there — ListApplicable filters, it does not delete.
	if n, err := s.CountHooks(); err != nil || n != 1 {
		t.Fatalf("CountHooks = %d, %v; want 1 row still present while disabled", n, err)
	}

	if err := s.SetHookEnabled("g", true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	got, err := s.ListApplicable(msg.HarnessClaudeCode, "", "")
	if err != nil {
		t.Fatalf("list applicable: %v", err)
	}
	if len(got) != 1 || got[0].Command != "cmd" {
		t.Fatalf("re-enabled hook did not come back: %+v", got)
	}
}

// TestListHooksEnabledFilterMatchesBothValues is the first test of
// ListFilter.EnabledSet at all. No test in this repo had ever set it.
//
// It is the one query that compares a Go bool against the INTEGER column
// directly — `AND enabled = ?` with filter.Enabled bound straight in — so
// unlike the writes, which at least round-trip through scanHook's `!= 0`,
// nothing here would coerce a mismatched type into a plausible answer. It
// would just silently match no rows.
func TestListHooksEnabledFilterMatchesBothValues(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "on1", "PreToolUse", "Bash", "c", msg.HookScopeGlobal, "")
	mustCreate(t, s, "on2", "PreToolUse", "Edit", "c", msg.HookScopeGlobal, "")
	mustCreate(t, s, "off1", "PostToolUse", "Bash", "c", msg.HookScopeGlobal, "")
	if err := s.SetHookEnabled("off1", false); err != nil {
		t.Fatalf("disable: %v", err)
	}

	enabled, err := s.ListHooks(ListFilter{EnabledSet: true, Enabled: true})
	if err != nil {
		t.Fatalf("list enabled: %v", err)
	}
	if len(enabled) != 2 {
		t.Errorf("Enabled:true filter returned %d hooks, want 2 — got %+v", len(enabled), enabled)
	}

	disabled, err := s.ListHooks(ListFilter{EnabledSet: true, Enabled: false})
	if err != nil {
		t.Fatalf("list disabled: %v", err)
	}
	if len(disabled) != 1 || disabled[0].ID != "off1" {
		t.Errorf("Enabled:false filter returned %+v, want just off1", disabled)
	}

	// EnabledSet false must ignore Enabled entirely and return both. This is
	// the branch that makes the pair of fields necessary rather than one
	// *bool, and it is why Enabled:false cannot mean "unset".
	both, err := s.ListHooks(ListFilter{Enabled: true})
	if err != nil {
		t.Fatalf("list unfiltered: %v", err)
	}
	if len(both) != 3 {
		t.Errorf("EnabledSet:false returned %d hooks, want all 3 — Enabled must be ignored unless EnabledSet is set", len(both))
	}
}

// TestSetHookEnabledReportsAMissingHookThroughBothValues. The not-found branch
// had only ever been reachable through the disable direction, and in fact no
// test reached it at all.
func TestSetHookEnabledReportsAMissingHook(t *testing.T) {
	s := newStore(t)
	if err := s.SetHookEnabled("nope", true); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetHookEnabled(missing, true) = %v, want sql.ErrNoRows", err)
	}
	if err := s.SetHookEnabled("nope", false); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("SetHookEnabled(missing, false) = %v, want sql.ErrNoRows", err)
	}
}

// TestSetHookEnabledIsIdempotent pins that setting the value a hook already
// has is a success, not a missing row. Same reasoning as tool-store's: the
// not-found signal comes from RowsAffected, so it is one narrowed WHERE clause
// away from reporting that an existing hook does not exist.
func TestSetHookEnabledIsIdempotent(t *testing.T) {
	s := newStore(t)
	mustCreate(t, s, "h", "PreToolUse", "Bash", "cmd", msg.HookScopeGlobal, "")

	if err := s.SetHookEnabled("h", true); err != nil {
		t.Fatalf("enable an already-enabled hook: %v", err)
	}
	if err := s.SetHookEnabled("h", false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := s.SetHookEnabled("h", false); err != nil {
		t.Fatalf("disable an already-disabled hook: %v", err)
	}
	got, err := s.GetHook("h")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Enabled {
		t.Error("hook should be disabled after two disables")
	}
}
