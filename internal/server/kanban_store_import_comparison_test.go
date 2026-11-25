package server

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/access"
	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
	kanbandb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	"github.com/kayushkin/llm-bridge/msg"
)

var (
	standaloneKanbanStoreCopy    = flag.String("standalone-kanban-store-copy-for-routes", "", "a copy of a standalone kanban-store database to import and serve")
	standalonePrincipalStoreCopy = flag.String("standalone-principal-store-copy", "", "a copy of a standalone principal-store database to import beside it")
	standaloneGrantStoreCopy     = flag.String("standalone-grant-store-copy", "", "a copy of a standalone grant-store database to import beside it")
	kanbanComparisonRequests     = flag.String("kanban-comparison-requests", "", `a file of "<principal id or -> <path>" lines to ask /kanban-store/ as`)
	kanbanComparisonOutput       = flag.String("kanban-comparison-output", "", "where to write the answers, one JSON object a line")
)

// TestImportedStandaloneKanbanStoreAnswers imports copies of a standalone
// kanban-store, the noteboard it read card content from, and the
// principal-store and grant-store it read callers from, serves them as main
// does, and writes what /kanban-store/<path> answers each caller, for a
// comparison with the standalone service's own answers to the same requests.
// A principal is asked as dash asks for one of its users (the service token
// and X-Principal-Id); "-" is the service token alone, the internal service.
func TestImportedStandaloneKanbanStoreAnswers(t *testing.T) {
	if *standaloneKanbanStoreCopy == "" {
		t.Skip("no -standalone-kanban-store-copy-for-routes given")
	}
	for name, value := range map[string]string{
		"-standalone-noteboard-copy":       *standaloneNoteboardCopy,
		"-standalone-principal-store-copy": *standalonePrincipalStoreCopy,
		"-standalone-grant-store-copy":     *standaloneGrantStoreCopy,
		"-kanban-comparison-requests":      *kanbanComparisonRequests,
		"-kanban-comparison-output":        *kanbanComparisonOutput,
	} {
		if value == "" {
			t.Fatalf("%s is needed beside -standalone-kanban-store-copy-for-routes", name)
		}
	}
	ctx := context.Background()
	server, _, _ := testServerWithInstance(t, msg.HarnessMock)
	// GET /api/entity-types serves reference-store's list of types, from
	// reference-store in this process.
	if err := server.EnableReferenceStore(&referencestore.Resolver{Types: referencestore.Types, Credentials: &referencestore.Credentials{}}); err != nil {
		t.Fatal(err)
	}

	accessModules, err := access.Modules()
	if err != nil {
		t.Fatal(err)
	}
	kanbanModule, err := kanbandb.Module()
	if err != nil {
		t.Fatal(err)
	}
	noteboardModule, err := items.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, append(accessModules, kanbanModule, noteboardModule)...)
	principalsPool, grantsPool := database.Open(accessModules[0]), database.Open(accessModules[1])
	if _, err := principalstore.ImportStandaloneDatabase(ctx, principalsPool, *standalonePrincipalStoreCopy); err != nil {
		t.Fatalf("principal-store: %v", err)
	}
	if _, err := grantstore.ImportStandaloneDatabase(ctx, grantsPool, *standaloneGrantStoreCopy); err != nil {
		t.Fatalf("grant-store: %v", err)
	}
	stores := access.StoresOnPools(principalsPool, grantsPool, database.Open(accessModules[2]), database.Open(accessModules[3]))
	server.EnableAccess(stores)

	noteboardPool, kanbanPool := database.Open(noteboardModule), database.Open(kanbanModule)
	if _, err := items.ImportStandaloneDatabase(ctx, noteboardPool, *standaloneNoteboardCopy); err != nil {
		t.Fatalf("noteboard: %v", err)
	}
	counts, err := kanbandb.ImportStandaloneDatabase(ctx, kanbanPool, *standaloneKanbanStoreCopy)
	if err != nil {
		t.Fatalf("kanban-store: %v", err)
	}
	t.Logf("kanban-store imported: %+v", counts)
	server.EnableKanbanStore(kanbandb.New(kanbanPool), KanbanStoreOwners{Noteboard: items.New(noteboardPool), Access: stores})

	requests, err := os.Open(*kanbanComparisonRequests)
	if err != nil {
		t.Fatal(err)
	}
	defer requests.Close()
	output, err := os.Create(*kanbanComparisonOutput)
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	encoder := json.NewEncoder(output)
	lines := bufio.NewScanner(requests)
	lines.Buffer(make([]byte, 1<<20), 1<<20)
	for lines.Scan() {
		principalID, path, found := strings.Cut(strings.TrimSpace(lines.Text()), " ")
		if !found {
			continue
		}
		request := httptest.NewRequest("GET", "/kanban-store"+path, nil)
		request.Header.Set(serviceTokenHeader, testServiceToken)
		if principalID != "-" {
			request.Header.Set(principalIdentityHeader, principalID)
		}
		response := serve(server, request)
		if err := encoder.Encode(map[string]any{"principal": principalID, "path": path, "status": response.Code, "body": response.Body.String()}); err != nil {
			t.Fatal(err)
		}
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
}
