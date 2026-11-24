package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantstore"
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/workgraphstore"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testReferenceAccess is principal-store and grant-store for reference-store
// in these tests: the administrator sees everything, the second principal
// holds can_see_references on the repo type, and the first holds nothing.
type testReferenceAccess struct{}

func (testReferenceAccess) Standing(_ context.Context, principalID string) (referencestore.PrincipalStanding, error) {
	return referencestore.PrincipalStanding{IsAdministrator: principalID == administratorTestPrincipalID}, nil
}

func (testReferenceAccess) Grants(_ context.Context, principalID string) ([]referencestore.HeldGrant, error) {
	if principalID != secondTestPrincipalID {
		return nil, nil
	}
	return []referencestore.HeldGrant{{Relation: referencestore.RelationCanSeeReferences, ResourceType: referencestore.ResourceTypeReferenceType, ResourceID: "repo"}}, nil
}

// testRepositoryModules are the three modules' pools and stores a test
// server serves.
type testRepositoryModules struct {
	repoStoreDB, workGraphStoreDB, fileStoreDB *sql.DB
	repos                                      *repostore.Store
	files                                      *filestore.Store
}

// enableRepositoriesAndReferencesOnTestDatabase opens repo-store,
// work-graph-store and file-store on a Postgres database of the test's own,
// builds reference-store over this server, and serves all four.
func enableRepositoriesAndReferencesOnTestDatabase(t *testing.T, server *Server) *repostore.Store {
	return enableRepositoryModulesOnTestDatabase(t, server).repos
}

func enableRepositoryModulesOnTestDatabase(t *testing.T, server *Server) testRepositoryModules {
	t.Helper()
	repoModule, err := repostore.Module()
	if err != nil {
		t.Fatal(err)
	}
	workGraphModule, err := workgraphstore.Module()
	if err != nil {
		t.Fatal(err)
	}
	fileModule, err := filestore.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, repoModule, workGraphModule, fileModule)
	modules := testRepositoryModules{repoStoreDB: database.Open(repoModule), workGraphStoreDB: database.Open(workGraphModule), fileStoreDB: database.Open(fileModule)}
	repos := repostore.New(modules.repoStoreDB)
	modules.repos = repos
	server.EnableRepositories(repos, nil, "no gh in tests", workgraphstore.New(modules.workGraphStoreDB))
	files, err := filestore.New(modules.fileStoreDB, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := server.EnableFileStore(files); err != nil {
		t.Fatal(err)
	}
	modules.files = files
	server.EnableReferenceStore(&referencestore.Resolver{
		Types:       referencestore.Types,
		Credentials: &referencestore.Credentials{HTTPClient: http.DefaultClient},
		Access:      &referencestore.AccessChecker{Directory: testReferenceAccess{}, TTL: time.Minute},
		HTTPClient:  http.DefaultClient,
		InProcess:   referencestore.InProcessServices{Handler: server.InProcessReferenceHandler(), PathPrefixes: InProcessReferenceServices},
		RecordTTL:   time.Minute, MissTTL: time.Second, LookupTimeout: 5 * time.Second, Concurrency: 4,
	})
	return modules
}

func TestTheMovedStoresAnswer503WithNoModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for path, code := range map[string]string{"/repo-store/repos": "repo_store_off", "/work-graph-store/activity": "work_graph_store_off",
		"/file-store/limits": "file_store_off", "/reference-store/types": "reference_store_off"} {
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
	if created := gated.requestAsServiceWithJSON(t, "POST", "/repo-store/repos", map[string]any{"name": "webapp"}); created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body)
	}
	person := gated.loginAs(t, firstTestPrincipalID)
	administrator := gated.loginAs(t, administratorTestPrincipalID)
	for _, path := range []string{"/repo-store/repos/by-name/webapp", "/work-graph-store/session-repos?since_unix_nano=1", "/file-store/limits"} {
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
	if response := gated.requestAsService(t, "POST", "/repo-store/repos/1/pull-requests/sync"); response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "no gh in tests") {
		t.Errorf("a sync with no gh = %d %s, want 503", response.Code, response.Body)
	}
}

// A file shared into a session goes to the file-store module in this process,
// and comes back through file-store's routes with its headers.
func TestSessionFilesGoToTheFileStoreModule(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	stored, err := gated.server.fileStore.Upload(strings.NewReader("the bytes"), 9, "text/plain", "note.txt", "br_files")
	if err != nil || stored.ID != "file_000001" || stored.SizeBytes != 9 {
		t.Fatalf("upload: %v %+v", err, stored)
	}
	response, err := gated.server.fileStore.Content(stored.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != "the bytes" || response.Header.Get("Content-Type") != "text/plain" ||
		response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("content = %d %q %v", response.StatusCode, body, response.Header)
	}
	listed := gated.requestAsService(t, "GET", "/file-store/files?owner_service=llm-bridge-server&owner_ref=br_files")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"id":"file_000001"`) {
		t.Fatalf("listed = %d %s", listed.Code, listed.Body)
	}
	if limits, err := gated.server.fileStore.Limits(); err != nil || limits.MaximumFileBytes != 25*1024*1024 {
		t.Fatalf("limits = %+v %v", limits, err)
	}
}

func resolveThroughTheServer(t *testing.T, gated *gatedTestServer, principalID string, ids ...string) (int, map[string][]referencestore.Match) {
	t.Helper()
	body, _ := json.Marshal(map[string][]string{"ids": ids})
	request := httptest.NewRequest(http.MethodPost, "/reference-store/resolve", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(serviceTokenHeader, testServiceToken)
	if principalID != "" {
		request.Header.Set(principalIdentityHeader, principalID)
	}
	response := serve(gated.server, request)
	var answer struct {
		Results map[string][]referencestore.Match `json:"results"`
		Errors  []referencestore.LookupError      `json:"errors"`
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
	repos := enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	if _, err := repos.UpsertRepo(&repostore.Repo{Name: "webapp", GitRemote: "git@github.com:example-owner/webapp.git"}); err != nil {
		t.Fatal(err)
	}
	if response := gated.requestAs(t, nil, "GET", "/reference-store/types/repo", nil); response.Code != http.StatusOK {
		t.Errorf("the catalog with no credential = %d %s, want 200", response.Code, response.Body)
	}
	if response := gated.requestAs(t, nil, "POST", "/reference-store/resolve", map[string]any{"ids": []string{"repo:webapp"}}); response.Code != http.StatusUnauthorized {
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

// Every route a reference type asks a service in this process for is a route
// this server has, under the prefix InProcessReferenceServices names. A
// search route is asked only for a type the @ picker offers (Mentionable).
func TestEveryInProcessReferenceRouteIsMounted(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	for _, row := range referencestore.Types {
		prefix, inProcess := InProcessReferenceServices[row.Service]
		if !inProcess {
			continue
		}
		routes := []string{row.Get, row.Attachment}
		if row.Mentionable() {
			routes = append(routes, row.Search)
		}
		for _, route := range routes {
			if route == "" {
				continue
			}
			path := prefix + strings.NewReplacer("{id}", "x").Replace(route)
			if _, pattern := gated.server.mux.Handler(httptest.NewRequest(http.MethodGet, path, nil)); pattern == "" {
				t.Errorf("type %s asks %s for GET %s, which this server does not mount", row.Type, row.Service, path)
			}
		}
	}
}

// grant-store checks a reference or a reference type with reference-store in
// this process.
func TestAGrantOnAReferenceIsCheckedInProcess(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	repos := enableRepositoriesAndReferencesOnTestDatabase(t, gated.server)
	if _, err := repos.UpsertRepo(&repostore.Repo{Name: "webapp"}); err != nil {
		t.Fatal(err)
	}
	checker := inProcessResourceChecker{server: gated.server}
	ctx := context.Background()
	for _, check := range []struct {
		resourceType, resourceID string
		exists                   bool
	}{
		{grantstore.ResourceTypeReference, "repo:webapp", true},
		{grantstore.ResourceTypeReference, "repo:nothing", false},
		{grantstore.ResourceTypeReferenceType, "repo", true},
		{grantstore.ResourceTypeReferenceType, "nothing", false},
	} {
		err := checker.CheckResourceExists(ctx, check.resourceType, check.resourceID)
		if check.exists && err != nil {
			t.Errorf("%s %s: %v", check.resourceType, check.resourceID, err)
		}
		if !check.exists && !errors.Is(err, grantstore.ErrResourceNotFound) {
			t.Errorf("%s %s: %v, want not found", check.resourceType, check.resourceID, err)
		}
	}
}
