// Package harnessstore provides persistent storage for harness instance
// registry, the machines that host them, credential bindings, and runner
// enrollment records. Static configuration data — runtime state (active
// sessions, credential slots) lives in llm-bridge-server.
package harnessstore

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Store manages harness/machine/enrollment state.
type Store struct {
	db *sql.DB
}

// busyTimeoutMillisecondsWanted is how long a connection waits for a lock
// before giving up. Named so the DSN that sets it and the check that proves it
// took effect cannot drift apart.
const busyTimeoutMillisecondsWanted = 5000

// dataSourceName builds the sqlite DSN for a database path.
//
// busy_timeout MUST be set here rather than with a PRAGMA statement after
// Open: it is a per-connection setting, and *sql.DB is a pool. A one-shot
// db.Exec("PRAGMA busy_timeout=5000") reaches only whichever connection the
// pool happened to hand out, so every other connection keeps SQLite's default
// of 0 and fails instantly on a lock instead of waiting. Putting it in the DSN
// applies it to every connection the pool opens.
//
// _pragma=... is modernc's syntax and it is the only one that works here. The
// mattn/go-sqlite3 spelling (_busy_timeout=5000) is silently ignored by this
// driver — measured, 0 of 8 pooled connections saw it — as is any other
// unrecognized key, which is why Open verifies the value after opening.
//
// ⚠️ foreign_keys is deliberately NOT set here. This database has live rows
// that violate its own REFERENCES clauses (PRAGMA foreign_key_check reports an
// orphan in runner_enrollments and one in instance_credentials), and the schema
// carries ON DELETE CASCADE. Turning enforcement on would start refusing writes
// that have worked for months and would arm a cascade that has never fired, so
// it needs the orphans cleaned and a human's say-so first. Card
// 882d977a-dfaf-47da-9857-205285dba647 holds that decision.
func dataSourceName(dbPath string) string {
	return fmt.Sprintf("%s?_pragma=busy_timeout(%d)", dbPath, busyTimeoutMillisecondsWanted)
}

// Open opens or creates a harness-store database. Migrations run on open.
func Open(dbPath string) (*Store, error) {
	dir := filepath.Dir(dbPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", dataSourceName(dbPath))
	if err != nil {
		return nil, err
	}

	// journal_mode is the one pragma that belongs here rather than in the DSN:
	// it is a property of the database file, so setting it once is permanent.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}

	// An unrecognized DSN key is silently ignored by this driver, so prove the
	// setting actually took effect instead of trusting the connection string.
	var busyTimeoutMilliseconds int
	if err := db.QueryRow("PRAGMA busy_timeout").Scan(&busyTimeoutMilliseconds); err != nil {
		db.Close()
		return nil, fmt.Errorf("read busy_timeout pragma: %w", err)
	}
	if busyTimeoutMilliseconds != busyTimeoutMillisecondsWanted {
		db.Close()
		return nil, fmt.Errorf("busy_timeout is %d, want %d: the DSN did not take effect",
			busyTimeoutMilliseconds, busyTimeoutMillisecondsWanted)
	}

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// DB returns the underlying database connection for advanced queries.
func (s *Store) DB() *sql.DB {
	return s.db
}

func (s *Store) migrate() error {
	// 1. Idempotent: tables that have always existed or are net-new and additive.
	if _, err := s.db.Exec(schemaCommonDDL); err != nil {
		return fmt.Errorf("schema common: %w", err)
	}

	// 2. Instances table — three cases:
	//    (A) doesn't exist (fresh DB): create with the new schema.
	//    (B) old schema (has 'host' column): migrate in place — create the
	//        default linode machine, add machine_id, drop the inline SSH cols.
	//    (C) new schema: done.
	exists, err := tableExists(s.db, "instances")
	if err != nil {
		return fmt.Errorf("check instances: %w", err)
	}
	if !exists {
		if _, err := s.db.Exec(schemaInstancesNewDDL); err != nil {
			return fmt.Errorf("create instances: %w", err)
		}
		return nil
	}

	hasHost, err := columnExists(s.db, "instances", "host")
	if err != nil {
		return fmt.Errorf("check instances.host: %w", err)
	}
	if hasHost {
		if err := migrateInstancesToMachines(s.db); err != nil {
			return fmt.Errorf("migrate instances to machines: %w", err)
		}
	}

	// Additive: machines.emoji was added after the initial machines schema.
	// ALTER TABLE ADD COLUMN is idempotent via the column-existence guard.
	hasEmoji, err := columnExists(s.db, "machines", "emoji")
	if err != nil {
		return fmt.Errorf("check machines.emoji: %w", err)
	}
	if !hasEmoji {
		if _, err := s.db.Exec(`ALTER TABLE machines ADD COLUMN emoji TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("add machines.emoji: %w", err)
		}
	}

	// Additive: machines.computer_use_allowed, the owner's switch for handing
	// a runner's computer-use tool to sessions; off for every existing machine.
	hasComputerUseAllowed, err := columnExists(s.db, "machines", "computer_use_allowed")
	if err != nil {
		return fmt.Errorf("check machines.computer_use_allowed: %w", err)
	}
	if !hasComputerUseAllowed {
		if _, err := s.db.Exec(`ALTER TABLE machines ADD COLUMN computer_use_allowed INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("add machines.computer_use_allowed: %w", err)
		}
	}

	return nil
}

// schemaCommonDDL is the always-applied baseline. CREATE TABLE IF NOT
// EXISTS lets it run cleanly on both fresh and migrated databases.
const schemaCommonDDL = `
CREATE TABLE IF NOT EXISTS harness_types (
    name        TEXT PRIMARY KEY,
    label       TEXT NOT NULL DEFAULT '',
    emoji       TEXT NOT NULL DEFAULT '',
    image       TEXT NOT NULL DEFAULT '',
    updated_at  DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS machines (
    id                       TEXT PRIMARY KEY,
    name                     TEXT NOT NULL UNIQUE,
    emoji                    TEXT NOT NULL DEFAULT '',
    hostname                 TEXT NOT NULL DEFAULT '',
    os                       TEXT NOT NULL DEFAULT '',
    arch                     TEXT NOT NULL DEFAULT '',
    transport                TEXT NOT NULL DEFAULT 'local',
    ssh_user                 TEXT NOT NULL DEFAULT '',
    ssh_key_path             TEXT NOT NULL DEFAULT '',
    ssh_port                 INTEGER NOT NULL DEFAULT 22,
    default_working_dir      TEXT NOT NULL DEFAULT '',
    user                     TEXT NOT NULL DEFAULT '',
    notes                    TEXT NOT NULL DEFAULT '',
    computer_use_allowed     INTEGER NOT NULL DEFAULT 0,
    runner_token_hash        TEXT NOT NULL DEFAULT '',
    last_seen_at             DATETIME,
    created_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at               DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_machines_name ON machines(name);
CREATE INDEX IF NOT EXISTS idx_machines_transport ON machines(transport);

CREATE TABLE IF NOT EXISTS runner_enrollments (
    id                  TEXT PRIMARY KEY,
    passphrase_hash     TEXT NOT NULL UNIQUE,
    expires_at          DATETIME NOT NULL,
    used_at             DATETIME,
    consumed_machine_id TEXT,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (consumed_machine_id) REFERENCES machines(id) ON DELETE SET NULL
);
CREATE INDEX IF NOT EXISTS idx_enrollments_hash ON runner_enrollments(passphrase_hash);

CREATE TABLE IF NOT EXISTS instance_credentials (
    instance_id    TEXT NOT NULL,
    credential_id  TEXT NOT NULL,
    priority       INTEGER NOT NULL DEFAULT 0,
    enabled        INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (instance_id, credential_id)
);
CREATE INDEX IF NOT EXISTS idx_instance_creds_instance ON instance_credentials(instance_id);
CREATE INDEX IF NOT EXISTS idx_instance_creds_credential ON instance_credentials(credential_id);
`

// schemaInstancesNewDDL is the post-split instances schema. Used both for
// fresh databases and as the target shape after migration.
const schemaInstancesNewDDL = `
CREATE TABLE IF NOT EXISTS instances (
    id                      TEXT PRIMARY KEY,
    harness_type            TEXT NOT NULL,
    name                    TEXT NOT NULL,
    machine_id              TEXT NOT NULL,
    working_dir             TEXT NOT NULL DEFAULT '',
    max_concurrent_sessions INTEGER NOT NULL DEFAULT 1,
    enabled                 INTEGER NOT NULL DEFAULT 1,
    created_at              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at              DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (machine_id) REFERENCES machines(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS idx_instances_harness ON instances(harness_type);
CREATE INDEX IF NOT EXISTS idx_instances_enabled ON instances(enabled);
CREATE INDEX IF NOT EXISTS idx_instances_machine ON instances(machine_id);
`

// migrateInstancesToMachines converts the pre-machine-split schema into
// the post-split schema. Old instances had inline host/transport/ssh_*
// columns; we lift the (transport, host, ssh_user, ssh_key_path, ssh_port)
// tuple onto a machine row and replace those columns with a machine_id FK.
//
// Strategy: group existing rows by their (transport, host, ssh_user,
// ssh_key_path, ssh_port) tuple, create one machine per distinct tuple
// (named after the host/transport), then rewrite instances to reference
// it. Runs inside a single transaction so a partial migration cannot
// leave the DB in a mixed state.
func migrateInstancesToMachines(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`
		SELECT id, harness_type, name, host, transport, ssh_user, ssh_key_path, ssh_port,
		       working_dir, max_concurrent_sessions, enabled, created_at, updated_at
		FROM instances
	`)
	if err != nil {
		return fmt.Errorf("read old instances: %w", err)
	}

	type oldInstance struct {
		id, harnessType, name, host, transport, sshUser, sshKeyPath, workingDir, createdAt, updatedAt string
		sshPort, maxConc, enabled                                                                     int
	}
	type machineKey struct {
		transport, host, sshUser, sshKeyPath string
		sshPort                              int
	}

	var olds []oldInstance
	machineByKey := make(map[machineKey]string) // → machine.id

	for rows.Next() {
		var o oldInstance
		if err := rows.Scan(&o.id, &o.harnessType, &o.name, &o.host, &o.transport, &o.sshUser,
			&o.sshKeyPath, &o.sshPort, &o.workingDir, &o.maxConc, &o.enabled, &o.createdAt, &o.updatedAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan old instance: %w", err)
		}
		olds = append(olds, o)
		k := machineKey{transport: o.transport, host: o.host, sshUser: o.sshUser, sshKeyPath: o.sshKeyPath, sshPort: o.sshPort}
		machineByKey[k] = "" // populated below
	}
	rows.Close()

	// Mint machine rows for each distinct connection tuple. Name them by
	// host (the most-recognizable label), disambiguating with a suffix
	// when transport differs for the same host.
	usedNames := make(map[string]bool)
	for k := range machineByKey {
		base := k.host
		if base == "" {
			base = "default"
		}
		name := base
		// If multiple transports collide on the same host, append the
		// transport so names stay unique.
		if usedNames[name] {
			name = fmt.Sprintf("%s-%s", base, k.transport)
			i := 1
			for usedNames[name] {
				i++
				name = fmt.Sprintf("%s-%s-%d", base, k.transport, i)
			}
		}
		usedNames[name] = true
		id := "m_" + safeID(name)
		machineByKey[k] = id
		if _, err := tx.Exec(`
			INSERT INTO machines (id, name, hostname, transport, ssh_user, ssh_key_path, ssh_port)
			VALUES (?, ?, ?, ?, ?, ?, ?)
		`, id, name, k.host, k.transport, k.sshUser, k.sshKeyPath, k.sshPort); err != nil {
			return fmt.Errorf("create machine row %s: %w", id, err)
		}
	}

	// Drop & recreate instances with the new shape, then copy.
	if _, err := tx.Exec(`DROP TABLE instances`); err != nil {
		return fmt.Errorf("drop old instances: %w", err)
	}
	if _, err := tx.Exec(schemaInstancesNewDDL); err != nil {
		return fmt.Errorf("create new instances: %w", err)
	}

	for _, o := range olds {
		k := machineKey{transport: o.transport, host: o.host, sshUser: o.sshUser, sshKeyPath: o.sshKeyPath, sshPort: o.sshPort}
		machineID := machineByKey[k]
		if _, err := tx.Exec(`
			INSERT INTO instances (id, harness_type, name, machine_id, working_dir, max_concurrent_sessions, enabled, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		`, o.id, o.harnessType, o.name, machineID, o.workingDir, o.maxConc, o.enabled, o.createdAt, o.updatedAt); err != nil {
			return fmt.Errorf("rewrite instance %s: %w", o.id, err)
		}
	}

	return tx.Commit()
}

// tableExists reports whether a table is present in sqlite_master.
func tableExists(db *sql.DB, name string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// columnExists reports whether a column is present on a table.
func columnExists(db *sql.DB, table, col string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf(`PRAGMA table_info(%q)`, table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == col {
			return true, nil
		}
	}
	return false, rows.Err()
}

// safeID lower-cases and replaces non-alphanumerics with underscores so a
// machine name can be used as part of its primary key.
func safeID(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z':
			out = append(out, c+32)
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9'):
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "m"
	}
	return string(out)
}
