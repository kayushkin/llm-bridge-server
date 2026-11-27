package machines

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// standaloneHarnessSchema is the schema of a SQLite harness-store file after
// the server's start-up migrations, with the first schema's
// instance_credentials.max_concurrent column, which older files still have.
const standaloneHarnessSchema = `
CREATE TABLE instance_credentials (instance_id TEXT NOT NULL, credential_id TEXT NOT NULL, priority INTEGER NOT NULL DEFAULT 0,
	max_concurrent INTEGER NOT NULL DEFAULT 1, enabled INTEGER NOT NULL DEFAULT 1, PRIMARY KEY (instance_id, credential_id));
CREATE TABLE harness_types (name TEXT PRIMARY KEY, label TEXT NOT NULL DEFAULT '', emoji TEXT NOT NULL DEFAULT '',
	image TEXT NOT NULL DEFAULT '', updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE machines (id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE, hostname TEXT NOT NULL DEFAULT '', os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '', transport TEXT NOT NULL DEFAULT 'local', ssh_user TEXT NOT NULL DEFAULT '',
	ssh_key_path TEXT NOT NULL DEFAULT '', ssh_port INTEGER NOT NULL DEFAULT 22, default_working_dir TEXT NOT NULL DEFAULT '',
	user TEXT NOT NULL DEFAULT '', notes TEXT NOT NULL DEFAULT '', runner_token_hash TEXT NOT NULL DEFAULT '', last_seen_at DATETIME,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
	emoji TEXT NOT NULL DEFAULT '', computer_use_allowed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE runner_enrollments (id TEXT PRIMARY KEY, passphrase_hash TEXT NOT NULL UNIQUE, expires_at DATETIME NOT NULL,
	used_at DATETIME, consumed_machine_id TEXT, created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE instances (id TEXT PRIMARY KEY, harness_type TEXT NOT NULL, name TEXT NOT NULL, machine_id TEXT NOT NULL,
	working_dir TEXT NOT NULL DEFAULT '', max_concurrent_sessions INTEGER NOT NULL DEFAULT 1, enabled INTEGER NOT NULL DEFAULT 1,
	created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP, updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
`

func writeStandaloneHarnessFile(t *testing.T, extraStatements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "harness.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	statements := append([]string{standaloneHarnessSchema,
		`INSERT INTO harness_types (name, label, emoji, updated_at) VALUES ('claude_code', 'Claude Code', 'c', '2026-11-25 22:43:19')`,
		// Times as the Go driver wrote them, and as RFC 3339.
		`INSERT INTO machines (id, name, hostname, transport, ssh_port, user, runner_token_hash, last_seen_at, created_at, updated_at, emoji, computer_use_allowed)
			VALUES ('m_0123456789abcdef', 'laptop', 'laptop.example', 'runner', 22, 'someone', 'hash-1',
			'2026-11-25 22:43:35.02897481 +0000 UTC', '2026-04-26 21:47:58.760434063 +0000 UTC', '2026-04-26 21:47:58.760434063 +0000 UTC', 'L', 1)`,
		`INSERT INTO machines (id, name, created_at, updated_at) VALUES ('m_localhost', 'localhost', '2026-04-26 21:15:59', '2026-04-26 21:15:59')`,
		`INSERT INTO instances VALUES ('inst_1776397787575066867', 'claude_code', 'laptop-cc', 'm_0123456789abcdef', '/work', 2, 0,
			'2026-04-12T23:54:46Z', '2026-04-13T02:23:00.88255646Z')`,
		// A binding of an instance that is gone and an enrollment of a machine
		// that is gone, as the live file holds.
		`INSERT INTO instance_credentials VALUES ('inst_1776397787575066867', 'credential-1', 0, 3, 1)`,
		`INSERT INTO instance_credentials VALUES ('inst_gone', 'credential-2', 1, 1, 0)`,
		`INSERT INTO runner_enrollments VALUES ('enr_0123456789ab', 'hash-a', '2026-04-26 21:52:57.418550255 +0000 UTC',
			'2026-04-26 21:23:03.186950743 +0000 UTC', 'm_gone', '2026-04-26 21:20:00 +0000 UTC')`,
		`INSERT INTO runner_enrollments (id, passphrase_hash, expires_at, created_at) VALUES ('enr_ba9876543210', 'hash-b',
			'2026-04-27 00:58:31.499828196 +0000 UTC', '2026-04-27 00:43:31.499828196 +0000 UTC')`,
	}, extraStatements...)
	for _, statement := range statements {
		if _, err := source.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func importIntoNewModule(t *testing.T, path string) (*Store, ImportCounts, error) {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	db := postgrestest.NewDatabase(t, module).Open(module)
	counts, err := ImportStandaloneDatabase(context.Background(), db, path)
	return New(db), counts, err
}

func TestImportCopiesEveryHarnessStoreRowAsItIs(t *testing.T) {
	store, counts, err := importIntoNewModule(t, writeStandaloneHarnessFile(t))
	if err != nil {
		t.Fatal(err)
	}
	if counts != (ImportCounts{HarnessTypes: 1, Machines: 2, RunnerEnrollments: 2, Instances: 1, InstanceCredentials: 2}) {
		t.Fatalf("counts: %+v", counts)
	}
	machine, err := store.GetMachineByRunnerTokenHash("hash-1")
	if err != nil {
		t.Fatal(err)
	}
	if machine.ID != "m_0123456789abcdef" || machine.User != "someone" || !machine.ComputerUseAllowed || machine.Emoji != "L" ||
		!machine.LastSeenAt.Equal(time.Date(2026, 11, 25, 22, 43, 35, 28974000, time.UTC)) || machine.CreatedAt.Location() != time.UTC {
		t.Fatalf("machine: %+v", machine)
	}
	instance, err := store.GetInstanceWithMachine("inst_1776397787575066867")
	if err != nil {
		t.Fatal(err)
	}
	if instance.Enabled || instance.MaxConcurrentSessions != 2 || instance.WorkingDir != "/work" || instance.Machine.Name != "laptop" ||
		!instance.UpdatedAt.Equal(time.Date(2026, 4, 13, 2, 23, 0, 882556000, time.UTC)) {
		t.Fatalf("instance: %+v", instance)
	}
	bindings, err := store.ListCredentialInstances("credential-2")
	if err != nil || len(bindings) != 1 || bindings[0].InstanceID != "inst_gone" || bindings[0].Enabled {
		t.Fatalf("binding of an instance that is gone: %+v %v", bindings, err)
	}
	var maximum int
	if err := store.DB().QueryRow(`SELECT max_concurrent FROM instance_credentials WHERE credential_id = 'credential-1'`).Scan(&maximum); err != nil || maximum != 3 {
		t.Fatalf("max_concurrent: %d %v", maximum, err)
	}
	var consumedMachine sql.NullString
	if err := store.DB().QueryRow(`SELECT consumed_machine_id FROM runner_enrollments WHERE id = 'enr_0123456789ab'`).Scan(&consumedMachine); err != nil || consumedMachine.String != "m_gone" {
		t.Fatalf("consumed machine: %v %v", consumedMachine, err)
	}
	harnessType, err := store.GetHarnessType("claude_code")
	if err != nil || harnessType.Label != "Claude Code" {
		t.Fatalf("harness type: %+v %v", harnessType, err)
	}

	if _, err := ImportStandaloneDatabase(context.Background(), store.DB(), writeStandaloneHarnessFile(t)); err == nil || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second import: %v", err)
	}
}

func TestImportTakesAFileWithoutTheOldCredentialColumn(t *testing.T) {
	path := writeStandaloneHarnessFile(t, `ALTER TABLE instance_credentials DROP COLUMN max_concurrent`)
	store, counts, err := importIntoNewModule(t, path)
	if err != nil || counts.InstanceCredentials != 2 {
		t.Fatalf("import: %+v %v", counts, err)
	}
	var maximum int
	if err := store.DB().QueryRow(`SELECT max_concurrent FROM instance_credentials WHERE credential_id = 'credential-1'`).Scan(&maximum); err != nil || maximum != 1 {
		t.Fatalf("max_concurrent: %d %v", maximum, err)
	}
}

func TestImportRefusesAHarnessFileItCannotCopyWhole(t *testing.T) {
	cases := map[string]struct {
		statement string
		want      string
	}{
		"an unknown table":           {`CREATE TABLE machine_notes (id TEXT)`, "machine_notes"},
		"the old inline host column": {`ALTER TABLE instances ADD COLUMN host TEXT NOT NULL DEFAULT ''`, "host"},
		"a time that does not parse": {`UPDATE machines SET created_at = 'yesterday' WHERE id = 'm_localhost'`, "not a time"},
	}
	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := importIntoNewModule(t, writeStandaloneHarnessFile(t, test.statement)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("import: %v, want an error naming %s", err, test.want)
			}
		})
	}
}
