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
	ListenAddr = ":8160"
	// PinchtabURL is where tool-store's in-process browser tool finds
	// PinchTab with nothing set.
	PinchtabURL    = "http://localhost:9867"
	MailstackURL   = "http://localhost:8195"
	HealthcheckURL = "http://localhost:8099"
	// kanban-store, principal-store and grant-store run in this process, so
	// they have no address here and no row in the three tables below.
)

// The on-disk state this gateway owns or shares. These are functions rather
// than constants because they are relative to HOME, and a test that relocates
// HOME has relocated the production state along with it — the guard should
// compare against wherever HOME points now, not wherever it pointed at
// package-initialization time.

func BridgeDatabasePath() string {
	return filepath.Join(homeDirectory(), ".llm-bridge", "bridge.db")
}

// SessionFilesDirectory is where files shared into sessions are copied for
// their agents to read.
func SessionFilesDirectory() string {
	return filepath.Join(homeDirectory(), ".llm-bridge", "session-files")
}

// OperationsDatabasePath is where operations, their events and leases are kept.
func OperationsDatabasePath() string {
	return filepath.Join(homeDirectory(), ".llm-bridge", "operations.db")
}

// SkillStoreDataDirectory is where the skill-store module keeps skill files
// and cloned sources. It is under this server's own folder, not the standalone
// store's ~/.config/skill-store, so the module and a standalone store still
// running beside it never prune each other's files.
func SkillStoreDataDirectory() string {
	return filepath.Join(homeDirectory(), ".llm-bridge", "skill-store")
}

// FileStoreDataDirectory is where the file-store module keeps the bytes of
// every file (blobs/, named by their SHA-256). It is under this server's own
// folder, not the standalone store's ~/.config/file-store, so the module and
// a standalone store still running beside it never purge each other's
// blobs.
func FileStoreDataDirectory() string {
	return filepath.Join(homeDirectory(), ".llm-bridge", "file-store")
}

// WorkGraphHookSpoolPath is the file work-graph-store's git hook appends ref
// updates to and the store's ingest reads. The hook reads no settings, so
// this is the one place both agree on.
func WorkGraphHookSpoolPath() string {
	return filepath.Join(homeDirectory(), ".local", "state", "work-graph-store", "ref-updates.jsonl")
}

// SkillStoreInstallDirectory is where an installed skill is copied: where
// Claude Code reads a user's skills.
func SkillStoreInstallDirectory() string {
	return filepath.Join(homeDirectory(), ".claude", "skills")
}

func HarnessStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "harness-store", "harness.db")
}

func HookStoreDatabasePath() string {
	return filepath.Join(homeDirectory(), ".config", "hook-store", "hooks.db")
}

// StandaloneModelStoreDatabasePath is the standalone model-store's SQLite
// file, which this server read until the registry moved into Postgres. Only
// the live harness test reads it now, to import a copy.
func StandaloneModelStoreDatabasePath() string {
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
	"ListenAddr":       "LLMBRIDGE_LISTEN_ADDR",
	"DBPath":           "LLMBRIDGE_DB_PATH",
	"HarnessStoreDB":   "LLMBRIDGE_HARNESS_DB",
	"HookStoreDB":      "LLMBRIDGE_HOOK_DB",
	"BridgePrefsPath":  "LLMBRIDGE_BRIDGE_PREFS",
	"ConformancePath":  "LLMBRIDGE_CONFORMANCE_PATH",
	"MailstackURL":     "LLMBRIDGE_MAILSTACK_URL",
	"HealthcheckURL":   "LLMBRIDGE_HEALTHCHECK_URL",
	"SnapshotStoreDB":  "LLMBRIDGE_SNAPSHOT_DB",
	"SnapshotStoreGit": "LLMBRIDGE_SNAPSHOT_GIT",
	"OperationsDBPath": "LLMBRIDGE_OPERATIONS_DB",
}

// AddressByConfigField returns the production address each guarded config
// field holds when nothing overrides it.
func AddressByConfigField() map[string]string {
	return map[string]string{
		"ListenAddr":       ListenAddr,
		"DBPath":           BridgeDatabasePath(),
		"HarnessStoreDB":   HarnessStoreDatabasePath(),
		"HookStoreDB":      HookStoreDatabasePath(),
		"BridgePrefsPath":  BridgePreferencesPath(),
		"ConformancePath":  ConformancePath(),
		"MailstackURL":     MailstackURL,
		"HealthcheckURL":   HealthcheckURL,
		"SnapshotStoreDB":  SnapshotStoreDatabasePath(),
		"SnapshotStoreGit": SnapshotStoreGitPath(),
		"OperationsDBPath": OperationsDatabasePath(),
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
