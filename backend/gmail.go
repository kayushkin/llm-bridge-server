package backend

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const gmailBase = "https://gmail.googleapis.com/gmail/v1"

type TokenSource interface {
	Token(provider, account string) (string, error)
	Invalidate(provider, account string)
}

// Gmail implements Backend using the Gmail REST API via direct HTTP.
type Gmail struct {
	accountID       string
	apiauthProvider string
	apiauthAccount  string
	tokens          TokenSource
	http            *http.Client
	// base is the Gmail API root every request is built on. It is a field
	// rather than the gmailBase constant so a test can point this client at a
	// stand-in server and judge the wire traffic against Gmail's published API.
	base string
}

func NewGmail(accountID, apiauthProvider, apiauthAccount string, tokens TokenSource) *Gmail {
	return &Gmail{
		accountID:       accountID,
		apiauthProvider: apiauthProvider,
		apiauthAccount:  apiauthAccount,
		tokens:          tokens,
		http:            &http.Client{Timeout: 30 * time.Second},
		base:            gmailBase,
	}
}

var gmailCanonical = map[string]CanonicalKind{
	"INBOX": KindInbox,
	"SENT":  KindSent,
	"DRAFT": KindDrafts,
	"TRASH": KindTrash,
	"SPAM":  KindSpam,
}

func (g *Gmail) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	token, err := g.tokens.Token(g.apiauthProvider, g.apiauthAccount)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
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
		// Token may have been revoked/rotated; drop cache and retry once.
		g.tokens.Invalidate(g.apiauthProvider, g.apiauthAccount)

		// The retry has to carry the same bytes as the first attempt. That
		// first Do drained the reader this method was handed, so building the
		// retry from it again puts an EMPTY body on the wire — and an empty
		// modify is a well-formed request that changes nothing, which Gmail
		// answers 200 and every layer above reports as a successful write.
		retryBody, replayable, err := bodyForRetry(req)
		if err != nil {
			resp.Body.Close()
			return nil, fmt.Errorf("gmail: rebuilding the request body for the 401 retry: %w", err)
		}
		if !replayable {
			// Nothing can rebuild this body, so there is no retry to make that
			// is not a silent no-op. Hand the 401 to the caller instead.
			return resp, nil
		}

		resp.Body.Close()
		token, err = g.tokens.Token(g.apiauthProvider, g.apiauthAccount)
		if err != nil {
			return nil, err
		}
		retryReq, err := http.NewRequestWithContext(ctx, method, g.base+path, retryBody)
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

// bodyForRetry returns a fresh reader over the bytes an already-sent request
// carried, so a retry of that request can send the same body. Sending drains
// whatever io.Reader the caller supplied, so the original is at EOF and cannot
// be reused.
//
// net/http records a GetBody hook for the in-memory body types this package
// passes (*bytes.Reader, *bytes.Buffer, *strings.Reader). For anything else —
// a streaming body, a pipe — there is no hook and no way to replay the bytes:
// replayable is false and the caller must not retry.
func bodyForRetry(req *http.Request) (fresh io.Reader, replayable bool, err error) {
	if req.Body == nil {
		return nil, true, nil // nothing was sent, so nothing needs replaying
	}
	if req.GetBody == nil {
		return nil, false, nil
	}
	rc, err := req.GetBody()
	if err != nil {
		return nil, false, err
	}
	return rc, true, nil
}

type gmailLabel struct {
	ID                    string `json:"id"`
	Name                  string `json:"name"`
	Type                  string `json:"type"`
	MessagesUnread        int    `json:"messagesUnread"`
	LabelListVisibility   string `json:"labelListVisibility"`
	MessageListVisibility string `json:"messageListVisibility"`
}

func (g *Gmail) Folders(ctx context.Context) ([]Folder, error) {
	resp, err := g.do(ctx, "GET", "/users/me/labels", nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gmail labels %d: %s", resp.StatusCode, string(b))
	}
	var out struct {
		Labels []gmailLabel `json:"labels"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}

	// Build the visible folder set first; labels.list returns 0 for unread
	// counts, so we fan out labels.get below to fill them in.
	folders := make([]Folder, 0, len(out.Labels))
	for _, l := range out.Labels {
		if l.LabelListVisibility == "labelHide" {
			continue
		}
		f := Folder{
			ID:        l.ID,
			AccountID: g.accountID,
			Name:      l.Name,
			Path:      l.Name,
		}
		if k, ok := gmailCanonical[l.ID]; ok {
			f.Canonical = k
		}
		folders = append(folders, f)
	}

	// Fan out labels.get for accurate unread counts. Concurrency cap of 8
	// keeps us well below Gmail's per-user 15 req/s soft limit.
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i := range folders {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if n, err := g.labelUnread(ctx, folders[i].ID); err == nil {
				folders[i].UnreadCount = n
			}
		}(i)
	}
	wg.Wait()
	return folders, nil
}

func (g *Gmail) labelUnread(ctx context.Context, id string) (int, error) {
	resp, err := g.do(ctx, "GET", "/users/me/labels/"+url.PathEscape(id), nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return 0, fmt.Errorf("gmail labels.get(%s) %d: %s", id, resp.StatusCode, string(b))
	}
	var out struct {
		MessagesUnread int `json:"messagesUnread"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, err
	}
	return out.MessagesUnread, nil
}

type gmailMessageStub struct {
	ID       string `json:"id"`
	ThreadID string `json:"threadId"`
}

type gmailMessagesList struct {
	Messages           []gmailMessageStub `json:"messages"`
	NextPageToken      string             `json:"nextPageToken"`
	ResultSizeEstimate int                `json:"resultSizeEstimate"`
}

func (g *Gmail) List(ctx context.Context, folderID, cursor string, limit int) ([]MessageMeta, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	q := url.Values{}
	if folderID != "" {
		q.Set("labelIds", folderID)
	}
	q.Set("maxResults", strconv.Itoa(limit))
	if cursor != "" {
		q.Set("pageToken", cursor)
	}
	resp, err := g.do(ctx, "GET", "/users/me/messages?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, "", fmt.Errorf("gmail messages.list %d: %s", resp.StatusCode, string(b))
	}
	var list gmailMessagesList
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, "", err
	}
	if len(list.Messages) == 0 {
		return nil, list.NextPageToken, nil
	}

	// Fetch each message's metadata in parallel. Cap concurrency at 8.
	out := make([]MessageMeta, len(list.Messages))
	errs := make([]error, len(list.Messages))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, stub := range list.Messages {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m, err := g.getMeta(ctx, id)
			if err != nil {
				errs[i] = err
				return
			}
			out[i] = m
		}(i, stub.ID)
	}
	wg.Wait()
	for _, e := range errs {
		if e != nil {
			return nil, "", e
		}
	}
	return out, list.NextPageToken, nil
}

// gmailMessage is the relevant slice of the Gmail messages.get response.
type gmailMessage struct {
	ID           string       `json:"id"`
	ThreadID     string       `json:"threadId"`
	LabelIDs     []string     `json:"labelIds"`
	Snippet      string       `json:"snippet"`
	InternalDate string       `json:"internalDate"` // ms since epoch, as string
	Payload      gmailPayload `json:"payload"`
	SizeEstimate int          `json:"sizeEstimate"`
}

type gmailPayload struct {
	PartID   string         `json:"partId"`
	MimeType string         `json:"mimeType"`
	Filename string         `json:"filename"`
	Headers  []gmailHeader  `json:"headers"`
	Body     gmailBody      `json:"body"`
	Parts    []gmailPayload `json:"parts"`
}

type gmailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type gmailBody struct {
	Size         int    `json:"size"`
	Data         string `json:"data"`
	AttachmentID string `json:"attachmentId"`
}

// gmailMetadataHeaders is the header set requested on the list path.
//
// A set rather than a sequence of q.Add calls so it can be asserted on: toMeta
// can only parse what this asked Gmail to return, and those two live in
// different functions. A correct parser paired with an unrequested header
// produces empty values and no error anywhere, which is a silent failure.
//
// The threading three ride along in the SAME messages.get call that already
// happens per message, so they cost nothing beyond a slightly larger response.
var gmailMetadataHeaders = map[string]bool{
	"Subject": true, "From": true, "To": true, "Cc": true, "Date": true,
	"Message-ID": true, "References": true, "In-Reply-To": true,
}

func (g *Gmail) getMeta(ctx context.Context, id string) (MessageMeta, error) {
	q := url.Values{}
	q.Set("format", "metadata")
	for h := range gmailMetadataHeaders {
		q.Add("metadataHeaders", h)
	}
	resp, err := g.do(ctx, "GET", "/users/me/messages/"+url.PathEscape(id)+"?"+q.Encode(), nil)
	if err != nil {
		return MessageMeta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return MessageMeta{}, fmt.Errorf("gmail messages.get(%s) %d: %s", id, resp.StatusCode, string(b))
	}
	var m gmailMessage
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return MessageMeta{}, err
	}
	return g.toMeta(&m), nil
}

func (g *Gmail) toMeta(m *gmailMessage) MessageMeta {
	meta := MessageMeta{
		ID:        m.ID,
		AccountID: g.accountID,
		Snippet:   m.Snippet,
		// Present on every message resource, including format=metadata, so this
		// is free on the list path — it was simply never read.
		ThreadID: m.ThreadID,
	}
	for _, h := range m.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "subject":
			meta.Subject = h.Value
		case "from":
			if a, err := mail.ParseAddress(h.Value); err == nil {
				meta.From = Address{Name: a.Name, Email: a.Address}
			} else {
				meta.From = Address{Email: h.Value}
			}
		case "to":
			meta.To = parseAddressList(h.Value)
		case "cc":
			meta.Cc = parseAddressList(h.Value)
		case "date":
			if t, err := mail.ParseDate(h.Value); err == nil {
				meta.Date = t
			}
		case "message-id":
			meta.MessageID = StripAngleBrackets(h.Value)
		case "in-reply-to":
			meta.InReplyTo = StripAngleBrackets(h.Value)
		case "references":
			meta.References = SplitReferences(h.Value)
		}
	}
	if meta.Date.IsZero() && m.InternalDate != "" {
		if ms, err := strconv.ParseInt(m.InternalDate, 10, 64); err == nil {
			meta.Date = time.UnixMilli(ms)
		}
	}
	for _, lid := range m.LabelIDs {
		if lid == "UNREAD" {
			meta.Unread = true
		}
		if lid == "STARRED" {
			meta.Flagged = true
		}
	}
	// Gmail's messages.list is scoped to a single label; set the folder to
	// the first non-category label we see, else INBOX.
	for _, lid := range m.LabelIDs {
		if strings.HasPrefix(lid, "CATEGORY_") {
			continue
		}
		meta.FolderID = lid
		break
	}
	return meta
}

func parseAddressList(raw string) []Address {
	if raw == "" {
		return nil
	}
	parsed, err := mail.ParseAddressList(raw)
	if err != nil {
		return []Address{{Email: raw}}
	}
	out := make([]Address, 0, len(parsed))
	for _, a := range parsed {
		out = append(out, Address{Name: a.Name, Email: a.Address})
	}
	return out
}

func (g *Gmail) Get(ctx context.Context, id string) (Message, error) {
	q := url.Values{}
	q.Set("format", "full")
	resp, err := g.do(ctx, "GET", "/users/me/messages/"+url.PathEscape(id)+"?"+q.Encode(), nil)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return Message{}, fmt.Errorf("gmail messages.get(%s) %d: %s", id, resp.StatusCode, string(b))
	}
	var m gmailMessage
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return Message{}, err
	}

	out := Message{
		Meta:    g.toMeta(&m),
		Headers: map[string]string{},
	}
	for _, h := range m.Payload.Headers {
		out.Headers[h.Name] = h.Value
	}
	out.InReplyTo = out.Headers["In-Reply-To"]

	if err := walkParts(&m.Payload, &out); err != nil {
		return Message{}, fmt.Errorf("gmail messages.get(%s): %w", id, err)
	}
	return out, nil
}

// decodeGmailBodyData decodes the base64url payload Gmail puts in a part's
// body. Gmail documents the field as base64url and does not promise padding,
// so both spellings are accepted; anything else is a real error rather than an
// empty message body.
func decodeGmailBodyData(data string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(data, "="))
}

// walkParts flattens a Gmail payload tree into the message's body and
// attachment list. A part that fails to decode is reported: it used to be
// dropped, which rendered as a message with an empty body — indistinguishable
// from a message that genuinely has one.
func walkParts(p *gmailPayload, out *Message) error {
	switch {
	case strings.HasPrefix(p.MimeType, "multipart/"):
		for i := range p.Parts {
			if err := walkParts(&p.Parts[i], out); err != nil {
				return err
			}
		}
	case p.MimeType == "text/html":
		data, err := decodeGmailBodyData(p.Body.Data)
		if err != nil {
			return fmt.Errorf("decoding the text/html part: %w", err)
		}
		out.BodyHTML = string(data)
	case p.MimeType == "text/plain":
		data, err := decodeGmailBodyData(p.Body.Data)
		if err != nil {
			return fmt.Errorf("decoding the text/plain part: %w", err)
		}
		out.BodyText = string(data)
	case p.Filename != "" && p.Body.AttachmentID != "":
		att := Attachment{
			ID:       p.Body.AttachmentID,
			Filename: p.Filename,
			MimeType: p.MimeType,
			Size:     int64(p.Body.Size),
		}
		for _, h := range p.Headers {
			if strings.EqualFold(h.Name, "Content-ID") {
				att.InlineCID = strings.Trim(h.Value, "<>")
			}
		}
		out.Attachments = append(out.Attachments, att)
		out.Meta.HasAttach = true
	}
	return nil
}

var errNotImplemented = errors.New("not implemented yet")

type gmailModifyRequest struct {
	AddLabelIDs    []string `json:"addLabelIds,omitempty"`
	RemoveLabelIDs []string `json:"removeLabelIds,omitempty"`
}

func (g *Gmail) modify(ctx context.Context, id string, req gmailModifyRequest) error {
	raw, _ := json.Marshal(req)
	resp, err := g.do(ctx, "POST", "/users/me/messages/"+url.PathEscape(id)+"/modify", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gmail modify(%s) %d: %s", id, resp.StatusCode, string(b))
	}
	return nil
}

func (g *Gmail) MarkRead(ctx context.Context, id string, read bool) error {
	if read {
		return g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"UNREAD"}})
	}
	return g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"UNREAD"}})
}

func (g *Gmail) Flag(ctx context.Context, id string, flagged bool) error {
	if flagged {
		return g.modify(ctx, id, gmailModifyRequest{AddLabelIDs: []string{"STARRED"}})
	}
	return g.modify(ctx, id, gmailModifyRequest{RemoveLabelIDs: []string{"STARRED"}})
}

// Move semantics for Gmail: add the target label and (when target != INBOX)
// remove INBOX so the message disappears from the inbox view. This matches
// the behavior most users expect from "move to folder X" in mail UIs.
func (g *Gmail) Move(ctx context.Context, id, folderID string) error {
	if folderID == "" {
		return fmt.Errorf("gmail Move: folder is required")
	}
	req := gmailModifyRequest{AddLabelIDs: []string{folderID}}
	if folderID != "INBOX" {
		req.RemoveLabelIDs = []string{"INBOX"}
	}
	return g.modify(ctx, id, req)
}

func (g *Gmail) Delete(ctx context.Context, id string) error {
	resp, err := g.do(ctx, "POST", "/users/me/messages/"+url.PathEscape(id)+"/trash", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gmail trash(%s) %d: %s", id, resp.StatusCode, string(b))
	}
	return nil
}

func (g *Gmail) PermDelete(ctx context.Context, id string) error {
	resp, err := g.do(ctx, "DELETE", "/users/me/messages/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("gmail delete(%s) %d: %s", id, resp.StatusCode, string(b))
	}
	return nil
}

func (g *Gmail) Send(ctx context.Context, d Draft) (string, error) { return "", errNotImplemented }
func (g *Gmail) SaveDraft(ctx context.Context, d Draft, existingID string) (string, error) {
	return "", errNotImplemented
}
func (g *Gmail) Attachment(ctx context.Context, msgID, attID string) (io.ReadCloser, string, error) {
	return nil, "", errNotImplemented
}
