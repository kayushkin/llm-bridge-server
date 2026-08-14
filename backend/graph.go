package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const graphBase = "https://graph.microsoft.com/v1.0"

// Graph implements Backend using Microsoft Graph REST API via direct HTTP.
// Read-only for Phase 3; write methods return errNotImplemented.
type Graph struct {
	accountID       string
	apiauthProvider string
	apiauthAccount  string
	tokens          TokenSource
	http            *http.Client
	// base is the Graph API root every request is built on. It is a field
	// rather than the graphBase constant so a test can point this client at a
	// stand-in server: the continuation walk below can only be judged by
	// watching the exact URLs that go out on the second and third requests.
	base string
}

func NewGraph(accountID, apiauthProvider, apiauthAccount string, tokens TokenSource) *Graph {
	return &Graph{
		accountID:       accountID,
		apiauthProvider: apiauthProvider,
		apiauthAccount:  apiauthAccount,
		tokens:          tokens,
		http:            &http.Client{Timeout: 30 * time.Second},
		base:            graphBase,
	}
}

// Graph well-known folder names → canonical kinds.
// https://learn.microsoft.com/en-us/graph/api/resources/mailfolder
var graphCanonical = map[string]CanonicalKind{
	"inbox":        KindInbox,
	"sentitems":    KindSent,
	"drafts":       KindDrafts,
	"deleteditems": KindTrash,
	"junkemail":    KindSpam,
	"archive":      KindArchive,
}

// do issues a request against a path relative to the Graph API root.
func (g *Graph) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return g.doAbsoluteURL(ctx, method, g.base+path, body)
}

// doAbsoluteURL issues a request against an already-complete URL. Continuation
// links arrive from Graph absolute and must be sent back unchanged, so they
// cannot go through do, which would prepend the API root a second time.
func (g *Graph) doAbsoluteURL(ctx context.Context, method, fullURL string, body io.Reader) (*http.Response, error) {
	token, err := g.tokens.Token(g.apiauthProvider, g.apiauthAccount)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		g.tokens.Invalidate(g.apiauthProvider, g.apiauthAccount)

		// Same rule as Gmail.do: the first Do drained the reader this method
		// was handed, so the retry has to replay the bytes rather than resend
		// that reader. Every Graph write op is errNotImplemented today, so no
		// caller passes a body and the empty-retry defect is latent here — it
		// goes live with the first write op somebody implements.
		retryBody, replayable, err := bodyForRetry(req)
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("graph: rebuilding the request body for the 401 retry: %w", err)
		}
		if !replayable {
			return resp, nil
		}

		resp.Body.Close()
		token, err = g.tokens.Token(g.apiauthProvider, g.apiauthAccount)
		if err != nil {
			return nil, err
		}
		retryReq, err := http.NewRequestWithContext(ctx, method, fullURL, retryBody)
		if err != nil {
			return nil, err
		}
		retryReq.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			retryReq.Header.Set("Content-Type", "application/json")
		}
		return g.http.Do(retryReq)
	}
	return resp, nil
}

// validatedContinuationURL checks an @odata.nextLink before it is followed.
//
// The link must be followed verbatim: its $skiptoken is only valid against the
// query that issued it, so rebuilding the URL from its parts restarts the walk
// at page one and terminates only on a loop guard. But "verbatim" must not
// become "wherever the response points" — the follow-up request carries the
// account's bearer token, and a link off the Graph host would hand that token
// to whoever the link names. So the link is checked and then used unchanged.
func (g *Graph) validatedContinuationURL(nextLink string) (string, error) {
	if !strings.HasPrefix(nextLink, g.base+"/") {
		return "", fmt.Errorf("graph: refusing to follow continuation link outside %s: %s", g.base, nextLink)
	}
	return nextLink, nil
}

type graphFolder struct {
	ID              string `json:"id"`
	DisplayName     string `json:"displayName"`
	ParentFolderID  string `json:"parentFolderId"`
	UnreadItemCount int    `json:"unreadItemCount"`
	WellKnownName   string `json:"wellKnownName,omitempty"`
}

type graphFolderList struct {
	Value    []graphFolder `json:"value"`
	NextLink string        `json:"@odata.nextLink"`
}

const folderPath = "/me/mailFolders?$top=100&$select=id,displayName,parentFolderId,unreadItemCount,wellKnownName"

// maxFolderPages bounds the continuation walk. At $top=100 this allows 5,000
// folders, far past any real mailbox, so reaching it means the walk is not
// terminating rather than that the account is large — and it is reported as an
// error for that reason. A guard that stopped quietly here would return a
// truncated folder list, which is the defect this walk exists to fix.
const maxFolderPages = 50

func (g *Graph) Folders(ctx context.Context) ([]Folder, error) {
	// Graph pages the folder list like any other collection: $top caps what one
	// response carries and the rest arrives behind @odata.nextLink. Reading only
	// the first page returns a short folder list that looks complete to the UI.
	next := g.base + folderPath
	fetched := make(map[string]bool)
	var out []Folder

	for pages := 0; ; pages++ {
		if pages >= maxFolderPages {
			return nil, fmt.Errorf("graph mailFolders: still paging after %d pages, giving up", maxFolderPages)
		}
		if fetched[next] {
			return nil, fmt.Errorf("graph mailFolders: continuation re-served a page already read, so the walk is not advancing: %s", next)
		}
		fetched[next] = true

		list, err := g.fetchFolderPage(ctx, next)
		if err != nil {
			return nil, err
		}
		for _, f := range list.Value {
			out = append(out, g.toFolder(f))
		}
		if list.NextLink == "" {
			return out, nil
		}
		if next, err = g.validatedContinuationURL(list.NextLink); err != nil {
			return nil, err
		}
	}
}

func (g *Graph) fetchFolderPage(ctx context.Context, fullURL string) (*graphFolderList, error) {
	resp, err := g.doAbsoluteURL(ctx, "GET", fullURL, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("graph mailFolders %d: %s", resp.StatusCode, string(b))
	}
	var list graphFolderList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	return &list, nil
}

func (g *Graph) toFolder(f graphFolder) Folder {
	folder := Folder{
		ID:          f.ID,
		AccountID:   g.accountID,
		Name:        f.DisplayName,
		Path:        f.DisplayName,
		ParentID:    f.ParentFolderID,
		UnreadCount: f.UnreadItemCount,
	}
	if f.WellKnownName != "" {
		if k, ok := graphCanonical[strings.ToLower(f.WellKnownName)]; ok {
			folder.Canonical = k
		}
	}
	return folder
}

type graphRecipient struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

type graphMessage struct {
	ID               string           `json:"id"`
	Subject          string           `json:"subject"`
	BodyPreview      string           `json:"bodyPreview"`
	ReceivedDateTime string           `json:"receivedDateTime"`
	From             graphRecipient   `json:"from"`
	ToRecipients     []graphRecipient `json:"toRecipients"`
	CcRecipients     []graphRecipient `json:"ccRecipients"`
	ParentFolderID   string           `json:"parentFolderId"`
	IsRead           bool             `json:"isRead"`
	Flag             struct {
		FlagStatus string `json:"flagStatus"`
	} `json:"flag"`
	HasAttachments bool `json:"hasAttachments"`
	Body           struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	InternetMessageHeaders []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"internetMessageHeaders"`
	InternetMessageID string `json:"internetMessageId"`
	ConversationID    string `json:"conversationId"`
}

type graphMessageList struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

func (g *Graph) List(ctx context.Context, folderID, cursor string, limit int) ([]MessageMeta, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	// Cursor is the full $skiptoken (we pass it back via $top+$skiptoken params).
	var path string
	if cursor != "" {
		// Graph returns an absolute nextLink URL; we stored just the query portion.
		path = cursor
	} else {
		q := url.Values{}
		q.Set("$top", strconv.Itoa(limit))
		// conversationId and internetMessageId are selected here too, not only on the
		// Get path: without them the list path yields no threading data at all, and
		// Graph charges nothing extra for two more projected fields.
		q.Set("$select", "id,subject,bodyPreview,receivedDateTime,from,toRecipients,ccRecipients,parentFolderId,isRead,flag,hasAttachments,conversationId,internetMessageId")
		q.Set("$orderby", "receivedDateTime desc")
		if folderID != "" {
			path = "/me/mailFolders/" + url.PathEscape(folderID) + "/messages?" + q.Encode()
		} else {
			path = "/me/messages?" + q.Encode()
		}
	}
	resp, err := g.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("graph messages.list %d: %s", resp.StatusCode, string(b))
	}
	var list graphMessageList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, "", err
	}
	out := make([]MessageMeta, 0, len(list.Value))
	for i := range list.Value {
		out = append(out, g.toMeta(&list.Value[i]))
	}
	// Strip the absolute base so the cursor round-trips through our handler.
	next := list.NextLink
	if next != "" && strings.HasPrefix(next, g.base) {
		next = strings.TrimPrefix(next, g.base)
	}
	return out, next, nil
}

func (g *Graph) toMeta(m *graphMessage) MessageMeta {
	meta := MessageMeta{
		ID:        m.ID,
		AccountID: g.accountID,
		FolderID:  m.ParentFolderID,
		Subject:   m.Subject,
		Snippet:   m.BodyPreview,
		Unread:    !m.IsRead,
		Flagged:   m.Flag.FlagStatus == "flagged",
		HasAttach: m.HasAttachments,
		MessageID: StripAngleBrackets(m.InternetMessageID),
		ThreadID:  m.ConversationID,
	}
	if m.From.EmailAddress.Address != "" {
		meta.From = Address{Name: m.From.EmailAddress.Name, Email: m.From.EmailAddress.Address}
	}
	for _, r := range m.ToRecipients {
		meta.To = append(meta.To, Address{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address})
	}
	for _, r := range m.CcRecipients {
		meta.Cc = append(meta.Cc, Address{Name: r.EmailAddress.Name, Email: r.EmailAddress.Address})
	}
	if m.ReceivedDateTime != "" {
		if t, err := time.Parse(time.RFC3339Nano, m.ReceivedDateTime); err == nil {
			meta.Date = t
		} else if t, err := time.Parse(time.RFC3339, m.ReceivedDateTime); err == nil {
			meta.Date = t
		}
	}
	return meta
}

func (g *Graph) Get(ctx context.Context, id string) (Message, error) {
	q := url.Values{}
	q.Set("$select",
		"id,subject,bodyPreview,receivedDateTime,from,toRecipients,ccRecipients,"+
			"parentFolderId,isRead,flag,hasAttachments,body,internetMessageHeaders,internetMessageId,conversationId")
	path := "/me/messages/" + url.PathEscape(id) + "?" + q.Encode()
	resp, err := g.do(ctx, "GET", path, nil)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return Message{}, fmt.Errorf("graph messages.get(%s) %d: %s", id, resp.StatusCode, string(b))
	}
	var m graphMessage
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return Message{}, err
	}

	out := Message{
		Meta:    g.toMeta(&m),
		Headers: map[string]string{},
	}
	for _, h := range m.InternetMessageHeaders {
		out.Headers[h.Name] = h.Value
	}
	if m.InternetMessageID != "" {
		out.Headers["Message-ID"] = m.InternetMessageID
	}
	if in, ok := out.Headers["In-Reply-To"]; ok {
		out.InReplyTo = in
	}
	switch strings.ToLower(m.Body.ContentType) {
	case "html":
		out.BodyHTML = m.Body.Content
	case "text":
		out.BodyText = m.Body.Content
	default:
		out.BodyText = m.Body.Content
	}

	if m.HasAttachments {
		atts, err := g.listAttachments(ctx, id)
		if err == nil {
			out.Attachments = atts
		}
	}
	return out, nil
}

type graphAttachment struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
	IsInline    bool   `json:"isInline"`
	ContentID   string `json:"contentId"`
}

func (g *Graph) listAttachments(ctx context.Context, msgID string) ([]Attachment, error) {
	path := "/me/messages/" + url.PathEscape(msgID) + "/attachments?$select=id,name,contentType,size,isInline,contentId"
	resp, err := g.do(ctx, "GET", path, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("graph attachments.list %d: %s", resp.StatusCode, string(b))
	}
	var list struct {
		Value []graphAttachment `json:"value"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, err
	}
	out := make([]Attachment, 0, len(list.Value))
	for _, a := range list.Value {
		out = append(out, Attachment{
			ID:        a.ID,
			Filename:  a.Name,
			MimeType:  a.ContentType,
			Size:      a.Size,
			InlineCID: a.ContentID,
		})
	}
	return out, nil
}

func (g *Graph) MarkRead(ctx context.Context, id string, read bool) error { return errNotImplemented }
func (g *Graph) Flag(ctx context.Context, id string, flagged bool) error  { return errNotImplemented }
func (g *Graph) Move(ctx context.Context, id, folderID string) error      { return errNotImplemented }
func (g *Graph) Delete(ctx context.Context, id string) error              { return errNotImplemented }
func (g *Graph) PermDelete(ctx context.Context, id string) error          { return errNotImplemented }
func (g *Graph) Send(ctx context.Context, d Draft) (string, error)        { return "", errNotImplemented }
func (g *Graph) SaveDraft(ctx context.Context, d Draft, existingID string) (string, error) {
	return "", errNotImplemented
}
func (g *Graph) Attachment(ctx context.Context, msgID, attID string) (io.ReadCloser, string, error) {
	return nil, "", errNotImplemented
}
