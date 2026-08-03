package productiondefaults

import (
	"strings"
	"testing"
)

func TestUsedByConfigFieldReportsOnlyProductionAddresses(t *testing.T) {
	used := UsedByConfigField(map[string]string{
		"LogStoreURL":  LogStoreURL,
		"ToolStoreURL": "http://localhost:0",
		"DBPath":       "/tmp/whatever/test.db",
	})

	if len(used) != 1 {
		t.Fatalf("used = %v, want exactly LogStoreURL", used)
	}
	if used["LogStoreURL"] != LogStoreURL {
		t.Errorf("used[LogStoreURL] = %q, want %q", used["LogStoreURL"], LogStoreURL)
	}
}

// An unguarded field must be ignored rather than treated as safe-by-absence or
// unsafe-by-default. Callers hand this whole config maps, and a field this
// package has no opinion about is not evidence either way.
func TestUsedByConfigFieldIgnoresFieldsItDoesNotGuard(t *testing.T) {
	used := UsedByConfigField(map[string]string{
		"PublicURL":     LogStoreURL,
		"AgentStoreURL": LogStoreURL,
		"ImagesDir":     "images",
	})
	if len(used) != 0 {
		t.Errorf("used = %v, want empty — none of those fields are guarded", used)
	}
}

func TestPanicIfUsedUnderTestIsQuietWhenNothingPointsAtProduction(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("panicked on a clean config: %v", recovered)
		}
	}()
	PanicIfUsedUnderTest(map[string]string{"LogStoreURL": "http://localhost:0"})
}

func TestPanicMessageListsEveryLeakedFieldInAStableOrder(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("did not panic on two production addresses")
		}
		message := recovered.(string)
		toolStore := strings.Index(message, "ToolStoreURL")
		logStore := strings.Index(message, "LogStoreURL")
		if toolStore < 0 || logStore < 0 {
			t.Fatalf("message names %d of 2 leaked fields:\n%s", 2-btoi(toolStore < 0)-btoi(logStore < 0), message)
		}
		// Sorted by field name, so the same two leaks always read the same
		// way and a diff of two failures is meaningful.
		if logStore > toolStore {
			t.Errorf("fields are not sorted by name:\n%s", message)
		}
	}()

	PanicIfUsedUnderTest(map[string]string{
		"ToolStoreURL": ToolStoreURL,
		"LogStoreURL":  LogStoreURL,
	})
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
