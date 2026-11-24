package bundlestore

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These are the first tests in this repo to go through RegisterHandlers. Until
// they were written, every route in server.go was executed by nothing: a
// panic() at the first line of handler.setEnabled left `go test ./...` green.
//
// The subject here is the enable/disable pair, because it is the only place
// where two routes share one handler and differ only by a bool the caller
// supplies. That shape has a specific failure — the two routes passing the same
// value — and it is invisible to any test that exercises just one of them.

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	s := openTest(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, s)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := srv.Client().Post(srv.URL+path, "application/json", nil)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body)
}

// storedEnabled reads the flag back out of the database rather than off the
// response. This distinction is the whole point of the two tests below: the
// handler builds its reply from the bool it was called with and never consults
// the store, so a handler that skipped SetEnabled entirely would still answer
// {"enabled":false} with a 200. Asserting the response alone tests the literal
// on line 136 and nothing else.
func storedEnabled(t *testing.T, s *Store, id int64) bool {
	t.Helper()
	b, err := s.GetBundle(id)
	if err != nil {
		t.Fatalf("get bundle %d: %v", id, err)
	}
	return b.Enabled
}

func TestTheDisableRouteTurnsTheBundleOffInTheDatabase(t *testing.T) {
	srv, s := newTestServer(t)
	b := &Bundle{Name: "go-service", MatchTags: []string{"go"}, Enabled: true}
	mustUpsert(t, s, b)

	status, body := post(t, srv, "/bundles/"+itoa(b.ID)+"/disable")
	if status != 200 {
		t.Fatalf("status %d, body %s", status, body)
	}
	var got struct {
		ID      int64 `json:"id"`
		Enabled bool  `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	if got.ID != b.ID || got.Enabled {
		t.Fatalf("response says id=%d enabled=%v, want id=%d enabled=false", got.ID, got.Enabled, b.ID)
	}
	if storedEnabled(t, s, b.ID) {
		t.Fatal("the route answered 200 and enabled=false but the row is still enabled — " +
			"the reply is built from the handler's own argument, so it can say this while writing nothing")
	}
}

func TestTheEnableRouteTurnsTheBundleBackOnInTheDatabase(t *testing.T) {
	srv, s := newTestServer(t)
	b := &Bundle{Name: "react", MatchTags: []string{"react"}, Enabled: false}
	mustUpsert(t, s, b)

	status, body := post(t, srv, "/bundles/"+itoa(b.ID)+"/enable")
	if status != 200 {
		t.Fatalf("status %d, body %s", status, body)
	}
	if !strings.Contains(body, `"enabled":true`) {
		t.Fatalf("reply does not report the new state: %s", body)
	}
	if !storedEnabled(t, s, b.ID) {
		t.Fatal("the route answered 200 and enabled=true but the row is still disabled")
	}
}

// The pair invariant, and the reason both routes are worth a test rather than
// one standing in for the other. enableBundle and disableBundle are one-liners
// that differ only in the bool they forward. If both forwarded the same value —
// a copy-paste that changes the function name and not the argument — every
// assertion above still holds for whichever route matches that value, and the
// service quietly loses one of its two verbs. Only driving both against one row
// and requiring the results to differ catches it.
func TestTheEnableAndDisableRoutesForwardOppositeValues(t *testing.T) {
	srv, s := newTestServer(t)
	b := &Bundle{Name: "base", Enabled: true}
	mustUpsert(t, s, b)

	post(t, srv, "/bundles/"+itoa(b.ID)+"/disable")
	afterDisable := storedEnabled(t, s, b.ID)
	post(t, srv, "/bundles/"+itoa(b.ID)+"/enable")
	afterEnable := storedEnabled(t, s, b.ID)

	if afterDisable == afterEnable {
		t.Fatalf("both routes left the row enabled=%v — /enable and /disable are forwarding "+
			"the same value, so one of the two verbs does nothing", afterDisable)
	}
	if afterDisable || !afterEnable {
		t.Fatalf("routes are wired backwards: after /disable enabled=%v, after /enable enabled=%v",
			afterDisable, afterEnable)
	}
}

func TestTheEnabledRoutesRejectAnUnparseableID(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, path := range []string{"/bundles/not-a-number/enable", "/bundles/0/disable", "/bundles/-3/enable"} {
		status, body := post(t, srv, path)
		if status != 400 {
			t.Fatalf("POST %s: status %d, want 400 (body %s)", path, status, body)
		}
	}
}

// A missing bundle must be 404, not 200. SetEnabled distinguishes the two by
// RowsAffected, and that check is the only thing standing between "you disabled
// bundle 999" and the fact that there is no bundle 999. Without it the UPDATE
// succeeds against zero rows and the handler reports success.
func TestTheEnabledRoutesAnswer404ForABundleThatDoesNotExist(t *testing.T) {
	srv, _ := newTestServer(t)
	status, body := post(t, srv, "/bundles/999/disable")
	if status != 404 {
		t.Fatalf("status %d, want 404 (body %s)", status, body)
	}
	if !strings.Contains(body, "bundle not found") {
		t.Fatalf("body %q does not name the failure", body)
	}
}

// The id in the reply must be the id that was written, not one the handler
// invented or defaulted. A consumer that echoes this back into its own state
// has no other way to tell which row moved.
func TestTheEnabledRoutesReportTheIDTheyWrote(t *testing.T) {
	srv, s := newTestServer(t)
	first := &Bundle{Name: "first", Enabled: true}
	mustUpsert(t, s, first)
	second := &Bundle{Name: "second", Enabled: true}
	mustUpsert(t, s, second)
	if first.ID == second.ID {
		t.Fatal("fixture is broken: two bundles share an id")
	}

	_, body := post(t, srv, "/bundles/"+itoa(second.ID)+"/disable")
	var got struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != second.ID {
		t.Fatalf("reply names id %d, wrote id %d", got.ID, second.ID)
	}
	if !storedEnabled(t, s, first.ID) {
		t.Fatal("disabling the second bundle also disabled the first")
	}
}
