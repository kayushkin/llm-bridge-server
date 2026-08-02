//go:build harness_binaries

// This file re-runs one written-down measurement against the binaries it is
// about. Run it with:
//
//	go test -tags harness_binaries ./internal/server/ -run TestScaffold
//
// It sits behind a build tag because it executes harness wrapper binaries, and
// two of them do real work before they notice stdin is closed:
// llm-bridge-codex spawns the codex CLI, llm-bridge-kilocode binds a local
// port. Neither is safe on every `go test ./...`, and the binaries are deploy
// artifacts — a clean checkout has none of them, so an untagged version of
// this test would be a no-op everywhere but this host.
package server

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// TestScaffoldHarnessesAreStillScaffolds closes the blind spot the audit in
// harness_capability_columns_test.go declares about itself: those columns fail
// when someone edits harnessCapabilities without re-measuring, but they
// "cannot notice a harness changing in its own repo".
//
// For most of the audit that is unavoidable — whether codex applies a model
// mid-turn is not a question a binary answers from outside. For the six
// harnesses withheld with the reason "scaffold: no dispatcher" it is, because
// that reason is a claim about whether the binary runs at all, and running it
// settles it. llm-bridge-{goose,autohand,dexto,commander,roocode,gemini}/main.go
// is thirty-four lines: it answers -version and -discover to keep the protocol
// honest, then prints "not yet implemented" and exits 2.
//
// The direction that matters is the one the audit warns about and no other
// test can see. If one of these scaffolds is implemented in its own repo, its
// entry here does not become a wrong value that a reviewer might spot — it
// becomes a stale reason silently withholding every capability from a working
// harness, which is the "withheld one is a working feature no UI can reach"
// half of the same defect. Nothing in this repo changes on that day. The
// binary does.
//
// The expectation is read out of auditedColumns rather than written again, so
// there is one list of scaffolds and this test cannot drift from the audit it
// checks.
//
// Discriminator: exit status with stdin closed. The subprocess protocol is
// NDJSON on stdin, so closing it is the protocol's own end-of-input — an
// implemented bridge logs "stdin closed, shutting down" and exits 0, and a
// scaffold never reaches that loop. Measured on this host 2026-08-02: the
// eleven implemented bridges exit 0, these six exit 2.
func TestScaffoldHarnessesAreStillScaffolds(t *testing.T) {
	scaffolds := harnessesAuditedAsScaffolds()
	if len(scaffolds) == 0 {
		t.Fatal("no harness is withheld with a \"scaffold\" reason in auditedColumns; " +
			"either the audit stopped using that word or this test is reading the wrong table")
	}

	checked := 0
	for _, h := range scaffolds {
		path, available := harness.Available(h)
		if !available {
			t.Logf("%s: no binary on PATH, cannot re-measure", h)
			continue
		}
		checked++
		if runs, detail := harnessBinaryStarts(t, path); runs {
			t.Errorf("harness %q is withheld from every capability column with the reason "+
				"\"scaffold: no dispatcher\", but %s now starts (%s); re-measure it against its own "+
				"repo and move it to the has side of the columns it earns — until then the UI hides "+
				"every control of a harness that works", h, path, detail)
		}
	}

	// A run that measured nothing must not read like a run that measured
	// everything: with no binaries on PATH the loop above is silent and
	// passes.
	if checked == 0 {
		t.Skip("none of the audited scaffolds has a binary on PATH; nothing to re-measure")
	}
	t.Logf("re-measured %d of %d audited scaffolds against their binaries", checked, len(scaffolds))
}

// harnessesAuditedAsScaffolds reads the scaffold verdicts back out of the
// audit tables. Deriving the list keeps one source of truth: a scaffold the
// audit stops naming stops being checked here, and one it starts naming is
// picked up with no edit.
func harnessesAuditedAsScaffolds() []msg.Harness {
	seen := map[msg.Harness]bool{}
	var out []msg.Harness
	for _, col := range auditedColumns {
		for h, why := range col.lacks {
			if !strings.Contains(why, "scaffold") || seen[h] {
				continue
			}
			seen[h] = true
			out = append(out, h)
		}
	}
	return out
}

// harnessBinaryStarts runs the binary with stdin already closed and reports
// whether it got as far as the protocol loop. A binary still alive at the
// deadline counts as started: it blocked on something, which a scaffold that
// exits on its second statement never does.
func harnessBinaryStarts(t *testing.T, path string) (bool, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, path)
	cmd.Stdin = strings.NewReader("")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = &bytes.Buffer{}

	err := cmd.Run()
	last := lastStderrLine(stderr.String())
	if ctx.Err() != nil {
		return true, "still running at the deadline: " + last
	}
	if err != nil {
		return false, "exited with " + err.Error() + ": " + last
	}
	return true, "exited 0: " + last
}

func lastStderrLine(s string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	out := strings.TrimSpace(lines[len(lines)-1])
	if len(out) > 160 {
		out = out[:160]
	}
	if out == "" {
		return "(no stderr)"
	}
	return out
}

// TestHarnessBinaryNamesResolve is the cheap half of the same question. A
// capability column is a claim about a binary, and msg.HarnessBinaryName
// returning "" means harness.Available can only ever report the harness
// missing — /harnesses would show it permanently unavailable for a reason
// that has nothing to do with what is deployed.
func TestHarnessBinaryNamesResolve(t *testing.T) {
	for _, h := range allHarnesses {
		if msg.HarnessBinaryName(h) == "" {
			t.Errorf("harness %q has no binary name; harness.Available can only ever "+
				"report it unavailable, whatever is deployed", h)
		}
	}
}
