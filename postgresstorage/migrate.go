package postgresstorage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
)

// Migration is one step that changes a module's schema. Versions count up from
// 1 with none skipped. Once a migration has run anywhere, its SQL is never
// edited: a change is a new migration.
//
// Several versions of a server run against one database while an install rolls
// out a new one, so a migration only adds what the older version can ignore (a
// table, a column with a default). Taking something away is a later migration,
// shipped after every running version has stopped using it.
type Migration struct {
	Version int
	// Name says what the step does, for the record of what ran.
	Name string
	SQL  string
}

func (migration Migration) checksum() string {
	sum := sha256.Sum256([]byte(migration.SQL))
	return hex.EncodeToString(sum[:])
}

func checkMigrations(migrations []Migration) error {
	for index, migration := range migrations {
		if migration.Version != index+1 {
			return fmt.Errorf("postgresstorage: migration %d of the list has version %d; versions count up from 1 with none skipped", index+1, migration.Version)
		}
		if migration.Name == "" {
			return fmt.Errorf("postgresstorage: migration %d has no name", migration.Version)
		}
		if migration.SQL == "" {
			return fmt.Errorf("postgresstorage: migration %d (%s) has no SQL", migration.Version, migration.Name)
		}
	}
	return nil
}

var migrationFileName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// MigrationsFromFiles reads a module's migrations from the .sql files of one
// folder, named 0001_what_it_does.sql, 0002_…, as a module embeds them.
// Any other file in the folder is an error, so a misnamed migration cannot be
// skipped in silence.
func MigrationsFromFiles(files fs.FS, folder string) ([]Migration, error) {
	entries, err := fs.ReadDir(files, folder)
	if err != nil {
		return nil, fmt.Errorf("postgresstorage: reading migrations from %s: %w", folder, err)
	}
	var migrations []Migration
	for _, entry := range entries {
		parts := migrationFileName.FindStringSubmatch(entry.Name())
		if entry.IsDir() || parts == nil {
			return nil, fmt.Errorf("postgresstorage: %s/%s is not a migration file; name it like 0001_what_it_does.sql", folder, entry.Name())
		}
		version, _ := strconv.Atoi(parts[1])
		body, err := fs.ReadFile(files, folder+"/"+entry.Name())
		if err != nil {
			return nil, fmt.Errorf("postgresstorage: reading %s/%s: %w", folder, entry.Name(), err)
		}
		migrations = append(migrations, Migration{Version: version, Name: parts[2], SQL: string(body)})
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	if err := checkMigrations(migrations); err != nil {
		return nil, err
	}
	return migrations, nil
}

// MigrationReport says what one call to Migrate found and did.
type MigrationReport struct {
	// Applied are the migrations this call ran, in order.
	Applied []Migration
	// NewerThanThisBinary are versions the database has that this binary does
	// not know: a newer server migrated first. That is the normal state of the
	// older server during a rollout, and migrations are written so it keeps
	// working, so it is reported and not refused.
	NewerThanThisBinary []int
}

// ErrSchemaNotProvisioned is returned by Migrate when the module's schema does
// not exist.
var ErrSchemaNotProvisioned = errors.New("postgresstorage: the module's schema does not exist; Provision makes it, or the install's infrastructure must")

// ErrMigrationEdited is returned by Migrate when a migration the database has
// already run no longer matches the one in this binary.
var ErrMigrationEdited = errors.New("postgresstorage: a migration was edited after it ran")

// Migrate brings the module's schema up to the migrations in this binary. db is
// the module's own pool, from Open. Each migration runs in a transaction with
// the record that it ran, so one that fails leaves no trace and the next start
// tries it again. Servers that start together take turns: one migrates and the
// others find nothing left to do.
func Migrate(ctx context.Context, db *sql.DB, module Module) (MigrationReport, error) {
	var report MigrationReport
	if err := module.check(); err != nil {
		return report, err
	}

	// The lock belongs to a session, so everything runs on one connection.
	connection, err := db.Conn(ctx)
	if err != nil {
		return report, fmt.Errorf("postgresstorage: migrating %s: %w", module.Schema, err)
	}
	defer connection.Close()

	var schemaExists bool
	if err := connection.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", module.Schema).Scan(&schemaExists); err != nil {
		return report, fmt.Errorf("postgresstorage: looking for schema %s: %w", module.Schema, err)
	}
	if !schemaExists {
		return report, fmt.Errorf("%w: %s", ErrSchemaNotProvisioned, module.Schema)
	}

	lockName := "postgresstorage.migrate." + module.Schema
	if _, err := connection.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", lockName); err != nil {
		return report, fmt.Errorf("postgresstorage: taking the migration lock of %s: %w", module.Schema, err)
	}
	defer connection.ExecContext(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(hashtext($1))", lockName)

	recordTable := module.Schema + ".schema_migrations"
	if _, err := connection.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+recordTable+` (
		version    integer PRIMARY KEY,
		name       text NOT NULL,
		checksum   text NOT NULL,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return report, fmt.Errorf("postgresstorage: creating %s: %w", recordTable, err)
	}

	appliedChecksums := map[int]string{}
	rows, err := connection.QueryContext(ctx, "SELECT version, checksum FROM "+recordTable+" ORDER BY version")
	if err != nil {
		return report, fmt.Errorf("postgresstorage: reading %s: %w", recordTable, err)
	}
	for rows.Next() {
		var version int
		var checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			return report, fmt.Errorf("postgresstorage: reading %s: %w", recordTable, err)
		}
		appliedChecksums[version] = checksum
	}
	if err := rows.Close(); err != nil {
		return report, fmt.Errorf("postgresstorage: reading %s: %w", recordTable, err)
	}
	if err := rows.Err(); err != nil {
		return report, fmt.Errorf("postgresstorage: reading %s: %w", recordTable, err)
	}

	for version := range appliedChecksums {
		if version > len(module.Migrations) {
			report.NewerThanThisBinary = append(report.NewerThanThisBinary, version)
		}
	}
	sort.Ints(report.NewerThanThisBinary)

	for _, migration := range module.Migrations {
		if applied, ran := appliedChecksums[migration.Version]; ran {
			if applied != migration.checksum() {
				return report, fmt.Errorf("%w: %s migration %d (%s)", ErrMigrationEdited, module.Schema, migration.Version, migration.Name)
			}
			continue
		}
		tx, err := connection.BeginTx(ctx, nil)
		if err != nil {
			return report, fmt.Errorf("postgresstorage: %s migration %d (%s): %w", module.Schema, migration.Version, migration.Name, err)
		}
		if _, err := tx.ExecContext(ctx, migration.SQL); err != nil {
			tx.Rollback()
			return report, fmt.Errorf("postgresstorage: %s migration %d (%s): %w", module.Schema, migration.Version, migration.Name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO "+recordTable+" (version, name, checksum) VALUES ($1, $2, $3)", migration.Version, migration.Name, migration.checksum()); err != nil {
			tx.Rollback()
			return report, fmt.Errorf("postgresstorage: recording %s migration %d (%s): %w", module.Schema, migration.Version, migration.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return report, fmt.Errorf("postgresstorage: committing %s migration %d (%s): %w", module.Schema, migration.Version, migration.Name, err)
		}
		report.Applied = append(report.Applied, migration)
	}
	return report, nil
}
