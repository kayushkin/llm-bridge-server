package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/files"
	"github.com/kayushkin/llm-bridge-server/internal/references"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/gitactivity"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repos"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testReferenceAccess is principal-store and grant-store for reference-store
// in these tests: the administrator sees everything, the second principal
// holds can_see_references on the repo type, and the first holds nothing.
type testReferenceAccess struct{}

func (testReferenceAccess) Standing(_ context.Context, principalID string) (references.PrincipalStanding, error) {
	return references.PrincipalStanding{IsAdministrator: principalID == administratorTestPrincipalID}, nil
}

func (testReferenceAccess) Grants(_ context.Context, principalID string) ([]references.HeldGrant, error) {
	if principalID != secondTestPrincipalID {
		return nil, nil
	}
	return []references.HeldGrant{{Relation: references.RelationCanSeeReferences, ResourceType: references.ResourceTypeReferenceType, ResourceID: "repo"}}, nil
}

// testRepositoryModules are the three modules' pools and stores a test
// server serves.
type testRepositoryModules struct {
	repoStoreDB, workGraphStoreDB, fileStoreDB *sql.DB
	repos                                      *repos.Store
	files                                      *files.Store
}

// enableRepositoriesAndReferencesOnTestDatabase opens repo-store,
// work-graph-store and file-store on a Postgres database of the test's own,
// builds reference-store over this server, and serves all four.
func enableRepositoriesAndReferencesOnTestDatabase(t *testing.T, server *Server) *repos.Store {
	return enableRepositoryModulesOnTestDatabase(t, server).repos
}

func enableRepositoryModulesOnTestDatabase(t *testing.T, server *Server) testRepositoryModules {
	t.Helper()
	repoModule, err := repos.Module()
	if err != nil {
		t.Fatal(err)
	}
	workGraphModule, err := gitactivity.Module()
	if err != nil {
		t.Fatal(err)
	}
	fileModule, err := files.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, repoModule, workGraphModule, fileModule)
	modules := testRepositoryModules{repoStoreDB: database.Open(repoModule), workGraphStoreDB: database.Open(workGraphModule), fileStoreDB: database.Open(fileModule)}
	repos := repos.New(modules.repoStoreDB)
	modules.repos = repos
	server.EnableRepositories(repos, nil, "no gh in tests", gitactivity.New(modules.workGraphStoreDB))
	files, err := files.New(modules.fileStoreDB, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.EnableFiles(files); err != nil {
		t.Fatal(err)
	}
	modules.files = files
	if err := server.EnableReferences(&references.Resolver{
		Types:       references.Types,
		Credentials: &references.Credentials{HTTPClient: http.DefaultClient},
		Access:      &references.AccessChecker{Directory: testReferenceAccess{}, TTL: time.Minute},
		HTTPClient:  http.DefaultClient,
		InProcess:   server.InProcessReferenceReaders(),
		RecordTTL:   time.Minute, MissTTL: time.Second, LookupTimeout: 5 * time.Second, Concurrency: 4,
	}); err != nil {
		t.Fatal(err)
	}
	return modules
}

func TestTheMovedStoresAnswer503WithNoModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for path, code := range map[string]string{"/repos/repos": "repo_store_off", "/git-activity/activity": "work_graph_store_off",
		"/file-storage/limits": "file_store_off", "/references/types": "reference_store_off"} {
		if response := gated.requestAsService(t, "GET", path); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), code) {
			t.Errorf("GET %s = %d %s, want 503 %s", path, response.Code, response.Body, code)
		}
	}
}

// repo-store, work-graph-store and file-store hold rows with no owner, so only
// the operator reaches them.
func TestRepoStoreWorkGraphStoreAndFileStoreAreTheOperators(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	if created := gated.requestAsServiceWithJSON(t, "POST", "/repos/repos", map[string]any{"name": "webapp"}); created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	person := gated.loginAs(t, firstTestPrincipalID)
	administrator := gated.loginAs(t, administratorTestPrincipalID)
	for _, path := range []string{"/repos/repos/by-name/webapp", "/git-activity/session-repos?since_unix_nano=1", "/file-storage/limits"} {
		if response := gated.requestAs(t, nil, "GET", path, nil); response.Code != http.StatusUnauthorized {
			t.Errorf("GET %s with no credential = %d, want 401", path, response.Code)
		}
		if response := gated.requestAs(t, person, "GET", path, nil); response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), "operator_route") {
			t.Errorf("GET %s as a person = %d %s, want 403 operator_route", path, response.Code, response.Body)
		}
		if response := gated.requestAs(t, administrator, "GET", path, nil); response.Code != http.StatusOK {
			t.Errorf("GET %s as an administrator = %d %s, want 200", path, response.Code, response.Body)
		}
	}
	if response := gated.requestAsService(t, "POST", "/repos/repos/1/pull-requests/sync"); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "no gh in tests") {
		t.Errorf("a sync with no gh = %d %s, want 503", response.Code, response.Body)
	}
}

// A file shared into a session goes to the file-store module in this process,
// and comes back through file-store's routes with its headers.
func TestSessionFilesGoToTheFileStoreModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	stored, err := gated.server.fileStore.Accept(strings.NewReader("the bytes"), 9, "text/plain", files.NewFile{Filename: "note.txt", OwnerService: sessionFileOwnerService, OwnerRef: "br_files"})
	if err != nil || stored.ID != "file_000001" || stored.SizeBytes != 9 {
		t.Fatalf("upload: %v %+v", err, stored)
	}
	response := httptest.NewRecorder()
	gated.server.fileStore.ServeContentOf(response, httptest.NewRequest(http.MethodGet, "/", nil), stored.ID, "true")
	if response.Code != http.StatusOK || response.Body.String() != "the bytes" || response.Header().Get("Content-Type") != "text/plain" ||
		response.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("content = %d %q %v", response.Code, response.Body, response.Header())
	}
	// A part of the file, as a browser asks for a video.
	partial := httptest.NewRecorder()
	ranged := httptest.NewRequest(http.MethodGet, "/", nil)
	ranged.Header.Set("Range", "bytes=4-6")
	gated.server.fileStore.ServeContentOf(partial, ranged, stored.ID, "false")
	if partial.Code != http.StatusPartialContent || partial.Body.String() != "byt" || partial.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("range = %d %q %v", partial.Code, partial.Body, partial.Header())
	}
	missing := httptest.NewRecorder()
	gated.server.fileStore.ServeContentOf(missing, httptest.NewRequest(http.MethodGet, "/", nil), "file_000099", "true")
	if missing.Code != http.StatusNotFound || !strings.Contains(missing.Body.String(), `"error":"not found"`) {
		t.Fatalf("an unknown file = %d %s", missing.Code, missing.Body)
	}
	var refusal *files.Refusal
	if _, err := gated.server.fileStore.Accept(strings.NewReader("x"), 1, "text/plain", files.NewFile{Filename: "../x", OwnerService: sessionFileOwnerService, OwnerRef: "br_files"}); !errors.As(err, &refusal) || refusal.Status != http.StatusBadRequest {
		t.Fatalf("a name with a path separator: %v", err)
	}
	if _, err := gated.server.fileStore.Accept(strings.NewReader("x"), 26*1024*1024, "text/plain", files.NewFile{Filename: "big", OwnerService: sessionFileOwnerService, OwnerRef: "br_files"}); !errors.As(err, &refusal) || refusal.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("a file over the limit: %v", err)
	}
	listed := gated.requestAsService(t, "GET", "/file-storage/files?owner_service=llm-bridge-server&owner_ref=br_files")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"file_000001"`) {
		t.Fatalf("listed = %d %s", listed.Code, listed.Body)
	}
	if limits := gated.server.fileStore.CurrentLimits(); limits.MaximumFileBytes != 25*1024*1024 {
		t.Fatalf("limits = %+v", limits)
	}
}

func resolveThroughTheServer(t *testing.T, gated *gatedTestServer, principalID string, ids ...string) (int, map[string][]references.Match) {
	t.Helper()
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	request := httptest.NewRequest(http.MethodPost, "/references/resolve", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(serviceTokenHeader, testServiceToken)
	if principalID != "" {
		request.Header.Set(principalIdentityHeader, principalID)
	}
	response := serve(gated.server, request)
	var answer struct {
		Results map[string][]references.Match `json:"results"`
		Errors  []references.LookupError      `json:"errors"`
	}
	if response.Code == http.StatusOK {
		if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
			t.Fatal(err)
		}
		if len(answer.Errors) > 0 {
			t.Fatalf("errors: %+v", answer.Errors)
		}
	}
	return response.Code, answer.Results
}

// reference-store answers as the caller: a record of a type the caller holds
// no grant on is refused, a session is the owner's to show, and the internal
// service acting as itself sees every record. The records come from the
// stores in this process.
func TestReferenceStoreResolvesInProcessAsTheCaller(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	repoStore := enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	if _, err := repoStore.UpsertRepo(&repos.Repo{Name: "webapp", GitRemote: "git@github.com:example-owner/webapp.git"}); err != nil {
		t.Fatal(err)
	}
	if response := gated.requestAs(t, nil, "GET", "/references/types/repo", nil); response.Code != http.StatusOK {
		t.Errorf("the catalog with no credential = %d %s, want 200", response.Code, response.Body)
	}
	if response := gated.requestAs(t, nil, "POST", "/references/resolve", map[string]any{"ids": []string{"repo:webapp"}}); response.Code != http.StatusUnauthorized {
		t.Errorf("a lookup with no credential = %d, want 401", response.Code)
	}

	if _, results := resolveThroughTheServer(t, gated, firstTestPrincipalID, "repo:webapp"); len(results["repo:webapp"]) != 1 || !results["repo:webapp"][0].Refused {
		t.Errorf("a principal with no grant: %+v", results)
	}
	_, results := resolveThroughTheServer(t, gated, secondTestPrincipalID, "repo:webapp", "repo:nothing")
	if m := results["repo:webapp"]; len(m) != 1 || m[0].Refused || m[0].Label != "webapp" || m[0].PageURL != "/?view=work-graph&repo=1" {
		t.Errorf("a principal granted the type: %+v", m)
	}
	if m := results["repo:nothing"]; len(m) != 0 {
		t.Errorf("an unknown repo: %+v", m)
	}
	if _, results := resolveThroughTheServer(t, gated, "", "repo:webapp"); len(results["repo:webapp"]) != 1 || results["repo:webapp"][0].Refused {
		t.Errorf("the internal service: %+v", results)
	}

	session := gated.createSessionAs(t, gated.loginAs(t, firstTestPrincipalID), firstTestPrincipalID)
	if _, results := resolveThroughTheServer(t, gated, firstTestPrincipalID, session.SessionID); len(results[session.SessionID]) != 1 || results[session.SessionID][0].Refused {
		t.Errorf("the owner's own session: %+v", results)
	}
	if _, results := resolveThroughTheServer(t, gated, secondTestPrincipalID, session.SessionID); len(results[session.SessionID]) != 0 {
		t.Errorf("another principal's session reads as unknown: %+v", results)
	}
}

// Every type of a service in this process has a reader for each thing a
// row asks of it, and nothing is read over the network: the server's
// readers pass the check EnableReferences makes at start.
func TestEveryInProcessReferenceTypeHasAReader(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	if err := references.ValidateInProcessReaders(references.Types, gated.server.InProcessReferenceReaders()); err != nil {
		t.Fatal(err)
	}
	inProcessTypes := 0
	for _, row := range references.Types {
		if slices.Contains(InProcessReferenceServices, row.Service) && row.Get != "" {
			inProcessTypes++
		}
	}
	if inProcessTypes < 40 {
		t.Errorf("only %d types with a record route are read in this process; the list of in-process services lost some", inProcessTypes)
	}
}

// grant-store checks a reference or a reference type with reference-store in
// this process.
func TestAGrantOnAReferenceIsCheckedInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	repoStore := enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	if _, err := repoStore.UpsertRepo(&repos.Repo{Name: "webapp"}); err != nil {
		t.Fatal(err)
	}
	checker := inProcessResourceChecker{server: gated.server}
	ctx := context.Background()
	for _, check := range []struct {
		resourceType, resourceID string
		exists                   bool
	}{
		{grants.ResourceTypeReference, "repo:webapp", true},
		{grants.ResourceTypeReference, "repo:nothing", false},
		{grants.ResourceTypeReferenceType, "repo", true},
		{grants.ResourceTypeReferenceType, "nothing", false},
	} {
		err := checker.CheckResourceExists(ctx, check.resourceType, check.resourceID)
		if check.exists && err != nil {
			t.Errorf("%s %s: %v", check.resourceType, check.resourceID, err)
		}
		if !check.exists && !errors.Is(err, grants.ErrResourceNotFound) {
			t.Errorf("%s %s: %v, want not found", check.resourceType, check.resourceID, err)
		}
	}
}
