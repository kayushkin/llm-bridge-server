package serviceinventory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
)

func fixtureDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	stmts := []string{
		`CREATE TABLE credentials (id INTEGER PRIMARY KEY, label TEXT NOT NULL, api_key TEXT, token TEXT)`,
		`INSERT INTO credentials (label, api_key, token) VALUES ('first', 'sk-live-1', 'tok-1'), ('second', 'sk-live-2', NULL), ('third', 'sk-live-3', 'tok-3')`,
		`CREATE TABLE notes (id INTEGER PRIMARY KEY, body TEXT, priority INTEGER)`,
		`INSERT INTO notes (body, priority) VALUES ('alpha', 1), ('beta_%', 2), ('gamma', 3)`,
		`CREATE VIEW open_notes AS SELECT id, body FROM notes WHERE priority > 1`,
		`CREATE TABLE keyed (k TEXT PRIMARY KEY, v TEXT) WITHOUT ROWID`,
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return path
}

func column(columns []msg.DatabaseColumn, name string) msg.DatabaseColumn {
	for _, c := range columns {
		if c.Name == name {
			return c
		}
	}
	return msg.DatabaseColumn{}
}

func TestReadSchemaListsTablesViewsColumnsAndMasks(t *testing.T) {
	path := fixtureDatabase(t)
	schema, err := ReadSchema(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]msg.DatabaseTable{}
	for _, tb := range schema.Tables {
		byName[tb.Name] = tb
	}
	if byName["credentials"].Kind != "table" || byName["credentials"].RowCount != 3 {
		t.Fatalf("credentials = %+v", byName["credentials"])
	}
	if byName["open_notes"].Kind != "view" || byName["open_notes"].RowCount != 2 {
		t.Fatalf("open_notes = %+v", byName["open_notes"])
	}
	if !column(byName["credentials"].Columns, "api_key").Masked || !column(byName["credentials"].Columns, "token").Masked {
		t.Fatalf("api_key and token must be masked: %+v", byName["credentials"].Columns)
	}
	if column(byName["credentials"].Columns, "label").Masked || !column(byName["credentials"].Columns, "label").NotNull {
		t.Fatalf("label: %+v", column(byName["credentials"].Columns, "label"))
	}
	if !column(byName["credentials"].Columns, "id").PrimaryKey {
		t.Fatalf("id must be the primary key")
	}
}

func TestReadRowsMasksValuesAndOrdersNewestFirst(t *testing.T) {
	path := fixtureDatabase(t)
	rows, err := ReadRows(context.Background(), path, RowsQuery{Table: "credentials", Limit: 2, Descending: true})
	if err != nil {
		t.Fatal(err)
	}
	if rows.OrderBy != "rowid" || rows.TotalRows != 3 || len(rows.Rows) != 2 {
		t.Fatalf("order_by=%q total=%d rows=%d", rows.OrderBy, rows.TotalRows, len(rows.Rows))
	}
	if rows.Rows[0][1] != "third" || rows.Rows[1][1] != "second" {
		t.Fatalf("want newest first, got %v", rows.Rows)
	}
	for _, row := range rows.Rows {
		if row[2] != nil || row[3] != nil {
			t.Fatalf("masked columns must come back null, got %v", row)
		}
	}
}

func TestReadRowsFiltersEscapeLikeAndRejectUnknownNames(t *testing.T) {
	path := fixtureDatabase(t)
	ctx := context.Background()
	rows, err := ReadRows(ctx, path, RowsQuery{Table: "notes", Limit: 50, Filters: []msg.DatabaseRowFilter{{Column: "body", Op: "contains", Value: "_%"}}})
	if err != nil {
		t.Fatal(err)
	}
	if rows.TotalRows != 1 || rows.Rows[0][1] != "beta_%" {
		t.Fatalf("contains '_%%' must match only the literal, got total=%d rows=%v", rows.TotalRows, rows.Rows)
	}
	rows, err = ReadRows(ctx, path, RowsQuery{Table: "notes", Limit: 50, OrderBy: "priority", Descending: false,
		Filters: []msg.DatabaseRowFilter{{Column: "priority", Op: "gte", Value: "2"}}})
	if err != nil {
		t.Fatal(err)
	}
	if rows.TotalRows != 2 || rows.Rows[0][1] != "beta_%" {
		t.Fatalf("gte 2 ascending by priority: total=%d rows=%v", rows.TotalRows, rows.Rows)
	}
	rows, err = ReadRows(ctx, path, RowsQuery{Table: "credentials", Limit: 50, Filters: []msg.DatabaseRowFilter{{Column: "token", Op: "null"}}})
	if err != nil {
		t.Fatal(err)
	}
	if rows.TotalRows != 1 {
		t.Fatalf("a masked column is still filterable by null-ness: total=%d", rows.TotalRows)
	}

	for _, q := range []RowsQuery{
		{Table: "nope", Limit: 1},
		{Table: "notes", Limit: 1, OrderBy: "nope"},
		{Table: "notes", Limit: 1, Filters: []msg.DatabaseRowFilter{{Column: "nope", Op: "eq", Value: "x"}}},
	} {
		_, err := ReadRows(ctx, path, q)
		var requestErr *RequestError
		if err == nil || !asRequestError(err, &requestErr) {
			t.Fatalf("%+v: want a RequestError, got %v", q, err)
		}
	}
}

func TestViewsAndWithoutRowidTablesHaveNoDefaultOrder(t *testing.T) {
	path := fixtureDatabase(t)
	for _, table := range []string{"open_notes", "keyed"} {
		rows, err := ReadRows(context.Background(), path, RowsQuery{Table: table, Limit: 5})
		if err != nil {
			t.Fatalf("%s: %v", table, err)
		}
		if rows.OrderBy != "" {
			t.Fatalf("%s: order_by=%q, want none", table, rows.OrderBy)
		}
	}
}

func TestParseFilter(t *testing.T) {
	f, err := ParseFilter("created_at:gte:2026-09-10T00:00:00Z")
	if err != nil || f.Column != "created_at" || f.Op != "gte" || f.Value != "2026-09-10T00:00:00Z" {
		t.Fatalf("got %+v, %v: the value keeps its own colons", f, err)
	}
	if _, err := ParseFilter("x:like:y"); err == nil {
		t.Fatal("unknown op must be refused")
	}
	if _, err := ParseFilter("x"); err == nil {
		t.Fatal("a filter without an op must be refused")
	}
}

// The file must stay untouched: a write through the handle is refused.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	path := fixtureDatabase(t)
	db, err := openReadOnly(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("DELETE FROM notes"); err == nil {
		t.Fatal("a DELETE through the viewer's handle must fail")
	}
}
