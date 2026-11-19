package harnessstore

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestHarnessTypeUpsertAndGet(t *testing.T) {
	s := newTestStore(t)

	ht := &HarnessType{Name: msg.HarnessClaudeCode, Label: "Claude Code", Emoji: "🤖", Image: "cc.png"}
	if err := s.UpsertHarnessType(ht); err != nil {
		t.Fatalf("UpsertHarnessType: %v", err)
	}

	got, err := s.GetHarnessType(msg.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("GetHarnessType: %v", err)
	}
	if got.Name != msg.HarnessClaudeCode || got.Label != "Claude Code" || got.Emoji != "🤖" || got.Image != "cc.png" {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	// Upsert again with the same name updates in place (no duplicate row).
	ht.Label = "Claude Code (updated)"
	ht.Emoji = "✨"
	if err := s.UpsertHarnessType(ht); err != nil {
		t.Fatalf("re-UpsertHarnessType: %v", err)
	}
	got, err = s.GetHarnessType(msg.HarnessClaudeCode)
	if err != nil {
		t.Fatalf("GetHarnessType: %v", err)
	}
	if got.Label != "Claude Code (updated)" || got.Emoji != "✨" {
		t.Errorf("upsert did not update: %+v", got)
	}

	all, err := s.ListHarnessTypes()
	if err != nil {
		t.Fatalf("ListHarnessTypes: %v", err)
	}
	if len(all) != 1 {
		t.Errorf("ListHarnessTypes len = %d, want 1 (upsert, not insert)", len(all))
	}
}

func TestGetHarnessTypeNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.GetHarnessType(msg.HarnessCodex); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

func TestListHarnessTypesOrdered(t *testing.T) {
	s := newTestStore(t)
	if all, err := s.ListHarnessTypes(); err != nil {
		t.Fatalf("ListHarnessTypes empty: %v", err)
	} else if len(all) != 0 {
		t.Errorf("len = %d, want 0", len(all))
	}

	// Insert in non-alphabetical order; list is ordered by name.
	types := []*HarnessType{
		{Name: msg.HarnessHermes, Label: "Hermes"},
		{Name: msg.HarnessCodex, Label: "Codex"},
		{Name: msg.HarnessClaudeCode, Label: "Claude Code"},
	}
	for _, ht := range types {
		if err := s.UpsertHarnessType(ht); err != nil {
			t.Fatalf("UpsertHarnessType %s: %v", ht.Name, err)
		}
	}

	all, err := s.ListHarnessTypes()
	if err != nil {
		t.Fatalf("ListHarnessTypes: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len = %d, want 3", len(all))
	}
	// claude_code < codex < hermes lexically.
	want := []msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex, msg.HarnessHermes}
	for i, w := range want {
		if all[i].Name != w {
			t.Errorf("all[%d].Name = %q, want %q", i, all[i].Name, w)
		}
	}
}

func TestDeleteHarnessType(t *testing.T) {
	s := newTestStore(t)
	if err := s.UpsertHarnessType(&HarnessType{Name: msg.HarnessCodex, Label: "Codex"}); err != nil {
		t.Fatalf("UpsertHarnessType: %v", err)
	}
	if err := s.DeleteHarnessType(msg.HarnessCodex); err != nil {
		t.Fatalf("DeleteHarnessType: %v", err)
	}
	if _, err := s.GetHarnessType(msg.HarnessCodex); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("GetHarnessType after delete err = %v, want sql.ErrNoRows", err)
	}
	// DeleteHarnessType on a missing name is a no-op DELETE (no error).
	if err := s.DeleteHarnessType(msg.HarnessCodex); err != nil {
		t.Errorf("DeleteHarnessType on missing: %v", err)
	}
}
