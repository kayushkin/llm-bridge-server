package accounts

import (
	"encoding/json"
	"time"
)

// ============================ message index ============================
//
// Metadata for messages mailstack has seen. This is an INDEX, not a cache of
// mail: it answers questions about identity and relationships that the provider
// APIs cannot answer cheaply or at all — "what is this Message-ID's locator",
// "what else is in this conversation" — while message content still comes live
// from the provider on every read.
//
// It is populated opportunistically as reads happen rather than by a sync job.
// A message nobody has ever listed or opened is simply not in here, and callers
// must treat a miss as "not seen yet", never as "does not exist".

// IndexedMessage is one row of the index.
type IndexedMessage struct {
	AccountID   string   `json:"account_id"`
	FolderScope string   `json:"folder_scope"`
	ProviderID  string   `json:"provider_id"`
	MessageID   string   `json:"message_id,omitempty"`
	ThreadID    string   `json:"thread_id,omitempty"`
	RootID      string   `json:"root_message_id,omitempty"`
	InReplyTo   string   `json:"in_reply_to,omitempty"`
	References  []string `json:"references,omitempty"`
	Subject     string   `json:"subject"`
	FromName    string   `json:"from_name,omitempty"`
	FromEmail   string   `json:"from_email,omitempty"`
	DateMs      int64    `json:"date_ms"`
	FolderID    string   `json:"folder_id,omitempty"`
	FirstSeenMs int64    `json:"first_seen_ms"`
	LastSeenMs  int64    `json:"last_seen_ms"`
}

// Locator is the addressable reference to a message: the only pair mailstack
// can turn back into a real message. Returned by lookups keyed on things that
// are not addressable, such as an RFC 5322 Message-ID.
type Locator struct {
	AccountID  string `json:"account_id"`
	ProviderID string `json:"provider_id"`
	FolderID   string `json:"folder_id,omitempty"`
}

// UpsertMessages records what a read just saw, in one transaction.
//
// first_seen_ms is preserved across upserts so the index can report when a
// message was first observed, while last_seen_ms always advances. Fields that
// arrive empty do NOT overwrite a non-empty stored value: the list path carries
// less than the get path (no References on IMAP, for one), and letting a
// thinner later read blank out a richer earlier one would lose real data.
func (s *Store) UpsertMessages(msgs []IndexedMessage) error {
	if len(msgs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO messages (
			account_id, folder_scope, provider_id,
			message_id, thread_id, root_message_id, in_reply_to, references_ids,
			subject, from_name, from_email, date_ms, folder_id, uid_validity,
			first_seen_ms, last_seen_ms
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, folder_scope, provider_id) DO UPDATE SET
			message_id      = CASE WHEN excluded.message_id      != '' THEN excluded.message_id      ELSE messages.message_id END,
			thread_id       = CASE WHEN excluded.thread_id       != '' THEN excluded.thread_id       ELSE messages.thread_id END,
			root_message_id = CASE WHEN excluded.root_message_id != '' THEN excluded.root_message_id ELSE messages.root_message_id END,
			in_reply_to     = CASE WHEN excluded.in_reply_to     != '' THEN excluded.in_reply_to     ELSE messages.in_reply_to END,
			references_ids  = CASE WHEN excluded.references_ids  != '[]' THEN excluded.references_ids ELSE messages.references_ids END,
			subject         = CASE WHEN excluded.subject         != '' THEN excluded.subject         ELSE messages.subject END,
			from_name       = CASE WHEN excluded.from_name       != '' THEN excluded.from_name       ELSE messages.from_name END,
			from_email      = CASE WHEN excluded.from_email      != '' THEN excluded.from_email      ELSE messages.from_email END,
			date_ms         = CASE WHEN excluded.date_ms         != 0  THEN excluded.date_ms         ELSE messages.date_ms END,
			folder_id       = CASE WHEN excluded.folder_id       != '' THEN excluded.folder_id       ELSE messages.folder_id END,
			last_seen_ms    = excluded.last_seen_ms
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().UnixMilli()
	for _, m := range msgs {
		refs, _ := json.Marshal(m.References)
		if m.References == nil {
			refs = []byte("[]")
		}
		if _, err := stmt.Exec(
			m.AccountID, m.FolderScope, m.ProviderID,
			m.MessageID, m.ThreadID, m.RootID, m.InReplyTo, string(refs),
			m.Subject, m.FromName, m.FromEmail, m.DateMs, m.FolderID, 0,
			now, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LocatorsForMessageID resolves an RFC 5322 Message-ID to every locator that
// carries it.
//
// Plural on purpose. One message delivered to two accounts is one Message-ID
// and two locators, and collapsing that to a single answer would hide the
// duplicate that callers most need to know about.
//
// This is the lookup mailstack could not do at all before the index: there is
// no provider API for "find the message with this Message-ID".
func (s *Store) LocatorsForMessageID(messageID string) ([]Locator, error) {
	if messageID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT account_id, provider_id, folder_id
		FROM messages WHERE message_id = ?
		ORDER BY account_id, date_ms DESC
	`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Locator
	for rows.Next() {
		var l Locator
		if err := rows.Scan(&l.AccountID, &l.ProviderID, &l.FolderID); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ThreadMessages returns the conversation a message belongs to, newest last.
//
// Two thread notions are consulted because neither alone is sufficient. The
// provider's thread id is authoritative but exists only within one account and
// not at all on IMAP; the root of the References chain is universal and crosses
// accounts and providers. A message is in the thread if it matches either.
func (s *Store) ThreadMessages(accountID, threadID, rootID string) ([]IndexedMessage, error) {
	if threadID == "" && rootID == "" {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT account_id, folder_scope, provider_id, message_id, thread_id,
		       root_message_id, in_reply_to, references_ids, subject,
		       from_name, from_email, date_ms, folder_id, first_seen_ms, last_seen_ms
		FROM messages
		WHERE (? != '' AND account_id = ? AND thread_id = ?)
		   OR (? != '' AND root_message_id = ?)
		ORDER BY date_ms ASC
	`, threadID, accountID, threadID, rootID, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanIndexedMessages(rows)
}

// MessageByLocator returns one indexed row, or nil when the message has not been
// seen. FolderScope is passed because an IMAP UID means nothing without it.
func (s *Store) MessageByLocator(accountID, folderScope, providerID string) (*IndexedMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`
		SELECT account_id, folder_scope, provider_id, message_id, thread_id,
		       root_message_id, in_reply_to, references_ids, subject,
		       from_name, from_email, date_ms, folder_id, first_seen_ms, last_seen_ms
		FROM messages
		WHERE account_id = ? AND folder_scope = ? AND provider_id = ?
	`, accountID, folderScope, providerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanIndexedMessages(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return &list[0], nil
}

// IndexStats reports coverage, so a caller can tell an empty answer caused by
// "not indexed yet" from one caused by "no such message". Without this the
// index's opportunistic population would be invisible.
type IndexStats struct {
	Messages       int   `json:"messages"`
	WithMessageID  int   `json:"with_message_id"`
	WithThreadID   int   `json:"with_thread_id"`
	DistinctThread int   `json:"distinct_threads"`
	Accounts       int   `json:"accounts"`
	OldestDateMs   int64 `json:"oldest_date_ms"`
	NewestDateMs   int64 `json:"newest_date_ms"`
	// BodiesStored counts message_bodies rows: how much of the index has a
	// recorded payload behind it. The gap Messages−BodiesStored is the mail
	// search cannot see yet.
	BodiesStored int `json:"bodies_stored"`
}

func (s *Store) IndexStats() (IndexStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var st IndexStats
	err := s.db.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN message_id != '' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN thread_id  != '' THEN 1 ELSE 0 END), 0),
		       COUNT(DISTINCT CASE WHEN thread_id != '' THEN account_id || ':' || thread_id END),
		       COUNT(DISTINCT account_id),
		       COALESCE(MIN(CASE WHEN date_ms > 0 THEN date_ms END), 0),
		       COALESCE(MAX(date_ms), 0)
		FROM messages
	`).Scan(&st.Messages, &st.WithMessageID, &st.WithThreadID, &st.DistinctThread,
		&st.Accounts, &st.OldestDateMs, &st.NewestDateMs)
	if err != nil {
		return st, err
	}
	err = s.db.QueryRow(`SELECT COUNT(*) FROM message_bodies`).Scan(&st.BodiesStored)
	return st, err
}

func scanIndexedMessages(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]IndexedMessage, error) {
	var out []IndexedMessage
	for rows.Next() {
		var m IndexedMessage
		var refs string
		if err := rows.Scan(&m.AccountID, &m.FolderScope, &m.ProviderID, &m.MessageID,
			&m.ThreadID, &m.RootID, &m.InReplyTo, &refs, &m.Subject,
			&m.FromName, &m.FromEmail, &m.DateMs, &m.FolderID,
			&m.FirstSeenMs, &m.LastSeenMs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(refs), &m.References)
		out = append(out, m)
	}
	return out, rows.Err()
}
