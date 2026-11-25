package productiondefaults

import (
	"strings"
	"testing"
)

func TestUsedByConfigFieldReportsOnlyProductionAddresses(t *testing.T) {
	used := UsedByConfigField(map[string]string{
		"HealthcheckURL":     HealthcheckURL,
		"PermissionStoreURL": "http://localhost:0",
		"DBPath":             "/tmp/whatever/test.db",
	})

	if len(used) != 1 {
		t.Fatalf("used = %v, want exactly HealthcheckURL", used)
	}
	if used["HealthcheckURL"] != HealthcheckURL {
		t.Errorf("used[HealthcheckURL] = %q, want %q", used["HealthcheckURL"], HealthcheckURL)
	}
}

// An unguarded field must be ignored rather than treated as safe-by-absence or
// unsafe-by-default. Callers hand this whole config maps, and a field this
// package has no opinion about is not evidence either way.
func TestUsedByConfigFieldIgnoresFieldsItDoesNotGuard(t *testing.T) {
	used := UsedByConfigField(map[string]string{
		"PublicURL":     HealthcheckURL,
		"AgentStoreURL": HealthcheckURL,
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
	PanicIfUsedUnderTest(map[string]string{"HealthcheckURL": "http://localhost:0"})
}

func TestPanicMessageListsEveryLeakedFieldInAStableOrder(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("did not panic on two production addresses")
		}
		message := recovered.(string)
		usageStore := strings.Index(message, "UsageStoreURL")
		healthcheck := strings.Index(message, "HealthcheckURL")
		if usageStore < 0 || healthcheck < 0 {
			t.Fatalf("message names %d of 2 leaked fields:\n%s", 2-btoi(usageStore < 0)-btoi(healthcheck < 0), message)
		}
		// Sorted by field name, so the same two leaks always read the same
		// way and a diff of two failures is meaningful.
		if healthcheck > usageStore {
			t.Errorf("fields are not sorted by name:\n%s", message)
		}
	}()

	PanicIfUsedUnderTest(map[string]string{
		"UsageStoreURL":  UsageStoreURL,
		"HealthcheckURL": HealthcheckURL,
	})
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
