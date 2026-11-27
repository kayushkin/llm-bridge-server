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
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
	// FormerNames are the schema and role names the module had in earlier
	// builds, newest first. Provision renames a database's schema of a former
	// name, and its role, to the current names, so an install keeps its
	// records across a rename. See Provision.
	FormerNames []FormerName
	// Migrations build the schema, in order. See Migration.
	Migrations []Migration
}

// FormerName is a schema and role a module was called in an earlier build.
// Role is empty for a module that acted as the login role itself.
type FormerName struct {
	Schema string
	Role   string
}

// ProvisionReport says what Provision changed besides making what was missing.
type ProvisionReport struct {
	// RenamedFrom is the former name whose schema, and role, Provision renamed
	// to the module's current names. Nil when nothing was renamed.
	RenamedFrom *FormerName
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
	for _, former := range module.FormerNames {
		if err := checkIdentifier("former schema", former.Schema); err != nil {
			return err
		}
		if former.Schema == module.Schema {
			return fmt.Errorf("postgresstorage: module %s names its own schema as a former name", module.Schema)
		}
		if former.Role != "" {
			if err := checkIdentifier("former role", former.Role); err != nil {
				return err
			}
		}
		if (former.Role == "") != (module.Role == "") {
			return fmt.Errorf("postgresstorage: module %s and its former name %s must both have a role or both have none", module.Schema, former.Schema)
		}
	}
	return checkMigrations(module.Migrations)
}

// Open returns the module's connection pool on the database at url. Every
// connection acts as the module's role and resolves a bare table name in the
// module's schema and nowhere else, so a query cannot land in another module's
// tables by leaving the schema off. The schema must exist: Provision makes it.
//
// options go to the pool as it is made, for a module that needs its
// connections set up beyond this (ScanTimestampsInUTC).
func Open(ctx context.Context, url string, module Module, options ...stdlib.OptionOpenDB) (*sql.DB, error) {
	return OpenWithSessionSettings(ctx, url, module, nil, options...)
}

// ScanTimestampsInUTC makes every connection of a pool read a timestamptz as a
// time in UTC. pgx otherwise reads it in the process's local zone, so the same
// stored instant would print with whatever offset the server runs under.
var ScanTimestampsInUTC = stdlib.OptionAfterConnect(func(_ context.Context, connection *pgx.Conn) error {
	connection.TypeMap().RegisterType(&pgtype.Type{
		Name:  "timestamptz",
		OID:   pgtype.TimestamptzOID,
		Codec: &pgtype.TimestamptzCodec{ScanLocation: time.UTC},
	})
	return nil
})

// sessionSettingPattern is the name of a setting a module defines for its own
// use: its schema's name, a dot and a plain name (possessions.owner_principal_id).
var sessionSettingPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}\.[a-z][a-z0-9_]{0,62}$`)

// OpenWithSessionSettings is Open with settings every connection of the pool
// starts with, each named after the module's schema (<schema>.<name>). A
// module whose row-level security policies read current_setting opens one
// pool per value, so a connection can never carry another pool's value.
func OpenWithSessionSettings(ctx context.Context, url string, module Module, settings map[string]string, options ...stdlib.OptionOpenDB) (*sql.DB, error) {
	if err := module.check(); err != nil {
		return nil, err
	}
	config, err := pgx.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("postgresstorage: the database URL does not parse: %w", err)
	}
	for name, value := range settings {
		if !sessionSettingPattern.MatchString(name) || !strings.HasPrefix(name, module.Schema+".") {
			return nil, fmt.Errorf("postgresstorage: session setting %q must be named %s.<lower-case name>", name, module.Schema)
		}
		config.RuntimeParams[name] = value
	}
	config.RuntimeParams["search_path"] = module.Schema
	if module.Role != "" {
		config.RuntimeParams["role"] = module.Role
	}
	db := stdlib.OpenDB(*config, options...)
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
// When the database holds no schema of the module's name but holds one of a
// former name (Module.FormerNames), Provision renames that schema, and the role
// that owns it, to the current names first, in the same transaction, so the
// records stay where the module now looks for them. It refuses, changing
// nothing, when the current and a former schema both exist, or when the role
// to rename is not the one that owns the former schema: either needs a person
// to say which records are the module's.
//
// The login needs no superuser: CREATEROLE, and CREATE on the database, are
// enough. A role that already exists must be one this login may grant (one it
// made, or one it was given WITH ADMIN OPTION); renaming a role needs the same.
func Provision(ctx context.Context, administrator *sql.DB, module Module) (ProvisionReport, error) {
	var report ProvisionReport
	if err := module.check(); err != nil {
		return report, err
	}
	err := InTransaction(ctx, administrator, func(tx *sql.Tx) error {
		// Servers that start together would both find the schema missing and
		// both try to create it; the lock makes one wait for the other.
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", "postgresstorage.provision"); err != nil {
			return fmt.Errorf("postgresstorage: taking the provisioning lock: %w", err)
		}
		renamedFrom, err := renameFormerSchema(ctx, tx, module)
		if err != nil {
			return err
		}
		report.RenamedFrom = renamedFrom
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
	if err != nil {
		return ProvisionReport{}, err
	}
	return report, nil
}

// renameFormerSchema renames the schema of the first former name the database
// holds, and the role that owns it, to the module's current names, and returns
// that former name. It returns nil when the database holds the current schema
// already or no former one. It runs inside Provision's transaction and lock,
// so a failure leaves every name as it was.
//
// A role belongs to the whole Postgres server, so another database's server
// may have renamed it already: then only the schema is renamed here, once the
// current role is found to own it.
func renameFormerSchema(ctx context.Context, tx *sql.Tx, module Module) (*FormerName, error) {
	currentExists, err := schemaExists(ctx, tx, module.Schema)
	if err != nil {
		return nil, err
	}
	for _, former := range module.FormerNames {
		formerExists, err := schemaExists(ctx, tx, former.Schema)
		if err != nil {
			return nil, err
		}
		if !formerExists {
			continue
		}
		if currentExists {
			return nil, fmt.Errorf("postgresstorage: this database holds both schema %s and schema %s, the name module %s had before; move what %s holds into %s or drop it, by hand, then start again",
				module.Schema, former.Schema, module.Schema, former.Schema, module.Schema)
		}
		if former.Role != "" {
			if err := renameFormerRole(ctx, tx, module, former); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, "ALTER SCHEMA "+former.Schema+" RENAME TO "+module.Schema); err != nil {
			return nil, fmt.Errorf("postgresstorage: renaming schema %s to %s: %w", former.Schema, module.Schema, err)
		}
		renamed := former
		return &renamed, nil
	}
	return nil, nil
}

// renameFormerRole makes the role that owns former's schema carry the
// module's current role name: it renames former's role, unless another
// database's server already did. Either way the current role must then own
// the former schema, or the records would sit under a role the module does
// not act as.
func renameFormerRole(ctx context.Context, tx *sql.Tx, module Module, former FormerName) error {
	formerRoleExists, err := roleExists(ctx, tx, former.Role)
	if err != nil {
		return err
	}
	currentRoleExists, err := roleExists(ctx, tx, module.Role)
	if err != nil {
		return err
	}
	switch {
	case formerRoleExists && currentRoleExists:
		return fmt.Errorf("postgresstorage: roles %s and %s both exist, so renaming schema %s to %s cannot tell which role should own it; drop the one that owns nothing, by hand, then start again",
			former.Role, module.Role, former.Schema, module.Schema)
	case formerRoleExists:
		if _, err := tx.ExecContext(ctx, "ALTER ROLE "+former.Role+" RENAME TO "+module.Role); err != nil {
			return fmt.Errorf("postgresstorage: renaming role %s to %s: %w", former.Role, module.Role, err)
		}
	case !currentRoleExists:
		return fmt.Errorf("postgresstorage: schema %s, the name module %s had before, is there, but neither role %s nor role %s is", former.Schema, module.Schema, former.Role, module.Role)
	}
	var owner string
	if err := tx.QueryRowContext(ctx, "SELECT pg_get_userbyid(nspowner) FROM pg_namespace WHERE nspname = $1", former.Schema).Scan(&owner); err != nil {
		return fmt.Errorf("postgresstorage: reading the owner of schema %s: %w", former.Schema, err)
	}
	if owner != module.Role {
		return fmt.Errorf("postgresstorage: schema %s, the name module %s had before, is owned by role %s, not %s", former.Schema, module.Schema, owner, module.Role)
	}
	return nil
}

func schemaExists(ctx context.Context, tx *sql.Tx, schema string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", schema).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgresstorage: looking for schema %s: %w", schema, err)
	}
	return exists, nil
}

func roleExists(ctx context.Context, tx *sql.Tx, role string) (bool, error) {
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = $1)", role).Scan(&exists); err != nil {
		return false, fmt.Errorf("postgresstorage: looking for role %s: %w", role, err)
	}
	return exists, nil
}
