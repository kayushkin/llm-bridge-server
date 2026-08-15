package server

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

// TestOneShotRefusesAHarnessWhoseBinaryIsNotOnPath pins the availability guard
// in handleInstanceOneShot. Nothing reached that handler before this test:
// replacing its `harness.Available(h)` result with a hardcoded `ok := true`
// left the whole internal/server package green, so the one check standing
// between a request and an exec of a binary that isn't there was unheld.
//
// Wrapper binaries resolve through exec.LookPath, so an empty PATH is the
// condition that makes every harness unavailable — the same fixture
// TestPlanConformanceRunReportsEveryHarnessSkippedForAnEmptyPath uses for the
// conformance planner's copy of this call.
//
// The status matters as much as the refusal. 502 says the gateway could not
// reach the thing behind it, which is what a missing wrapper binary is; a 400
// would blame the caller for a request that was well formed.
func TestOneShotRefusesAHarnessWhoseBinaryIsNotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	srv, _, instID := testServerWithInstance(t, msg.HarnessClaudeCode)

	resp := doJSON(t, srv, "POST", "/instances/"+instID+"/oneshot", msg.OneShotRequest{Prompt: "hello"})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("one-shot against an unavailable harness returned %d, want %d\nbody: %s",
			resp.StatusCode, http.StatusBadGateway, body)
	}

	body, _ := io.ReadAll(resp.Body)
	if want := msg.HarnessBinaryName(msg.HarnessClaudeCode); !strings.Contains(string(body), want) {
		t.Errorf("refusal does not name the binary it looked for: got %q, want it to mention %q", body, want)
	}
}
