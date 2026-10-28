package server

import (
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestDisplayNameForDiscoveredSessionRefusesCodexText(t *testing.T) {
	cases := []struct{ prompt, project, want string }{
		{"Which are the recent orders?", "/work/repo", "Which are the recent orders?"},
		{"# AGENTS.md instructions for /work/repo\n\n<INSTRUCTIONS>", "/work/repo", "/work/repo"},
		{"<environment_context>\n  <cwd>/work/repo</cwd>", "/work/repo", "/work/repo"},
		{"", "/work/repo", "/work/repo"},
	}
	for _, c := range cases {
		if got := displayNameForDiscoveredSession(c.prompt, c.project); got != c.want {
			t.Errorf("displayNameForDiscoveredSession(%q, %q) = %q, want %q", c.prompt, c.project, got, c.want)
		}
	}
}

// A session discovered before the wrapper skipped codex's own text, titled
// with its AGENTS.md instructions and with no working folder, is corrected
// the next time a discovery pass reports it.
func TestFillDiscoveredSessionMetadataCorrectsARowDiscoveredEarlier(t *testing.T) {
	srv, st := testServer(t)
	now := time.Now()
	harnessID := "01a17f13-5a13-7950-be14-de83e37aff1d"
	bridgeID, inserted, err := st.UpsertDiscoveredSession(harnessID, harnessID, "# AGENTS.md instructions for /work/repo <INSTRUCTIONS> # Directives", "codex", "", "", "", now, now)
	if err != nil || !inserted {
		t.Fatalf("UpsertDiscoveredSession: inserted=%v err=%v", inserted, err)
	}

	reported := msg.StoredSession{HarnessSessionID: harnessID, Harness: msg.HarnessCodex, Prompt: "Which are the recent orders?", Project: "/work/repo"}
	if !srv.fillDiscoveredSessionMetadata("[test]", bridgeID, reported) {
		t.Fatal("fillDiscoveredSessionMetadata changed nothing")
	}
	row, err := st.GetSession(bridgeID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if row.DisplayName != "Which are the recent orders?" || row.WorkingDir != "/work/repo" {
		t.Errorf("row = (%q, %q), want the typed request and the rollout's cwd", row.DisplayName, row.WorkingDir)
	}
	if srv.fillDiscoveredSessionMetadata("[test]", bridgeID, reported) {
		t.Error("a second pass changed the row again")
	}
}
