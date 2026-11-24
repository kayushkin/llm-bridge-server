package server

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/filestorage/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/workgraphstore"
)

// Copies of standalone stores' files, made with sqlite3 .backup, to check
// the imports against: the test imports each copy given into a fresh test
// database and writes what the mounted routes answer for each path, for a
// comparison with the standalone stores' own answers.
//
//	go test ./internal/server -run TestImportedRepositoriesAndFilesAnswer -args \
//	  -standalone-repo-store-copy=/tmp/repo.db -standalone-work-graph-store-copy=/tmp/workgraph.db \
//	  -standalone-file-store-copy=/tmp/file.db -standalone-file-store-blobs=<data dir>/blobs \
//	  -imported-answers-paths=/tmp/paths.txt -imported-answers-output=/tmp/answers.json
//
// The paths file holds one route per line, with its store's prefix
// (/repo-store/repos); a POST /reference-store/resolve line carries its ids
// after the path, comma separated, asked as the internal service.
var (
	standaloneRepoStoreCopy      = flag.String("standalone-repo-store-copy", "", "a copy of a standalone repo-store database to import")
	standaloneWorkGraphStoreCopy = flag.String("standalone-work-graph-store-copy", "", "a copy of a standalone work-graph-store database to import")
	standaloneFileStoreCopy      = flag.String("standalone-file-store-copy", "", "a copy of a standalone file-store database to import")
	standaloneFileStoreBlobs     = flag.String("standalone-file-store-blobs", "", "the standalone file-store's blobs directory, copied from")
	importedAnswersPaths         = flag.String("imported-answers-paths", "", "the routes to ask, one per line")
	importedAnswersOutput        = flag.String("imported-answers-output", "", "where to write what each route answered")
)

func TestImportedRepositoriesAndFilesAnswer(t *testing.T) {
	if *importedAnswersPaths == "" || *importedAnswersOutput == "" {
		t.Skip("no -imported-answers-paths and -imported-answers-output given")
	}
	gated := newGatedTestServer(t, nil)
	modules := enableRepositoryModulesOnTestDatabase(t, gated.server)
	ctx := context.Background()
	imported := map[string]any{}
	if *standaloneRepoStoreCopy != "" {
		counts, err := repostore.ImportStandaloneDatabase(ctx, modules.repoStoreDB, *standaloneRepoStoreCopy)
		if err != nil {
			t.Fatalf("repo-store import: %v", err)
		}
		imported["repo-store"] = counts
	}
	if *standaloneWorkGraphStoreCopy != "" {
		counts, err := workgraphstore.ImportStandaloneDatabase(ctx, modules.workGraphStoreDB, *standaloneWorkGraphStoreCopy)
		if err != nil {
			t.Fatalf("work-graph-store import: %v", err)
		}
		imported["work-graph-store"] = counts
	}
	if *standaloneFileStoreCopy != "" {
		counts, err := filestore.ImportStandaloneDatabase(ctx, modules.fileStoreDB, modules.files, *standaloneFileStoreCopy, *standaloneFileStoreBlobs, filestore.CopyBlobs)
		if err != nil {
			t.Fatalf("file-store import: %v", err)
		}
		imported["file-store"] = counts
	}
	paths, err := os.ReadFile(*importedAnswersPaths)
	if err != nil {
		t.Fatal(err)
	}
	answers := map[string]json.RawMessage{}
	for _, line := range strings.Split(strings.TrimSpace(string(paths)), "\n") {
		var response *httptest.ResponseRecorder
		if path, ids, isResolve := strings.Cut(line, " "); isResolve {
			body, _ := json.Marshal(map[string][]string{"ids": strings.Split(ids, ",")})
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
			request.Header.Set(serviceTokenHeader, testServiceToken)
			request.Header.Set("Content-Type", "application/json")
			response = serve(gated.server, request)
		} else {
			response = gated.requestAsService(t, "GET", line)
		}
		if response.Code != http.StatusOK || !json.Valid(response.Body.Bytes()) {
			t.Errorf("%s answered %d: %s", line, response.Code, response.Body)
			continue
		}
		answers[line] = json.RawMessage(response.Body.Bytes())
	}
	encoded, err := json.MarshalIndent(map[string]any{"imported": imported, "answers": answers}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*importedAnswersOutput, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}
