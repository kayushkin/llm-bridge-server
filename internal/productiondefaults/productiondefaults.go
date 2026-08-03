// Package productiondefaults owns the address of every resource an
// llm-bridge-server process shares with the rest of the machine — the live
// event log, the live databases, the port the gateway listens on — and
// refuses to hand one to a process built by `go test`.
//
// # Why this package exists
//
// config.Load falls back to these addresses when the matching environment
// variable is unset, which is what makes the ordinary case work with no
// configuration. It is also what makes a test dangerous: a test that calls
// config.Load with a clean environment gets the production log-store, the
// production agent-store database and the production listen port, and it
// writes to all three.
//
// That is not hypothetical. On 2026-08-01 a throwaway gateway booted with
// LLMBRIDGE_DB_PATH pointed at a temp file and LLMBRIDGE_LOG_STORE_URL left
// unset imported 2,863 duplicate transcripts into the live log-store — it
// isolated its local state and not its remote state (noteboard bf06041c).
// The same shape is one forgotten field away in every test in this repo.
//
// Until now the only thing standing between `go test ./internal/server/` and
// the live event log was that six test files each remembered to spell
// "http://localhost:0" into their config literal. A string repeated in six
// places is a convention, not a guarantee, and the seventh test to be written
// inherits nothing from it. This package makes the guarantee structural: the
// addresses live in one place, and reaching one of them from a test binary
// panics with the field that leaked and the variable that would have set it.
//
// Scope: this guards the test binary only. Whether a *production* gateway
// should be allowed to boot pointed at a log-store it did not name is a
// separate, open question — noteboard 813dd57f prices four answers — and
// nothing here pre-empts it.
package productiondefaults

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// AllowInTestsEnvironmentVariable names the opt-in that lets a test reach a
// production address on purpose. Set it to any non-empty value. It exists for
// an integration test run deliberately against the live services; a unit test
// should never need it.
const AllowInTestsEnvironmentVariable = "LLMBRIDGE_ALLOW_PRODUCTION_IN_TESTS"

// Addresses of the services this gateway talks to over HTTP.
const (
	ListenAddr         = ":8160"
	LogStoreURL        = "http://localhost:8175"
	ToolStoreURL       = "http://localhost:8302"
	PermissionStoreURL = "http://localhost:8304"
)

// The on-disk state this gateway owns or shares. These are functions rather
// than constants because they are relative to HOME, and a test that relocates
// HOME has relocated the production state along with it — the guard should
// compare against wherever HOME points now, not wherever it pointed at
// package-initialisation time.

func BridgeDatabasePath() string {
	return filepath.Join(homeDirectory(), ".llm-bridge", "bridge.db")
}

func AgentStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "agent-store", "agents.db")
}

func MemoryStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "memory-store", "memory.db")
}

func HarnessStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "harness-store", "harness.db")
}

func HookStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "hook-store", "hooks.db")
}

func ModelStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "model-store", "store.db")
}

func SnapshotStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "snapshot-store", "snapshots.db")
}

func SnapshotStoreGitPath() string {
	return filepath.Join(homeDirectory(), ".config", "snapshot-store", "snapshots.git")
}

func BridgePreferencesPath() string {
	return filepath.Join(homeDirectory(), ".config", "llm-bridge", "bridge-prefs.json")
}

func ConformancePath() string {
	return filepath.Join(homeDirectory(), ".config", "llm-bridge", "conformance.json")
}

// EnvironmentVariableByConfigField names, for each guarded config field, the
// variable that overrides it. Reported in the panic so the message says how to
// fix the test rather than only what it did wrong.
var EnvironmentVariableByConfigField = map[string]string{
	"ListenAddr":         "LLMBRIDGE_LISTEN_ADDR",
	"DBPath":             "LLMBRIDGE_DB_PATH",
	"AgentStoreDB":       "LLMBRIDGE_AGENT_DB",
	"MemoryStoreDB":      "LLMBRIDGE_MEMORY_DB",
	"HarnessStoreDB":     "LLMBRIDGE_HARNESS_DB",
	"HookStoreDB":        "LLMBRIDGE_HOOK_DB",
	"ModelStoreDB":       "LLMBRIDGE_MODEL_STORE_DB",
	"BridgePrefsPath":    "LLMBRIDGE_BRIDGE_PREFS",
	"ConformancePath":    "LLMBRIDGE_CONFORMANCE_PATH",
	"LogStoreURL":        "LLMBRIDGE_LOG_STORE_URL",
	"ToolStoreURL":       "LLMBRIDGE_TOOL_STORE_URL",
	"PermissionStoreURL": "LLMBRIDGE_PERMISSION_STORE_URL",
	"SnapshotStoreDB":    "LLMBRIDGE_SNAPSHOT_DB",
	"SnapshotStoreGit":   "LLMBRIDGE_SNAPSHOT_GIT",
}

// AddressByConfigField returns the production address each guarded config
// field holds when nothing overrides it.
func AddressByConfigField() map[string]string {
	return map[string]string{
		"ListenAddr":         ListenAddr,
		"DBPath":             BridgeDatabasePath(),
		"AgentStoreDB":       AgentStoreDatabasePath(),
		"MemoryStoreDB":      MemoryStoreDatabasePath(),
		"HarnessStoreDB":     HarnessStoreDatabasePath(),
		"HookStoreDB":        HookStoreDatabasePath(),
		"ModelStoreDB":       ModelStoreDatabasePath(),
		"BridgePrefsPath":    BridgePreferencesPath(),
		"ConformancePath":    ConformancePath(),
		"LogStoreURL":        LogStoreURL,
		"ToolStoreURL":       ToolStoreURL,
		"PermissionStoreURL": PermissionStoreURL,
		"SnapshotStoreDB":    SnapshotStoreDatabasePath(),
		"SnapshotStoreGit":   SnapshotStoreGitPath(),
	}
}

// UsedByConfigField reports which of the given field values point at a
// production address. The argument is field name → the value the process is
// about to use; fields this package does not guard are ignored, so a caller
// may pass one field or all of them.
func UsedByConfigField(values map[string]string) map[string]string {
	addresses := AddressByConfigField()
	used := make(map[string]string)
	for field, value := range values {
		if address, guarded := addresses[field]; guarded && value == address {
			used[field] = address
		}
	}
	return used
}

// PanicIfUsedUnderTest stops a `go test` binary that is about to read or write
// production state. It is a no-op outside a test binary, and a no-op inside
// one when AllowInTestsEnvironmentVariable is set.
//
// It panics rather than returning an error on purpose. The callers are
// constructors with no error to return, and a test that has already opened the
// live database has done the damage — the only useful moment to fail is before
// the value is used, and the only reliable way to be heard from inside a
// constructor is to stop the process.
func PanicIfUsedUnderTest(values map[string]string) {
	if !testing.Testing() || os.Getenv(AllowInTestsEnvironmentVariable) != "" {
		return
	}
	used := UsedByConfigField(values)
	if len(used) == 0 {
		return
	}

	fields := make([]string, 0, len(used))
	for field := range used {
		fields = append(fields, field)
	}
	sort.Strings(fields)

	var message strings.Builder
	message.WriteString("productiondefaults: a test is about to use live production state.\n")
	message.WriteString("These config fields hold the address this machine's real services run on:\n")
	for _, field := range fields {
		message.WriteString(fmt.Sprintf("  %s = %q (override with %s)\n",
			field, used[field], EnvironmentVariableByConfigField[field]))
	}
	message.WriteString("\nPoint them at a t.TempDir() path, an httptest server, or an unreachable\n")
	message.WriteString("address such as \"http://localhost:0\". If this test really is meant to run\n")
	message.WriteString("against the live services, set " + AllowInTestsEnvironmentVariable + "=1.")
	panic(message.String())
}

func homeDirectory() string {
	return os.Getenv("HOME")
}
