package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
)

// clearGuardedEnvironment blanks every variable that overrides a guarded
// field, so Load falls back to the production address for all of them.
// envOr treats an empty value as unset, and t.Setenv restores the previous
// value when the test ends — which matters here, because the process running
// this test may be a real gateway's environment.
func clearGuardedEnvironment(t *testing.T) {
	t.Helper()
	for _, variable := range productiondefaults.EnvironmentVariableByConfigField {
		t.Setenv(variable, "")
	}
}

func TestLoadPanicsWhenATestWouldGetProductionState(t *testing.T) {
	clearGuardedEnvironment(t)

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("Load with a clean environment did not panic; a test can reach production state")
		}
		message, ok := recovered.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", recovered)
		}
		// The message has to be actionable on its own: the field that
		// leaked, the address it leaked to, and the variable that fixes it.
		for _, want := range []string{
			"LogStoreURL",
			productiondefaults.LogStoreURL,
			"LLMBRIDGE_LOG_STORE_URL",
			"DBPath",
			productiondefaults.BridgeDatabasePath(),
			productiondefaults.AllowInTestsEnvironmentVariable,
		} {
			if !strings.Contains(message, want) {
				t.Errorf("panic message does not mention %q:\n%s", want, message)
			}
		}
	}()

	_ = Load()
}

func TestLoadAcceptsAnEnvironmentThatOverridesEveryGuardedField(t *testing.T) {
	dir := t.TempDir()
	for field, variable := range productiondefaults.EnvironmentVariableByConfigField {
		t.Setenv(variable, filepath.Join(dir, field))
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Load panicked on a fully-overridden environment: %v", recovered)
		}
	}()

	cfg := Load()
	if used := productiondefaults.UsedByConfigField(cfg.GuardedAddresses()); len(used) != 0 {
		t.Errorf("overridden config still points at production: %v", used)
	}
}

func TestOptInLetsATestReachProductionOnPurpose(t *testing.T) {
	clearGuardedEnvironment(t)
	t.Setenv(productiondefaults.AllowInTestsEnvironmentVariable, "1")

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("Load panicked despite %s being set: %v",
				productiondefaults.AllowInTestsEnvironmentVariable, recovered)
		}
	}()

	_ = Load()
}

// TestLoadFallbacksMatchTheGuardedAddresses is the drift test, and it is the
// one that keeps the guard honest.
//
// The guard recognises production by comparing Load's output against
// productiondefaults' table. If someone moves a default in Load and not in the
// table — or the other way round — nothing breaks, no test reddens, and the
// guard quietly stops covering that field while still looking like it does.
// This asserts the two agree on every guarded field at once.
func TestLoadFallbacksMatchTheGuardedAddresses(t *testing.T) {
	clearGuardedEnvironment(t)
	t.Setenv(productiondefaults.AllowInTestsEnvironmentVariable, "1")

	cfg := Load()
	actual := cfg.GuardedAddresses()

	for field, address := range productiondefaults.AddressByConfigField() {
		got, present := actual[field]
		if !present {
			t.Errorf("Config.GuardedAddresses omits %q, so the guard never sees it", field)
			continue
		}
		if got != address {
			t.Errorf("Load's fallback for %s = %q, productiondefaults says %q — the guard is blind to this field",
				field, got, address)
		}
	}
}

// TestGuardedFieldSetsAgree catches the other half of the same drift: a field
// added to one of the three places and not the others.
func TestGuardedFieldSetsAgree(t *testing.T) {
	addresses := fieldNames(productiondefaults.AddressByConfigField())
	variables := fieldNames(productiondefaults.EnvironmentVariableByConfigField)
	guarded := fieldNames((&Config{}).GuardedAddresses())

	if strings.Join(addresses, ",") != strings.Join(variables, ",") {
		t.Errorf("AddressByConfigField and EnvironmentVariableByConfigField disagree:\n  addresses: %v\n  variables: %v",
			addresses, variables)
	}
	if strings.Join(addresses, ",") != strings.Join(guarded, ",") {
		t.Errorf("AddressByConfigField and Config.GuardedAddresses disagree:\n  addresses: %v\n  guarded:   %v",
			addresses, guarded)
	}
}

// TestEveryGuardedVariableIsReadByLoad proves the override named in the panic
// message actually works. A guard that recommends a variable Load never reads
// would send its reader in a circle.
func TestEveryGuardedVariableIsReadByLoad(t *testing.T) {
	for field, variable := range productiondefaults.EnvironmentVariableByConfigField {
		t.Run(field, func(t *testing.T) {
			clearGuardedEnvironment(t)
			t.Setenv(productiondefaults.AllowInTestsEnvironmentVariable, "1")
			sentinel := filepath.Join(t.TempDir(), "override")
			t.Setenv(variable, sentinel)

			if got := Load().GuardedAddresses()[field]; got != sentinel {
				t.Errorf("%s did not reach %s: got %q, want %q", variable, field, got, sentinel)
			}
		})
	}
}

// TestHomeRelativeAddressesFollowHome documents why the on-disk addresses are
// functions rather than constants: a test that relocates HOME has relocated
// the state the guard is protecting, and the guard has to follow it.
func TestHomeRelativeAddressesFollowHome(t *testing.T) {
	relocated := t.TempDir()
	t.Setenv("HOME", relocated)

	if got := productiondefaults.BridgeDatabasePath(); !strings.HasPrefix(got, relocated) {
		t.Errorf("BridgeDatabasePath() = %q, want a path under the relocated HOME %q", got, relocated)
	}
	if _, err := os.Stat(relocated); err != nil {
		t.Fatalf("relocated HOME is unusable: %v", err)
	}
}

func fieldNames(m map[string]string) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
