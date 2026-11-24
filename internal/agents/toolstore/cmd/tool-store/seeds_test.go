package main

import (
	"testing"

	toolstore "github.com/kayushkin/tool-store"
	"github.com/kayushkin/tool-store/tools"
)

func getSeededTool(t *testing.T, s *toolstore.Store, name string) *toolstore.Tool {
	t.Helper()
	got, err := s.GetToolByName(name)
	if err != nil {
		t.Fatalf("GetToolByName(%q): %v", name, err)
	}
	return got
}

// The registry's whole contents come from these two functions, so "the seed ran"
// is the claim worth pinning first: every in-process tool must have a row.
func TestSeedLocalToolsWritesARowForEveryRegisteredTool(t *testing.T) {
	s := openSeedTestStore(t)

	if err := seedLocalTools(s); err != nil {
		t.Fatalf("seedLocalTools: %v", err)
	}

	all := tools.All()
	if len(all) == 0 {
		t.Fatal("tools.All() is empty, so this test asserts nothing")
	}
	for _, impl := range all {
		got := getSeededTool(t, s, impl.Name)
		if got.Kind != toolstore.KindLocal {
			t.Errorf("%s: kind = %q, want %q", impl.Name, got.Kind, toolstore.KindLocal)
		}
		if got.Description != impl.Description {
			t.Errorf("%s: description not taken from code", impl.Name)
		}
		if got.Local == nil || got.Local.Symbol != impl.Name {
			t.Errorf("%s: local symbol = %+v, want symbol %q", impl.Name, got.Local, impl.Name)
		}
		if len(got.InputSchema) == 0 {
			t.Errorf("%s: input schema was not stored", impl.Name)
		}
	}
}

// A new local row must arrive disabled. This is the security-relevant half of
// the seed: an operator opts each tool in, so a tool added to the code must not
// switch itself on across a restart.
func TestSeedLocalToolsLeavesNewRowsDisabled(t *testing.T) {
	s := openSeedTestStore(t)

	if err := seedLocalTools(s); err != nil {
		t.Fatalf("seedLocalTools: %v", err)
	}

	for _, impl := range tools.All() {
		if got := getSeededTool(t, s, impl.Name); got.Enabled {
			t.Errorf("%s: seeded enabled, want disabled", impl.Name)
		}
	}
}

// The seed runs on every restart, so it must not undo the operator's choice.
func TestSeedLocalToolsPreservesAnOperatorsEnabledState(t *testing.T) {
	s := openSeedTestStore(t)
	if err := seedLocalTools(s); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	all := tools.All()
	name := all[0].Name
	enabledByOperator := getSeededTool(t, s, name)
	enabledByOperator.Enabled = true
	if _, err := s.UpsertTool(enabledByOperator); err != nil {
		t.Fatalf("enable %s: %v", name, err)
	}
	// A second tool is deliberately left disabled, so this test can tell
	// "enabled state preserved" from "everything came back enabled".
	if len(all) < 2 {
		t.Fatal("need two registered tools to tell preservation from a blanket enable")
	}
	untouched := all[1].Name

	if err := seedLocalTools(s); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	if got := getSeededTool(t, s, name); !got.Enabled {
		t.Errorf("%s: re-seeding disabled an operator-enabled tool", name)
	}
	if got := getSeededTool(t, s, untouched); got.Enabled {
		t.Errorf("%s: re-seeding enabled a tool the operator never enabled", untouched)
	}
}

// The description and schema are refreshed from code on every seed, which is
// what makes this file the canonical source rather than the database.
func TestSeedLocalToolsRefreshesDescriptionFromCode(t *testing.T) {
	s := openSeedTestStore(t)
	if err := seedLocalTools(s); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	name := tools.All()[0].Name
	stale := getSeededTool(t, s, name)
	stale.Description = "a stale description no longer in the code"
	if _, err := s.UpsertTool(stale); err != nil {
		t.Fatalf("write stale description: %v", err)
	}

	if err := seedLocalTools(s); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	got := getSeededTool(t, s, name)
	if got.Description == "a stale description no longer in the code" {
		t.Errorf("%s: re-seeding kept the stale description", name)
	}
	if got.Description != tools.All()[0].Description {
		t.Errorf("%s: description = %q, want the one in code", name, got.Description)
	}
}

func TestSeedMCPToolsWritesTheCuratedServers(t *testing.T) {
	s := openSeedTestStore(t)

	if err := seedMCPTools(s); err != nil {
		t.Fatalf("seedMCPTools: %v", err)
	}

	// Named explicitly rather than read back from the seed list: a test that
	// takes its expectations from the thing under test cannot see it drift.
	for _, name := range []string{"brave-search", "playwright", "chrome-devtools"} {
		got := getSeededTool(t, s, name)
		if got.Kind != toolstore.KindMCP {
			t.Errorf("%s: kind = %q, want %q", name, got.Kind, toolstore.KindMCP)
		}
		if got.MCP == nil {
			t.Fatalf("%s: no MCP spec stored", name)
		}
		if got.MCP.Transport != "stdio" {
			t.Errorf("%s: transport = %q, want stdio", name, got.MCP.Transport)
		}
		if got.MCP.Command != "npx" {
			t.Errorf("%s: command = %q, want npx", name, got.MCP.Command)
		}
		if len(got.MCP.Args) == 0 {
			t.Errorf("%s: launcher args were not stored", name)
		}
		if got.Enabled {
			t.Errorf("%s: seeded enabled, want disabled", name)
		}
	}

	// brave-search is the only seed carrying a credential binding, so it pins
	// that the env-key and credential maps survive the round trip.
	brave := getSeededTool(t, s, "brave-search")
	if len(brave.EnvKeys) != 1 || brave.EnvKeys[0] != "BRAVE_API_KEY" {
		t.Errorf("brave-search: env keys = %v, want [BRAVE_API_KEY]", brave.EnvKeys)
	}
	if brave.Credentials["BRAVE_API_KEY"] != "brave" {
		t.Errorf("brave-search: credential binding = %v, want BRAVE_API_KEY->brave", brave.Credentials)
	}
}

func TestSeedMCPToolsPreservesAnOperatorsEnabledState(t *testing.T) {
	s := openSeedTestStore(t)
	if err := seedMCPTools(s); err != nil {
		t.Fatalf("first seed: %v", err)
	}

	enabledByOperator := getSeededTool(t, s, "playwright")
	enabledByOperator.Enabled = true
	if _, err := s.UpsertTool(enabledByOperator); err != nil {
		t.Fatalf("enable playwright: %v", err)
	}

	if err := seedMCPTools(s); err != nil {
		t.Fatalf("second seed: %v", err)
	}

	if got := getSeededTool(t, s, "playwright"); !got.Enabled {
		t.Error("playwright: re-seeding disabled an operator-enabled server")
	}
	if got := getSeededTool(t, s, "brave-search"); got.Enabled {
		t.Error("brave-search: re-seeding enabled a server the operator never enabled")
	}
}

// Both seeds run on every boot, so running them twice must not duplicate rows
// or fail on the second pass. The second pass is compared against the first
// rather than against a count of seeds, which would go stale whenever the
// curated list grows.
func TestSeedingTwiceIsIdempotent(t *testing.T) {
	s := openSeedTestStore(t)

	rowsAfterPass := make([]int, 0, 2)
	for pass := 1; pass <= 2; pass++ {
		if err := seedLocalTools(s); err != nil {
			t.Fatalf("pass %d seedLocalTools: %v", pass, err)
		}
		if err := seedMCPTools(s); err != nil {
			t.Fatalf("pass %d seedMCPTools: %v", pass, err)
		}
		rows, err := s.ListTools(toolstore.ListFilter{})
		if err != nil {
			t.Fatalf("pass %d list tools: %v", pass, err)
		}
		rowsAfterPass = append(rowsAfterPass, len(rows))
	}

	if rowsAfterPass[0] <= len(tools.All()) {
		t.Fatalf("the first pass wrote %d rows, want more than the %d local tools — the MCP seed wrote nothing",
			rowsAfterPass[0], len(tools.All()))
	}
	if rowsAfterPass[1] != rowsAfterPass[0] {
		t.Errorf("after seeding twice: %d rows, after once: %d — the seed is not idempotent",
			rowsAfterPass[1], rowsAfterPass[0])
	}
}
