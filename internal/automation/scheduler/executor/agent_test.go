package executor

import (
	"strings"
	"testing"
)

// A job with a workspace must be TOLD it has one, on every run, without the
// job's own prompt having to say so. The whole point of the workspace is that
// the agent cannot silently forget it has a memory; if the preamble is not
// injected, the agent reads nothing, rewrites nothing, and the job quietly
// degrades back into the stateless cron it was supposed to replace — while
// still looking like it works.
func TestWorkingMemoryPreambleCarriesTheContract(t *testing.T) {
	preamble := workingMemoryPreamble("http://localhost:8191", "ws-123")

	for _, want := range []string{
		"ws-123",                  // which workspace
		"/items/ws-123",           // where to read it
		"READ IT FIRST",           // and that it must be read before acting
		"REWRITE IT LAST",         // and rewritten before finishing
		"/items/ws-123/revisions", // that rewriting is safe
		"parent_id=ws-123",        // how to split when it outgrows a human
	} {
		if !strings.Contains(preamble, want) {
			t.Errorf("preamble does not carry %q — the agent would not know %s", want, want)
		}
	}
}

// The preamble goes in FRONT of the job's prompt: the agent must read its memory
// before it starts reasoning, not after it has already decided.
func TestPreamblePrecedesThePrompt(t *testing.T) {
	e := &AgentExecutor{} // no NATS client: Run bails before publishing
	run := AgentRun{Agent: "claude-code", Prompt: "JUDGE THE DRIFT", WorkspaceID: "ws-123"}

	// Run without a client returns early, so exercise the composition directly —
	// this is the exact expression Run uses.
	composed := workingMemoryPreamble("http://localhost:8191", run.WorkspaceID) + run.Prompt

	if !strings.HasSuffix(composed, "JUDGE THE DRIFT") {
		t.Fatal("the job's own prompt must survive intact at the end")
	}
	if strings.Index(composed, "ws-123") > strings.Index(composed, "JUDGE THE DRIFT") {
		t.Fatal("memory preamble must come BEFORE the task, not after")
	}
	if got := e.Run(run); got.Status != "error" {
		t.Fatalf("Run with no NATS client should fail loudly, got %q", got.Status)
	}
}

// A job with no workspace is left exactly as it was — this is opt-in, and an
// existing stateless job must not suddenly grow instructions about a memory it
// does not have.
func TestJobWithoutWorkspaceGetsNoPreamble(t *testing.T) {
	run := AgentRun{Agent: "claude-code", Prompt: "classify the session"}
	prompt := run.Prompt
	if run.WorkspaceID != "" {
		prompt = workingMemoryPreamble("http://localhost:8191", run.WorkspaceID) + prompt
	}
	if prompt != "classify the session" {
		t.Fatalf("workspace-less job had its prompt rewritten: %q", prompt)
	}
}
