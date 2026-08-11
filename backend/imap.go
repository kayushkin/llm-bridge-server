package backend

import (
	"context"
	"fmt"
	"io"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// FolderMapStore caches SPECIAL-USE resolutions so we don't reprobe per request.
type FolderMapStore interface {
	GetFolderMap(accountID string) (map[string]string, error)
	SaveFolderMap(accountID string, m map[string]string) error
}

// ImapConfig is the minimal info needed to connect to an IMAP account.
type ImapConfig struct {
	Address  string
	ImapHost string
	ImapPort int
	Username string
	Password string
	// "tls" (implicit TLS on 993) or "starttls" (plain + STARTTLS upgrade).
	TLSMode string
}

// Imap implements Backend using IMAP4rev1/rev2 via emersion/go-imap/v2.
// One persistent connection per account; serialized via mu.
type Imap struct {
	accountID string
	cfg       ImapConfig
	store     FolderMapStore

	mu        sync.Mutex
	client    *imapclient.Client
	selected  string            // currently selected mailbox, if any
	canonical map[string]string // canonical kind → mailbox name
}

func NewImap(accountID string, cfg ImapConfig, store FolderMapStore) *Imap {
	return &Imap{accountID: accountID, cfg: cfg, store: store}
}

// Close disconnects the IMAP client if connected.
func (b *Imap) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.client == nil {
		return nil
	}
	err := b.client.Logout().Wait()
	b.client.Close()
	b.client = nil
	b.selected = ""
	return err
}

// dial opens a new IMAP connection and logs in. Caller must hold b.mu.
func (b *Imap) dial() error {
	addr := fmt.Sprintf("%s:%d", b.cfg.ImapHost, b.cfg.ImapPort)
	var c *imapclient.Client
	var err error
	switch strings.ToLower(b.cfg.TLSMode) {
	case "starttls":
		c, err = imapclient.DialStartTLS(addr, nil)
	case "tls", "":
		c, err = imapclient.DialTLS(addr, nil)
	default:
		return fmt.Errorf("unknown tls_mode %q (want tls|starttls)", b.cfg.TLSMode)
	}
	if err != nil {
		return fmt.Errorf("imap dial %s: %w", addr, err)
	}
	if err := c.Login(b.cfg.Username, b.cfg.Password).Wait(); err != nil {
		c.Close()
		return fmt.Errorf("imap login: %w", err)
	}
	b.client = c
	b.selected = ""
	return nil
}

// ensureClient returns the live IMAP client, redialing if needed. Caller must hold b.mu.
func (b *Imap) ensureClient() (*imapclient.Client, error) {
	if b.client != nil {
		// Ping with Noop; if the server dropped us, redial.
		if err := b.client.Noop().Wait(); err == nil {
			return b.client, nil
		}
		b.client.Close()
		b.client = nil
		b.selected = ""
	}
	if err := b.dial(); err != nil {
		return nil, err
	}
	return b.client, nil
}

func (b *Imap) selectMailbox(c *imapclient.Client, mailbox string) error {
	if b.selected == mailbox {
		return nil
	}
	if _, err := c.Select(mailbox, nil).Wait(); err != nil {
		return fmt.Errorf("imap select %q: %w", mailbox, err)
	}
	b.selected = mailbox
	return nil
}

// ─── Folders ──────────────────────────────────────────────────────────

var imapAttrCanonical = map[imap.MailboxAttr]CanonicalKind{
	imap.MailboxAttrDrafts:  KindDrafts,
	imap.MailboxAttrSent:    KindSent,
	imap.MailboxAttrTrash:   KindTrash,
	imap.MailboxAttrJunk:    KindSpam,
	imap.MailboxAttrArchive: KindArchive,
}

func (b *Imap) Folders(ctx context.Context) ([]Folder, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.ensureClient()
	if err != nil {
		return nil, err
	}
	data, err := c.List("", "*", &imap.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return nil, fmt.Errorf("imap list: %w", err)
	}

	canonical := map[string]string{}
	out := make([]Folder, 0, len(data))
	for _, d := range data {
		// Skip non-selectable containers.
		skip := false
		for _, a := range d.Attrs {
			if a == imap.MailboxAttrNoSelect || a == imap.MailboxAttrNonExistent {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		f := Folder{
			ID:        d.Mailbox,
			AccountID: b.accountID,
			Name:      leafName(d.Mailbox, d.Delim),
			Path:      d.Mailbox,
		}
		// INBOX is canonical inbox by convention (not a SPECIAL-USE attr).
		if strings.EqualFold(d.Mailbox, "INBOX") {
			f.Canonical = KindInbox
			canonical[string(KindInbox)] = d.Mailbox
		}
		for _, a := range d.Attrs {
			if k, ok := imapAttrCanonical[a]; ok {
				f.Canonical = k
				canonical[string(k)] = d.Mailbox
			}
		}
		out = append(out, f)
	}

	if b.store != nil {
		// Persist whatever we resolved. If SPECIAL-USE is missing on this
		// server the map may be sparse; surfacing that to callers is a phase-4
		// UX concern. Per the no-fallback-chains directive we do not guess.
		_ = b.store.SaveFolderMap(b.accountID, canonical)
	}
	b.canonical = canonical
	return out, nil
}

func leafName(path string, delim rune) string {
	if delim == 0 {
		return path
	}
	if i := strings.LastIndex(path, string(delim)); i >= 0 {
		return path[i+1:]
	}
	return path
}

// ─── List ─────────────────────────────────────────────────────────────

func (b *Imap) List(ctx context.Context, folderID, cursor string, limit int) ([]MessageMeta, string, error) {
	if folderID == "" {
		folderID = "INBOX"
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.ensureClient()
	if err != nil {
		return nil, "", err
	}
	mbox, err := c.Select(folderID, nil).Wait()
	if err != nil {
		return nil, "", fmt.Errorf("imap select %q: %w", folderID, err)
	}
	b.selected = folderID

	total := int(mbox.NumMessages)
	if total == 0 {
		return nil, "", nil
	}

	// Cursor = the highest seqnum already returned. We walk backwards through
	// the mailbox (newest first), which matches seqnum=N → seqnum=1 ordering.
	end := total
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor %q", cursor)
		}
		end = n - 1
	}
	if end < 1 {
		return nil, "", nil
	}
	start := end - limit + 1
	if start < 1 {
		start = 1
	}

	seqSet := imap.SeqSet{}
	seqSet.AddRange(uint32(start), uint32(end))
	fetchOpts := &imap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
		// References is the one threading header IMAP's ENVELOPE does not carry
		// — it holds Message-ID and In-Reply-To but not the ancestor chain, and
		// the chain is what identifies a conversation rather than just a parent.
		//
		// This asks for one header rather than the message: a HEADER.FIELDS
		// section is a few dozen bytes per message against a body that is
		// routinely tens of kilobytes.
		BodySection: []*imap.FetchItemBodySection{{
			Specifier:    imap.PartSpecifierHeader,
			HeaderFields: []string{"References"},
		}},
	}
	msgs, err := c.Fetch(seqSet, fetchOpts).Collect()
	if err != nil {
		return nil, "", fmt.Errorf("imap fetch: %w", err)
	}
	// Fetch returns ascending by seqnum — reverse for newest-first ordering.
	out := make([]MessageMeta, 0, len(msgs))
	for i := len(msgs) - 1; i >= 0; i-- {
		out = append(out, b.toMeta(folderID, msgs[i]))
	}
	var next string
	if start > 1 {
		next = strconv.Itoa(start)
	}
	return out, next, nil
}

func (b *Imap) toMeta(folderID string, m *imapclient.FetchMessageBuffer) MessageMeta {
	meta := MessageMeta{
		ID:        strconv.FormatUint(uint64(m.UID), 10),
		AccountID: b.accountID,
		FolderID:  folderID,
		Date:      m.InternalDate,
	}
	if m.Envelope != nil {
		meta.Subject = m.Envelope.Subject
		// Already on the wire: List fetches Envelope, which carries both of
		// these. They were simply never read. IMAP has no provider thread id, so
		// these are the only threading signal this backend can offer.
		meta.MessageID = StripAngleBrackets(m.Envelope.MessageID)
		// go-imap models In-Reply-To as a list, because the header may name more
		// than one parent. InReplyTo keeps the first (the direct parent, by
		// convention) and the whole list is kept as the ancestor chain, so
		// nothing is dropped just because our field is singular.
		for _, ref := range m.Envelope.InReplyTo {
			if id := StripAngleBrackets(ref); id != "" && meta.InReplyTo == "" {
				meta.InReplyTo = id
			}
		}
		if len(m.Envelope.From) > 0 {
			a := m.Envelope.From[0]
			meta.From = Address{Name: a.Name, Email: a.Addr()}
		}
		for _, a := range m.Envelope.To {
			meta.To = append(meta.To, Address{Name: a.Name, Email: a.Addr()})
		}
		for _, a := range m.Envelope.Cc {
			meta.Cc = append(meta.Cc, Address{Name: a.Name, Email: a.Addr()})
		}
		if !m.Envelope.Date.IsZero() {
			meta.Date = m.Envelope.Date
		}
	}
	for _, f := range m.Flags {
		switch f {
		case imap.FlagSeen:
			// present = read
		case imap.FlagFlagged:
			meta.Flagged = true
		}
	}
	meta.Unread = !containsFlag(m.Flags, imap.FlagSeen)

	// The References header, when the caller asked for it. Preferred over
	// In-Reply-To because it is the whole chain; In-Reply-To is the fallback so
	// a message whose server omitted References still reports its one ancestor.
	for _, body := range m.BodySection {
		if refs := parseReferencesHeader(string(body.Bytes)); len(refs) > 0 {
			meta.References = refs
			break
		}
	}
	if len(meta.References) == 0 && meta.InReplyTo != "" {
		meta.References = []string{meta.InReplyTo}
	}
	return meta
}

// parseReferencesHeader pulls the ids out of a raw HEADER.FIELDS (REFERENCES)
// response, which arrives as header lines rather than a bare value and may be
// folded across continuation lines.
func parseReferencesHeader(raw string) []string {
	var value strings.Builder
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimRight(line, "\r")
		if trimmed == "" {
			continue
		}
		if lower := strings.ToLower(trimmed); strings.HasPrefix(lower, "references:") {
			value.WriteString(trimmed[len("references:"):])
			continue
		}
		// A folded continuation line starts with whitespace and belongs to the
		// header above it.
		if value.Len() > 0 && (strings.HasPrefix(trimmed, " ") || strings.HasPrefix(trimmed, "\t")) {
			value.WriteString(" ")
			value.WriteString(trimmed)
		}
	}
	return SplitReferences(value.String())
}

func containsFlag(flags []imap.Flag, want imap.Flag) bool {
	for _, f := range flags {
		if f == want {
			return true
		}
	}
	return false
}

// ─── Get ─────────────────────────────────────────────────────────────

func (b *Imap) Get(ctx context.Context, id string) (Message, error) {
	uidN, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		return Message{}, fmt.Errorf("imap Get: id must be UID, got %q", id)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.ensureClient()
	if err != nil {
		return Message{}, err
	}
	// Caller is expected to have run List or Folders recently so a mailbox
	// is selected; if not, default to INBOX.
	if b.selected == "" {
		if _, err := c.Select("INBOX", nil).Wait(); err != nil {
			return Message{}, fmt.Errorf("imap select INBOX: %w", err)
		}
		b.selected = "INBOX"
	}

	uidSet := imap.UIDSetNum(imap.UID(uidN))
	fetchOpts := &imap.FetchOptions{
		Envelope:     true,
		Flags:        true,
		InternalDate: true,
		UID:          true,
		BodySection:  []*imap.FetchItemBodySection{{}},
	}
	msgs, err := c.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		return Message{}, fmt.Errorf("imap fetch uid %d: %w", uidN, err)
	}
	if len(msgs) == 0 {
		return Message{}, fmt.Errorf("imap message uid %d not found", uidN)
	}
	m := msgs[0]

	out := Message{
		Meta:    b.toMeta(b.selected, m),
		Headers: map[string]string{},
	}

	// Parse the raw RFC822 via net/mail for headers and body. Multipart body
	// parsing and attachment extraction is deferred to a later pass — v1
	// shows plaintext or raw content and lists attachments separately.
	for _, bs := range m.BodySection {
		raw := bs.Bytes
		if len(raw) == 0 {
			continue
		}
		msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
		if err != nil {
			out.BodyText = string(raw)
			continue
		}
		for k, vs := range msg.Header {
			if len(vs) > 0 {
				out.Headers[k] = vs[0]
			}
		}
		out.InReplyTo = msg.Header.Get("In-Reply-To")
		body, _ := io.ReadAll(msg.Body)
		ct := msg.Header.Get("Content-Type")
		switch {
		case strings.HasPrefix(strings.ToLower(ct), "text/html"):
			out.BodyHTML = string(body)
		case strings.HasPrefix(strings.ToLower(ct), "text/plain"), ct == "":
			out.BodyText = string(body)
		default:
			out.BodyText = string(body)
		}
	}

	return out, nil
}

// ─── Write stubs ──────────────────────────────────────────────────────

func (b *Imap) MarkRead(ctx context.Context, id string, read bool) error { return errNotImplemented }
func (b *Imap) Flag(ctx context.Context, id string, flagged bool) error  { return errNotImplemented }
func (b *Imap) Move(ctx context.Context, id, folderID string) error      { return errNotImplemented }
func (b *Imap) Delete(ctx context.Context, id string) error              { return errNotImplemented }
func (b *Imap) PermDelete(ctx context.Context, id string) error          { return errNotImplemented }
func (b *Imap) Send(ctx context.Context, d Draft) (string, error)        { return "", errNotImplemented }
func (b *Imap) SaveDraft(ctx context.Context, d Draft, existingID string) (string, error) {
	return "", errNotImplemented
}
func (b *Imap) Attachment(ctx context.Context, msgID, attID string) (io.ReadCloser, string, error) {
	return nil, "", errNotImplemented
}

// time import used for godoc references in types above.
var _ = time.Time{}
