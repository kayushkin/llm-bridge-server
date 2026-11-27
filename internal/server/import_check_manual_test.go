package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	logstoreserver "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/server"
	logstorestore "github.com/kayushkin/llm-bridge-server/internal/sessionlog/logstore/store"
	"github.com/kayushkin/llm-bridge-server/internal/sessionlog/sessionevents"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// TestImportedBridgeDatabaseAnswersTheCoreRoutes writes this server's answers
// to GET requests over a bridge core module that import-bridge-database
// filled, for comparing with the answers of the server the file came from.
// It is for landing day and runs only when all three are named:
//
//	BRIDGE_IMPORT_CHECK_POSTGRES_URL=<the Postgres the file was imported into>
//	BRIDGE_IMPORT_CHECK_PATHS=<a JSON file: an object whose keys are the paths>
//	BRIDGE_IMPORT_CHECK_ANSWERS=<the JSON file to write the answers to>
//
// The server starts nothing: no harness, no discovery, no delivery. The
// session log module is opened on the same database, empty.
func TestImportedBridgeDatabaseAnswersTheCoreRoutes(t *testing.T) {
	postgresURL, pathsFile, answersFile := os.Getenv("BRIDGE_IMPORT_CHECK_POSTGRES_URL"), os.Getenv("BRIDGE_IMPORT_CHECK_PATHS"), os.Getenv("BRIDGE_IMPORT_CHECK_ANSWERS")
	if postgresURL == "" || pathsFile == "" || answersFile == "" {
		t.Skip("set BRIDGE_IMPORT_CHECK_POSTGRES_URL, BRIDGE_IMPORT_CHECK_PATHS and BRIDGE_IMPORT_CHECK_ANSWERS to check an import")
	}
	ctx := context.Background()
	coreModule, err := store.Module()
	if err != nil {
		t.Fatal(err)
	}
	writer, err := postgresstorage.Open(ctx, postgresURL, coreModule, postgresstorage.ScanTimestampsInUTC)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenReader(ctx, postgresURL, coreModule)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(writer, reader)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	sessionLogModule, err := logstorestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	administrator, err := postgresstorage.Open(ctx, postgresURL, postgresstorage.Module{Schema: "public"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := postgresstorage.Provision(ctx, administrator, sessionLogModule); err != nil {
		t.Fatal(err)
	}
	administrator.Close()
	sessionLogWriter, err := postgresstorage.Open(ctx, postgresURL, sessionLogModule)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := postgresstorage.Migrate(ctx, sessionLogWriter, sessionLogModule); err != nil {
		t.Fatal(err)
	}
	sessionLogReader, err := logstorestore.OpenReader(ctx, postgresURL, sessionLogModule)
	if err != nil {
		t.Fatal(err)
	}
	logServer := logstoreserver.New(logstorestore.New(sessionLogWriter, sessionLogReader), nil)
	if err := st.GrantSessionLogWrites(ctx, sessionLogModule.Role); err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	cfg := &config.Config{ImagesDir: filepath.Join(directory, "images"), BridgePrefsPath: filepath.Join(directory, "prefs.json")}
	testAuthorizationConfig(cfg)
	srv := New(st, nil, nil, nil, nil, testModelStore(t), nil, cfg)
	srv.EnableSessionLog(logServer, logServer)
	singleCopy, err := sessionevents.OpenSingleCopy(ctx, st, logServer)
	if err != nil {
		t.Fatal(err)
	}
	srv.UseSessionEventStore(singleCopy)

	raw, err := os.ReadFile(pathsFile)
	if err != nil {
		t.Fatal(err)
	}
	var paths map[string]json.RawMessage
	if err := json.Unmarshal(raw, &paths); err != nil {
		t.Fatal(err)
	}
	type answer struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
	}
	answers := map[string]answer{}
	for path := range paths {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set(serviceTokenHeader, testServiceToken)
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		body := recorder.Body.Bytes()
		if !json.Valid(body) {
			encoded, _ := json.Marshal(string(body))
			body = encoded
		}
		answers[path] = answer{Status: recorder.Code, Body: body}
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(answersFile, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d answers written", len(answers))
}
