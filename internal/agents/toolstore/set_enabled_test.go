package toolstore

import (
	"errors"
	"testing"
)

// SetEnabled ships with two values and only one of them had ever been passed.
//
// All three test call sites — store_test.go:199, instance_tools_test.go:115,
// provision_test.go:212 — pass false. server.go reaches both through
// setEnabled(w, r, v), so the enable route is live in production; it is the
// tests that had only ever turned tools off.
//
// The gap is not academic here, because of what this repo's master switch
// does. ~/repos/CLAUDE.md records that every `local` tool carries enabled=0 in
// this table and that no per-instance opt-in can override it. That is a claim
// about the *disabled* direction. The claim these tests were missing is the
// return trip: that turning the master switch back on restores exactly what
// was there before, rather than something that had been quietly deleted while
// it was off.
//
// Filed by the 201st nightly pass, 2026-08-15, working step 2 of noteboard card
// 25ac6a72-a5a0-4ada-b4a9-aeeb4e688177.

// TestSetEnabledTrueRestoresATool is the plain inverse of the existing
// disable assertion in TestDeleteAndSetEnabled.
func TestSetEnabledTrueRestoresATool(t *testing.T) {
	s := openTest(t)
	in := &Tool{Name: "shell", Kind: KindLocal, Local: &LocalSpec{Symbol: "x.Shell"}, Enabled: true}
	if _, err := s.UpsertTool(testContext, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := s.SetEnabled(testContext, in.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := s.SetEnabled(testContext, in.ID, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	got, err := s.GetTool(testContext, in.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !got.Enabled {
		t.Error("tool is still disabled after SetEnabled(id, true)")
	}
}

// TestSetEnabledIsIdempotentInBothDirections pins that SetEnabled reports
// success when the value is already what was asked for.
//
// It matters because the not-found signal is derived from RowsAffected. SQLite
// counts rows the UPDATE matched, not rows whose values changed, so a no-op
// write still reports one row and the call returns nil. Narrow that WHERE
// clause — add `AND enabled != ?` as an optimization, say — and every
// already-enabled tool starts answering ErrNotFound, which server.go turns
// into a 404 for a tool that plainly exists.
func TestSetEnabledIsIdempotentInBothDirections(t *testing.T) {
	s := openTest(t)
	in := &Tool{Name: "shell", Kind: KindLocal, Local: &LocalSpec{Symbol: "x.Shell"}, Enabled: true}
	if _, err := s.UpsertTool(testContext, in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Already enabled; enabling again must not read as a missing row.
	if err := s.SetEnabled(testContext, in.ID, true); err != nil {
		t.Fatalf("enable an already-enabled tool: %v", err)
	}
	if err := s.SetEnabled(testContext, in.ID, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := s.SetEnabled(testContext, in.ID, false); err != nil {
		t.Fatalf("disable an already-disabled tool: %v", err)
	}
	got, _ := s.GetTool(testContext, in.ID)
	if got.Enabled {
		t.Error("tool should be disabled after two disables")
	}
}

// TestSetEnabledTrueReportsAMissingToolThroughBothValues. The existing tests
// only ever reach ErrNotFound through a delete followed by a disable, so the
// enable path's own not-found branch had never run.
func TestSetEnabledTrueReportsAMissingTool(t *testing.T) {
	s := openTest(t)
	if err := s.SetEnabled(testContext, 9999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnabled(missing, true) = %v, want ErrNotFound", err)
	}
	if err := s.SetEnabled(testContext, 9999, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetEnabled(missing, false) = %v, want ErrNotFound", err)
	}
}

// TestGlobalDisableHidesAnInstanceOptInWithoutDestroyingIt is the one worth
// the night.
//
// TestListInstanceToolsFiltersGloballyDisabled carries the comment
// "Per-instance row stays, but list should drop it" — and then asserts only
// the second half. Nothing in that test can tell a filtered row from a deleted
// one: if a global disable cascaded a DELETE over instance_tools, it would
// still see zero tools and still pass. The comment states a property the test
// is structurally unable to observe.
//
// Passing true is what makes it observable. Re-enable globally, and either the
// opt-in comes back or it was never there.
func TestGlobalDisableHidesAnInstanceOptInWithoutDestroyingIt(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", true)
	if err := s.EnableForInstance(testContext, "i", "x"); err != nil {
		t.Fatalf("enable for instance: %v", err)
	}
	tool, err := s.GetToolByName(testContext, "x")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}

	if err := s.SetEnabled(testContext, tool.ID, false); err != nil {
		t.Fatalf("disable global: %v", err)
	}
	hidden, _ := s.ListInstanceTools(testContext, "i")
	if len(hidden) != 0 {
		t.Fatalf("globally-disabled tool should be filtered, got %v", hidden)
	}

	if err := s.SetEnabled(testContext, tool.ID, true); err != nil {
		t.Fatalf("re-enable global: %v", err)
	}
	restored, err := s.ListInstanceTools(testContext, "i")
	if err != nil {
		t.Fatalf("list instance tools: %v", err)
	}
	if len(restored) != 1 || restored[0].Name != "x" {
		t.Fatalf("opt-in did not survive the global disable: got %v, want the one tool back — a global disable must hide the row, not delete it", restored)
	}
}

// TestReEnablingGloballyReopensTheOptInPath covers the other consequence of
// the master switch: while a tool is globally disabled, EnableForInstance
// refuses with ErrGloballyDisabled. Nothing asserted that the refusal lifts.
func TestReEnablingGloballyReopensTheOptInPath(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", false)
	tool, err := s.GetToolByName(testContext, "x")
	if err != nil {
		t.Fatalf("get by name: %v", err)
	}

	if err := s.EnableForInstance(testContext, "i", "x"); !errors.Is(err, ErrGloballyDisabled) {
		t.Fatalf("opt-in while globally disabled = %v, want ErrGloballyDisabled", err)
	}

	if err := s.SetEnabled(testContext, tool.ID, true); err != nil {
		t.Fatalf("enable global: %v", err)
	}
	if err := s.EnableForInstance(testContext, "i", "x"); err != nil {
		t.Fatalf("opt-in after the master switch went back on: %v", err)
	}
	tools, _ := s.ListInstanceTools(testContext, "i")
	if len(tools) != 1 {
		t.Fatalf("want 1 tool after opting in, got %d", len(tools))
	}
}
