package toolstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testContext is the context every store call in these tests runs under.
var testContext = context.Background()

// newTestStore gives the test a store on a Postgres database of its own, made
// from a template with the module migrated (postgresstorage/postgrestest).
func newTestStore(t testing.TB) *Store {
	t.Helper()
	return New(newTestDatabase(t))
}

// newTestDatabase gives the test the module's pool on a database of its own.
func newTestDatabase(t testing.TB) *sql.DB {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	return postgrestest.NewDatabase(t, module).Open(module)
}

// registerTestRoutes mounts the handlers on mux under the paths the standalone
// service served, which are the paths behind the prefix the server mounts
// them at.
func registerTestRoutes(mux *http.ServeMux, s *Store, options HandlerOptions) {
	h := NewHandlers(s, options)
	mux.HandleFunc("GET /health", h.Health)
	mux.HandleFunc("GET /kinds", h.ListKinds)
	mux.HandleFunc("GET /tools", h.ListTools)
	mux.HandleFunc("POST /tools", h.UpsertTool)
	mux.HandleFunc("GET /tools/{id}", h.GetTool)
	mux.HandleFunc("PATCH /tools/{id}", h.PatchTool)
	mux.HandleFunc("DELETE /tools/{id}", h.DeleteTool)
	mux.HandleFunc("POST /tools/{id}/enable", h.EnableTool)
	mux.HandleFunc("POST /tools/{id}/disable", h.DisableTool)
	mux.HandleFunc("GET /tools/by-name/{name}", h.GetToolByName)
	mux.HandleFunc("GET /tools/by-name/{name}/spec", h.SpecByName)
	mux.HandleFunc("POST /tools/by-name/{name}/invoke", h.InvokeByName)
	mux.HandleFunc("POST /harness-tools/observed", h.RecordObservedHarnessTools)
	mux.HandleFunc("GET /locals", h.ListLocals)
	mux.HandleFunc("POST /provision", h.Provision)
	mux.HandleFunc("GET /instances/{id}/tools", h.ListInstanceTools)
	mux.HandleFunc("POST /instances/{id}/tools/by-name/{name}", h.EnableForInstance)
	mux.HandleFunc("DELETE /instances/{id}/tools/by-name/{name}", h.DisableForInstance)
	mux.HandleFunc("GET /tools/by-name/{name}/instances", h.ListInstancesForTool)
}

// dumpRows reads every row query returns as "column=value" text, for
// comparing rows before and after a change.
func dumpRows(t *testing.T, db *sql.DB, query string, arguments ...any) []string {
	t.Helper()
	rows, err := db.Query(query, arguments...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		var fields []string
		for i, column := range columns {
			if b, ok := values[i].([]byte); ok {
				values[i] = string(b)
			}
			encoded, err := json.Marshal(values[i])
			if err != nil {
				t.Fatal(err)
			}
			fields = append(fields, column+"="+strings.TrimSpace(strings.ReplaceAll(string(encoded), "\n", " ")))
		}
		out = append(out, strings.Join(fields, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
