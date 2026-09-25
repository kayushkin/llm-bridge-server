package backend

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// wireRequest is everything about a call that the far API would see.
// The point of these tests is that the far API is Gmail or Microsoft Graph,
// which cannot be reached from here, so what gets judged is the wire traffic
// against those APIs' published contracts.
type wireRequest struct {
	Method        string
	Path          string
	RawRequestURI string
	Query         url.Values
	Body          string
	Authorization string
	ContentType   string
}

// cannedResponse is what the fake far API answers with.
type cannedResponse struct {
	Status int
	Body   string
}

// recordingAPI is an httptest.Server that records every request and answers
// from a per-path route table. A path with no route is a test failure rather
// than a polite 200: the hundred-and-thirteenth pass measured that a catch-all
// fixture cheerfully serves a client that has the URL wrong, which turns a
// protocol test into a test of nothing.
type recordingAPI struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	requests []wireRequest
	routes   map[string]cannedResponse
	// firstCallStatus, when set for a path, is served once and then removed.
	// This is how a token that expires between two calls is modelled.
	firstCallStatus map[string]cannedResponse
	unroutedPaths   []string
}

func newRecordingAPI(t *testing.T) *recordingAPI {
	t.Helper()
	api := &recordingAPI{
		t:               t,
		routes:          map[string]cannedResponse{},
		firstCallStatus: map[string]cannedResponse{},
	}
	api.server = httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(api.server.Close)
	return api
}

// route registers the answer for an exact path (no query string).
func (a *recordingAPI) route(path string, status int, body string) *recordingAPI {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.routes[path] = cannedResponse{Status: status, Body: body}
	return a
}

// routeOnce registers an answer served for the first call to path only;
// subsequent calls fall through to whatever route() registered.
func (a *recordingAPI) routeOnce(path string, status int, body string) *recordingAPI {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.firstCallStatus[path] = cannedResponse{Status: status, Body: body}
	return a
}

func (a *recordingAPI) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		a.t.Errorf("reading request body for %s: %v", r.URL.Path, err)
	}

	a.mu.Lock()
	a.requests = append(a.requests, wireRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		RawRequestURI: r.RequestURI,
		Query:         r.URL.Query(),
		Body:          string(body),
		Authorization: r.Header.Get("Authorization"),
		ContentType:   r.Header.Get("Content-Type"),
	})
	resp, ok := a.firstCallStatus[r.URL.Path]
	if ok {
		delete(a.firstCallStatus, r.URL.Path)
	} else {
		resp, ok = a.routes[r.URL.Path]
	}
	if !ok {
		a.unroutedPaths = append(a.unroutedPaths, r.URL.Path)
	}
	a.mu.Unlock()

	if !ok {
		// Answer the way a real API answers a path it does not route. A
		// fixture that answers everything cannot disagree with a client.
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":404,"message":"no such route in the fixture"}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write([]byte(resp.Body))
}

func (a *recordingAPI) recorded() []wireRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]wireRequest, len(a.requests))
	copy(out, a.requests)
	return out
}

// only returns the single recorded request, failing if there was not exactly one.
func (a *recordingAPI) only() wireRequest {
	a.t.Helper()
	got := a.recorded()
	if len(got) != 1 {
		a.t.Fatalf("want exactly 1 request, got %d: %+v", len(got), got)
	}
	return got[0]
}

// assertEveryPathWasRouted fails if the client asked for a path the fixture
// does not know, which is how a wrong URL surfaces.
func (a *recordingAPI) assertEveryPathWasRouted() {
	a.t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.unroutedPaths) > 0 {
		a.t.Fatalf("client called paths the far API does not route: %v", a.unroutedPaths)
	}
}

// fakeTokenSource stands in for the auth-store client. It hands out a
// different token after each Invalidate so a retry is distinguishable from
// the first attempt by the Authorization header alone.
type fakeTokenSource struct {
	mu           sync.Mutex
	generation   int
	invalidated  int
	err          error
	errAfterCall int // when >0, Token fails from this call number onward
	calls        int
}

func (f *fakeTokenSource) Token(provider, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil && (f.errAfterCall == 0 || f.calls >= f.errAfterCall) {
		return "", f.err
	}
	return tokenForGeneration(f.generation), nil
}

func (f *fakeTokenSource) Invalidate(provider, account string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.invalidated++
	f.generation++
}

func (f *fakeTokenSource) invalidations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.invalidated
}

func tokenForGeneration(n int) string {
	return "token-generation-" + string(rune('0'+n))
}

func newRecordedGmail(t *testing.T, api *recordingAPI, tokens TokenSource) *Gmail {
	t.Helper()
	g := NewGmail("account-1", "google", "default", tokens)
	g.base = api.server.URL
	g.http = api.server.Client()
	return g
}

func newRecordedGraph(t *testing.T, api *recordingAPI, tokens TokenSource) *Graph {
	t.Helper()
	g := NewGraph("account-1", "microsoft", "default", tokens)
	g.base = api.server.URL
	g.http = api.server.Client()
	return g
}
