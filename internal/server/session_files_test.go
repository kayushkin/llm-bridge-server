package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/filestoreclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// fakeFileStore is file-store's routes as this server uses them: /limits, an
// upload, and content with the headers file-store sends.
type fakeFileStore struct {
	mu               sync.Mutex
	maximumFileBytes int64
	uploads          map[string][]byte
	contentTypes     map[string]string
	ownerRefs        map[string]string
	tokensSeen       []string
}

func newFakeFileStore(t *testing.T) (*fakeFileStore, *httptest.Server) {
	fake := &fakeFileStore{maximumFileBytes: 1024, uploads: map[string][]byte{}, contentTypes: map[string]string{}, ownerRefs: map[string]string{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.tokensSeen = append(fake.tokensSeen, r.Header.Get(filestoreclient.ServiceTokenHeader))
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/limits":
			json.NewEncoder(w).Encode(map[string]any{"maximum_file_bytes": fake.maximumFileBytes})
		case r.Method == http.MethodPost && r.URL.Path == "/files":
			body, _ := io.ReadAll(r.Body)
			id := "file_00000" + string(rune('1'+len(fake.uploads)))
			fake.uploads[id] = body
			fake.contentTypes[id] = r.Header.Get("Content-Type")
			fake.ownerRefs[id] = r.URL.Query().Get("owner_ref")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": id, "filename": r.URL.Query().Get("filename"),
				"size_bytes": len(body), "content_type": r.Header.Get("Content-Type")})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/content"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/files/"), "/content")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
			if r.URL.Query().Get("inline") == "true" {
				w.Header().Set("Content-Type", fake.contentTypes[id])
			} else {
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", "attachment")
			}
			w.Write(fake.uploads[id])
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return fake, server
}

// fakeLogStore takes every event push, as log-store does.
func fakeLogStore(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server
}

func testServerWithFileStore(t *testing.T) (*Server, *store.Store, *fakeFileStore, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	fake, fileStoreServer := newFakeFileStore(t)
	sessionFilesDir := filepath.Join(dir, "session-files")
	cfg := &config.Config{
		ImagesDir:             filepath.Join(dir, "images"),
		BridgePrefsPath:       filepath.Join(dir, "prefs.json"),
		LogStoreURL:           fakeLogStore(t).URL,
		FileStoreURL:          fileStoreServer.URL,
		FileStoreServiceToken: "file-store-token",
		SessionFilesDir:       sessionFilesDir,
		ListenAddr:            "127.0.0.1:8160",
	}
	testAuthorizationConfig(cfg)
	srv := New(st, nil, nil, nil, nil, testModelStore(t), nil, cfg)
	return srv, st, fake, sessionFilesDir
}

func shareRequest(sessionID, filename, contentType string, body []byte) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/sessions/"+sessionID+"/files?filename="+filename, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	return request
}

func TestTheUserSharesAFileIntoASession(t *testing.T) {
	srv, st, fake, sessionFilesDir := testServerWithFileStore(t)
	newSessionForSignals(t, st, "br_files", msg.SessionTypeInteractive)
	events := srv.harness.Subscribe("br_files")

	recorder := httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, shareRequest("br_files", "chart.png", "image/png", []byte("PNGBYTES")))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("share: %d %s", recorder.Code, recorder.Body)
	}
	var shared msg.SessionFile
	if err := json.Unmarshal(recorder.Body.Bytes(), &shared); err != nil {
		t.Fatal(err)
	}
	if shared.SharedBy != msg.SessionFileSharedByUser || shared.Filename != "chart.png" || shared.MediaType != "image/png" || shared.SizeBytes != 8 {
		t.Errorf("shared = %+v", shared)
	}
	if fake.ownerRefs[shared.FileID] != "br_files" || fake.tokensSeen[0] != "file-store-token" {
		t.Errorf("file-store got owner %q and token %q", fake.ownerRefs[shared.FileID], fake.tokensSeen[0])
	}

	// The agent reads the same bytes at the path the record gives.
	wantPath := filepath.Join(sessionFilesDir, "br_files", shared.FileID, "chart.png")
	if shared.Path != wantPath {
		t.Errorf("path = %q, want %q", shared.Path, wantPath)
	}
	if onDisk, err := os.ReadFile(shared.Path); err != nil || string(onDisk) != "PNGBYTES" {
		t.Errorf("agent copy = %q, %v", onDisk, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(sessionFilesDir, "br_files", ".incoming-*")); len(leftovers) != 0 {
		t.Errorf("spool files left behind: %v", leftovers)
	}

	listed, err := st.ListSessionFiles("br_files")
	if err != nil || len(listed) != 1 || listed[0].FileID != shared.FileID {
		t.Errorf("listed = %+v, %v", listed, err)
	}

	select {
	case stored := <-events:
		if stored.Event.Type != msg.EventSessionFile || stored.Event.SessionFile == nil || stored.Event.SessionFile.FileID != shared.FileID {
			t.Errorf("event = %+v", stored.Event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no session_file event reached a subscriber")
	}

	// The bytes come back through this server with file-store's headers.
	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sessions/br_files/files/"+shared.FileID+"/content?inline=true", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "PNGBYTES" {
		t.Fatalf("content: %d %q", recorder.Code, recorder.Body)
	}
	if recorder.Header().Get("Content-Security-Policy") == "" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("content lost file-store's safety headers: %v", recorder.Header())
	}

	// A file is reached only through the session it was shared into.
	newSessionForSignals(t, st, "br_other", msg.SessionTypeInteractive)
	recorder = httptest.NewRecorder()
	asInternalService(srv).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/sessions/br_other/files/"+shared.FileID+"/content", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("another session's file: %d, want 404", recorder.Code)
	}
}

func TestAShareIsRefusedBeforeItIsKept(t *testing.T) {
	srv, st, fake, sessionFilesDir := testServerWithFileStore(t)
	newSessionForSignals(t, st, "br_files", msg.SessionTypeInteractive)
	cases := []struct {
		name     string
		request  *http.Request
		wantCode int
	}{
		{"a path for a name", shareRequest("br_files", "..%2Fescape.txt", "text/plain", []byte("x")), http.StatusBadRequest},
		{"no name", shareRequest("br_files", "", "text/plain", []byte("x")), http.StatusBadRequest},
		{"no content type", shareRequest("br_files", "a.txt", "", []byte("x")), http.StatusBadRequest},
		{"larger than file-store takes", shareRequest("br_files", "big.bin", "application/octet-stream", bytes.Repeat([]byte("x"), 2048)), http.StatusRequestEntityTooLarge},
		{"no such session", shareRequest("br_missing", "a.txt", "text/plain", []byte("x")), http.StatusNotFound},
	}
	for _, c := range cases {
		recorder := httptest.NewRecorder()
		asInternalService(srv).ServeHTTP(recorder, c.request)
		if recorder.Code != c.wantCode {
			t.Errorf("%s: %d %s, want %d", c.name, recorder.Code, recorder.Body, c.wantCode)
		}
	}
	if len(fake.uploads) != 0 {
		t.Errorf("file-store kept %d refused uploads", len(fake.uploads))
	}
	files, _ := filepath.Glob(filepath.Join(sessionFilesDir, "*", "*"))
	if len(files) != 0 {
		t.Errorf("refused uploads left files on disk: %v", files)
	}
}

func TestAnAgentSharesIntoItsOwnSessionAndNoOther(t *testing.T) {
	srv, st, _, _ := testServerWithFileStore(t)
	session := newSessionForSignals(t, st, "br_agent", msg.SessionTypeInteractive)
	newSessionForSignals(t, st, "br_neighbour", msg.SessionTypeInteractive)

	environment := map[string]string{}
	for _, variable := range srv.sessionFileSharingEnvironment(session) {
		name, value, _ := strings.Cut(variable, "=")
		environment[name] = value
	}
	token := environment[sessionFileTokenEnvironmentVariable]
	if token == "" || environment[sessionFilesURLEnvironmentVariable] != "http://127.0.0.1:8160/sessions/br_agent/files" {
		t.Fatalf("child environment = %v", environment)
	}

	withToken := func(request *http.Request) *httptest.ResponseRecorder {
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		srv.ServeHTTP(recorder, request)
		return recorder
	}

	recorder := withToken(shareRequest("br_agent", "plot.png", "image/png", []byte("PLOT")))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("agent share into its own session: %d %s", recorder.Code, recorder.Body)
	}
	var shared msg.SessionFile
	json.Unmarshal(recorder.Body.Bytes(), &shared)
	if shared.SharedBy != msg.SessionFileSharedByAgent {
		t.Errorf("shared_by = %q, want agent", shared.SharedBy)
	}

	if code := withToken(shareRequest("br_neighbour", "plot.png", "image/png", []byte("PLOT"))).Code; code != http.StatusNotFound {
		t.Errorf("agent share into another session: %d, want 404", code)
	}
	// The token opens the share route and nothing else — not even reading
	// its own session's files back.
	if code := withToken(httptest.NewRequest(http.MethodGet, "/sessions/br_agent/files", nil)).Code; code != http.StatusUnauthorized {
		t.Errorf("token on the list route: %d, want 401", code)
	}
	if code := withToken(httptest.NewRequest(http.MethodPost, "/sessions/br_agent/send", strings.NewReader(`{"message":"x"}`))).Code; code != http.StatusUnauthorized {
		t.Errorf("token on send: %d, want 401", code)
	}
}

func TestASessionFileTokenIsNoOtherKindOfCredential(t *testing.T) {
	srv, _ := testServer(t)
	codec := srv.principalSessionCookieCodec
	token := codec.encodeSessionFileToken("br_1")
	if sessionID, err := codec.decodeSessionFileToken(token); err != nil || sessionID != "br_1" {
		t.Fatalf("round trip: %q, %v", sessionID, err)
	}
	if _, err := codec.decodeSessionAgentToken(token); err == nil {
		t.Error("a session file token verified as a session agent token")
	}
	agentToken := codec.encodeSessionAgentToken(sessionAgentTokenClaims{PrincipalID: "principal_000001", SessionID: "br_1", ExpiresAt: time.Now().Add(time.Hour)})
	if _, err := codec.decodeSessionFileToken(agentToken); err == nil {
		t.Error("a session agent token verified as a session file token")
	}
	forged := strings.TrimSuffix(token, token[len(token)-4:]) + "AAAA"
	if _, err := codec.decodeSessionFileToken(forged); err == nil {
		t.Error("a token with a changed signature verified")
	}
}

func TestNoChildIsGivenTheFileStoreToken(t *testing.T) {
	for _, name := range config.SecretEnvironmentVariableNames() {
		if name == config.FileStoreServiceTokenEnvironmentVariable {
			return
		}
	}
	t.Fatalf("%s reads every file file-store holds and is not stripped from harness children", config.FileStoreServiceTokenEnvironmentVariable)
}

func TestAnUnpromptedTurnRetiresTheQuestionItMovedPast(t *testing.T) {
	srv, st := testServer(t)
	newSessionForSignals(t, st, "br_q", msg.SessionTypeInteractive)
	if err := st.CreateSignal(&store.Signal{ID: "sig_stale", SessionID: "br_q", Kind: msg.SignalKindQuestion,
		Source: msg.SignalSourceDerived, Surface: msg.SignalSurfaceChat, Title: "Ready for dialog-wording check", State: msg.SignalStateOpen}); err != nil {
		t.Fatal(err)
	}
	srv.onUnpromptedTurnStart("br_q", &msg.Event{Type: msg.EventUserMessage})
	if open := openSignals(t, st, "br_q"); len(open) != 0 {
		t.Errorf("open after an unprompted turn started: %+v", open)
	}
}
