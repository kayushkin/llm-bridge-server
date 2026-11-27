package server

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/store"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

var (
	standaloneLogStoreCopy     = flag.String("standalone-log-store-copy", "", "a copy of a standalone log-store database to import and serve")
	logStoreComparisonRequests = flag.String("log-store-comparison-requests", "", "a file of GET paths, one a line, to ask the imported copy")
	logStoreComparisonOutput   = flag.String("log-store-comparison-output", "", "where to write the answers, one JSON object a line")
)

// TestImportedStandaloneLogStoreAnswers imports a copy of a standalone
// log-store's SQLite file into a Postgres database of its own, serves it, asks
// it every path in the requests file, and writes each answer, so the answers
// can be compared with what the standalone service gives for the same paths.
// It also logs how long the import took and how much room each table takes in
// Postgres. Skipped unless the three flags are given.
func TestImportedStandaloneLogStoreAnswers(t *testing.T) {
	if *standaloneLogStoreCopy == "" || *logStoreComparisonRequests == "" || *logStoreComparisonOutput == "" {
		t.Skip("give -standalone-log-store-copy, -log-store-comparison-requests and -log-store-comparison-output to run")
	}
	module, err := store.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	writer := database.Open(module)
	counts, err := store.ImportStandaloneDatabase(context.Background(), writer, *standaloneLogStoreCopy)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	t.Logf("imported in %s, last event number %d: %v", counts.Took, counts.LastEventNumber, counts.RowsByTable)

	sizes, err := writer.Query(`SELECT c.relname, pg_total_relation_size(c.oid), pg_relation_size(c.oid)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = current_schema() AND c.relkind = 'r' ORDER BY 2 DESC`)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for sizes.Next() {
		var name string
		var withIndexesAndToast, heap int64
		if err := sizes.Scan(&name, &withIndexesAndToast, &heap); err != nil {
			t.Fatal(err)
		}
		total += withIndexesAndToast
		t.Logf("table %s: %d bytes with indexes and TOAST (%d in the heap)", name, withIndexesAndToast, heap)
	}
	sizes.Close()
	t.Logf("module total: %d bytes", total)

	reader, err := store.OpenReader(context.Background(), database.URL(), module)
	if err != nil {
		t.Fatal(err)
	}
	s := store.New(writer, reader)
	defer s.Close()
	srv := New(s, nil)

	requests, err := os.Open(*logStoreComparisonRequests)
	if err != nil {
		t.Fatal(err)
	}
	defer requests.Close()
	output, err := os.Create(*logStoreComparisonOutput)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	encoder := json.NewEncoder(output)
	scanner := bufio.NewScanner(requests)
	asked := 0
	for scanner.Scan() {
		path := strings.TrimSpace(scanner.Text())
		if path == "" {
			continue
		}
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if err := encoder.Encode(map[string]any{"path": path, "status": recorder.Code, "body": recorder.Body.String()}); err != nil {
			t.Fatal(err)
		}
		asked++
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	t.Logf("asked %d paths", asked)
}
