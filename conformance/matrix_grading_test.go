package conformance

import "testing"

// The whole point of the Unsupported verdict is that it is not Skipped. A
// harness that answered and cannot do something is a finding; a test that never
// ran is not. Counting them together is what made sixty per cent of the matrix
// unreadable.
func TestAddResultCountsUnsupportedApartFromSkipped(t *testing.T) {
	var hr HarnessResult
	hr.AddResult(TestResult{Feature: FeatureMessage, Passed: true})
	hr.AddResult(TestResult{Feature: FeatureBlock, Unsupported: true, Error: "no block events emitted"})
	hr.AddResult(TestResult{Feature: FeatureFork, Skipped: true, Error: "fork start failed"})
	hr.AddResult(TestResult{Feature: FeatureStart, Error: "timeout waiting for event"})

	want := Summary{Total: 4, Passed: 1, Failed: 1, Skipped: 1, Unsupported: 1}
	if hr.Summary != want {
		t.Errorf("summary = %+v, want %+v", hr.Summary, want)
	}
}

func TestVerdictNamesEachOutcome(t *testing.T) {
	cases := []struct {
		name   string
		result TestResult
		want   string
	}{
		{"passed", TestResult{Passed: true}, "passed"},
		{"failed", TestResult{Error: "boom"}, "failed"},
		{"skipped", TestResult{Skipped: true}, "skipped"},
		{"unsupported", TestResult{Unsupported: true}, "unsupported"},
		// A skipped test reached no verdict, so it cannot also claim the
		// harness lacks the feature.
		{"skipped wins over unsupported", TestResult{Skipped: true, Unsupported: true}, "skipped"},
		// Unsupported is the narrower claim, so it wins over a stray Passed.
		{"unsupported wins over passed", TestResult{Unsupported: true, Passed: true}, "unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.result.Verdict(); got != c.want {
				t.Errorf("verdict = %q, want %q", got, c.want)
			}
		})
	}
}

// Verdict and AddResult must agree, or the per-row label in the UI and the
// summary counts drift apart.
func TestVerdictAgreesWithSummaryCounts(t *testing.T) {
	results := []TestResult{
		{Passed: true},
		{Error: "boom"},
		{Skipped: true},
		{Unsupported: true},
		{Skipped: true, Unsupported: true},
		{Unsupported: true, Passed: true},
	}
	var hr HarnessResult
	counts := map[string]int{}
	for _, r := range results {
		hr.AddResult(r)
		counts[r.Verdict()]++
	}
	if counts["passed"] != hr.Summary.Passed {
		t.Errorf("passed: verdict says %d, summary says %d", counts["passed"], hr.Summary.Passed)
	}
	if counts["failed"] != hr.Summary.Failed {
		t.Errorf("failed: verdict says %d, summary says %d", counts["failed"], hr.Summary.Failed)
	}
	if counts["skipped"] != hr.Summary.Skipped {
		t.Errorf("skipped: verdict says %d, summary says %d", counts["skipped"], hr.Summary.Skipped)
	}
	if counts["unsupported"] != hr.Summary.Unsupported {
		t.Errorf("unsupported: verdict says %d, summary says %d", counts["unsupported"], hr.Summary.Unsupported)
	}
}

// Supports answers "did this harness demonstrate the feature", so an
// unsupported verdict must not read as support.
func TestSupportsIsFalseForUnsupported(t *testing.T) {
	var hr HarnessResult
	hr.AddResult(TestResult{Feature: FeatureHook, Unsupported: true})
	if hr.Supports(FeatureHook) {
		t.Error("Supports should be false for an unsupported feature")
	}
}
