package toolstore

import (
	"errors"
	"path/filepath"
	"testing"
)

func openInstTest(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "ts"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seedTool(t *testing.T, s *Store, name string, enabled bool) {
	t.Helper()
	in := &Tool{
		Name:    name,
		Kind:    KindMCP,
		MCP:     &MCPSpec{Transport: "stdio", Command: "npx"},
		Enabled: enabled,
	}
	if _, err := s.UpsertTool(in); err != nil {
		t.Fatalf("upsert %s: %v", name, err)
	}
}

func TestEnableForInstanceHappyPath(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "brave-search", true)

	if err := s.EnableForInstance("inst-1", "brave-search"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	tools, err := s.ListInstanceTools("inst-1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "brave-search" {
		t.Fatalf("expected 1 brave-search, got %v", tools)
	}
}

func TestEnableForInstanceIdempotent(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", true)
	if err := s.EnableForInstance("i", "x"); err != nil {
		t.Fatalf("enable 1: %v", err)
	}
	if err := s.EnableForInstance("i", "x"); err != nil {
		t.Fatalf("enable 2 (should be no-op): %v", err)
	}
	tools, _ := s.ListInstanceTools("i")
	if len(tools) != 1 {
		t.Fatalf("expected 1 row after dup enable, got %d", len(tools))
	}
}

func TestEnableForInstanceRejectsGloballyDisabled(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", false) // global disabled
	err := s.EnableForInstance("i", "x")
	if !errors.Is(err, ErrGloballyDisabled) {
		t.Fatalf("expected ErrGloballyDisabled, got %v", err)
	}
}

func TestEnableForInstanceMissingTool(t *testing.T) {
	s := openInstTest(t)
	err := s.EnableForInstance("i", "nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestEnableForInstanceRequiresInstanceID(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", true)
	if err := s.EnableForInstance("", "x"); err == nil {
		t.Fatal("expected error for empty instance_id")
	}
}

func TestDisableForInstance(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", true)
	_ = s.EnableForInstance("i", "x")

	if err := s.DisableForInstance("i", "x"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	tools, _ := s.ListInstanceTools("i")
	if len(tools) != 0 {
		t.Fatalf("expected 0 after disable, got %d", len(tools))
	}
	// Second disable should error
	if err := s.DisableForInstance("i", "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound on dup disable, got %v", err)
	}
}

func TestListInstanceToolsFiltersGloballyDisabled(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", true)
	if err := s.EnableForInstance("i", "x"); err != nil {
		t.Fatalf("enable: %v", err)
	}
	// Now globally disable. Per-instance row stays, but list should drop it.
	got, _ := s.GetToolByName("x")
	if err := s.SetEnabled(got.ID, false); err != nil {
		t.Fatalf("disable global: %v", err)
	}
	tools, _ := s.ListInstanceTools("i")
	if len(tools) != 0 {
		t.Fatalf("globally-disabled tool should be filtered, got %v", tools)
	}
}

func TestListInstancesForTool(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", true)
	_ = s.EnableForInstance("i1", "x")
	_ = s.EnableForInstance("i2", "x")
	_ = s.EnableForInstance("i3", "x")
	ids, err := s.ListInstancesForTool("x")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 3 || ids[0] != "i1" || ids[2] != "i3" {
		t.Fatalf("expected [i1 i2 i3], got %v", ids)
	}
}

func TestCascadeDeleteFromTools(t *testing.T) {
	s := openInstTest(t)
	seedTool(t, s, "x", true)
	got, _ := s.GetToolByName("x")
	_ = s.EnableForInstance("i", "x")

	if err := s.DeleteTool(got.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	tools, _ := s.ListInstanceTools("i")
	if len(tools) != 0 {
		t.Fatalf("instance_tools row should cascade-delete, got %v", tools)
	}
}
