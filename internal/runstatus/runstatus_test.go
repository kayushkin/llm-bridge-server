package runstatus

import "testing"

func TestSuccessIsTheOnlyStatusThatIsNotAFailure(t *testing.T) {
	if TerminalStatusIsFailure(Success) {
		t.Fatal("Success was reported as a failure")
	}
	for _, status := range []string{Error, Timeout, Interrupted, Running} {
		if !TerminalStatusIsFailure(status) {
			t.Fatalf("%q was not reported as a failure", status)
		}
	}
}

// TestAnUnknownStatusIsAFailure is the whole reason this is a function and not a
// list. The finish logger's old hand-listed pair is the failure mode being
// guarded against: a status nobody thought to add stayed invisible.
func TestAnUnknownStatusIsAFailure(t *testing.T) {
	for _, status := range []string{"", "cancelled", "killed", "SUCCESS", "succeeded"} {
		if !TerminalStatusIsFailure(status) {
			t.Fatalf("unknown status %q was reported as a success", status)
		}
	}
}

// TestTheVocabularyHasNoDuplicateSpellings guards against a second authoring
// creeping back in under a different constant name.
func TestTheVocabularyHasNoDuplicateSpellings(t *testing.T) {
	seen := map[string]string{}
	for name, value := range map[string]string{
		"Running":     Running,
		"Success":     Success,
		"Error":       Error,
		"Timeout":     Timeout,
		"Interrupted": Interrupted,
	} {
		if other, dup := seen[value]; dup {
			t.Fatalf("%s and %s are both %q", name, other, value)
		}
		seen[value] = name
	}
}
