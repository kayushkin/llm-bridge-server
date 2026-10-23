package backend

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// These cases judge Gmail.do's wire traffic against the published Gmail REST
// API rather than against mailstack's own beliefs about it:
//
//	https://developers.google.com/workspace/gmail/api/reference/rest/v1
//
// Every read and write of the user's Gmail mailbox goes through Gmail.do, and
// the package had no test file at all before this one.

func TestGmailFoldersReadsLabelsListThenFansOutToLabelsGet(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/labels", http.StatusOK, `{"labels":[
		{"id":"INBOX","name":"INBOX","type":"system"},
		{"id":"Label_9","name":"Receipts","type":"user"}
	]}`)
	api.route("/users/me/labels/INBOX", http.StatusOK, `{"messagesUnread":7}`)
	api.route("/users/me/labels/Label_9", http.StatusOK, `{"messagesUnread":2}`)

	folders, err := newRecordedGmail(t, api, &fakeTokenSource{}).Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	api.assertEveryPathWasRouted()

	if len(folders) != 2 {
		t.Fatalf("want 2 folders, got %d: %+v", len(folders), folders)
	}
	if folders[0].Canonical != KindInbox {
		t.Errorf("INBOX should map to the canonical inbox kind, got %q", folders[0].Canonical)
	}
	if folders[0].UnreadCount != 7 || folders[1].UnreadCount != 2 {
		t.Errorf("unread counts should come from labels.get, got %d and %d",
			folders[0].UnreadCount, folders[1].UnreadCount)
	}
	if folders[0].AccountID != "account-1" {
		t.Errorf("account id should be stamped on every folder, got %q", folders[0].AccountID)
	}

	for _, r := range api.recorded() {
		if r.Method != http.MethodGet {
			t.Errorf("%s must be a GET, got %s", r.Path, r.Method)
		}
		if r.Body != "" {
			t.Errorf("%s must carry no request body, got %q", r.Path, r.Body)
		}
		if r.Authorization != "Bearer "+tokenForGeneration(0) {
			t.Errorf("%s must carry the bearer token, got %q", r.Path, r.Authorization)
		}
	}
}

func TestGmailFoldersDropsLabelsHiddenFromTheLabelList(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/labels", http.StatusOK, `{"labels":[
		{"id":"INBOX","name":"INBOX"},
		{"id":"Label_hidden","name":"Hidden","labelListVisibility":"labelHide"}
	]}`)
	api.route("/users/me/labels/INBOX", http.StatusOK, `{"messagesUnread":1}`)

	folders, err := newRecordedGmail(t, api, &fakeTokenSource{}).Folders(context.Background())
	if err != nil {
		t.Fatalf("Folders: %v", err)
	}
	api.assertEveryPathWasRouted()
	if len(folders) != 1 || folders[0].ID != "INBOX" {
		t.Fatalf("a labelHide label must not become a folder, got %+v", folders)
	}
}

// A folder whose unread count could not be read is reported as zero unread,
// which is indistinguishable from a folder that is genuinely all-read. This
// case pins that deliberately rather than endorsing it: whether one flaky
// labels.get should blank a count or fail the whole sidebar is a judgement
// about how mailstack degrades, and it is filed rather than decided here.
func TestGmailFoldersSurvivesALabelsGetFailure(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/labels", http.StatusOK, `{"labels":[{"id":"INBOX","name":"INBOX"}]}`)
	api.route("/users/me/labels/INBOX", http.StatusInternalServerError, `{"error":{"code":500}}`)

	folders, err := newRecordedGmail(t, api, &fakeTokenSource{}).Folders(context.Background())
	if err != nil {
		t.Fatalf("a failed unread count must not fail the whole folder list: %v", err)
	}
	if len(folders) != 1 {
		t.Fatalf("want 1 folder, got %d", len(folders))
	}
	if folders[0].UnreadCount != 0 {
		t.Errorf("want 0 unread when labels.get failed, got %d", folders[0].UnreadCount)
	}
}

func TestGmailListSendsLabelAndPagingAsQueryParameters(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/messages", http.StatusOK,
		`{"messages":[{"id":"m1","threadId":"t1"}],"nextPageToken":"page-2"}`)
	api.route("/users/me/messages/m1", http.StatusOK, `{
		"id":"m1","labelIds":["INBOX","UNREAD","STARRED"],"snippet":"hello",
		"internalDate":"1700000000000",
		"payload":{"headers":[
			{"name":"Subject","value":"A subject"},
			{"name":"From","value":"Ada <ada@example.com>"},
			{"name":"To","value":"bob@example.com"}
		]}}`)

	metas, next, err := newRecordedGmail(t, api, &fakeTokenSource{}).
		List(context.Background(), "INBOX", "page-1", 25)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	api.assertEveryPathWasRouted()

	if next != "page-2" {
		t.Errorf("nextPageToken must become the cursor, got %q", next)
	}
	if len(metas) != 1 {
		t.Fatalf("want 1 message, got %d", len(metas))
	}
	if !metas[0].Unread || !metas[0].Flagged {
		t.Errorf("UNREAD and STARRED labels must set unread/flagged, got %+v", metas[0])
	}
	if metas[0].From.Email != "ada@example.com" || metas[0].From.Name != "Ada" {
		t.Errorf("From header must be parsed into name and address, got %+v", metas[0].From)
	}

	list := requestForPath(t, api, "/users/me/messages")
	if got := list.Query.Get("labelIds"); got != "INBOX" {
		t.Errorf("folder must be sent as labelIds, got %q", got)
	}
	if got := list.Query.Get("maxResults"); got != "25" {
		t.Errorf("limit must be sent as maxResults, got %q", got)
	}
	if got := list.Query.Get("pageToken"); got != "page-1" {
		t.Errorf("cursor must be sent as pageToken, got %q", got)
	}

	// messages.list returns id-only stubs, so each message is fetched with
	// format=metadata and an explicit header allowlist.
	get := requestForPath(t, api, "/users/me/messages/m1")
	if got := get.Query.Get("format"); got != "metadata" {
		t.Errorf("want format=metadata on the list fan-out, got %q", got)
	}
	// Written out here rather than read from gmailMetadataHeaders, so a header
	// dropped from that set fails this test. The set is a map, so the order on
	// the wire is not fixed and the comparison ignores it.
	wantHeaders := []string{"Cc", "Date", "From", "In-Reply-To", "Message-ID", "References", "Subject", "To"}
	gotHeaders := append([]string(nil), get.Query["metadataHeaders"]...)
	sort.Strings(gotHeaders)
	if !equalStrings(gotHeaders, wantHeaders) {
		t.Errorf("want metadataHeaders %v in any order, got %v", wantHeaders, get.Query["metadataHeaders"])
	}
}

func TestGmailListClampsTheLimitToTheRangeGmailAccepts(t *testing.T) {
	for _, tc := range []struct {
		name string
		give int
		want string
	}{
		{"zero means the default page", 0, "50"},
		{"negative means the default page", -3, "50"},
		{"over the maximum is capped", 5000, "500"},
		{"in range is sent through", 137, "137"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRecordingAPI(t)
			api.route("/users/me/messages", http.StatusOK, `{"messages":[]}`)
			if _, _, err := newRecordedGmail(t, api, &fakeTokenSource{}).
				List(context.Background(), "", "", tc.give); err != nil {
				t.Fatalf("List: %v", err)
			}
			if got := api.only().Query.Get("maxResults"); got != tc.want {
				t.Errorf("limit %d must be sent as maxResults=%s, got %q", tc.give, tc.want, got)
			}
		})
	}
}

func TestGmailGetAsksForTheFullMessageAndDecodesBothBodyParts(t *testing.T) {
	api := newRecordingAPI(t)
	html := base64.URLEncoding.EncodeToString([]byte("<p>body</p>"))
	text := base64.URLEncoding.EncodeToString([]byte("body"))
	api.route("/users/me/messages/m1", http.StatusOK, `{
		"id":"m1","labelIds":["INBOX"],
		"payload":{"mimeType":"multipart/alternative","headers":[
			{"name":"Subject","value":"A subject"},
			{"name":"In-Reply-To","value":"<prior@example.com>"}
		],"parts":[
			{"mimeType":"text/plain","body":{"data":"`+text+`"}},
			{"mimeType":"text/html","body":{"data":"`+html+`"}}
		]}}`)

	msg, err := newRecordedGmail(t, api, &fakeTokenSource{}).Get(context.Background(), "", "m1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := api.only().Query.Get("format"); got != "full" {
		t.Errorf("want format=full, got %q", got)
	}
	if msg.BodyHTML != "<p>body</p>" || msg.BodyText != "body" {
		t.Errorf("both body parts must be base64url-decoded, got html=%q text=%q",
			msg.BodyHTML, msg.BodyText)
	}
	if msg.InReplyTo != "<prior@example.com>" {
		t.Errorf("In-Reply-To must be lifted out of the headers, got %q", msg.InReplyTo)
	}
}

// Gmail documents the part payload as base64url and does not promise padding.
// Both spellings must decode to the same bytes.
func TestGmailGetAcceptsPaddedAndUnpaddedBase64URLBodies(t *testing.T) {
	const want = "body needing padding"
	padded := base64.URLEncoding.EncodeToString([]byte(want))
	unpadded := base64.RawURLEncoding.EncodeToString([]byte(want))
	if padded == unpadded {
		t.Fatalf("pick a fixture whose encoding actually needs padding; both were %q", padded)
	}

	for name, encoded := range map[string]string{"padded": padded, "unpadded": unpadded} {
		t.Run(name, func(t *testing.T) {
			api := newRecordingAPI(t)
			api.route("/users/me/messages/m1", http.StatusOK,
				`{"id":"m1","payload":{"mimeType":"text/plain","body":{"data":"`+encoded+`"}}}`)

			msg, err := newRecordedGmail(t, api, &fakeTokenSource{}).Get(context.Background(), "", "m1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if msg.BodyText != want {
				t.Errorf("want body %q, got %q", want, msg.BodyText)
			}
		})
	}
}

// A part that cannot be decoded used to be dropped, so the message arrived
// with an empty body and no error — the same value a genuinely empty message
// produces. The caller has to be able to tell those apart.
func TestGmailGetReportsAnUndecodableBodyRatherThanReturningAnEmptyOne(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/messages/m1", http.StatusOK,
		`{"id":"m1","payload":{"mimeType":"text/plain","body":{"data":"!!!not base64!!!"}}}`)

	msg, err := newRecordedGmail(t, api, &fakeTokenSource{}).Get(context.Background(), "", "m1")
	if err == nil {
		t.Fatalf("an undecodable body must be an error, got message %+v", msg)
	}
	if !strings.Contains(err.Error(), "text/plain") {
		t.Errorf("the error should say which part failed, got %q", err)
	}
}

// An id has to be escaped into the path rather than concatenated. The
// separator is the discriminator: a space survives either way, because Go
// escapes it when it writes the request, so a case built on one is green with
// or without the escaping. A slash is not — unescaped it becomes another path
// segment, and Gmail routes it somewhere else entirely.
func TestGmailGetEscapesTheMessageIDIntoThePath(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/messages/a/b", http.StatusOK, `{"id":"a/b","payload":{}}`)

	if _, err := newRecordedGmail(t, api, &fakeTokenSource{}).
		Get(context.Background(), "", "a/b"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// Asserting the decoded Path would pass whether or not the id was escaped,
	// because net/http decodes %2F straight back to a slash. RequestURI is
	// the only field that tells the two apart.
	if got := api.only().RawRequestURI; !strings.HasPrefix(got, "/users/me/messages/a%2Fb?") {
		t.Errorf("message id must be escaped into the path, got raw URI %q", got)
	}
}

func TestGmailWritesPostToModifyWithTheDocumentedLabelFields(t *testing.T) {
	for _, tc := range []struct {
		name       string
		call       func(g *Gmail) error
		wantAdd    []string
		wantRemove []string
	}{
		{"mark read removes UNREAD",
			func(g *Gmail) error { return g.MarkRead(context.Background(), "m1", true) },
			nil, []string{"UNREAD"}},
		{"mark unread adds UNREAD",
			func(g *Gmail) error { return g.MarkRead(context.Background(), "m1", false) },
			[]string{"UNREAD"}, nil},
		{"flag adds STARRED",
			func(g *Gmail) error { return g.Flag(context.Background(), "m1", true) },
			[]string{"STARRED"}, nil},
		{"unflag removes STARRED",
			func(g *Gmail) error { return g.Flag(context.Background(), "m1", false) },
			nil, []string{"STARRED"}},
		{"move adds the target label and takes the message out of the inbox",
			func(g *Gmail) error { return g.Move(context.Background(), "m1", "Label_9") },
			[]string{"Label_9"}, []string{"INBOX"}},
		{"move to the inbox does not then remove the inbox",
			func(g *Gmail) error { return g.Move(context.Background(), "m1", "INBOX") },
			[]string{"INBOX"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRecordingAPI(t)
			api.route("/users/me/messages/m1/modify", http.StatusOK, `{"id":"m1"}`)

			if err := tc.call(newRecordedGmail(t, api, &fakeTokenSource{})); err != nil {
				t.Fatalf("write: %v", err)
			}
			api.assertEveryPathWasRouted()

			r := api.only()
			if r.Method != http.MethodPost {
				t.Errorf("messages.modify is a POST, got %s", r.Method)
			}
			if r.ContentType != "application/json" {
				t.Errorf("a request with a body must declare application/json, got %q", r.ContentType)
			}
			var sent struct {
				AddLabelIDs    []string `json:"addLabelIds"`
				RemoveLabelIDs []string `json:"removeLabelIds"`
			}
			if err := json.Unmarshal([]byte(r.Body), &sent); err != nil {
				t.Fatalf("request body must be JSON, got %q: %v", r.Body, err)
			}
			if !equalStrings(sent.AddLabelIDs, tc.wantAdd) {
				t.Errorf("want addLabelIds %v, got %v", tc.wantAdd, sent.AddLabelIDs)
			}
			if !equalStrings(sent.RemoveLabelIDs, tc.wantRemove) {
				t.Errorf("want removeLabelIds %v, got %v", tc.wantRemove, sent.RemoveLabelIDs)
			}
		})
	}
}

func TestGmailMoveWithNoFolderNeverReachesTheAPI(t *testing.T) {
	api := newRecordingAPI(t)
	if err := newRecordedGmail(t, api, &fakeTokenSource{}).
		Move(context.Background(), "m1", ""); err == nil {
		t.Fatal("Move with no folder must be an error")
	}
	if got := len(api.recorded()); got != 0 {
		t.Errorf("Move with no folder must not call Gmail at all, got %d requests", got)
	}
}

func TestGmailDeleteTrashesAndPermDeleteRemoves(t *testing.T) {
	t.Run("Delete posts to trash", func(t *testing.T) {
		api := newRecordingAPI(t)
		api.route("/users/me/messages/m1/trash", http.StatusOK, `{"id":"m1"}`)
		if err := newRecordedGmail(t, api, &fakeTokenSource{}).
			Delete(context.Background(), "m1"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		api.assertEveryPathWasRouted()
		r := api.only()
		if r.Method != http.MethodPost {
			t.Errorf("messages.trash is a POST, got %s", r.Method)
		}
		if r.Body != "" {
			t.Errorf("messages.trash takes no body, got %q", r.Body)
		}
		if r.ContentType != "" {
			t.Errorf("a bodyless request must not declare a content type, got %q", r.ContentType)
		}
	})

	// messages.delete answers 204 No Content, so a handler that only accepts
	// 200 would report every successful permanent delete as a failure.
	for _, status := range []int{http.StatusNoContent, http.StatusOK} {
		t.Run("PermDelete accepts "+http.StatusText(status), func(t *testing.T) {
			api := newRecordingAPI(t)
			api.route("/users/me/messages/m1", status, ``)
			if err := newRecordedGmail(t, api, &fakeTokenSource{}).
				PermDelete(context.Background(), "m1"); err != nil {
				t.Fatalf("PermDelete: %v", err)
			}
			if got := api.only().Method; got != http.MethodDelete {
				t.Errorf("messages.delete is a DELETE, got %s", got)
			}
		})
	}
}

// Every read and write must report the status Gmail answered with. Asserting
// only that an error came back is green on a client that has stopped reading
// status codes at all, because the decode of an error envelope fails one step
// later and blames the message instead of the request.
func TestGmailReportsTheFarStatusInEveryError(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		call func(g *Gmail) error
	}{
		{"Folders", "/users/me/labels", func(g *Gmail) error {
			_, err := g.Folders(context.Background())
			return err
		}},
		{"List", "/users/me/messages", func(g *Gmail) error {
			_, _, err := g.List(context.Background(), "", "", 10)
			return err
		}},
		{"Get", "/users/me/messages/m1", func(g *Gmail) error {
			_, err := g.Get(context.Background(), "", "m1")
			return err
		}},
		{"MarkRead", "/users/me/messages/m1/modify", func(g *Gmail) error {
			return g.MarkRead(context.Background(), "m1", true)
		}},
		{"Delete", "/users/me/messages/m1/trash", func(g *Gmail) error {
			return g.Delete(context.Background(), "m1")
		}},
		{"PermDelete", "/users/me/messages/m1", func(g *Gmail) error {
			return g.PermDelete(context.Background(), "m1")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newRecordingAPI(t)
			api.route(tc.path, http.StatusForbidden,
				`{"error":{"code":403,"message":"Insufficient Permission"}}`)

			err := tc.call(newRecordedGmail(t, api, &fakeTokenSource{}))
			if err == nil {
				t.Fatal("a 403 must be an error")
			}
			if !strings.Contains(err.Error(), "403") {
				t.Errorf("the status must reach the message, got %q", err)
			}
			if !strings.Contains(err.Error(), "Insufficient Permission") {
				t.Errorf("Gmail's own message must reach the caller, got %q", err)
			}
		})
	}
}

// A 403 is a hard failure and must not spend a token refresh, while a 401 is
// the one status that means the cached token is stale.
func TestGmailRefreshesTheTokenOnlyOnA401(t *testing.T) {
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
			api.routeOnce("/users/me/labels", tc.status, `{"error":{"code":0}}`)
			api.route("/users/me/labels", http.StatusOK, `{"labels":[]}`)
			tokens := &fakeTokenSource{}

			_, _ = newRecordedGmail(t, api, tokens).Folders(context.Background())

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

func TestGmailRetryAfterA401CarriesTheRefreshedToken(t *testing.T) {
	api := newRecordingAPI(t)
	api.routeOnce("/users/me/labels", http.StatusUnauthorized, `{"error":{"code":401}}`)
	api.route("/users/me/labels", http.StatusOK, `{"labels":[]}`)

	if _, err := newRecordedGmail(t, api, &fakeTokenSource{}).
		Folders(context.Background()); err != nil {
		t.Fatalf("Folders: %v", err)
	}
	got := api.recorded()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	if got[0].Authorization == got[1].Authorization {
		t.Fatalf("the retry must carry a new token, both were %q", got[0].Authorization)
	}
	if got[1].Authorization != "Bearer "+tokenForGeneration(1) {
		t.Errorf("want the refreshed token on the retry, got %q", got[1].Authorization)
	}
}

// The whole point of the 401 branch is to make a write succeed after a token
// rotation. messages.modify documents its request body as required, so a
// retry that drops the body cannot do that.
func TestGmailRetryAfterA401ResendsTheRequestBody(t *testing.T) {
	api := newRecordingAPI(t)
	api.routeOnce("/users/me/messages/m1/modify", http.StatusUnauthorized, `{"error":{"code":401}}`)
	api.route("/users/me/messages/m1/modify", http.StatusOK, `{"id":"m1"}`)

	if err := newRecordedGmail(t, api, &fakeTokenSource{}).
		MarkRead(context.Background(), "m1", true); err != nil {
		t.Fatalf("MarkRead: %v", err)
	}
	got := api.recorded()
	if len(got) != 2 {
		t.Fatalf("want 2 requests, got %d", len(got))
	}
	if got[1].Body != got[0].Body {
		t.Fatalf("the retry must resend the same body\n first: %q\n retry: %q", got[0].Body, got[1].Body)
	}
	var sent gmailModifyRequest
	if err := json.Unmarshal([]byte(got[1].Body), &sent); err != nil {
		t.Fatalf("the retry body must still be the modify request, got %q: %v", got[1].Body, err)
	}
	if !equalStrings(sent.RemoveLabelIDs, []string{"UNREAD"}) {
		t.Errorf("the retry must still ask to remove UNREAD, got %+v", sent)
	}
}

func TestGmailReportsAFailureToResolveTheTokenWithoutCallingGmail(t *testing.T) {
	api := newRecordingAPI(t)
	tokens := &fakeTokenSource{err: errors.New("auth-store unavailable")}

	_, err := newRecordedGmail(t, api, tokens).Folders(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auth-store unavailable") {
		t.Fatalf("a credential failure must reach the caller unchanged, got %v", err)
	}
	if got := len(api.recorded()); got != 0 {
		t.Errorf("Gmail must not be called without a token, got %d requests", got)
	}
}

func requestForPath(t *testing.T, api *recordingAPI, path string) wireRequest {
	t.Helper()
	for _, r := range api.recorded() {
		if r.Path == path {
			return r
		}
	}
	t.Fatalf("no request recorded for %s", path)
	return wireRequest{}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Search hands the query to Gmail's own search and names no label, so it
// reaches every folder; List names the label and sends no query.
func TestGmailSearchSendsTheQueryAndNoLabel(t *testing.T) {
	api := newRecordingAPI(t)
	api.route("/users/me/messages", http.StatusOK, `{"messages":[]}`)

	if _, _, err := newRecordedGmail(t, api, &fakeTokenSource{}).Search(context.Background(), "from:unr.edu after:2023/01/01", "", 20); err != nil {
		t.Fatalf("Search: %v", err)
	}
	request := api.only()
	if got := request.Query.Get("q"); got != "from:unr.edu after:2023/01/01" {
		t.Errorf("want q passed through, got %q", got)
	}
	if request.Query.Has("labelIds") {
		t.Errorf("a search must name no label, got labelIds=%q", request.Query.Get("labelIds"))
	}
	if got := request.Query.Get("maxResults"); got != "20" {
		t.Errorf("want maxResults=20, got %q", got)
	}
}
