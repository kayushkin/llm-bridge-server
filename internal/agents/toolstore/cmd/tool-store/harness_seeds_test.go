package main

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	toolstore "github.com/kayushkin/tool-store"
)

func openSeedTestStore(t *testing.T) *toolstore.Store {
	t.Helper()
	store, err := toolstore.Open(filepath.Join(t.TempDir(), "ts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSeedHarnessToolsCreatesEveryToolEnabled(t *testing.T) {
	store := openSeedTestStore(t)
	if err := seedHarnessTools(store); err != nil {
		t.Fatal(err)
	}
	for harness, seeds := range map[string][]harnessToolSeed{harnessClaudeCode: claudeCodeTools, harnessCodex: codexTools} {
		rows, err := store.ListTools(toolstore.ListFilter{Kind: toolstore.KindHarness, Harness: harness})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != len(seeds) {
			t.Fatalf("%s: %d rows for %d seeds", harness, len(rows), len(seeds))
		}
		for _, row := range rows {
			if !row.Enabled || row.Description == "" || row.Name != harness+"."+row.HarnessToolName {
				t.Fatalf("seeded row wrong: %+v", row)
			}
		}
	}
	if len(claudeCodeTools) != 37 || len(codexTools) != 12 {
		t.Fatalf("seed set changed size: claude_code %d, codex %d", len(claudeCodeTools), len(codexTools))
	}
}

func TestSeedHarnessToolsTagsExactlyTheShellRunners(t *testing.T) {
	store := openSeedTestStore(t)
	if err := seedHarnessTools(store); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListTools(toolstore.ListFilter{Kind: toolstore.KindHarness, Tag: tagRunsCommands})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, row := range rows {
		names = append(names, row.Name)
	}
	want := []string{"claude_code.Bash", "claude_code.Monitor", "codex.shell_tool", "codex.unified_exec"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("runs-commands tools = %v, want %v", names, want)
	}
}

func TestSeedHarnessToolsLeavesAnExistingRowExactlyAsItIs(t *testing.T) {
	store := openSeedTestStore(t)
	stale := &toolstore.Tool{
		Name: "claude_code.Bash", Kind: toolstore.KindHarness, Harness: harnessClaudeCode, HarnessToolName: "Bash",
		Description: "operator's words", Tags: []string{"old"}, Enabled: false,
	}
	if _, err := store.UpsertTool(stale); err != nil {
		t.Fatal(err)
	}
	if err := seedHarnessTools(store); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetToolByName("claude_code.Bash")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != stale.ID || got.Enabled || got.Description != "operator's words" {
		t.Fatalf("seeder overwrote what it must keep: %+v", got)
	}
	if !reflect.DeepEqual(got.Tags, []string{"old"}) {
		t.Fatalf("seeder rewrote the tags the database holds: %v", got.Tags)
	}
}

func TestSeedHarnessToolsRefusesANameAnotherKindHolds(t *testing.T) {
	store := openSeedTestStore(t)
	if _, err := store.UpsertTool(&toolstore.Tool{Name: "codex.web_search", Kind: toolstore.KindLocal, Local: &toolstore.LocalSpec{Symbol: "x"}}); err != nil {
		t.Fatal(err)
	}
	if err := seedHarnessTools(store); err == nil {
		t.Fatal("seeder took over a kind=local row")
	}
}

func TestSeedHarnessToolsLeavesAToolAHarnessReportedAlone(t *testing.T) {
	store := openSeedTestStore(t)
	if err := seedHarnessTools(store); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordObservedHarnessTools(context.Background(), toolstore.ObservedHarnessToolsRequest{
		Harness: harnessClaudeCode, ToolNames: []string{"Read", "Frobnicate"},
	}); err != nil {
		t.Fatal(err)
	}
	reported, err := store.GetToolByName("claude_code.Frobnicate")
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.GetToolByName("claude_code.Read")
	if err != nil {
		t.Fatal(err)
	}
	if err := seedHarnessTools(store); err != nil {
		t.Fatal(err)
	}
	reportedAfter, err := store.GetToolByName("claude_code.Frobnicate")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reportedAfter, reported) {
		t.Fatalf("seeding changed a row it does not seed:\nbefore %+v\nafter  %+v", reported, reportedAfter)
	}
	if reportedAfter.Enabled {
		t.Fatal("seeding switched on a tool nobody reviewed")
	}
	readAfter, err := store.GetToolByName("claude_code.Read")
	if err != nil {
		t.Fatal(err)
	}
	if readAfter.LastSeenAt == 0 || readAfter.LastSeenAt != read.LastSeenAt {
		t.Fatalf("seeding touched last_seen_at: %d then %d", read.LastSeenAt, readAfter.LastSeenAt)
	}
}

// A name a harness reported first and the seed file lists later keeps its
// enabled=false and its unreviewed tag: the seed file does not review it.
func TestSeedHarnessToolsKeepsAReportedRowOffWhenItsNameIsSeeded(t *testing.T) {
	store := openSeedTestStore(t)
	if _, err := store.RecordObservedHarnessTools(context.Background(), toolstore.ObservedHarnessToolsRequest{
		Harness: harnessClaudeCode, ToolNames: []string{"Bash"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := seedHarnessTools(store); err != nil {
		t.Fatal(err)
	}
	bash, err := store.GetToolByName("claude_code.Bash")
	if err != nil {
		t.Fatal(err)
	}
	if bash.Enabled || !reflect.DeepEqual(bash.Tags, []string{"unreviewed"}) || bash.LastSeenAt == 0 {
		t.Fatalf("seeded-after-report row: %+v", bash)
	}
}
