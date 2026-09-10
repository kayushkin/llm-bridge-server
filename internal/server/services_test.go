package server

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/serviceinventory"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// A host in miniature: a fake healthcheck that watches one user unit, and a
// fake /proc in which one process of that unit holds a SQLite file open.
// Every route is then exercised through the real mux.
func servicesTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	openDB := filepath.Join(dir, "quote-store.db")
	db, err := sql.Open("sqlite", openDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE quotes (id INTEGER PRIMARY KEY, text TEXT, api_key TEXT); INSERT INTO quotes (text, api_key) VALUES ('one', 'k1'), ('two', 'k2')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	closedDB := filepath.Join(dir, "nobody-holds-this.db")
	if err := os.WriteFile(closedDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	proc := filepath.Join(dir, "proc")
	pidDir := filepath.Join(proc, strconv.Itoa(4242))
	if err := os.MkdirAll(filepath.Join(pidDir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "cgroup"), []byte("0::/user.slice/user-1000.slice/user@1000.service/app.slice/quote-store.service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(openDB, filepath.Join(pidDir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
	serviceinventory.ProcRoot = proc
	t.Cleanup(func() { serviceinventory.ProcRoot = "/proc" })

	healthcheck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/status" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"timestamp": "2026-09-10T20:00:00Z",
			"services": []map[string]any{
				{"name": "quote-store", "type": "systemd", "status": "up", "unit": "quote-store"},
				{"name": "repo-build-guard", "type": "command", "status": "down", "last_error": "FAIL"},
			},
		})
	}))
	t.Cleanup(healthcheck.Close)

	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     "http://localhost:0",
		HealthcheckURL:  healthcheck.URL,
	}
	return New(st, nil, nil, nil, nil, nil, nil, cfg), openDB, closedDB
}

func getJSON(t *testing.T, srv *Server, target string, into any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	if into != nil && rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
			t.Fatalf("%s: decode: %v\n%s", target, err, rec.Body.String())
		}
	}
	return rec.Code
}

func TestServicesInventoryJoinsHealthcheckToOpenDatabases(t *testing.T) {
	srv, openDB, _ := servicesTestServer(t)
	var inv msg.ServiceInventoryResponse
	if code := getJSON(t, srv, "/services", &inv); code != http.StatusOK {
		t.Fatalf("GET /services: %d", code)
	}
	if inv.CheckedAt != "2026-09-10T20:00:00Z" || len(inv.Services) != 2 {
		t.Fatalf("inventory = %+v", inv)
	}
	byName := map[string]msg.ServiceInventoryEntry{}
	for _, s := range inv.Services {
		byName[s.Name] = s
	}
	qs := byName["quote-store"]
	if len(qs.PIDs) != 1 || qs.PIDs[0] != 4242 || len(qs.Databases) != 1 || qs.Databases[0].Path != openDB {
		t.Fatalf("quote-store = %+v", qs)
	}
	guard := byName["repo-build-guard"]
	if len(guard.PIDs) != 0 || len(guard.Databases) != 0 || guard.ProcessLookupError != "" || guard.Status != "down" {
		t.Fatalf("a command check has no process and no databases, and no lookup error: %+v", guard)
	}
}

func TestDatabaseRoutesReadOnlyFilesSomeServiceHoldsOpen(t *testing.T) {
	srv, openDB, closedDB := servicesTestServer(t)

	var schema msg.DatabaseSchemaResponse
	if code := getJSON(t, srv, "/services/databases/schema?path="+openDB, &schema); code != http.StatusOK {
		t.Fatalf("schema of an open file: %d", code)
	}
	if len(schema.Tables) != 1 || schema.Tables[0].Name != "quotes" || schema.Tables[0].RowCount != 2 {
		t.Fatalf("schema = %+v", schema)
	}

	var rows msg.DatabaseRowsResponse
	if code := getJSON(t, srv, "/services/databases/rows?path="+openDB+"&table=quotes&filter=text:eq:two", &rows); code != http.StatusOK {
		t.Fatalf("rows of an open file: %d", code)
	}
	if rows.TotalRows != 1 || len(rows.Rows) != 1 || rows.Rows[0][1] != "two" || rows.Rows[0][2] != nil {
		t.Fatalf("rows = %+v", rows)
	}

	if code := getJSON(t, srv, "/services/databases/schema?path="+closedDB, nil); code != http.StatusNotFound {
		t.Fatalf("a file no watched service holds open must be 404, got %d", code)
	}
	if code := getJSON(t, srv, "/services/databases/rows?path="+closedDB+"&table=quotes", nil); code != http.StatusNotFound {
		t.Fatalf("rows of a file no watched service holds open must be 404, got %d", code)
	}
	for _, target := range []string{
		"/services/databases/rows?path=" + openDB,
		"/services/databases/rows?path=" + openDB + "&table=nope",
		"/services/databases/rows?path=" + openDB + "&table=quotes&limit=0",
		"/services/databases/rows?path=" + openDB + "&table=quotes&order=sideways",
		"/services/databases/rows?path=" + openDB + "&table=quotes&filter=text:like:x",
	} {
		if code := getJSON(t, srv, target, nil); code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d", target, code)
		}
	}
}
