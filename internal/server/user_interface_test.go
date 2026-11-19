package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

const builtIndexForTest = `<!doctype html><script type="module" src="/ui/assets/index-abc123.js"></script>`

func serverWithBuiltUserInterface(t *testing.T) *Server {
	t.Helper()
	srv, _ := testServer(t)
	srv.userInterfaceBuild = fstest.MapFS{
		"index.html":             {Data: []byte(builtIndexForTest)},
		"assets/index-abc123.js": {Data: []byte(`console.log("front end")`)},
		"favicon.svg":            {Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
	}
	return srv
}

// getWithNoCredential asks as a browser that has not signed in: the front end
// must load before anyone can.
func getWithNoCredential(t *testing.T, srv *Server, path string) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	srv.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Result()
}

func bodyOf(t *testing.T, response *http.Response) string {
	t.Helper()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestUserInterfaceServesTheIndexForItsRootAndForEveryPage(t *testing.T) {
	srv := serverWithBuiltUserInterface(t)
	// /ui/sessions is a page of the app, while /sessions is a route of this
	// server: the prefix is what keeps the two apart.
	for _, path := range []string{"/ui/", "/ui/index.html", "/ui/sessions", "/ui/card/card_000012", "/ui/files/CLAUDE.md"} {
		response := getWithNoCredential(t, srv, path)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d, want 200", path, response.StatusCode)
		}
		if body := bodyOf(t, response); body != builtIndexForTest {
			t.Errorf("GET %s: body %q, want the build's index.html", path, body)
		}
		if got := response.Header.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Errorf("GET %s: Content-Type %q", path, got)
		}
		if got := response.Header.Get("Cache-Control"); got != "no-cache" {
			t.Errorf("GET %s: Cache-Control %q, want no-cache so a browser never keeps an old build's index", path, got)
		}
	}
}

func TestUserInterfaceServesAHashedAssetThatABrowserMayKeep(t *testing.T) {
	srv := serverWithBuiltUserInterface(t)
	response := getWithNoCredential(t, srv, "/ui/assets/index-abc123.js")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if body := bodyOf(t, response); body != `console.log("front end")` {
		t.Errorf("body %q", body)
	}
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Errorf("Content-Type %q, want a JavaScript type", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("Cache-Control %q", got)
	}
}

func TestUserInterfaceServesAFileOutsideAssetsWithoutLongCaching(t *testing.T) {
	srv := serverWithBuiltUserInterface(t)
	response := getWithNoCredential(t, srv, "/ui/favicon.svg")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200", response.StatusCode)
	}
	if got := response.Header.Get("Cache-Control"); got != "" {
		t.Errorf("Cache-Control %q, want none: only a name carrying a content hash is safe to keep", got)
	}
}

func TestUserInterfaceAnswers404ForAMissingAssetNotThePage(t *testing.T) {
	srv := serverWithBuiltUserInterface(t)
	response := getWithNoCredential(t, srv, "/ui/assets/index-from-an-older-build.js")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d, want 404: answering index.html here would hand a browser HTML where it asked for a script", response.StatusCode)
	}
}

func TestUserInterfaceSaysWhenTheServerWasCompiledWithoutIt(t *testing.T) {
	srv, _ := testServer(t)
	srv.userInterfaceBuild = fstest.MapFS{}
	response := getWithNoCredential(t, srv, "/ui/")
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", response.StatusCode)
	}
	var answer struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		t.Fatal(err)
	}
	if answer.Error.Code != "user_interface_not_built" {
		t.Errorf("code %q, want user_interface_not_built", answer.Error.Code)
	}
	if !strings.Contains(answer.Error.Message, "scripts/build-ui.sh") {
		t.Errorf("message %q does not name the script that builds it", answer.Error.Message)
	}
}

func TestBareAddressRedirectsToTheUserInterface(t *testing.T) {
	srv := serverWithBuiltUserInterface(t)
	response := getWithNoCredential(t, srv, "/")
	if response.StatusCode != http.StatusFound {
		t.Fatalf("status %d, want 302", response.StatusCode)
	}
	if got := response.Header.Get("Location"); got != "/ui/" {
		t.Errorf("Location %q, want /ui/", got)
	}
}

func TestUserInterfacePrefixDoesNotOpenTheServersOwnRoutes(t *testing.T) {
	srv := serverWithBuiltUserInterface(t)
	// The page /ui/sessions is public; the route /sessions still needs a credential.
	response := getWithNoCredential(t, srv, "/sessions")
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /sessions with no credential: status %d, want 401", response.StatusCode)
	}
}
