package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// The probe. Every character in it is legal in a URL, so net/http actually
// sends the request and a red pin means "the value was mangled", never
// "the request was never made". A space would make net/http refuse the URL
// outright and redden every pin for a reason that says nothing about
// escaping — see TestASpaceInAnIDIsNotWhatThesePinsAreAbout below.
//
//   - `&` is the injection: it adds a parameter to the upstream request.
//   - `=` and `/` are values a correct escape may leave alone.
//   - `+` is the silent one: an unescaped `+` decodes server-side to a space,
//     so the upstream reads a DIFFERENT id and reports "no such session"
//     rather than anything that names the cause.
const queryProbeID = "br_probe+id/with=all&injected=1"

// captureLogStore stands in for log-store and records the raw RequestURI of
// every call llm-bridge-server makes on a caller's behalf. Asserting on
// RequestURI (not on r.URL.Query()) is the point: Query() would decode the
// injection back into an innocent-looking value.
type captureLogStore struct {
	*httptest.Server
	requests []string
	body     string
}

func newCaptureLogStore(t *testing.T, body string) *captureLogStore {
	t.Helper()
	c := &captureLogStore{body: body}
	c.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests = append(c.requests, r.RequestURI)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(c.body))
	}))
	t.Cleanup(c.Close)
	return c
}

// upstreamQuery parses the recorded call and returns its query parameters.
func (c *captureLogStore) upstreamQuery(t *testing.T) url.Values {
	t.Helper()
	if len(c.requests) != 1 {
		t.Fatalf("want exactly 1 upstream call, got %d: %q", len(c.requests), c.requests)
	}
	u, err := url.ParseRequestURI(c.requests[0])
	if err != nil {
		t.Fatalf("upstream request URI is not parseable: %q: %v", c.requests[0], err)
	}
	return u.Query()
}

// assertIDsSurvivedIntact is the shared assertion for both sites: the upstream
// saw the id set the server meant to send, and nothing else.
func assertIDsSurvivedIntact(t *testing.T, q url.Values, wantIDs string) {
	t.Helper()
	if got := q.Get("injected"); got != "" {
		t.Errorf("caller injected a parameter into the upstream request: injected=%q", got)
	}
	if got := q.Get("ids"); got != wantIDs {
		t.Errorf("ids reached log-store mangled:\n  sent: %q\n  want: %q", got, wantIDs)
	}
}

// serverWithLogStore builds a test server pointed at the capture stub.
func serverWithLogStore(t *testing.T, logStoreURL string) (*Server, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := &config.Config{
		ImagesDir:       filepath.Join(dir, "images"),
		BridgePrefsPath: filepath.Join(dir, "prefs.json"),
		LogStoreURL:     logStoreURL,
	}
	return New(st, nil, nil, nil, nil, nil, nil, cfg), st
}

// ─────────────────────────────────────────────────────────────────────────────
// Site 1 — GET /sessions/validators?ids=… forwards an HTTP CALLER's value.
// Reflected: the injected parameter rides that one caller's own request.
// ─────────────────────────────────────────────────────────────────────────────

func TestValidatorsDoesNotForwardACallersInjectedParameter(t *testing.T) {
	logStore := newCaptureLogStore(t, `{}`)
	srv, _ := serverWithLogStore(t, logStore.URL)

	// The caller escapes the probe on the way IN, so Go hands the handler the
	// single value `br_probe+id/with=all&injected=1`. What the handler does
	// with it next is the whole subject of this pin.
	in := url.Values{"ids": {queryProbeID}}
	req := httptest.NewRequest("GET", "/sessions/validators?"+in.Encode(), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	assertIDsSurvivedIntact(t, logStore.upstreamQuery(t), queryProbeID)
}

// A space is the loud failure, kept as its own pin so it can never stand in
// for the silent one. net/http refuses to send a URL containing a raw space,
// so an unescaped site fails here with a transport error — which proves
// nothing about escaping. The pin asserts the request still goes out.
func TestASpaceInAnIDIsNotWhatThesePinsAreAbout(t *testing.T) {
	logStore := newCaptureLogStore(t, `{}`)
	srv, _ := serverWithLogStore(t, logStore.URL)

	in := url.Values{"ids": {"br a b"}}
	req := httptest.NewRequest("GET", "/sessions/validators?"+in.Encode(), nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if len(logStore.requests) != 1 {
		t.Fatalf("a space in the id stopped the upstream request from being sent at all "+
			"(%d calls) — the value never reached the wire, so nothing here is about escaping",
			len(logStore.requests))
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Site 2 — GET /sessions/recent-bundle. The ids do NOT come off this request:
// they are read from the store, where a caller minted them at create time
// (handleCreateSession honours req.SessionID unvalidated). So the injection is
// STORED, and it rides every later recent-bundle call, not just its author's.
// ─────────────────────────────────────────────────────────────────────────────

func TestRecentBundleDoesNotForwardAStoredInjectedParameter(t *testing.T) {
	logStore := newCaptureLogStore(t, `{}`)
	srv, st := serverWithLogStore(t, logStore.URL)

	if err := st.CreateSession(&store.Session{
		SessionID:  queryProbeID,
		Harness:    "claude-code",
		InstanceID: "inst_test",
		State:      "idle",
	}); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	req := httptest.NewRequest("GET", "/sessions/recent-bundle?n=5&turns=3", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	q := logStore.upstreamQuery(t)
	assertIDsSurvivedIntact(t, q, queryProbeID)
	// turns is an int and was never at risk; it is asserted so an escape that
	// swallowed the rest of the query would be caught rather than looking clean.
	if got := q.Get("turns"); got != "3" {
		t.Errorf("turns reached log-store as %q, want %q", got, "3")
	}
}

// The stored injection is worse than a reflected one, and this pin says so in
// a way that fails if that stops being true: a SECOND, innocent session is
// enough to carry the first one's payload upstream.
func TestAStoredInjectionRidesAnInnocentSessionsBundle(t *testing.T) {
	logStore := newCaptureLogStore(t, `{}`)
	srv, st := serverWithLogStore(t, logStore.URL)

	for _, id := range []string{"br_innocent", queryProbeID} {
		if err := st.CreateSession(&store.Session{
			SessionID:  id,
			Harness:    "claude-code",
			InstanceID: "inst_test",
			State:      "idle",
		}); err != nil {
			t.Fatalf("seed session %q: %v", id, err)
		}
	}

	req := httptest.NewRequest("GET", "/sessions/recent-bundle?n=5&turns=3", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	q := logStore.upstreamQuery(t)
	if got := q.Get("injected"); got != "" {
		t.Errorf("one poisoned session id injected %q into a bundle request covering another session", got)
	}
	ids := q.Get("ids")
	if ids == "" {
		t.Fatalf("no ids reached log-store at all")
	}
	// Both ids must arrive, comma-joined, each intact. log-store reads this
	// with r.URL.Query().Get("ids") and splits on "," — so a comma escaped as
	// %2C round-trips correctly and a raw one does too.
	var wantSeen int
	for _, want := range []string{"br_innocent", queryProbeID} {
		for _, got := range splitCommaForTest(ids) {
			if got == want {
				wantSeen++
			}
		}
	}
	if wantSeen != 2 {
		t.Errorf("ids arrived as %q; both %q and %q should be present and intact", ids, "br_innocent", queryProbeID)
	}
}

func splitCommaForTest(s string) []string {
	out := []string{}
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}

// A guard on the stub itself: if the capture server ever stops recording, every
// pin above passes vacuously. This asserts the recorder records.
func TestCaptureLogStoreActuallyRecords(t *testing.T) {
	logStore := newCaptureLogStore(t, `{}`)
	srv, _ := serverWithLogStore(t, logStore.URL)

	in := url.Values{"ids": {"br_plain"}}
	req := httptest.NewRequest("GET", "/sessions/validators?"+in.Encode(), nil)
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if len(logStore.requests) != 1 {
		t.Fatalf("stub recorded %d requests for one call", len(logStore.requests))
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(`{}`), &probe); err != nil {
		t.Fatal(err)
	}
}
