package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/agents/hooks"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/machinemanagement/harnessstore"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/snapshotstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
	_ "modernc.org/sqlite"
)

// The check of the four importers of the server's own former SQLite files
// against copies of a live server's files, run by hand:
//
//	go test ./internal/server -run TestTheImportedBridgeOwnStoresAnswerAsTheLiveServer \
//	  -harness-store-copy=<copy> -hook-store-copy=<copy> -snapshot-store-copy=<copy> -operations-copy=<copy> \
//	  -live-bridge-url=http://localhost:8160 -live-bridge-token-file=<file holding an administrator's bearer token>
//
// It imports each copy into a test database, serves the GET routes of the
// imported records from a server built on them, and asks the same routes of
// the live server, which it only reads. Times are compared to the
// microsecond, the precision Postgres keeps. It reports counts and the paths
// that differ, never a body. A record the live server changed after the copy
// was made (a runner checking in, a new operation) differs for that reason
// alone, so make the copies just before the run.
var (
	harnessStoreCopy        = flag.String("harness-store-copy", "", "a copy of the server's harness-store SQLite file")
	hookStoreCopy           = flag.String("hook-store-copy", "", "a copy of the server's hook-store SQLite file")
	snapshotStoreCopy       = flag.String("snapshot-store-copy", "", "a copy of the server's snapshot-store SQLite file")
	operationsCopy          = flag.String("operations-copy", "", "a copy of the server's operations SQLite file")
	liveBridgeURL           = flag.String("live-bridge-url", "", "the live server to compare with, read only")
	liveBridgeTokenFilePath = flag.String("live-bridge-token-file", "", "a file holding an administrator's bearer token for the live server")
)

// timesToTheMicrosecond rewrites every string in a decoded answer that is an
// RFC 3339 time as that time in UTC, cut to the microsecond.
func timesToTheMicrosecond(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, inner := range typed {
			typed[key] = timesToTheMicrosecond(inner)
		}
	case []any:
		for index, inner := range typed {
			typed[index] = timesToTheMicrosecond(inner)
		}
	case string:
		if at, err := time.Parse(time.RFC3339Nano, typed); err == nil {
			return at.UTC().Truncate(time.Microsecond).Format(time.RFC3339Nano)
		}
	}
	return value
}

func TestTheImportedBridgeOwnStoresAnswerAsTheLiveServer(t *testing.T) {
	if *harnessStoreCopy == "" || *liveBridgeURL == "" {
		t.Skip("no -harness-store-copy and -live-bridge-url given")
	}
	tokenBytes, err := os.ReadFile(*liveBridgeTokenFilePath)
	if err != nil {
		t.Fatal(err)
	}
	liveToken := strings.TrimSpace(string(tokenBytes))
	ctx := context.Background()

	harnessModule, err := harnessstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	hookModule, err := hooks.Module()
	if err != nil {
		t.Fatal(err)
	}
	snapshotModule, err := snapshotstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	operationModule, err := operationstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, harnessModule, hookModule, snapshotModule, operationModule)
	harnessDB, hookDB, snapshotDB, operationDB := database.Open(harnessModule), database.Open(hookModule), database.Open(snapshotModule), database.Open(operationModule)
	for name, run := range map[string]func() (any, error){
		"harness-store": func() (any, error) { return harnessstore.ImportStandaloneDatabase(ctx, harnessDB, *harnessStoreCopy) },
		"hook-store":    func() (any, error) { return hooks.ImportStandaloneDatabase(ctx, hookDB, *hookStoreCopy) },
		"snapshot-store": func() (any, error) {
			return snapshotstore.ImportStandaloneDatabase(ctx, snapshotDB, *snapshotStoreCopy)
		},
		"operations": func() (any, error) { return operationstore.ImportStandaloneDatabase(ctx, operationDB, *operationsCopy) },
	} {
		started := time.Now()
		counts, err := run()
		if err != nil {
			t.Fatalf("import %s: %v", name, err)
		}
		t.Logf("imported %s in %s: %+v", name, time.Since(started).Round(time.Millisecond), counts)
	}

	directory := t.TempDir()
	bridgeStore := newTestBridgeStore(t)
	snapshots, err := snapshotstore.New(snapshotDB, snapshotstore.Config{GitDir: filepath.Join(directory, "snapshots.git")})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{ImagesDir: filepath.Join(directory, "images"), BridgePrefsPath: filepath.Join(directory, "prefs.json")}
	testAuthorizationConfig(cfg)
	srv := New(bridgeStore, nil, nil, harnessstore.New(harnessDB), hooks.New(hookDB), nil, snapshots, cfg)
	operationStore := operationstore.New(operationDB)
	coordinator, err := operations.NewCoordinator(operationStore, operations.Config{WorkerCount: 1, LeaseDuration: time.Minute, LeaseOwner: "import check"})
	if err != nil {
		t.Fatal(err)
	}
	srv.EnableOperations(coordinator)
	imported := asInternalService(srv)

	plain := &http.Client{Timeout: time.Minute}
	// A path may end in #Last-Event-ID=<n>, sent as that header: an event
	// stream resumed after event n.
	splitResume := func(path string) (string, string) {
		route, resumeAfter, _ := strings.Cut(path, "#Last-Event-ID=")
		return route, resumeAfter
	}
	getLive := func(path string) (int, []byte) {
		route, resumeAfter := splitResume(path)
		request, err := http.NewRequest(http.MethodGet, *liveBridgeURL+route, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resumeAfter != "" {
			request.Header.Set("Last-Event-ID", resumeAfter)
		}
		request.Header.Set("Authorization", "Bearer "+liveToken)
		response, err := plain.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, body
	}
	getImported := func(path string) (int, []byte) {
		route, resumeAfter := splitResume(path)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, route, nil)
		if resumeAfter != "" {
			request.Header.Set("Last-Event-ID", resumeAfter)
		}
		imported.ServeHTTP(recorder, request)
		return recorder.Code, recorder.Body.Bytes()
	}
	ids := func(path, query string) []string {
		source, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		defer source.Close()
		rows, err := source.Query(query)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var found []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			found = append(found, id)
		}
		return found
	}

	paths := []string{"/machines", "/instances", "/hooks", "/operation-budgets"}
	for _, id := range ids(*harnessStoreCopy, `SELECT id FROM machines ORDER BY id`) {
		paths = append(paths, "/machines/"+id)
	}
	for _, id := range ids(*harnessStoreCopy, `SELECT id FROM instances ORDER BY id`) {
		paths = append(paths, "/instances/"+id, "/instances/"+id+"/credentials")
	}
	for _, id := range ids(*hookStoreCopy, `SELECT id FROM hooks ORDER BY id`) {
		paths = append(paths, "/hooks/"+id)
	}
	// Operations made before the copy, so a new one on the live server does
	// not shift the pages.
	operationIDs := ids(*operationsCopy, `SELECT id FROM operations ORDER BY created_at DESC, id DESC`)
	if len(operationIDs) > 0 {
		newest, err := operationStore.Get(operationIDs[0])
		if err != nil {
			t.Fatal(err)
		}
		before := newest.Receipt.CreatedAt.Add(time.Second).UTC().Format(time.RFC3339)
		paths = append(paths, "/operations?limit=500&created_before="+before)
		for _, state := range []string{"succeeded", "failed", "cancelled"} {
			paths = append(paths, "/operations?limit=500&state="+state+"&created_before="+before)
		}
	}
	for index, id := range operationIDs {
		paths = append(paths, "/operations/"+id, "/operations/"+id+"/children")
		if index < 200 {
			paths = append(paths, "/operations/"+id+"/events", "/operations/"+id+"/events#Last-Event-ID=2")
		}
	}

	same, different := 0, []string{}
	for _, path := range paths {
		liveStatus, liveBody := getLive(path)
		importedStatus, importedBody := getImported(path)
		var live, importedAnswer any
		liveErr := json.Unmarshal(liveBody, &live)
		importedErr := json.Unmarshal(importedBody, &importedAnswer)
		if liveStatus == importedStatus && liveErr == nil && importedErr == nil &&
			reflect.DeepEqual(timesToTheMicrosecond(live), timesToTheMicrosecond(importedAnswer)) {
			same++
			continue
		}
		if liveStatus == importedStatus && liveErr != nil && importedErr != nil && string(liveBody) == string(importedBody) {
			same++
			continue
		}
		different = append(different, fmt.Sprintf("%s (live %d, imported %d)", path, liveStatus, importedStatus))
	}

	// Snapshots: the session route needs the bridge's own session row, so the
	// rows are compared through the store instead, every tool call of the copy,
	// against the copy itself.
	snapshotCalls := ids(*snapshotStoreCopy, `SELECT DISTINCT session_id || char(9) || tool_use_id FROM snapshots ORDER BY 1`)
	snapshotSame := 0
	source, err := sql.Open("sqlite", "file:"+*snapshotStoreCopy+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, call := range snapshotCalls {
		sessionID, toolUseID, _ := strings.Cut(call, "\t")
		importedRows, err := snapshots.Get(sessionID, toolUseID)
		if err != nil {
			t.Fatal(err)
		}
		rows, err := source.Query(`SELECT phase, file_path, blob_sha, size, is_binary, too_large, missing, created_at FROM snapshots
			WHERE session_id = ? AND tool_use_id = ?`, sessionID, toolUseID)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]snapshotstore.Snapshot{}
		for rows.Next() {
			var snapshot snapshotstore.Snapshot
			var phase string
			if err := rows.Scan(&phase, &snapshot.FilePath, &snapshot.BlobSHA, &snapshot.Size, &snapshot.IsBinary, &snapshot.TooLarge, &snapshot.Missing, &snapshot.CreatedAt); err != nil {
				t.Fatal(err)
			}
			snapshot.SessionID, snapshot.ToolUseID, snapshot.Phase = sessionID, toolUseID, snapshotstore.Phase(phase)
			snapshot.CreatedAt = snapshot.CreatedAt.UTC().Truncate(time.Microsecond)
			want[phase+"\t"+snapshot.FilePath] = snapshot
		}
		rows.Close()
		got := map[string]snapshotstore.Snapshot{}
		for _, snapshot := range importedRows {
			got[string(snapshot.Phase)+"\t"+snapshot.FilePath] = snapshot
		}
		if reflect.DeepEqual(want, got) {
			snapshotSame++
		} else {
			different = append(different, "snapshots of one tool call (row comparison)")
		}
	}

	sort.Strings(different)
	t.Logf("%d of %d GET paths answered the same; %d of %d tool calls' snapshot rows the same", same, len(paths), snapshotSame, len(snapshotCalls))
	for _, path := range different {
		t.Logf("differs: %s", path)
	}
	if len(different) > 0 {
		t.Fail()
	}
}
