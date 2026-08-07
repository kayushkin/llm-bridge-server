package conformance

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeStubHarness writes an executable shell script that answers `-discover`
// with the given stdout text and exit code, and returns its path. It stands in
// for a harness binary so testDiscover can be graded without one.
func writeStubHarness(t *testing.T, stdout string, exitCode int) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "stub-harness")
	script := "#!/bin/sh\n" +
		"printf '%s' " + shellQuote(stdout) + "\n" +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub harness: %v", err)
	}
	return path
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestDiscoverGrading pins the three answers a harness can give to `-discover`
// and the verdict each one earns. The middle case is the one worth having a
// test for: a binary that accepts the flag, exits 0 and writes something that
// is not a []msg.StoredSession has implemented the feature and implemented it
// wrongly, so it must fail rather than be excused as a binary that has never
// heard of the flag.
func TestDiscoverGrading(t *testing.T) {
	tests := []struct {
		name        string
		stdout      string
		exitCode    int
		wantPassed  bool
		wantSkipped bool
		wantErrPart string
	}{
		{
			name:       "empty array passes",
			stdout:     "[]",
			exitCode:   0,
			wantPassed: true,
		},
		{
			name:       "populated array passes",
			stdout:     `[{"session_id":"abc","harness":"stub"}]`,
			exitCode:   0,
			wantPassed: true,
		},
		{
			name:        "flag not implemented skips",
			stdout:      "",
			exitCode:    1,
			wantSkipped: true,
			wantErrPart: "does not support -discover",
		},
		{
			name:        "exit 0 with non-JSON output fails",
			stdout:      "not json",
			exitCode:    0,
			wantErrPart: "does not decode as a JSON array",
		},
		{
			name:        "exit 0 with JSON of the wrong shape fails",
			stdout:      `{"sessions":[]}`,
			exitCode:    0,
			wantErrPart: "does not decode as a JSON array",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			binary := writeStubHarness(t, tc.stdout, tc.exitCode)
			got := testDiscover(context.Background(), binary)

			if got.Feature != FeatureDiscover {
				t.Errorf("feature = %q, want %q", got.Feature, FeatureDiscover)
			}
			if got.Passed != tc.wantPassed {
				t.Errorf("passed = %v, want %v (error: %q)", got.Passed, tc.wantPassed, got.Error)
			}
			if got.Skipped != tc.wantSkipped {
				t.Errorf("skipped = %v, want %v (error: %q)", got.Skipped, tc.wantSkipped, got.Error)
			}
			if tc.wantErrPart != "" && !strings.Contains(got.Error, tc.wantErrPart) {
				t.Errorf("error = %q, want it to contain %q", got.Error, tc.wantErrPart)
			}
		})
	}
}

// TestDiscoverMalformedOutputCountsAsFailure checks the verdict through the
// summary the UI reads, not just the TestResult fields: AddResult tests
// Skipped before Passed, so a Skipped result can never be counted red however
// its Error field reads.
func TestDiscoverMalformedOutputCountsAsFailure(t *testing.T) {
	binary := writeStubHarness(t, "not json", 0)

	hr := &HarnessResult{Harness: "stub", Binary: binary}
	hr.AddResult(testDiscover(context.Background(), binary))

	if hr.Summary.Failed != 1 {
		t.Errorf("summary.failed = %d, want 1 (summary: %+v)", hr.Summary.Failed, hr.Summary)
	}
	if hr.Summary.Skipped != 0 {
		t.Errorf("summary.skipped = %d, want 0 (summary: %+v)", hr.Summary.Skipped, hr.Summary)
	}
	if hr.Supports(FeatureDiscover) {
		t.Error("Supports(discover) = true, want false for a harness whose -discover output does not decode")
	}
}
