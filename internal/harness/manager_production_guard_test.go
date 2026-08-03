package harness

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// NewManager is the only place in this repo a log-store client is built, so it
// is the last point at which a test can be stopped before it writes into the
// live event log. config.Load makes the same check, but every test in this
// repo builds its config.Config as a literal and so never goes through Load —
// which is exactly the gap this covers.

func TestNewManagerRefusesTheProductionLogStoreUnderTest(t *testing.T) {
	st := temporaryStore(t)

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("NewManager accepted the production log-store URL inside a test binary")
		}
		message, ok := recovered.(string)
		if !ok {
			t.Fatalf("panic value is %T, want string", recovered)
		}
		for _, want := range []string{"LogStoreURL", productiondefaults.LogStoreURL, "LLMBRIDGE_LOG_STORE_URL"} {
			if !strings.Contains(message, want) {
				t.Errorf("panic message does not mention %q:\n%s", want, message)
			}
		}
	}()

	_ = NewManager(st, productiondefaults.LogStoreURL, "", "", 0, nil)
}

func TestNewManagerAcceptsAnUnreachableLogStore(t *testing.T) {
	st := temporaryStore(t)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("NewManager panicked on a deliberately-unreachable log-store: %v", recovered)
		}
	}()

	if m := NewManager(st, "http://localhost:0", "", "", 0, nil); m == nil {
		t.Fatal("NewManager returned nil")
	}
}

func TestNewManagerAcceptsTheProductionLogStoreWhenOptedIn(t *testing.T) {
	t.Setenv(productiondefaults.AllowInTestsEnvironmentVariable, "1")
	st := temporaryStore(t)

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("NewManager panicked despite %s being set: %v",
				productiondefaults.AllowInTestsEnvironmentVariable, recovered)
		}
	}()

	if m := NewManager(st, productiondefaults.LogStoreURL, "", "", 0, nil); m == nil {
		t.Fatal("NewManager returned nil")
	}
}

func temporaryStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
