package agentstore

import (
	"context"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/standalonesqlite"
)

// The check of the import against a copy of a live standalone database:
//
//	go test ./internal/agents/agentstore -run TestImportedStandaloneAgentStoreAnswers -args \
//	  -standalone-agent-store-copy=<copy.db> -standalone-agent-store-home=<that store's home> \
//	  -imported-answers-output=<folder> -imported-answers-paths=<file of GET paths>
//
// It imports the copy, compares every table's row count with the file, and
// writes the body each GET path answers through RegisterHandlers to the
// output folder, one file per path, for comparison with the answers of the
// store the copy came from. It never writes to the home directory: it calls
// no scan, render, reconcile or write route.
var (
	standaloneAgentStoreCopy = flag.String("standalone-agent-store-copy", "", "a copy (sqlite3 .backup) of a standalone agent-store database to import")
	standaloneAgentStoreHome = flag.String("standalone-agent-store-home", "", "the home directory of the store the copy came from, which its paths are under")
	importedAnswersOutput    = flag.String("imported-answers-output", "", "a folder to write each GET path's answer to")
	importedAnswersPaths     = flag.String("imported-answers-paths", "", "a file of GET paths, one per line")
)

func TestImportedStandaloneAgentStoreAnswers(t *testing.T) {
	if *standaloneAgentStoreCopy == "" {
		t.Skip("no -standalone-agent-store-copy given")
	}
	if *standaloneAgentStoreHome == "" || *importedAnswersOutput == "" || *importedAnswersPaths == "" {
		t.Fatal("-standalone-agent-store-home, -imported-answers-output and -imported-answers-paths are required with the copy")
	}
	s := testStoreWithHome(t, *standaloneAgentStoreHome)
	counts, err := ImportStandaloneDatabase(context.Background(), s.db, *standaloneAgentStoreCopy)
	if err != nil {
		t.Fatal(err)
	}
	source, err := standalonesqlite.Open(*standaloneAgentStoreCopy)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for table, copied := range counts.Rows {
		var inFile, inModule int64
		if err := source.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&inFile); err != nil {
			t.Fatal(err)
		}
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&inModule); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: file %d, copied %d, module %d", table, inFile, copied, inModule)
		if inFile != copied || inModule != copied {
			t.Errorf("%s: file %d, copied %d, module %d", table, inFile, copied, inModule)
		}
	}
	for table, last := range counts.NumberingContinuesAfter {
		t.Logf("%s numbers on after %d", table, last)
	}

	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	pathList, err := os.ReadFile(*importedAnswersPaths)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(*importedAnswersOutput, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, path := range strings.Split(strings.TrimSpace(string(pathList)), "\n") {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		request := httptest.NewRequest(http.MethodGet, path, nil)
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, request)
		name := strings.NewReplacer("/", "_", "?", "_", "&", "_", "=", "_", "%", "_").Replace(path)
		if err := os.WriteFile(filepath.Join(*importedAnswersOutput, name), recorder.Body.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("GET %s: %d, %d bytes", path, recorder.Code, recorder.Body.Len())
	}
}
