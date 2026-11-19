package harnessstore

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// oldInstancesDDL reconstructs the pre-machine-split instances schema: SSH
// connection details lived inline on each instance row instead of on a
// referenced machines row.
const oldInstancesDDL = `
CREATE TABLE instances (
    id                      TEXT PRIMARY KEY,
    harness_type            TEXT NOT NULL,
    name                    TEXT NOT NULL,
    host                    TEXT NOT NULL DEFAULT '',
    transport               TEXT NOT NULL DEFAULT 'local',
    ssh_user                TEXT NOT NULL DEFAULT '',
    ssh_key_path            TEXT NOT NULL DEFAULT '',
    ssh_port                INTEGER NOT NULL DEFAULT 22,
    working_dir             TEXT NOT NULL DEFAULT '',
    max_concurrent_sessions INTEGER NOT NULL DEFAULT 1,
    enabled                 INTEGER NOT NULL DEFAULT 1,
    created_at              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

// TestMigrateInstancesToMachines seeds a database with the old inline-SSH
// instances schema, then opens it through Open() — which must run the
// machine-split migration — and verifies instances were rewritten to
// reference newly-minted machine rows.
func TestMigrateInstancesToMachines(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Seed the legacy schema directly, bypassing Open()'s migrations.
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	if _, err := db.Exec(oldInstancesDDL); err != nil {
		t.Fatalf("create old instances: %v", err)
	}
	// Two instances share host alpha (one machine); a third on host bravo
	// (a second machine). Distinct (transport,host,ssh_*) tuples → machines.
	seed := []struct {
		id, harness, name, host, transport, sshUser string
	}{
		{"i_1", "claude_code", "cc-1", "alpha", "ssh", "deploy"},
		{"i_2", "codex", "cx-1", "alpha", "ssh", "deploy"},
		{"i_3", "claude_code", "cc-2", "bravo", "ssh", "deploy"},
	}
	for _, r := range seed {
		if _, err := db.Exec(`
			INSERT INTO instances (id, harness_type, name, host, transport, ssh_user, ssh_key_path, ssh_port, working_dir, max_concurrent_sessions, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, '/key', 22, '/w', 2, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`,
			r.id, r.harness, r.name, r.host, r.transport, r.sshUser); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	// Open() must detect the legacy 'host' column and migrate in place.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open (triggers migration): %v", err)
	}
	defer s.Close()

	// The legacy 'host' column is gone; the new 'machine_id' column exists.
	if has, err := columnExists(s.DB(), "instances", "host"); err != nil {
		t.Fatalf("columnExists host: %v", err)
	} else if has {
		t.Error("legacy 'host' column still present after migration")
	}
	if has, err := columnExists(s.DB(), "instances", "machine_id"); err != nil {
		t.Fatalf("columnExists machine_id: %v", err)
	} else if !has {
		t.Error("'machine_id' column missing after migration")
	}

	// Two distinct connection tuples → two machine rows.
	machines, err := s.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(machines) != 2 {
		t.Fatalf("got %d machines, want 2", len(machines))
	}
	byHost := map[string]string{} // hostname → machine id
	for _, m := range machines {
		byHost[m.Hostname] = m.ID
		if m.Transport != "ssh" || m.SSHUser != "deploy" {
			t.Errorf("machine %s lost connection detail: %+v", m.ID, m)
		}
	}
	if byHost["alpha"] == "" || byHost["bravo"] == "" {
		t.Fatalf("machines missing expected hosts: %v", byHost)
	}

	// All three instances survived, pointing at the right machine.
	insts, err := s.ListInstances()
	if err != nil {
		t.Fatalf("ListInstances: %v", err)
	}
	if len(insts) != 3 {
		t.Fatalf("got %d instances, want 3", len(insts))
	}
	wantMachine := map[string]string{
		"i_1": byHost["alpha"],
		"i_2": byHost["alpha"],
		"i_3": byHost["bravo"],
	}
	for _, in := range insts {
		if in.MachineID != wantMachine[in.ID] {
			t.Errorf("instance %s MachineID = %q, want %q", in.ID, in.MachineID, wantMachine[in.ID])
		}
		if in.WorkingDir != "/w" || in.MaxConcurrentSessions != 2 {
			t.Errorf("instance %s lost fields: %+v", in.ID, in)
		}
	}

	// Re-opening the now-migrated DB is a no-op (idempotent migration).
	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("re-Open migrated db: %v", err)
	}
	defer s2.Close()
	if m, err := s2.ListMachines(); err != nil || len(m) != 2 {
		t.Errorf("re-open machines = %d, %v; want 2", len(m), err)
	}
}
