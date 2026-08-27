package accounts

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// ============================ message payload record ============================
//
// Bodies of messages mailstack has fetched, recorded so that "which stored mail
// reads like this one" can be answered with BM25 over real text. Populated the
// same way the message index is: opportunistically, as reads happen. A message
// nobody has opened has no body here, and a miss means "not fetched yet", never
// "does not exist".
//
// The read path is untouched: GET /api/messages/{id} still fetches live from
// the provider every time. These rows feed /api/search and /api/search/similar
// and nothing else.

// MessageBody is one recorded payload.
type MessageBody struct {
	AccountID   string `json:"account_id"`
	FolderScope string `json:"folder_scope"`
	ProviderID  string `json:"provider_id"`
	MessageID   string `json:"message_id,omitempty"`
	Subject     string `json:"subject"`
	FromName    string `json:"from_name,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	DateMs      int64  `json:"date_ms"`
	BodyText    string `json:"body_text"`
	BodyHTML    string `json:"body_html"`
	// SearchText is the derived plain-text rendering that the FTS index reads.
	// Callers should build it with SearchableText.
	SearchText string `json:"-"`
	FetchedMs  int64  `json:"fetched_ms"`
}

// SearchHit is one search or similarity result. It carries everything a caller
// needs to name the message and fetch it live: the locator, the RFC 5322
// identity, and the header fields for display.
type SearchHit struct {
	AccountID   string `json:"account_id"`
	FolderScope string `json:"folder_scope,omitempty"`
	ProviderID  string `json:"provider_id"`
	MessageID   string `json:"message_id,omitempty"`
	Subject     string `json:"subject"`
	FromName    string `json:"from_name,omitempty"`
	FromEmail   string `json:"from_email,omitempty"`
	DateMs      int64  `json:"date_ms"`
	// Score is the raw bm25() value: negative, and LOWER means a better match.
	// Passed through unchanged so callers can apply their own recency decay
	// rather than having a freshness policy baked in here.
	Score   float64 `json:"score"`
	Snippet string  `json:"snippet,omitempty"`
}

// UpsertMessageBody records what a fetch just returned. A field that arrives
// empty does not overwrite a non-empty stored value — a later fetch that failed
// to decode a part must not blank out an earlier one that succeeded.
func (s *Store) UpsertMessageBody(b MessageBody) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.FetchedMs == 0 {
		b.FetchedMs = time.Now().UnixMilli()
	}
	_, err := s.db.Exec(`
		INSERT INTO message_bodies (
			account_id, folder_scope, provider_id, message_id,
			subject, from_name, from_email, date_ms,
			body_text, body_html, search_text, fetched_ms
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(account_id, folder_scope, provider_id) DO UPDATE SET
			message_id  = CASE WHEN excluded.message_id  != '' THEN excluded.message_id  ELSE message_bodies.message_id END,
			subject     = CASE WHEN excluded.subject     != '' THEN excluded.subject     ELSE message_bodies.subject END,
			from_name   = CASE WHEN excluded.from_name   != '' THEN excluded.from_name   ELSE message_bodies.from_name END,
			from_email  = CASE WHEN excluded.from_email  != '' THEN excluded.from_email  ELSE message_bodies.from_email END,
			date_ms     = CASE WHEN excluded.date_ms     != 0  THEN excluded.date_ms     ELSE message_bodies.date_ms END,
			body_text   = CASE WHEN excluded.body_text   != '' THEN excluded.body_text   ELSE message_bodies.body_text END,
			body_html   = CASE WHEN excluded.body_html   != '' THEN excluded.body_html   ELSE message_bodies.body_html END,
			search_text = CASE WHEN excluded.search_text != '' THEN excluded.search_text ELSE message_bodies.search_text END,
			fetched_ms  = excluded.fetched_ms
	`,
		b.AccountID, b.FolderScope, b.ProviderID, b.MessageID,
		b.Subject, b.FromName, b.FromEmail, b.DateMs,
		b.BodyText, b.BodyHTML, b.SearchText, b.FetchedMs,
	)
	return err
}

// bm25Weights ranks subject and sender matches above body matches. The column
// order matches the FTS table: subject, from_name, from_email, search_text. A
// shared subject term is a far stronger signal of "same kind of mail" than the
// same term buried in a footer, and the sender address is the single most
// predictive field for how mail gets filed — measured on this host's own email
// board, sender alone predicted the filed card for 93% of 846 linked messages.
const bm25Weights = "bm25(message_bodies_fts, 8.0, 2.0, 6.0, 1.0)"

// SearchMessages runs a full-text search over recorded payloads. The query is
// plain text: it is tokenized here and every term is quoted, so FTS5 operator
// syntax in the input cannot change the query's meaning. All terms must match
// (implicit AND). accountID narrows to one account when non-empty.
func (s *Store) SearchMessages(query, accountID string, limit int) ([]SearchHit, error) {
	match := ftsQuery(tokenizeForSearch(query), " ")
	if match == "" {
		return nil, fmt.Errorf("query %q contains no searchable terms", query)
	}
	return s.runFTSQuery(match, accountID, "", "", limit)
}

// SimilarMessages finds recorded messages that read like the given text — a
// subject and body, typically of a message that is not stored yet. The text is
// reduced to its distinct terms joined with OR, and bm25 ranking does the
// discrimination: terms common across the corpus contribute almost nothing,
// distinctive shared terms dominate. The message identified by
// excludeAccountID/excludeProviderID is left out of the results, so a stored
// message can be asked about without matching itself.
func (s *Store) SimilarMessages(text string, excludeAccountID, excludeProviderID string, limit int) ([]SearchHit, error) {
	terms := tokenizeForSearch(text)
	if len(terms) > maxSimilarityTerms {
		terms = terms[:maxSimilarityTerms]
	}
	match := ftsQuery(terms, " OR ")
	if match == "" {
		return nil, fmt.Errorf("text contains no searchable terms")
	}
	return s.runFTSQuery(match, "", excludeAccountID, excludeProviderID, limit)
}

func (s *Store) runFTSQuery(match, accountID, excludeAccountID, excludeProviderID string, limit int) ([]SearchHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	where := "message_bodies_fts MATCH ?"
	args := []any{match}
	if accountID != "" {
		where += " AND b.account_id = ?"
		args = append(args, accountID)
	}
	if excludeAccountID != "" || excludeProviderID != "" {
		where += " AND NOT (b.account_id = ? AND b.provider_id = ?)"
		args = append(args, excludeAccountID, excludeProviderID)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`
		SELECT b.account_id, b.folder_scope, b.provider_id, b.message_id,
		       b.subject, b.from_name, b.from_email, b.date_ms,
		       `+bm25Weights+`,
		       snippet(message_bodies_fts, 3, '', '', '…', 12)
		FROM message_bodies_fts
		JOIN message_bodies b ON b.id = message_bodies_fts.rowid
		WHERE `+where+`
		ORDER BY `+bm25Weights+`
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SearchHit
	for rows.Next() {
		var h SearchHit
		if err := rows.Scan(&h.AccountID, &h.FolderScope, &h.ProviderID, &h.MessageID,
			&h.Subject, &h.FromName, &h.FromEmail, &h.DateMs, &h.Score, &h.Snippet); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// maxSimilarityTerms caps how many distinct terms a similarity query carries.
// Past this point additional terms are body boilerplate that adds query cost
// and no discrimination — bm25 already down-weights common terms, but every
// term still has to be looked up.
const maxSimilarityTerms = 48

// searchStopwords are terms that appear in nearly every email and would make
// every message "similar" to every other. Deliberately tiny: bm25's own IDF
// handles ordinary common words, this list only removes the handful so
// universal in mail that looking them up is pure waste.
var searchStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "you": true, "your": true,
	"this": true, "that": true, "with": true, "from": true, "have": true,
	"are": true, "not": true, "was": true, "email": true, "unsubscribe": true,
	"click": true, "here": true, "view": true, "http": true, "https": true,
	"www": true, "com": true,
}

// tokenizeForSearch reduces text to its distinct searchable terms, in first-
// appearance order: lowercased runs of letters and digits, at least three
// characters, stopwords removed, duplicates dropped.
func tokenizeForSearch(text string) []string {
	var terms []string
	seen := map[string]bool{}
	var current strings.Builder
	flush := func() {
		if current.Len() == 0 {
			return
		}
		term := current.String()
		current.Reset()
		if len(term) < 3 || searchStopwords[term] || seen[term] {
			return
		}
		seen[term] = true
		terms = append(terms, term)
	}
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			current.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return terms
}

// ftsQuery joins terms into an FTS5 MATCH expression, each term quoted so no
// input can be read as FTS5 operator syntax.
func ftsQuery(terms []string, separator string) string {
	if len(terms) == 0 {
		return ""
	}
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = `"` + t + `"`
	}
	return strings.Join(quoted, separator)
}
