package backend

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Graph folder list is paged like every other Graph collection, and until
// this file existed nothing here read the second page: graphFolderList had no
// field for @odata.nextLink at all, so a mailbox with more folders than $top
// returned a short list that the UI drew as a complete folder tree.
//
// Every case below drives Folders itself rather than the page-fetching helper.
// A repair that tested only the helper would leave the call site — the thing
// that was actually broken — unpinned.

type stubTokenSource struct {
	token       string
	invalidated int
}

func (s *stubTokenSource) Token(provider, account string) (string, error) {
	return s.token, nil
}

func (s *stubTokenSource) Invalidate(provider, account string) {
	s.invalidated++
}

// scriptedGraph answers each request from a script, one entry per call, and
// records the exact request-target it was asked for. The recorded targets are
// the point: a continuation link is only followed correctly if the bytes that
// go out on request N+1 are the bytes the server handed back on request N.
type scriptedGraph struct {
	t         *testing.T
	responses []string
	seen      []string
	authSeen  []string
	server    *httptest.Server
	// statusFor overrides the status code for a given 1-based request number.
	statusFor map[int]int
}

func newScriptedGraph(t *testing.T, responses ...string) *scriptedGraph {
	t.Helper()
	s := &scriptedGraph{t: t, responses: responses}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.seen = append(s.seen, r.RequestURI)
		s.authSeen = append(s.authSeen, r.Header.Get("Authorization"))
		i := len(s.seen) - 1
		if i >= len(s.responses) {
			t.Errorf("unscripted request %d for %s", len(s.seen), r.RequestURI)
			w.WriteHeader(http.StatusInternalServerError)
			// A body that decodes cleanly, so a failing case fails on the
			// assertion under test and not on a decode error.
			fmt.Fprint(w, `{"value":[]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if code, ok := s.statusFor[len(s.seen)]; ok {
			w.WriteHeader(code)
		}
		fmt.Fprint(w, s.responses[i])
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *scriptedGraph) client() (*Graph, *stubTokenSource) {
	tokens := &stubTokenSource{token: "test-token"}
	g := NewGraph("acct-1", "microsoft", "default", tokens)
	g.base = s.server.URL
	return g, tokens
}

func folderPage(nextLink string, names ...string) string {
	var b strings.Builder
	b.WriteString(`{"value":[`)
	for i, n := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":%q,"displayName":%q,"unreadItemCount":%d}`, "id-"+n, n, i)
	}
	b.WriteString(`]`)
	if nextLink != "" {
		fmt.Fprintf(&b, `,"@odata.nextLink":%q`, nextLink)
	}
	b.WriteString(`}`)
	return b.String()
}

func TestFolderWalkReturnsEveryPageNotJustTheFirst(t *testing.T) {
	s := newScriptedGraph(t, "", "", "")
	g, _ := s.client()
	s.responses = []string{
		folderPage(s.server.URL+"/me/mailFolders?$skiptoken=page2", "Inbox", "Sent Items"),
		folderPage(s.server.URL+"/me/mailFolders?$skiptoken=page3", "Archive"),
		folderPage("", "Projects", "Receipts"),
	}

	folders, err := g.Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 5 {
		t.Fatalf("got %d folders, want all 5 across the three pages: %+v", len(folders), folders)
	}
	if len(s.seen) != 3 {
		t.Fatalf("made %d requests, want 3: %v", len(s.seen), s.seen)
	}
	if folders[4].Name != "Receipts" {
		t.Errorf("last folder is %q, want the last page's last folder", folders[4].Name)
	}
	if folders[0].AccountID != "acct-1" {
		t.Errorf("folder carries account %q, want acct-1", folders[0].AccountID)
	}
}

// The Graph-specific trap. A $skiptoken is only valid against the query that
// issued it, so a follow-up built by re-encoding the parts restarts the walk at
// page one and terminates only when a loop guard fires. The only thing that can
// observe the difference is the raw request-target of the second request.
func TestContinuationIsFollowedByteForByte(t *testing.T) {
	s := newScriptedGraph(t, "", "")
	g, _ := s.client()
	// Parameters deliberately out of alphabetical order, and a skiptoken
	// carrying the '+', '/' and '=' a re-encode would escape: url.Values.Encode
	// sorts keys and percent-escapes these, so any rebuild changes these bytes.
	continuation := s.server.URL + "/me/mailFolders?$top=100&$skiptoken=a+b/c=="
	s.responses = []string{
		folderPage(continuation, "Inbox"),
		folderPage("", "Archive"),
	}

	if _, err := g.Folders(context.Background()); err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(s.seen) != 2 {
		t.Fatalf("made %d requests, want 2: %v", len(s.seen), s.seen)
	}
	want := strings.TrimPrefix(continuation, s.server.URL)
	if s.seen[1] != want {
		t.Errorf("second request went to\n  %q\nwant the continuation verbatim\n  %q", s.seen[1], want)
	}
	// The follow-up is a fresh request, not a redirect the client library
	// carries headers through, so it has to be authenticated on its own.
	if s.authSeen[1] != "Bearer test-token" {
		t.Errorf("continuation request carried auth %q, want the account's bearer token", s.authSeen[1])
	}
}

// Termination must be driven by the presence of a continuation, not by a page
// looking short. Graph is free to return fewer items than $top and still have
// more to give, so "stop when the page is small" silently drops the tail.
func TestAShortPageCarryingAContinuationIsStillFollowed(t *testing.T) {
	s := newScriptedGraph(t, "", "")
	g, _ := s.client()
	s.responses = []string{
		folderPage(s.server.URL+"/me/mailFolders?$skiptoken=more", "Inbox"),
		folderPage("", "Archive"),
	}

	folders, err := g.Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2 — a one-item page still carried a continuation", len(folders))
	}
}

func TestNoContinuationMeansExactlyOneRequest(t *testing.T) {
	s := newScriptedGraph(t, folderPage("", "Inbox", "Sent Items"))
	g, _ := s.client()

	folders, err := g.Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(s.seen) != 1 {
		t.Fatalf("made %d requests, want exactly 1: %v", len(s.seen), s.seen)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2", len(folders))
	}
	if !strings.Contains(s.seen[0], "$top=100") {
		t.Errorf("first request %q lost the $top the folder query is built with", s.seen[0])
	}
}

// The follow-up request carries the account's bearer token. Following a link
// verbatim must not mean following it anywhere the response points, or a
// continuation naming another host would be handed the user's credentials.
func TestAContinuationOffTheGraphHostIsRefusedAndNeverReceivesTheToken(t *testing.T) {
	var foreignHits int
	var foreignAuth string
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignHits++
		foreignAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"value":[]}`)
	}))
	defer foreign.Close()

	s := newScriptedGraph(t, "")
	g, _ := s.client()
	s.responses = []string{folderPage(foreign.URL+"/me/mailFolders?$skiptoken=steal", "Inbox")}

	_, err := g.Folders(context.Background())
	if err == nil {
		t.Fatal("following a continuation to another host succeeded, want an error")
	}
	if !strings.Contains(err.Error(), "refusing to follow continuation") {
		t.Errorf("error %q does not name the refusal", err)
	}
	if foreignHits != 0 {
		t.Errorf("the foreign host got %d requests, want 0 (auth header seen: %q)", foreignHits, foreignAuth)
	}
}

// A continuation that hands back a page already read is a server or client bug
// that would otherwise spin until the page cap. It is reported rather than
// swallowed: quietly stopping here returns exactly the truncated folder list
// this walk was written to prevent.
func TestARepeatedContinuationIsReportedRatherThanLoopingForever(t *testing.T) {
	s := newScriptedGraph(t, "", "", "", "")
	g, _ := s.client()
	loop := s.server.URL + "/me/mailFolders?$skiptoken=same"
	s.responses = []string{
		folderPage(loop, "Inbox"),
		folderPage(loop, "Inbox"),
		folderPage(loop, "Inbox"),
		folderPage(loop, "Inbox"),
	}

	folders, err := g.Folders(context.Background())
	if err == nil {
		t.Fatalf("a self-referencing continuation returned %d folders and no error", len(folders))
	}
	if !strings.Contains(err.Error(), "not advancing") {
		t.Errorf("error %q does not say the walk stopped advancing", err)
	}
	if len(s.seen) != 2 {
		t.Errorf("made %d requests before noticing the repeat, want 2: %v", len(s.seen), s.seen)
	}
}

// The page cap is a second guard behind the already-fetched check, and it only
// fires for a server that keeps handing back distinct links forever — so it is
// the kind of error surface that can sit in the code unreachable. This drives
// enough distinct pages to reach it. It deliberately does not assert the cap's
// value: the number is a judgment call, and pinning it would turn any future
// retuning into a test failure that says nothing about behavior.
func TestTheWalkStopsRatherThanFollowingAnEndlessChainOfNewLinks(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		// Every page is distinct, so the already-fetched guard never fires and
		// only the cap can stop this.
		fmt.Fprint(w, folderPage(
			fmt.Sprintf("http://%s/me/mailFolders?$skiptoken=p%d", r.Host, calls), "F"))
	}))
	defer srv.Close()

	tokens := &stubTokenSource{token: "test-token"}
	g := NewGraph("acct-1", "microsoft", "default", tokens)
	g.base = srv.URL

	folders, err := g.Folders(context.Background())
	if err == nil {
		t.Fatalf("an endless chain of new links returned %d folders and no error", len(folders))
	}
	if !strings.Contains(err.Error(), "still paging") {
		t.Errorf("error %q does not say the walk was abandoned for running long", err)
	}
	if calls < 2 {
		t.Errorf("the walk made %d requests, so the cap was not what stopped it", calls)
	}
}

// The first page's continuation is followed even when the first page is where
// the well-known-name mapping happens, so canonical kinds must survive the walk
// rather than being applied only to page one.
func TestCanonicalKindsAreMappedOnEveryPage(t *testing.T) {
	s := newScriptedGraph(t, "", "")
	g, _ := s.client()
	s.responses = []string{
		`{"value":[{"id":"1","displayName":"Inbox","wellKnownName":"inbox"}],` +
			fmt.Sprintf(`"@odata.nextLink":%q}`, s.server.URL+"/me/mailFolders?$skiptoken=p2"),
		`{"value":[{"id":"2","displayName":"Deleted Items","wellKnownName":"deleteditems"}]}`,
	}

	folders, err := g.Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 2 {
		t.Fatalf("got %d folders, want 2", len(folders))
	}
	if folders[0].Canonical != KindInbox {
		t.Errorf("page 1 folder canonical = %q, want inbox", folders[0].Canonical)
	}
	if folders[1].Canonical != KindTrash {
		t.Errorf("page 2 folder canonical = %q, want trash — the mapping must outlive page one", folders[1].Canonical)
	}
}

// An upstream error on a later page must not pass for a complete short list.
func TestAFailureOnALaterPageIsReportedNotTruncated(t *testing.T) {
	s := newScriptedGraph(t, "", "")
	g, _ := s.client()
	s.responses = []string{
		folderPage(s.server.URL+"/me/mailFolders?$skiptoken=p2", "Inbox"),
		// Status 503 with a body that decodes cleanly, so the case turns on the
		// status code being read rather than on a decode failure rescuing the
		// assertion by accident.
		"",
	}
	s.statusFor = map[int]int{2: http.StatusServiceUnavailable}
	s.responses[1] = `{"value":[]}`

	folders, err := g.Folders(context.Background())
	if err == nil {
		t.Fatalf("a 503 on page two returned %d folders and no error", len(folders))
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q does not carry the upstream status", err)
	}
}
