package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The Gmail write ops — MarkRead, Flag, Move, Delete, PermDelete and the
// modify/do helpers underneath them — were executed by no test at all. A
// panic() at the entry of any of the seven left `go test ./...` green on main.
//
// gmailBase is a const, so the seam these tests use is the unexported http
// client on Gmail: an in-package test can swap its transport and read the
// request the production code actually builds. No production code changes.

// recordedRequest is one request as the transport saw it, with the body already
// drained — the body is the thing most of these assertions are about, and it is
// not readable after the round trip returns.
type recordedRequest struct {
	Method        string
	Path          string
	RawPath       string
	Body          string
	Authorization string
	ContentType   string
}

// scriptedTransport answers a fixed list of responses in order and records what
// it was asked. Running past the end of the script is a test bug, so it fails
// loudly rather than answering something plausible.
type scriptedTransport struct {
	t         *testing.T
	responses []*http.Response
	seen      []recordedRequest
}

func (s *scriptedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	body := ""
	if r.Body != nil {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			s.t.Fatalf("reading request body: %v", err)
		}
		body = string(b)
	}
	s.seen = append(s.seen, recordedRequest{
		Method:        r.Method,
		Path:          r.URL.Path,
		RawPath:       r.URL.EscapedPath(),
		Body:          body,
		Authorization: r.Header.Get("Authorization"),
		ContentType:   r.Header.Get("Content-Type"),
	})
	if len(s.seen) > len(s.responses) {
		s.t.Fatalf("transport asked for request %d but the script only has %d responses", len(s.seen), len(s.responses))
	}
	return s.responses[len(s.seen)-1], nil
}

func respond(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
	}
}

// closeTrackingBody fails a read made after Close, so a test can tell a
// response handed back intact from one whose body was closed on the way out.
// io.NopCloser cannot: its Close is a no-op and the reader stays readable, so
// against it "the body must still be readable" is unfalsifiable — a control
// arm that closes the returned 401 survives every assertion.
type closeTrackingBody struct {
	r      *strings.Reader
	closed bool
}

func (c *closeTrackingBody) Read(p []byte) (int, error) {
	if c.closed {
		return 0, errors.New("read after close")
	}
	return c.r.Read(p)
}

func (c *closeTrackingBody) Close() error {
	c.closed = true
	return nil
}

// respondClosable is respond() with a body whose Close is observable.
func respondClosable(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       &closeTrackingBody{r: strings.NewReader(body)},
		Header:     http.Header{},
	}
}

// fakeTokens records how many times a token was asked for and how many times
// the cache was invalidated. Both counts matter: the 401 path is defined by
// invalidate-then-re-fetch, and a retry that skipped either would still answer
// 200 to the caller.
type fakeTokens struct {
	token       string
	err         error
	tokenCalls  int
	invalidated []string
}

func (f *fakeTokens) Token(provider, account string) (string, error) {
	f.tokenCalls++
	return f.token, f.err
}

func (f *fakeTokens) Invalidate(provider, account string) {
	f.invalidated = append(f.invalidated, provider+"/"+account)
}

// newTestGmail wires a Gmail against a scripted transport. It returns both so a
// test can assert on the requests that were built.
func newTestGmail(t *testing.T, tokens *fakeTokens, responses ...*http.Response) (*Gmail, *scriptedTransport) {
	t.Helper()
	tr := &scriptedTransport{t: t, responses: responses}
	g := NewGmail("acct-1", "google", "default", tokens)
	g.http = &http.Client{Transport: tr}
	return g, tr
}

func okTokens() *fakeTokens { return &fakeTokens{token: "tok-abc"} }

// decodeModify reads a recorded body as the modify request Gmail receives.
func decodeModify(t *testing.T, body string) gmailModifyRequest {
	t.Helper()
	var req gmailModifyRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("request body is not a gmail modify request: %v (body %q)", err, body)
	}
	return req
}

// ─── Label arithmetic ──────────────────────────────────────────────────
//
// MarkRead and Flag have OPPOSITE polarity: read=true REMOVES the UNREAD
// label, flagged=true ADDS the STARRED one. Both are one-line functions whose
// entire content is that choice, and swapping either is invisible to any test
// that only asserts a request was made.

func TestMarkReadTrueRemovesTheUnreadLabel(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	req := decodeModify(t, tr.seen[0].Body)
	if got := req.RemoveLabelIDs; len(got) != 1 || got[0] != "UNREAD" {
		t.Errorf("marking a message READ must REMOVE the UNREAD label; removeLabelIds = %v", got)
	}
	if len(req.AddLabelIDs) != 0 {
		t.Errorf("marking a message read must add no label; addLabelIds = %v", req.AddLabelIDs)
	}
}

func TestMarkReadFalseAddsTheUnreadLabel(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", false); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	req := decodeModify(t, tr.seen[0].Body)
	if got := req.AddLabelIDs; len(got) != 1 || got[0] != "UNREAD" {
		t.Errorf("marking a message UNREAD must ADD the UNREAD label; addLabelIds = %v", got)
	}
	if len(req.RemoveLabelIDs) != 0 {
		t.Errorf("marking a message unread must remove no label; removeLabelIds = %v", req.RemoveLabelIDs)
	}
}

func TestFlagTrueAddsTheStarredLabel(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.Flag(context.Background(), "m1", true); err != nil {
		t.Fatalf("Flag: %v", err)
	}
	req := decodeModify(t, tr.seen[0].Body)
	if got := req.AddLabelIDs; len(got) != 1 || got[0] != "STARRED" {
		t.Errorf("flagging a message must ADD the STARRED label; addLabelIds = %v", got)
	}
	if len(req.RemoveLabelIDs) != 0 {
		t.Errorf("flagging a message must remove no label; removeLabelIds = %v", req.RemoveLabelIDs)
	}
}

func TestFlagFalseRemovesTheStarredLabel(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.Flag(context.Background(), "m1", false); err != nil {
		t.Fatalf("Flag: %v", err)
	}
	req := decodeModify(t, tr.seen[0].Body)
	if got := req.RemoveLabelIDs; len(got) != 1 || got[0] != "STARRED" {
		t.Errorf("unflagging a message must REMOVE the STARRED label; removeLabelIds = %v", got)
	}
	if len(req.AddLabelIDs) != 0 {
		t.Errorf("unflagging a message must add no label; addLabelIds = %v", req.AddLabelIDs)
	}
}

// The two functions disagree about which direction "on" means, and that is the
// invariant most at risk from a copy-paste between them. Pin it as a property
// rather than as four independent values, so a change that makes them agree
// fails here even if someone updates the four cases above to match.
func TestReadAndFlagHaveOppositePolarity(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(),
		respond(http.StatusOK, "{}"), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if err := g.Flag(context.Background(), "m1", true); err != nil {
		t.Fatalf("Flag: %v", err)
	}
	read := decodeModify(t, tr.seen[0].Body)
	flag := decodeModify(t, tr.seen[1].Body)
	if len(read.RemoveLabelIDs) == 0 {
		t.Errorf("the TRUE side of MarkRead must be a removal; got add=%v remove=%v", read.AddLabelIDs, read.RemoveLabelIDs)
	}
	if len(flag.AddLabelIDs) == 0 {
		t.Errorf("the TRUE side of Flag must be an addition; got add=%v remove=%v", flag.AddLabelIDs, flag.RemoveLabelIDs)
	}
}

// omitempty is load-bearing on the wire, not cosmetic: Gmail is being told
// which labels to change, and a key that is present-and-null is a different
// statement from a key that is absent.
// Both keys carry omitempty and each is the unused one on a different call, so
// one direction cannot stand in for the other: asserting only the add side
// leaves the remove side's tag free to change.
func TestTheUnusedLabelKeyIsAbsentFromTheWireNotNull(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(),
		respond(http.StatusOK, "{}"), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if err := g.MarkRead(context.Background(), "m1", false); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if strings.Contains(tr.seen[0].Body, "addLabelIds") {
		t.Errorf("a removal must not carry an addLabelIds key at all; body = %s", tr.seen[0].Body)
	}
	if strings.Contains(tr.seen[1].Body, "removeLabelIds") {
		t.Errorf("an addition must not carry a removeLabelIds key at all; body = %s", tr.seen[1].Body)
	}
}

// ─── Move ──────────────────────────────────────────────────────────────

func TestMoveToANonInboxFolderAlsoRemovesInbox(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.Move(context.Background(), "m1", "Label_7"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	req := decodeModify(t, tr.seen[0].Body)
	if got := req.AddLabelIDs; len(got) != 1 || got[0] != "Label_7" {
		t.Errorf("Move must add the target label; addLabelIds = %v", got)
	}
	if got := req.RemoveLabelIDs; len(got) != 1 || got[0] != "INBOX" {
		t.Errorf("moving out of the inbox must remove INBOX or the message stays visible there; removeLabelIds = %v", got)
	}
}

// INBOX is the one folder where the remove half must NOT happen — adding and
// removing the same label in one request is what "move to inbox" would become.
func TestMoveToInboxDoesNotRemoveInbox(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.Move(context.Background(), "m1", "INBOX"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	req := decodeModify(t, tr.seen[0].Body)
	if got := req.AddLabelIDs; len(got) != 1 || got[0] != "INBOX" {
		t.Errorf("moving to the inbox must add INBOX; addLabelIds = %v", got)
	}
	if len(req.RemoveLabelIDs) != 0 {
		t.Errorf("moving TO the inbox must not also remove it; removeLabelIds = %v", req.RemoveLabelIDs)
	}
}

// The guard has to fire before the request is built, not after: an empty folder
// reaching Gmail is a modify with no labels, which answers 200 and does nothing.
func TestMoveWithNoFolderFailsWithoutCallingGmail(t *testing.T) {
	g, tr := newTestGmail(t, okTokens())
	err := g.Move(context.Background(), "m1", "")
	if err == nil {
		t.Fatal("Move with an empty folder must fail")
	}
	if !strings.Contains(err.Error(), "folder is required") {
		t.Errorf("the error must say what is missing; got %q", err)
	}
	if len(tr.seen) != 0 {
		t.Errorf("no request may be sent when the folder is missing; sent %d", len(tr.seen))
	}
}

// ─── Delete and PermDelete are different verbs on different paths ──────

func TestDeleteTrashesAndPermDeleteRemoves(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(),
		respond(http.StatusOK, "{}"), respond(http.StatusNoContent, ""))
	if err := g.Delete(context.Background(), "m1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := g.PermDelete(context.Background(), "m1"); err != nil {
		t.Fatalf("PermDelete: %v", err)
	}
	del, perm := tr.seen[0], tr.seen[1]
	if del.Method != http.MethodPost || !strings.HasSuffix(del.Path, "/trash") {
		t.Errorf("Delete must POST to the trash path (recoverable); got %s %s", del.Method, del.Path)
	}
	if perm.Method != http.MethodDelete {
		t.Errorf("PermDelete must use DELETE (irrecoverable); got %s %s", perm.Method, perm.Path)
	}
	if strings.HasSuffix(perm.Path, "/trash") {
		t.Errorf("PermDelete must not go to the trash path; got %s", perm.Path)
	}
}

// PermDelete accepts two success codes. 200 is the one a 204-only reading would
// reject, so it is the half that a narrower guard breaks silently.
func TestPermDeleteAcceptsBoth200And204(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent} {
		g, _ := newTestGmail(t, okTokens(), respond(status, ""))
		if err := g.PermDelete(context.Background(), "m1"); err != nil {
			t.Errorf("PermDelete must accept %d as success; got %v", status, err)
		}
	}
}

func TestDeleteRejectsANonOKStatus(t *testing.T) {
	g, _ := newTestGmail(t, okTokens(), respond(http.StatusForbidden, "no"))
	err := g.Delete(context.Background(), "m1")
	if err == nil {
		t.Fatal("a 403 from the trash endpoint must be an error, not a silent success")
	}
}

func TestPermDeleteRejectsANonSuccessStatus(t *testing.T) {
	g, _ := newTestGmail(t, okTokens(), respond(http.StatusForbidden, "no"))
	err := g.PermDelete(context.Background(), "m1")
	if err == nil {
		t.Fatal("a 403 from the delete endpoint must be an error, not a silent success")
	}
}

// ─── The request modify builds ─────────────────────────────────────────

func TestModifyPostsToTheMessagesModifyPath(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	got := tr.seen[0]
	if got.Method != http.MethodPost {
		t.Errorf("modify must POST; got %s", got.Method)
	}
	if want := "/gmail/v1/users/me/messages/m1/modify"; got.Path != want {
		t.Errorf("modify path = %q, want %q", got.Path, want)
	}
}

// A Gmail message id is opaque and the path is built by concatenation, so the
// escape is the only thing between an id and a different endpoint.
func TestAMessageIdIsEscapedIntoThePath(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "a/b?c", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	raw := tr.seen[0].RawPath
	if !strings.Contains(raw, "a%2Fb%3Fc") {
		t.Errorf("the id must be escaped into one path segment; escaped path = %q", raw)
	}
	// Count on the ESCAPED path: URL.Path is the decoded form, so it shows the
	// slash back and cannot tell an escaped id from an unescaped one. An id
	// carrying a separator must still occupy exactly one segment on the wire.
	if got, want := strings.Count(raw, "/"), strings.Count("/gmail/v1/users/me/messages/x/modify", "/"); got != want {
		t.Errorf("an unescaped id split the path across segments: %q has %d separators, want %d", raw, got, want)
	}
}

func TestModifyReportsTheIdAndTheStatusWhenGmailRefuses(t *testing.T) {
	g, _ := newTestGmail(t, okTokens(), respond(http.StatusNotFound, "no such message"))
	err := g.MarkRead(context.Background(), "m-missing", true)
	if err == nil {
		t.Fatal("a 404 must be an error")
	}
	for _, want := range []string{"m-missing", "404", "no such message"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must carry %q so a failed write is diagnosable; got %q", want, err)
		}
	}
}

// ─── do: auth headers and the token path ───────────────────────────────

func TestTheBearerTokenIsSentOnAWriteOp(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if got := tr.seen[0].Authorization; got != "Bearer tok-abc" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer tok-abc")
	}
}

// Content-Type is set from whether there is a body, so the two verbs that pass
// a nil body are the ones that prove the condition is read.
func TestContentTypeIsSetForBodiedRequestsAndOmittedOtherwise(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(),
		respond(http.StatusOK, "{}"), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if err := g.Delete(context.Background(), "m1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := tr.seen[0].ContentType; got != "application/json" {
		t.Errorf("a request with a body must declare JSON; Content-Type = %q", got)
	}
	if got := tr.seen[1].ContentType; got != "" {
		t.Errorf("a request with no body must not declare a content type; Content-Type = %q", got)
	}
}

// A token failure must stop the write. Reaching Gmail unauthenticated would
// spend a 401 and drive the retry path on a request that never had a token.
func TestATokenFailureStopsTheWriteBeforeAnyRequest(t *testing.T) {
	tokens := &fakeTokens{err: errors.New("no credential")}
	g, tr := newTestGmail(t, tokens)
	err := g.MarkRead(context.Background(), "m1", true)
	if err == nil {
		t.Fatal("a token failure must fail the write")
	}
	if !strings.Contains(err.Error(), "no credential") {
		t.Errorf("the token error must reach the caller unchanged; got %q", err)
	}
	if len(tr.seen) != 0 {
		t.Errorf("no request may be sent without a token; sent %d", len(tr.seen))
	}
}

// ─── do: the 401 retry ─────────────────────────────────────────────────

func TestA401InvalidatesTheCachedTokenAndRetriesOnce(t *testing.T) {
	tokens := &fakeTokens{token: "tok-abc"}
	g, tr := newTestGmail(t, tokens,
		respond(http.StatusUnauthorized, "expired"), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("a 401 followed by a 200 must succeed; got %v", err)
	}
	if len(tr.seen) != 2 {
		t.Fatalf("a 401 must be retried exactly once; %d requests sent", len(tr.seen))
	}
	if len(tokens.invalidated) != 1 || tokens.invalidated[0] != "google/default" {
		t.Errorf("the stale token must be invalidated for this provider/account; invalidated = %v", tokens.invalidated)
	}
	if tokens.tokenCalls != 2 {
		t.Errorf("the token must be re-fetched after invalidation; Token called %d times", tokens.tokenCalls)
	}
}

// The retry is once, not until success — an endlessly-401ing endpoint must
// surface rather than loop.
func TestASecond401IsReturnedRatherThanRetriedAgain(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(),
		respond(http.StatusUnauthorized, "expired"), respond(http.StatusUnauthorized, "still expired"))
	err := g.MarkRead(context.Background(), "m1", true)
	if err == nil {
		t.Fatal("two consecutive 401s must reach the caller as an error")
	}
	if len(tr.seen) != 2 {
		t.Errorf("the retry must happen once only; %d requests sent", len(tr.seen))
	}
}

func TestTheRetriedRequestCarriesTheFreshToken(t *testing.T) {
	tokens := &fakeTokens{token: "tok-stale"}
	tr := &scriptedTransport{t: t, responses: []*http.Response{
		respond(http.StatusUnauthorized, "expired"), respond(http.StatusOK, "{}"),
	}}
	g := NewGmail("acct-1", "google", "default", tokens)
	g.http = &http.Client{Transport: tr}
	// Rotate the credential the way a real refresh would, on invalidation.
	tokens.token = "tok-stale"
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if got := tr.seen[1].Authorization; got == "" {
		t.Error("the retried request must carry an Authorization header")
	}
}

// A 401 is the ordinary consequence of a rotated or expired access token, so
// the retry that handles it runs on real writes. It has to carry the body the
// first attempt carried.
//
// This replaces a characterisation that pinned the opposite: do() used to build
// the retry from the io.Reader the first Do had already drained, so the second
// request went out with an empty body. For a modify that is a well-formed
// request with no labels to add and none to remove — Gmail answers 200, changes
// nothing, and mailstack reports {"ok":true}. Card ce787527.
func TestThe401RetryCarriesTheSameBodyAsTheFirstAttempt(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(),
		respond(http.StatusUnauthorized, "expired"), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if len(tr.seen) != 2 {
		t.Fatalf("expected a retry; %d requests sent", len(tr.seen))
	}
	first, second := tr.seen[0].Body, tr.seen[1].Body
	if !strings.Contains(first, "UNREAD") {
		t.Fatalf("the first attempt should carry the label change; body = %q", first)
	}
	if second != first {
		t.Fatalf("the retry must resend the first attempt's body\n first  = %q\n second = %q", first, second)
	}
}

// The same requirement stated as its consequence, which is the part that
// matters: a write that reports success has to have asked for the label change.
// The defect this pins was invisible precisely because it left every layer
// reporting success.
func TestARetriedWriteThatReportsSuccessAskedForTheLabelChange(t *testing.T) {
	g, tr := newTestGmail(t, okTokens(),
		respond(http.StatusUnauthorized, "expired"), respond(http.StatusOK, "{}"))
	if err := g.MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	if len(tr.seen) != 2 {
		t.Fatalf("expected a retry; %d requests sent", len(tr.seen))
	}
	got := decodeModify(t, tr.seen[1].Body)
	if len(got.RemoveLabelIDs) != 1 || got.RemoveLabelIDs[0] != "UNREAD" {
		t.Fatalf("a nil error must mean the retry asked Gmail to drop UNREAD; the request Gmail received was %+v", got)
	}
}

// unreplayableReader hides a reader behind a type net/http does not recognise,
// so http.NewRequest records no GetBody hook for it. This is the shape do()
// cannot resend: a streaming body, a pipe, anything not held in memory.
type unreplayableReader struct{ io.Reader }

// When the body cannot be replayed there is no retry to make that is not a
// silent no-op, so do() must return the 401 rather than send an empty request.
// Retrying anyway is the original defect; refusing to retry a replayable body
// would give up the retry on exactly the calls that need it, which the two
// tests above pin.
func TestABodyThatCannotBeReplayedIsNotRetried(t *testing.T) {
	tokens := okTokens()
	g, tr := newTestGmail(t, tokens,
		respondClosable(http.StatusUnauthorized, "expired"), respond(http.StatusOK, "{}"))
	body := unreplayableReader{strings.NewReader(`{"addLabelIds":["STARRED"]}`)}
	resp, err := g.do(context.Background(), "POST", "/users/me/messages/m1/modify", body)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if len(tr.seen) != 1 {
		t.Fatalf("an unreplayable body must not be retried; %d requests sent, the second body was %q",
			len(tr.seen), tr.seen[len(tr.seen)-1].Body)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the 401 must reach the caller; got %d", resp.StatusCode)
	}
	// The response handed back must still be readable — do() returns this one
	// instead of the retry's, so it must not have closed it on the way out.
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the returned 401 body must still be readable: %v", err)
	}
	if string(b) != "expired" {
		t.Fatalf("the returned response must be the 401 the server sent; body = %q", string(b))
	}
	// The credential really was rejected, so the cache is stale either way.
	if len(tokens.invalidated) != 1 {
		t.Errorf("the rejected token must be dropped from the cache even when the call is not retried; invalidated %v", tokens.invalidated)
	}
}

// Graph.do is Gmail.do with a different base URL, and it carried the same
// empty-retry defect. It is latent there only because every Graph write op is
// errNotImplemented, so nothing passes a body — these two pin the repair
// against the day one of them is written.
func TestGraphThe401RetryCarriesTheSameBodyAsTheFirstAttempt(t *testing.T) {
	tr := &scriptedTransport{t: t, responses: []*http.Response{
		respond(http.StatusUnauthorized, "expired"), respond(http.StatusOK, "{}"),
	}}
	g := NewGraph("acct-1", "microsoft", "default", okTokens())
	g.http = &http.Client{Transport: tr}

	resp, err := g.do(context.Background(), "PATCH", "/me/messages/m1",
		bytes.NewReader([]byte(`{"isRead":true}`)))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if len(tr.seen) != 2 {
		t.Fatalf("expected a retry; %d requests sent", len(tr.seen))
	}
	if tr.seen[1].Body != tr.seen[0].Body {
		t.Fatalf("the retry must resend the first attempt's body\n first  = %q\n second = %q",
			tr.seen[0].Body, tr.seen[1].Body)
	}
	if !strings.Contains(tr.seen[1].Body, "isRead") {
		t.Fatalf("the retry body lost the change; got %q", tr.seen[1].Body)
	}
}

func TestGraphABodyThatCannotBeReplayedIsNotRetried(t *testing.T) {
	tr := &scriptedTransport{t: t, responses: []*http.Response{
		respondClosable(http.StatusUnauthorized, "expired"), respond(http.StatusOK, "{}"),
	}}
	g := NewGraph("acct-1", "microsoft", "default", okTokens())
	g.http = &http.Client{Transport: tr}

	resp, err := g.do(context.Background(), "PATCH", "/me/messages/m1",
		unreplayableReader{strings.NewReader(`{"isRead":true}`)})
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if len(tr.seen) != 1 {
		t.Fatalf("an unreplayable body must not be retried; %d requests sent", len(tr.seen))
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the 401 must reach the caller; got %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the returned 401 body must still be readable: %v", err)
	}
	if string(b) != "expired" {
		t.Fatalf("the returned response must be the 401 the server sent; body = %q", string(b))
	}
}
