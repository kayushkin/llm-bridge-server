package harness

import (
	"slices"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func TestDiscoverableHarnessesIncludesHermes(t *testing.T) {
	got := discoverableHarnesses()
	for _, want := range []msg.Harness{msg.HarnessClaudeCode, msg.HarnessCodex, msg.HarnessHermes} {
		if !slices.Contains(got, want) {
			t.Errorf("discoverableHarnesses() missing %s; got %v", want, got)
		}
	}
}
