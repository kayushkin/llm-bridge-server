// Package localmail stores synthetic mail that mailstack itself owns.
//
// ⚠️ This package holds message BODIES, which is a deliberate exception to the
// rule stated on the message index in accounts/schema.go: "It stores METADATA
// ONLY and never bodies. mailstack stays a live proxy for content, so an
// indexed subject going slightly stale is cosmetic, while a cached body would
// be both a correctness problem and a storage one."
//
// Both halves of that reasoning are about a *cache*. This is not a cache. For a
// local account there is no upstream provider to be a stale copy of — this
// store IS the provider, and a body held here can no more go stale than a body
// held by Gmail. Refusing to store it would leave the account with no content
// at all.
//
// It lives in its own database file rather than in accounts.db, so that
// synthetic mail can never be confused with the real accounts' metadata and can
// be deleted by removing one file.
package localmail

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kayushkin/mailstack/backend"
	_ "modernc.org/sqlite"
)

// Folders a local account presents. Fixed rather than user-defined: a synthetic
// account exists to be read from, and every consumer that resolves a mailbox
// does it by canonical kind.
//
// The inbox is not optional. A caller that looks up the inbox by canonical kind
// — scheduler's email-classifier does — errors out on an account that has none.
var folderKinds = []struct {
	ID        string
	Name      string
	Canonical backend.CanonicalKind
}{
	{ID: "INBOX", Name: "Inbox", Canonical: backend.KindInbox},
	{ID: "SENT", Name: "Sent", Canonical: backend.KindSent},
	{ID: "ARCHIVE", Name: "Archive", Canonical: backend.KindArchive},
	{ID: "TRASH", Name: "Trash", Canonical: backend.KindTrash},
}

// DefaultFolderID is where an appended message lands when the caller does not
// say.
const DefaultFolderID = "INBOX"

// maxListLimit matches what the real backends clamp to, so a local account
// cannot be used to discover a different pagination contract from the one every
// other account enforces.
const maxListLimit = 500

// ErrNotFound is returned for an unknown message id.
var ErrNotFound = fmt.Errorf("localmail: message not found")

// Store is the synthetic mailbox.
type Store struct {
	mu sync.Mutex
	db *sql.DB
}

// DefaultDBPath is where synthetic mail lives when nothing overrides it.
func DefaultDBPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "mailstack", "local-mail.db")
}

// Open creates or opens the synthetic mailbox.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create local mail dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open local mail db: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate local mail schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("chmod local mail db: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// ============================ writing ============================

// Append stores one message and returns it as the account now holds it.
//
// The caller supplies what a sender would: who it is from, what it says, when
// it was sent, and how it threads. Everything a *provider* would assign is
// assigned here — the opaque id, the Message-ID if the caller left it blank,
// the snippet, and the received timestamp.
//
// The id is account-wide, matching Gmail and Graph rather than IMAP. That is
// what makes the "account_id:message_id" ref every downstream consumer builds
// resolve exactly: an IMAP-style per-folder id would need a third component the
// ref has nowhere to put.
func (s *Store) Append(ctx context.Context, accountID string, m backend.Message) (backend.MessageMeta, error) {
	if accountID == "" {
		return backend.MessageMeta{}, fmt.Errorf("localmail: account id is required")
	}
	if strings.TrimSpace(m.Meta.From.Email) == "" {
		return backend.MessageMeta{}, fmt.Errorf("localmail: from.email is required")
	}
	if strings.TrimSpace(m.BodyText) == "" && strings.TrimSpace(m.BodyHTML) == "" {
		return backend.MessageMeta{}, fmt.Errorf("localmail: a message needs a body")
	}

	meta := m.Meta
	meta.AccountID = accountID
	if meta.FolderID == "" {
		meta.FolderID = DefaultFolderID
	}
	if !knownFolder(meta.FolderID) {
		return backend.MessageMeta{}, fmt.Errorf("localmail: unknown folder %q", meta.FolderID)
	}
	if meta.Date.IsZero() {
		meta.Date = time.Now().UTC()
	}
	if meta.ID == "" {
		meta.ID = newMessageID(accountID, meta)
	}
	if meta.MessageID == "" {
		// RFC 5322 §3.6.4 leaves Message-ID optional, but a synthetic account
		// that omits it loses every threading and dedup path downstream, and
		// those are the paths this account exists to exercise.
		meta.MessageID = fmt.Sprintf("%s@localmail.invalid", meta.ID)
	}
	meta.MessageID = backend.StripAngleBrackets(meta.MessageID)
	meta.InReplyTo = backend.StripAngleBrackets(meta.InReplyTo)
	if meta.ThreadID == "" {
		meta.ThreadID = threadIDFor(meta)
	}
	if meta.Snippet == "" {
		meta.Snippet = snippet(m.BodyText, 200)
	}

	refs, err := json.Marshal(nonNil(meta.References))
	if err != nil {
		return backend.MessageMeta{}, fmt.Errorf("localmail: encode references: %w", err)
	}
	to, err := json.Marshal(nonNilAddrs(meta.To))
	if err != nil {
		return backend.MessageMeta{}, fmt.Errorf("localmail: encode to: %w", err)
	}
	cc, err := json.Marshal(nonNilAddrs(meta.Cc))
	if err != nil {
		return backend.MessageMeta{}, fmt.Errorf("localmail: encode cc: %w", err)
	}

	now := time.Now().UTC().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return backend.MessageMeta{}, fmt.Errorf("localmail: begin append: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `
		INSERT INTO local_messages (account_id, id, folder_id, message_id, thread_id,
			in_reply_to, references_ids, subject, from_name, from_email, to_addrs, cc_addrs,
			date_ms, snippet, text_bytes, html_bytes, unread, flagged, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		accountID, meta.ID, meta.FolderID, meta.MessageID, meta.ThreadID,
		meta.InReplyTo, string(refs), meta.Subject, meta.From.Name, meta.From.Email,
		string(to), string(cc), meta.Date.UTC().UnixMilli(), meta.Snippet,
		byteCountOrNull(m.BodyText), byteCountOrNull(m.BodyHTML),
		boolToInt(meta.Unread), boolToInt(meta.Flagged), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return backend.MessageMeta{}, fmt.Errorf("localmail: message %q already exists in %s", meta.ID, accountID)
		}
		return backend.MessageMeta{}, fmt.Errorf("localmail: insert message: %w", err)
	}
	// An appended body that is blank was not given: Append refuses a message
	// with neither, so a blank one is the representation the sender left out.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO local_message_bodies (account_id, id, body_text, body_html) VALUES (?,?,?,?)`,
		accountID, meta.ID, textOrNull(m.BodyText), textOrNull(m.BodyHTML)); err != nil {
		return backend.MessageMeta{}, fmt.Errorf("localmail: insert body: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return backend.MessageMeta{}, fmt.Errorf("localmail: commit append: %w", err)
	}
	return meta, nil
}

// threadIDFor derives the conversation key the same way mailstack's own
// rootMessageID does: the oldest ancestor if this is a reply, otherwise the
// message itself. Deriving it rather than inventing one keeps a local thread
// answerable by exactly the query that answers a Gmail thread.
func threadIDFor(meta backend.MessageMeta) string {
	if len(meta.References) > 0 {
		return backend.StripAngleBrackets(meta.References[0])
	}
	if meta.InReplyTo != "" {
		return meta.InReplyTo
	}
	return meta.MessageID
}

// newMessageID mints the opaque provider id. It is derived from the account,
// the RFC Message-ID when there is one, and the send time, so appending the
// same message twice collides on the primary key instead of quietly making a
// second copy.
func newMessageID(accountID string, meta backend.MessageMeta) string {
	seed := meta.MessageID
	if seed == "" {
		seed = meta.Subject + "|" + meta.From.Email + "|" + strconv.FormatInt(meta.Date.UnixMilli(), 10)
	}
	sum := fnv64(accountID + "|" + seed)
	return fmt.Sprintf("lm%016x", sum)
}

// fnv64 is FNV-1a. Not cryptographic and not meant to be: this is an id for
// synthetic mail, and the only property required is that the same input gives
// the same id so a re-run collides instead of duplicating.
func fnv64(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	return h
}

// ============================ reading ============================

// Folders reports the fixed folder set with live unread counts.
func (s *Store) Folders(ctx context.Context, accountID string) ([]backend.Folder, error) {
	unread := map[string]int{}
	rows, err := s.db.QueryContext(ctx, `
		SELECT folder_id, COUNT(*) FROM local_messages
		WHERE account_id = ? AND unread = 1 GROUP BY folder_id`, accountID)
	if err != nil {
		return nil, fmt.Errorf("localmail: count unread: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var folder string
		var n int
		if err := rows.Scan(&folder, &n); err != nil {
			return nil, fmt.Errorf("localmail: scan unread count: %w", err)
		}
		unread[folder] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("localmail: read unread counts: %w", err)
	}

	out := make([]backend.Folder, 0, len(folderKinds))
	for _, f := range folderKinds {
		out = append(out, backend.Folder{
			ID: f.ID, AccountID: accountID, Name: f.Name, Path: f.Name,
			Canonical: f.Canonical, UnreadCount: unread[f.ID],
		})
	}
	return out, nil
}

// List returns one page of a folder, newest first.
//
// The cursor is the (date, id) of the last row handed out, encoded opaquely.
// Paging on the sort key rather than an offset means a message appended between
// two pages cannot push a row from the first page onto the second.
func (s *Store) List(ctx context.Context, accountID, folderID, cursor string, limit int) ([]backend.MessageMeta, string, error) {
	if folderID == "" {
		folderID = DefaultFolderID
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}

	args := []any{accountID, folderID}
	where := "account_id = ? AND folder_id = ?"
	if cursor != "" {
		dateMS, id, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		where += " AND (date_ms < ? OR (date_ms = ? AND id < ?))"
		args = append(args, dateMS, dateMS, id)
	}
	// One more than asked for, so "is there a next page" is answered by the data
	// rather than guessed from a full page.
	args = append(args, limit+1)

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, folder_id, message_id, thread_id, in_reply_to, references_ids,
		       subject, from_name, from_email, to_addrs, cc_addrs, date_ms, snippet,
		       unread, flagged
		FROM local_messages WHERE `+where+`
		ORDER BY date_ms DESC, id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, "", fmt.Errorf("localmail: list messages: %w", err)
	}
	defer rows.Close()

	var out []backend.MessageMeta
	for rows.Next() {
		var m backend.MessageMeta
		var refsJSON, toJSON, ccJSON string
		var dateMS int64
		var unread, flagged int
		if err := rows.Scan(&m.ID, &m.FolderID, &m.MessageID, &m.ThreadID, &m.InReplyTo,
			&refsJSON, &m.Subject, &m.From.Name, &m.From.Email, &toJSON, &ccJSON,
			&dateMS, &m.Snippet, &unread, &flagged); err != nil {
			return nil, "", fmt.Errorf("localmail: scan message: %w", err)
		}
		m.AccountID = accountID
		m.Date = time.UnixMilli(dateMS).UTC()
		m.Unread = unread == 1
		m.Flagged = flagged == 1
		if err := decodeJSON(refsJSON, &m.References); err != nil {
			return nil, "", err
		}
		if err := decodeJSON(toJSON, &m.To); err != nil {
			return nil, "", err
		}
		if err := decodeJSON(ccJSON, &m.Cc); err != nil {
			return nil, "", err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("localmail: read messages: %w", err)
	}

	if len(out) > limit {
		out = out[:limit]
		last := out[len(out)-1]
		return out, encodeCursor(last.Date.UnixMilli(), last.ID), nil
	}
	return out, "", nil
}

// Get returns one message with its body. It is the whole-message read every
// backend answers; Metadata and Body are the split reads only this store has.
func (s *Store) Get(ctx context.Context, accountID, id string) (backend.Message, error) {
	var bodyText, bodyHTML sql.NullString
	stored, err := scanStored(s.db.QueryRowContext(ctx, `
		SELECT `+storedColumns+`, body_text, body_html
		FROM local_messages JOIN local_message_bodies USING (account_id, id)
		WHERE account_id = ? AND id = ?`, accountID, id), accountID, &bodyText, &bodyHTML)
	if err != nil {
		return backend.Message{}, err
	}
	stored, err = s.withAttachments(ctx)(stored, nil)
	if err != nil {
		return backend.Message{}, err
	}
	m := backend.Message{Meta: stored.MessageMeta, BodyText: bodyText.String, BodyHTML: bodyHTML.String}
	for _, a := range stored.Attachments {
		m.Attachments = append(m.Attachments, backend.Attachment{
			ID: a.ID, Filename: a.Filename, MimeType: a.MimeType, Size: a.Size, InlineCID: a.InlineCID})
	}
	m.InReplyTo = m.Meta.InReplyTo
	// The headers a reader of a real message would find. Synthesized from the
	// stored fields rather than kept separately, so the two can never disagree.
	// An imported message may have no Message-ID, and then it has no header.
	m.Headers = map[string]string{
		"Subject": m.Meta.Subject,
		"From":    formatAddress(m.Meta.From),
		"Date":    m.Meta.Date.Format(time.RFC1123Z),
	}
	if m.Meta.MessageID != "" {
		m.Headers["Message-ID"] = "<" + m.Meta.MessageID + ">"
	}
	if m.Meta.InReplyTo != "" {
		m.Headers["In-Reply-To"] = "<" + m.Meta.InReplyTo + ">"
	}
	if len(m.Meta.References) > 0 {
		m.Headers["References"] = "<" + strings.Join(m.Meta.References, "> <") + ">"
	}
	return m, nil
}

// ============================ mutating ============================

func (s *Store) SetUnread(ctx context.Context, accountID, id string, unread bool) error {
	return s.update(ctx, accountID, id, `UPDATE local_messages SET unread = ? WHERE account_id = ? AND id = ?`, boolToInt(unread))
}

func (s *Store) SetFlagged(ctx context.Context, accountID, id string, flagged bool) error {
	return s.update(ctx, accountID, id, `UPDATE local_messages SET flagged = ? WHERE account_id = ? AND id = ?`, boolToInt(flagged))
}

func (s *Store) MoveTo(ctx context.Context, accountID, id, folderID string) error {
	if !knownFolder(folderID) {
		return fmt.Errorf("localmail: unknown folder %q", folderID)
	}
	return s.update(ctx, accountID, id, `UPDATE local_messages SET folder_id = ? WHERE account_id = ? AND id = ?`, folderID)
}

// Delete moves a message to Trash, matching what every real backend means by a
// plain delete. PermDelete is the one that destroys it.
func (s *Store) Delete(ctx context.Context, accountID, id string) error {
	return s.MoveTo(ctx, accountID, id, "TRASH")
}

func (s *Store) PermDelete(ctx context.Context, accountID, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("localmail: begin delete: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM local_messages WHERE account_id = ? AND id = ?`, accountID, id)
	if err != nil {
		return fmt.Errorf("localmail: delete message: %w", err)
	}
	if err := checkAffected(res); err != nil {
		return err
	}
	// Every revision and attachment row goes with the message. Their bytes in
	// file-store are the caller's to purge first: see AttachmentFileIDs.
	for _, table := range []string{"local_message_bodies", "local_message_revisions", "local_message_attachments"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE account_id = ? AND id = ?`, accountID, id); err != nil {
			return fmt.Errorf("localmail: delete from %s: %w", table, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("localmail: commit delete: %w", err)
	}
	return nil
}

func (s *Store) update(ctx context.Context, accountID, id, query string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.ExecContext(ctx, query, value, accountID, id)
	if err != nil {
		return fmt.Errorf("localmail: update message: %w", err)
	}
	return checkAffected(res)
}

// checkAffected turns "updated nothing" into ErrNotFound. Without it every
// mutation on a missing message reports success, which is the failure that
// looks like working software.
func checkAffected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("localmail: rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ============================ helpers ============================

func knownFolder(id string) bool {
	for _, f := range folderKinds {
		if f.ID == id {
			return true
		}
	}
	return false
}

func encodeCursor(dateMS int64, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(dateMS, 10) + ":" + id))
}

// decodeCursor refuses a malformed cursor rather than starting over. A cursor
// read as "begin at the newest" answers page four with page one, and the caller
// sees a mailbox that appears to have changed underneath it.
func decodeCursor(cursor string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, "", fmt.Errorf("localmail: cursor is not valid base64: %w", err)
	}
	parts := strings.SplitN(string(raw), ":", 2)
	if len(parts) != 2 {
		return 0, "", fmt.Errorf("localmail: cursor has %d fields, want 2", len(parts))
	}
	dateMS, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", fmt.Errorf("localmail: cursor date: %w", err)
	}
	return dateMS, parts[1], nil
}

func decodeJSON(raw string, out any) error {
	if raw == "" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("localmail: decode stored JSON %q: %w", raw, err)
	}
	return nil
}

// Snippet is the list preview of a message body.
func Snippet(body string) string { return snippet(body, 200) }

// snippet cuts at a rune boundary: a byte cut through a multi-byte character
// stores invalid UTF-8, and JSON encoding then replaces it with U+FFFD.
func snippet(body string, limit int) string {
	s := strings.Join(strings.Fields(body), " ")
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func formatAddress(a backend.Address) string {
	if a.Name == "" {
		return a.Email
	}
	return fmt.Sprintf("%s <%s>", a.Name, a.Email)
}

// textOrNull stores a blank body as absent; see Append.
func textOrNull(body string) any {
	if body == "" {
		return nil
	}
	return body
}

func byteCountOrNull(body string) any {
	if body == "" {
		return nil
	}
	return len(body)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func nonNilAddrs(v []backend.Address) []backend.Address {
	if v == nil {
		return []backend.Address{}
	}
	return v
}
