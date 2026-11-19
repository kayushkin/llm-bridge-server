// Package postgresstorage is how a module keeps its records in Postgres.
//
// One Postgres database holds many modules. Each module owns one schema, and
// may act as a role of its own, so that reading another module's tables is a
// permission error in the database rather than a rule people remember. A
// module reaches another module's records by calling that module's code, never
// its tables.
//
// The three steps, in the order a server takes them at start:
//
//	Provision  makes the module's role and schema. It needs a login that may
//	           create roles and schemas; an install whose infrastructure makes
//	           them (Terraform, Bicep) skips it.
//	Open       gives the module a connection pool that acts as its role and
//	           sees its schema first.
//	Migrate    brings the schema up to the migrations compiled into the binary.
//
// The pool is a database/sql pool, so a store written for SQLite keeps its
// shape and changes only its SQL.
package postgresstorage

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

// Module is one owner of records: its schema, the role it acts as, and the
// migrations that build its tables.
type Module struct {
	// Schema holds every table of the module. Lower-case letters, digits and
	// underscores, starting with a letter.
	Schema string
	// Role is the Postgres role the module's connections act as. It owns the
	// schema. Empty means the module acts as the login role itself, which
	// gives up the wall between modules; use that only where one module has
	// the database to itself.
	Role string
	// Migrations build the schema, in order. See Migration.
	Migrations []Migration
}

var identifierPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// checkIdentifier refuses a name this package would have to quote. Schema and
// role names are written into statements, which take no parameters there, so
// only plain names are accepted.
func checkIdentifier(kind, name string) error {
	if !identifierPattern.MatchString(name) {
		return fmt.Errorf("postgresstorage: %s %q must be lower-case letters, digits and underscores, starting with a letter, at most 63 characters", kind, name)
	}
	return nil
}

func (module Module) check() error {
	if err := checkIdentifier("schema", module.Schema); err != nil {
		return err
	}
	if module.Role != "" {
		if err := checkIdentifier("role", module.Role); err != nil {
			return err
		}
	}
	return checkMigrations(module.Migrations)
}

// Open returns the module's connection pool on the database at url. Every
// connection acts as the module's role and resolves a bare table name in the
// module's schema and nowhere else, so a query cannot land in another module's
// tables by leaving the schema off. The schema must exist: Provision makes it.
func Open(ctx context.Context, url string, module Module) (*sql.DB, error) {
	if err := module.check(); err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("postgresstorage: the database URL does not parse: %w", err)
	}
	config.RuntimeParams["search_path"] = module.Schema
	if module.Role != "" {
		config.RuntimeParams["role"] = module.Role
	}
	db := stdlib.OpenDB(*config)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("postgresstorage: opening module %s: %w", module.Schema, err)
	}
	return db, nil
}

// Provision makes the module's role and schema if they are missing, and lets
// the login that administrator connects as act as the role. It changes nothing
// that is already in place, so every start may call it.
//
// The login needs no superuser: CREATEROLE, and CREATE on the database, are
// enough. A role that already exists must be one this login may grant (one it
// made, or one it was given WITH ADMIN OPTION).
func Provision(ctx context.Context, administrator *sql.DB, module Module) error {
	if err := module.check(); err != nil {
		return err
	}
	return InTransaction(ctx, administrator, func(tx *sql.Tx) error {
		// Servers that start together would both find the schema missing and
		// both try to create it; the lock makes one wait for the other.
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "postgresstorage.provision"); err != nil {
			return fmt.Errorf("postgresstorage: taking the provisioning lock: %w", err)
		}
		if module.Role == "" {
			if _, err := tx.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+module.Schema); err != nil {
				return fmt.Errorf("postgresstorage: creating schema %s: %w", module.Schema, err)
			}
			return nil
		}
		// A role belongs to the whole server, not to one database, and the lock
		// above reaches one database only. So "it already exists" is taken as
		// done here rather than checked first: another database's server may
		// be making the same role at this moment.
		if _, err := tx.ExecContext(ctx, `DO $$ BEGIN CREATE ROLE `+module.Role+` NOLOGIN; EXCEPTION WHEN duplicate_object THEN NULL; END $$`); err != nil {
			return fmt.Errorf("postgresstorage: creating role %s: %w", module.Role, err)
		}
		if _, err := tx.ExecContext(ctx, "GRANT "+module.Role+" TO CURRENT_USER"); err != nil {
			return fmt.Errorf("postgresstorage: letting this login act as role %s: %w", module.Role, err)
		}
		if _, err := tx.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+module.Schema+" AUTHORIZATION "+module.Role); err != nil {
			return fmt.Errorf("postgresstorage: creating schema %s: %w", module.Schema, err)
		}
		return nil
	})
}
