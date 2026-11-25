// Package postgrestest gives a test a Postgres database of its own.
//
// A test asks for a database with the modules it needs. The first test to ask
// for a given set of modules and migrations builds a template database, with
// each module provisioned and migrated; every test after that gets a copy of
// the template, which Postgres makes in a fraction of the time the migrations
// take. Each test's database is dropped when the test ends, so tests never see
// each other's rows and may run in parallel.
//
// The Postgres to use is named by POSTGRESTEST_ADMINISTRATOR_URL, a login that may
// create databases and roles. scripts/test-postgres.sh starts a throwaway one.
package postgrestest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/stdlib"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// AdministratorURLVariable names the environment variable holding the URL of
// the Postgres that tests use. It deliberately does not start with LLMBRIDGE_:
// the server refuses to start with a set LLMBRIDGE_ variable it does not
// declare, and this one is test tooling, not a setting of the server.
const AdministratorURLVariable = "POSTGRESTEST_ADMINISTRATOR_URL"

// Database is one test's own database.
type Database struct {
	t   testing.TB
	url string
}

// URL is the address of the test's database, for the login the tests run as.
func (database *Database) URL() string { return database.url }

// Open returns module's connection pool on the test's database, as a server
// would open it. The pool is closed when the test ends.
func (database *Database) Open(module postgresstorage.Module, options ...stdlib.OptionOpenDB) *sql.DB {
	database.t.Helper()
	db, err := postgresstorage.Open(context.Background(), database.url, module, options...)
	if err != nil {
		database.t.Fatalf("postgrestest: %v", err)
	}
	database.t.Cleanup(func() { db.Close() })
	return db
}

// OpenAsAdministrator returns a pool on the test's database for the login the
// tests run as, with no module's role or schema: what an install's
// provisioning step connects as. The pool is closed when the test ends.
func (database *Database) OpenAsAdministrator() *sql.DB {
	database.t.Helper()
	db := openPlain(database.t, database.url)
	database.t.Cleanup(func() { db.Close() })
	return db
}

// NewDatabase makes a database for one test, holding each of modules
// provisioned and migrated. With no modules it is empty, for a test of
// provisioning and migrating themselves.
func NewDatabase(t testing.TB, modules ...postgresstorage.Module) *Database {
	t.Helper()
	administratorURL := os.Getenv(AdministratorURLVariable)
	if administratorURL == "" {
		t.Fatalf("postgrestest: %s is not set. Start a throwaway Postgres and name it:\n  export %s=\"$(scripts/test-postgres.sh start)\"", AdministratorURLVariable, AdministratorURLVariable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	administrator := openPlain(t, administratorURL)
	defer administrator.Close()

	template := templateName(modules)
	buildTemplateOnce(ctx, t, administrator, administratorURL, template, modules)

	name := "test_" + randomHex(t, 8)
	if _, err := administrator.ExecContext(ctx, `CREATE DATABASE "`+name+`" TEMPLATE "`+template+`"`); err != nil {
		t.Fatalf("postgrestest: copying %s into %s: %v", template, name, err)
	}
	t.Cleanup(func() {
		// Runs after the pools Open handed out are closed: cleanups run last
		// registered first, and this one is registered before any Open.
		dropper := openPlain(t, administratorURL)
		defer dropper.Close()
		if _, err := dropper.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`); err != nil {
			t.Errorf("postgrestest: dropping %s: %v", name, err)
		}
	})
	return &Database{t: t, url: urlOfDatabase(t, administratorURL, name)}
}

// templateName names the template for one set of modules and migrations. A
// changed migration is a different name, so a stale template is never copied.
func templateName(modules []postgresstorage.Module) string {
	hash := sha256.New()
	for _, module := range modules {
		fmt.Fprintf(hash, "module\x00%s\x00%s\x00", module.Schema, module.Role)
		for _, migration := range module.Migrations {
			fmt.Fprintf(hash, "%d\x00%s\x00%s\x00", migration.Version, migration.Name, migration.SQL)
		}
	}
	return "template_" + hex.EncodeToString(hash.Sum(nil))[:24]
}

// buildTemplateOnce builds the template unless it exists. Test binaries of
// several packages run at once, so the check and the build happen under a lock
// in the database, and a half-built template is never left behind for the next
// to copy.
func buildTemplateOnce(ctx context.Context, t testing.TB, administrator *sql.DB, administratorURL, template string, modules []postgresstorage.Module) {
	t.Helper()
	connection, err := administrator.Conn(ctx)
	if err != nil {
		t.Fatalf("postgrestest: connecting to %s: %v", AdministratorURLVariable, err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, "SELECT pg_advisory_lock(hashtext($1))", "postgrestest."+template); err != nil {
		t.Fatalf("postgrestest: taking the template lock: %v", err)
	}
	defer connection.ExecContext(context.Background(), "SELECT pg_advisory_unlock(hashtext($1))", "postgrestest."+template)

	var exists bool
	if err := connection.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", template).Scan(&exists); err != nil {
		t.Fatalf("postgrestest: looking for %s: %v", template, err)
	}
	if exists {
		return
	}

	// Built under a name nothing copies, then renamed, so a build that fails
	// half-way leaves no template that looks finished.
	building := template + "_building"
	if _, err := connection.ExecContext(ctx, `DROP DATABASE IF EXISTS "`+building+`" WITH (FORCE)`); err != nil {
		t.Fatalf("postgrestest: clearing %s: %v", building, err)
	}
	if _, err := connection.ExecContext(ctx, `CREATE DATABASE "`+building+`"`); err != nil {
		t.Fatalf("postgrestest: creating %s: %v", building, err)
	}
	buildingURL := urlOfDatabase(t, administratorURL, building)
	func() {
		buildingAdministrator := openPlain(t, buildingURL)
		defer buildingAdministrator.Close()
		for _, module := range modules {
			if err := postgresstorage.Provision(ctx, buildingAdministrator, module); err != nil {
				t.Fatalf("postgrestest: %v", err)
			}
			db, err := postgresstorage.Open(ctx, buildingURL, module)
			if err != nil {
				t.Fatalf("postgrestest: %v", err)
			}
			_, err = postgresstorage.Migrate(ctx, db, module)
			db.Close()
			if err != nil {
				t.Fatalf("postgrestest: %v", err)
			}
		}
	}()
	if _, err := connection.ExecContext(ctx, `ALTER DATABASE "`+building+`" RENAME TO "`+template+`"`); err != nil {
		t.Fatalf("postgrestest: naming the template %s: %v", template, err)
	}
}

func openPlain(t testing.TB, databaseURL string) *sql.DB {
	t.Helper()
	db, err := sql.Open("pgx", databaseURL)
	if err != nil {
		t.Fatalf("postgrestest: %v", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		t.Fatalf("postgrestest: cannot reach the Postgres that %s names: %v", AdministratorURLVariable, err)
	}
	return db
}

// The "pgx" driver name openPlain uses is registered by this import.
var _ = stdlib.GetDefaultDriver

func urlOfDatabase(t testing.TB, administratorURL, database string) string {
	t.Helper()
	parsed, err := url.Parse(administratorURL)
	if err != nil {
		t.Fatalf("postgrestest: %s does not parse as a URL: %v", AdministratorURLVariable, err)
	}
	parsed.Path = "/" + database
	return parsed.String()
}

func randomHex(t testing.TB, bytes int) string {
	t.Helper()
	buffer := make([]byte, bytes)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("postgrestest: %v", err)
	}
	return hex.EncodeToString(buffer)
}
