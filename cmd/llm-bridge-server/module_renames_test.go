package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"os"
	"sort"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/onlineactivity"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/gitactivity"
	issuesdb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/notes"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// Five modules were renamed on 2026-11-27. An install made before holds their
// records under the old schema and role; the server's start renames both and
// the records stay. This builds each module under its old schema with its real
// migrations, writes a row, and starts it under its new name.
//
// The roles carry a suffix of the test's own: a role belongs to the whole
// Postgres server, and the shared test server holds the real names already.
func TestRenamedModulesKeepTheirRecordsAtStart(t *testing.T) {
	ctx := context.Background()
	for _, moduleOf := range []func() (postgresstorage.Module, error){issuesdb.Module, notes.Module, machines.Module, gitactivity.Module, onlineactivity.Module} {
		current, err := moduleOf()
		if err != nil {
			t.Fatal(err)
		}
		if len(current.FormerNames) != 1 {
			t.Fatalf("module %s names %d former names, want 1", current.Schema, len(current.FormerNames))
		}
		t.Run(current.Schema, func(t *testing.T) {
			suffix := randomSuffix(t)
			former := current.FormerNames[0]
			before := postgresstorage.Module{Schema: former.Schema, Role: "former_" + suffix, Migrations: current.Migrations}
			after := current
			after.Role = "current_" + suffix
			after.FormerNames = []postgresstorage.FormerName{{Schema: former.Schema, Role: before.Role}}
			dropRolesAtEnd(t, before.Role, after.Role)

			database := postgrestest.NewDatabase(t)
			administrator := database.OpenAsAdministrator()
			if _, err := postgresstorage.Provision(ctx, administrator, before); err != nil {
				t.Fatal(err)
			}
			old := database.Open(before)
			if _, err := postgresstorage.Migrate(ctx, old, before); err != nil {
				t.Fatal(err)
			}
			if _, err := old.ExecContext(ctx, "CREATE TABLE rename_check (value text NOT NULL); INSERT INTO rename_check VALUES ('kept')"); err != nil {
				t.Fatal(err)
			}
			tablesBefore := tablesOf(t, administrator, before.Schema)
			old.Close()

			report, err := postgresstorage.Provision(ctx, administrator, after)
			if err != nil {
				t.Fatalf("starting %s under its new name: %v", after.Schema, err)
			}
			if report.RenamedFrom == nil || report.RenamedFrom.Schema != former.Schema {
				t.Fatalf("RenamedFrom = %+v, want schema %s", report.RenamedFrom, former.Schema)
			}
			renamed := database.Open(after)
			migration, err := postgresstorage.Migrate(ctx, renamed, after)
			if err != nil {
				t.Fatal(err)
			}
			if len(migration.Applied) != 0 {
				t.Errorf("after the rename %d migrations ran again", len(migration.Applied))
			}
			var value string
			if err := renamed.QueryRowContext(ctx, "SELECT value FROM rename_check").Scan(&value); err != nil || value != "kept" {
				t.Fatalf("after the rename the row reads %q (err %v)", value, err)
			}
			if got, want := tablesOf(t, administrator, after.Schema), tablesBefore; !equalStrings(got, want) {
				t.Errorf("tables after the rename %v, before %v", got, want)
			}
		})
	}
}

func tablesOf(t *testing.T, db *sql.DB, schema string) []string {
	t.Helper()
	rows, err := db.Query("SELECT tablename FROM pg_tables WHERE schemaname = $1", schema)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var tables []string
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, table)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(tables)
	return tables
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func randomSuffix(t *testing.T) string {
	t.Helper()
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(buffer)
}

// dropRolesAtEnd is registered before the test's database, so it runs after
// the database is gone: a role cannot be dropped while it owns a schema there.
func dropRolesAtEnd(t *testing.T, roles ...string) {
	t.Cleanup(func() {
		server, err := sql.Open("pgx", os.Getenv(postgrestest.AdministratorURLVariable))
		if err != nil {
			t.Errorf("cleaning up roles: %v", err)
			return
		}
		defer server.Close()
		for _, role := range roles {
			if _, err := server.ExecContext(context.Background(), "DROP ROLE IF EXISTS "+role); err != nil {
				t.Errorf("dropping role %s: %v", role, err)
			}
		}
	})
}
