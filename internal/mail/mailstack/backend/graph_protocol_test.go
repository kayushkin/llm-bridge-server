package backend

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// These cases judge Graph.do's wire traffic against the published Microsoft
// Graph v1.0 mail API rather than against mailstack's own beliefs about it:
//
//	https://learn.microsoft.com/en-us/graph/api/resources/mail-api-overview
//
// Graph is read-only in mailstack today; the write methods return
// errNotImplemented and are pinned as such so that stops being true loudly.

func TestGraphFoldersSelectsTheFieldsItMapsAndCapsThePage(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/me/mailFolders", http.StatusOK, `{"value":[
		{"id":"f1","displayName":"Inbox","parentFolderId":"root","unreadItemCount":4,"wellKnownName":"inbox"},
		{"id":"f2","displayName":"Team","parentFolderId":"root","unreadItemCount":0}
	]}`)

	folders, err := newRecordedGraph(t, api, &fakeTokenSource{}).Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	api.assertEveryPathWasRouted()

	if len(folders) != 2 {
		t.Fatalf("want 2 folders, got %d", len(folders))
	}
	if folders[0].Canonical != KindInbox {
		t.Errorf("wellKnownName inbox must map to the canonical inbox, got %q", folders[0].Canonical)
	}
	if folders[1].Canonical != "" {
		t.Errorf("a folder with no wellKnownName has no canonical kind, got %q", folders[1].Canonical)
	}
	if folders[0].UnreadCount != 4 {
		t.Errorf("unreadItemCount must become the unread count, got %d", folders[0].UnreadCount)
	}

	r := api.only()
	if r.Method != http.MethodGet {
		t.Errorf("mailFolders is a GET, got %s", r.Method)
	}
	// Graph's default page is 10 folders, so $top has to be set or a mailbox
	// with more than ten folders silently loses the rest.
	if got := r.Query.Get("$top"); got != "100" {
		t.Errorf("want $top=100, got %q", got)
	}
	// Every field the mapping below reads has to be in $select, or Graph
	// simply omits it and the folder arrives blank.
	for _, field := range []string{"id", "displayName", "parentFolderId", "unreadItemCount", "wellKnownName"} {
		if !strings.Contains(r.Query.Get("$select"), field) {
			t.Errorf("$select must ask for %q, got %q", field, r.Query.Get("$select"))
		}
	}
}

// GET /me/mailFolders returns the children of the root folder only. Nested
// folders need childFolders traversal, and hidden folders need
// includeHiddenFolders=true. mailstack does neither, so an Outlook mailbox
// with subfolders shows a folder list that is short in a way nothing reports.
//
//	https://learn.microsoft.com/en-us/graph/api/user-list-mailfolders
//
// This case pins today's behavior so the gap is documented rather than
// rediscovered; closing it is a design call about how deep to walk.
func TestGraphFoldersReturnsOnlyTheRootChildrenAndNoHiddenFolders(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/me/mailFolders", http.StatusOK, `{"value":[
		{"id":"f1","displayName":"Inbox","wellKnownName":"inbox"}
	]}`)
	api.route("/me/mailFolders/f1/childFolders", http.StatusOK, `{"value":[
		{"id":"f1a","displayName":"Nested","parentFolderId":"f1"}
	]}`)

	folders, err := newRecordedGraph(t, api, &fakeTokenSource{}).Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("want the 1 root child that is fetched today, got %d: %+v", len(folders), folders)
	}
	for _, r := range api.recorded() {
		if strings.Contains(r.Path, "childFolders") {
			t.Errorf("no child-folder traversal is implemented, but one was requested: %s", r.Path)
		}
		if r.Query.Get("includeHiddenFolders") != "" {
			t.Errorf("hidden folders are not requested today, but the parameter was sent")
		}
	}
}

func TestGraphListBuildsTheODataQueryAndRoundTripsTheNextLink(t *testing.T) {
	api := newRecordingAPI(t)
	// Graph answers with an absolute nextLink under its own base, so the
	// fixture has to do the same or the round-trip is not being tested.
	api.route("/me/mailFolders/f1/messages", http.StatusOK, `{"value":[
		{"id":"m1","subject":"A subject","bodyPreview":"preview",
		 "receivedDateTime":"2026-08-10T12:00:00Z","isRead":false,
		 "from":{"emailAddress":{"name":"Ada","address":"ada@example.com"}},
		 "toRecipients":[{"emailAddress":{"name":"Bob","address":"bob@example.com"}}],
		 "parentFolderId":"f1","flag":{"flagStatus":"flagged"},"hasAttachments":true}
	],"@odata.nextLink":"`+api.server.URL+`/me/mailFolders/f1/messages?$skiptoken=abc"}`)

	graph := newRecordedGraph(t, api, &fakeTokenSource{})
	metas, next, err := graph.List(context.Background(), "f1", "", 20)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	api.assertEveryPathWasRouted()

	if len(metas) != 1 {
		t.Fatalf("want 1 message, got %d", len(metas))
	}
	got := metas[0]
	if !got.Unread {
		t.Error("isRead=false must become unread=true")
	}
	if !got.Flagged {
		t.Error("flagStatus=flagged must become flagged=true")
	}
	if !got.HasAttach {
		t.Error("hasAttachments must become has_attachments")
	}
	if got.From.Email != "ada@example.com" || got.From.Name != "Ada" {
		t.Errorf("from.emailAddress must be flattened, got %+v", got.From)
	}
	if got.Snippet != "preview" {
		t.Errorf("bodyPreview must become the snippet, got %q", got.Snippet)
	}
	if got.Date.IsZero() {
		t.Error("receivedDateTime must be parsed into the date")
	}

	// The cursor handed back must be relative, because it is fed straight
	// back into do(), which prefixes the base URL again.
	if next != "/me/mailFolders/f1/messages?$skiptoken=abc" {
		t.Fatalf("the nextLink must come back with the base stripped, got %q", next)
	}
	if _, _, err := graph.List(context.Background(), "f1", next, 20); err != nil {
		t.Fatalf("the cursor must be usable as-is on the next call: %v", err)
	}
	last := api.recorded()[len(api.recorded())-1]
	if last.Query.Get("$skiptoken") != "abc" {
		t.Errorf("the follow-up call must carry the skiptoken, got %q", last.RawRequestURI)
	}

	first := api.recorded()[0]
	if got := first.Query.Get("$top"); got != "20" {
		t.Errorf("limit must be sent as $top, got %q", got)
	}
	if got := first.Query.Get("$orderby"); got != "receivedDateTime desc" {
		t.Errorf("want newest first, got $orderby=%q", got)
	}
	for _, field := range []string{"id", "subject", "bodyPreview", "receivedDateTime", "from", "isRead", "flag", "hasAttachments"} {
		if !strings.Contains(first.Query.Get("$select"), field) {
			t.Errorf("$select must ask for %q, got %q", field, first.Query.Get("$select"))
		}
	}
}

func TestGraphListWithNoFolderReadsTheWholeMailbox(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/me/messages", http.StatusOK, `{"value":[]}`)

	if _, _, err := newRecordedGraph(t, api, &fakeTokenSource{}).
		List(context.Background(), "", "", 10); err != nil {
		t.Fatalf("List: %v", err)
	}
	api.assertEveryPathWasRouted()
}

func TestGraphListClampsTheLimitToTheRangeGraphAccepts(t *testing.T) {
	for _, tc := range []struct {
		give int
		want string
	}{{0, "50"}, {-1, "50"}, {5000, "500"}, {137, "137"}} {
		api := newRecordingAPI(t)
		api.route("/me/messages", http.StatusOK, `{"value":[]}`)
		if _, _, err := newRecordedGraph(t, api, &fakeTokenSource{}).
			List(context.Background(), "", "", tc.give); err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := api.only().Query.Get("$top"); got != tc.want {
			t.Errorf("limit %d must be sent as $top=%s, got %q", tc.give, tc.want, got)
		}
	}
}

func TestGraphGetReadsTheBodyAndFansOutToAttachmentsOnlyWhenThereAreSome(t *testing.T) {
	t.Run("with attachments", func(t *testing.T) {
		api := newRecordingAPI(t)
		api.route("/me/messages/m1", http.StatusOK, `{
			"id":"m1","subject":"A subject","hasAttachments":true,
			"body":{"contentType":"html","content":"<p>body</p>"},
			"internetMessageId":"<m1@example.com>",
			"internetMessageHeaders":[{"name":"In-Reply-To","value":"<prior@example.com>"}]}`)
		api.route("/me/messages/m1/attachments", http.StatusOK, `{"value":[
			{"id":"a1","name":"invoice.pdf","contentType":"application/pdf","size":1024,"contentId":"cid1"}
		]}`)

		msg, err := newRecordedGraph(t, api, &fakeTokenSource{}).Get(context.Background(), "", "m1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		api.assertEveryPathWasRouted()

		if msg.BodyHTML != "<p>body</p>" {
			t.Errorf("contentType html must land in body_html, got %q / %q", msg.BodyHTML, msg.BodyText)
		}
		if msg.Headers["Message-ID"] != "<m1@example.com>" {
			t.Errorf("internetMessageId must be surfaced as Message-ID, got %q", msg.Headers["Message-ID"])
		}
		if msg.InReplyTo != "<prior@example.com>" {
			t.Errorf("In-Reply-To must be lifted out of the headers, got %q", msg.InReplyTo)
		}
		if len(msg.Attachments) != 1 || msg.Attachments[0].Filename != "invoice.pdf" {
			t.Fatalf("want the one attachment, got %+v", msg.Attachments)
		}
		if msg.Attachments[0].InlineCID != "cid1" {
			t.Errorf("contentId must become the inline cid, got %q", msg.Attachments[0].InlineCID)
		}
	})

	t.Run("text body and no attachments", func(t *testing.T) {
		api := newRecordingAPI(t)
		api.route("/me/messages/m1", http.StatusOK,
			`{"id":"m1","hasAttachments":false,"body":{"contentType":"text","content":"plain"}}`)

		msg, err := newRecordedGraph(t, api, &fakeTokenSource{}).Get(context.Background(), "", "m1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if msg.BodyText != "plain" || msg.BodyHTML != "" {
			t.Errorf("contentType text must land in body_text, got html=%q text=%q", msg.BodyHTML, msg.BodyText)
		}
		if got := len(api.recorded()); got != 1 {
			t.Errorf("attachments must not be fetched when there are none, got %d requests", got)
		}
	})
}

// Graph's message and folder ids are long base64 strings that really do
// contain slashes, so concatenating one into the path silently retargets the
// request at a different resource. RequestURI is the discriminator: net/http
// decodes %2F back to a slash in URL.Path, so asserting on the decoded path
// would pass either way.
func TestGraphEscapesIDsIntoThePath(t *testing.T) {
	t.Run("message id", func(t *testing.T) {
		api := newRecordingAPI(t)
		api.route("/me/messages/AAMk/QBGAAA=", http.StatusOK, `{"id":"AAMk/QBGAAA="}`)

		if _, err := newRecordedGraph(t, api, &fakeTokenSource{}).
			Get(context.Background(), "", "AAMk/QBGAAA="); err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got := api.only().RawRequestURI; !strings.HasPrefix(got, "/me/messages/AAMk%2FQBGAAA=?") {
			t.Errorf("message id must be escaped into the path, got raw URI %q", got)
		}
	})

	t.Run("folder id", func(t *testing.T) {
		api := newRecordingAPI(t)
		api.route("/me/mailFolders/AAMk/QBGAAA=/messages", http.StatusOK, `{"value":[]}`)

		if _, _, err := newRecordedGraph(t, api, &fakeTokenSource{}).
			List(context.Background(), "AAMk/QBGAAA=", "", 10); err != nil {
			t.Fatalf("List: %v", err)
		}
		if got := api.only().RawRequestURI; !strings.HasPrefix(got, "/me/mailFolders/AAMk%2FQBGAAA=/messages?") {
			t.Errorf("folder id must be escaped into the path, got raw URI %q", got)
		}
	})
}

func TestGraphReportsTheFarStatusInEveryError(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		call func(g *Graph) error
	}{
		{"Folders", "/me/mailFolders", func(g *Graph) error {
			_, err := g.Folders(context.Background())
			return err
		}},
		{"List", "/me/messages", func(g *Graph) error {
			_, _, err := g.List(context.Background(), "", "", 10)
			return err
		}},
		{"Get", "/me/messages/m1", func(g *Graph) error {
			_, err := g.Get(context.Background(), "", "m1")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRecordingAPI(t)
			api.route(tc.path, http.StatusForbidden,
				`{"error":{"code":"ErrorAccessDenied","message":"Access is denied."}}`)

			err := tc.call(newRecordedGraph(t, api, &fakeTokenSource{}))
			if err == nil {
				t.Fatal("a 403 must be an error")
			}
			if !strings.Contains(err.Error(), "403") {
				t.Errorf("the status must reach the message, got %q", err)
			}
			if !strings.Contains(err.Error(), "Access is denied.") {
				t.Errorf("Graph's own message must reach the caller, got %q", err)
			}
		})
	}
}

func TestGraphRefreshesTheTokenOnlyOnA401(t *testing.T) {
	for _, tc := range []struct {
		status          int
		wantInvalidated int
		wantRequests    int
	}{
		{http.StatusUnauthorized, 1, 2},
		{http.StatusForbidden, 0, 1},
		{http.StatusTooManyRequests, 0, 1},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			api := newRecordingAPI(t)
			api.routeOnce("/me/mailFolders", tc.status, `{"error":{"code":"x"}}`)
			api.route("/me/mailFolders", http.StatusOK, `{"value":[]}`)
			tokens := &fakeTokenSource{}

			_, _ = newRecordedGraph(t, api, tokens).Folders(context.Background())

			if got := tokens.invalidations(); got != tc.wantInvalidated {
				t.Errorf("want %d cache invalidations for a %d, got %d",
					tc.wantInvalidated, tc.status, got)
			}
			if got := len(api.recorded()); got != tc.wantRequests {
				t.Errorf("want %d requests for a %d, got %d", tc.wantRequests, tc.status, got)
			}
		})
	}
}

func TestGraphRetryAfterA401CarriesTheRefreshedTokenAndTheSameBody(t *testing.T) {
	api := newRecordingAPI(t)
	api.routeOnce("/me/mailFolders", http.StatusUnauthorized, `{"error":{"code":"InvalidAuthenticationToken"}}`)
	api.route("/me/mailFolders", http.StatusOK, `{"value":[]}`)

	if _, err := newRecordedGraph(t, api, &fakeTokenSource{}).
		Folders(context.Background()); err != nil {
		t.Fatalf("Folders: %v", err)
	}
	got := api.recorded()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	if got[1].Authorization != "Bearer "+tokenForGeneration(1) {
		t.Errorf("want the refreshed token on the retry, got %q", got[1].Authorization)
	}
	if got[1].Body != got[0].Body {
		t.Errorf("the retry must resend the same body\n first: %q\n retry: %q", got[0].Body, got[1].Body)
	}
}

// The same drained-reader defect Gmail carried is reachable through Graph.do
// the moment a write method is written, so it is pinned at the transport
// rather than through a verb that does not exist yet.
func TestGraphRetryAfterA401ResendsARequestBody(t *testing.T) {
	api := newRecordingAPI(t)
	api.routeOnce("/me/messages/m1", http.StatusUnauthorized, `{"error":{"code":"x"}}`)
	api.route("/me/messages/m1", http.StatusOK, `{"id":"m1"}`)

	graph := newRecordedGraph(t, api, &fakeTokenSource{})
	resp, err := graph.do(context.Background(), http.MethodPatch, "/me/messages/m1", strings.NewReader(`{"isRead":true}`))
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	resp.Body.Close()

	got := api.recorded()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	if got[1].Body != `{"isRead":true}` {
		t.Fatalf("the retry must resend the body, got %q", got[1].Body)
	}
	if got[1].ContentType != "application/json" {
		t.Errorf("the retry must still declare the content type, got %q", got[1].ContentType)
	}
}

func TestGraphReportsAFailureToResolveTheTokenWithoutCallingGraph(t *testing.T) {
	api := newRecordingAPI(t)
	tokens := &fakeTokenSource{err: errors.New("auth-store unavailable")}

	_, err := newRecordedGraph(t, api, tokens).Folders(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auth-store unavailable") {
		t.Fatalf("a credential failure must reach the caller unchanged, got %v", err)
	}
	if got := len(api.recorded()); got != 0 {
		t.Errorf("Graph must not be called without a token, got %d requests", got)
	}
}

// Graph is read-only in mailstack today. These are pinned so that when a write
// is implemented the case fails and has to be replaced with a real one, rather
// than the verb quietly staying a stub.
func TestGraphWriteVerbsAreStillUnimplemented(t *testing.T) {
	api := newRecordingAPI(t)
	g := newRecordedGraph(t, api, &fakeTokenSource{})
	ctx := context.Background()

	for name, call := range map[string]func() error{
		"MarkRead":   func() error { return g.MarkRead(ctx, "m1", true) },
		"Flag":       func() error { return g.Flag(ctx, "m1", true) },
		"Move":       func() error { return g.Move(ctx, "m1", "f1") },
		"Delete":     func() error { return g.Delete(ctx, "m1") },
		"PermDelete": func() error { return g.PermDelete(ctx, "m1") },
		"Send":       func() error { _, err := g.Send(ctx, Draft{}); return err },
		"SaveDraft":  func() error { _, err := g.SaveDraft(ctx, Draft{}, ""); return err },
		"Attachment": func() error { _, _, err := g.Attachment(ctx, "m1", "a1"); return err },
	} {
		if err := call(); !errors.Is(err, errNotImplemented) {
			t.Errorf("%s: want errNotImplemented, got %v", name, err)
		}
	}
	if got := len(api.recorded()); got != 0 {
		t.Errorf("an unimplemented verb must not reach Graph, got %d requests", got)
	}
}
