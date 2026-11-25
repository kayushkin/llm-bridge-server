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
	// Score is the BM25 score, negated as FTS5's bm25() was: negative, and
	// LOWER means a better match. Passed through unchanged so callers can
	// apply their own recency decay rather than having a freshness policy
	// baked in here.
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

// Search ranks with BM25 as SQLite's FTS5 bm25() did, computed in SQL over
// the weighted tsvector the module keeps on each row (see the migration that
// makes message_bodies).
//
// For each query term: its rarity is ln((N - n + 0.5) / (n + 0.5)), at least
// 1e-6, where N is every recorded body and n those holding the term; its
// frequency in a row is the row's hits of it, each weighted by the field it is
// in; and it adds rarity × f × (k1 + 1) / (f + k1 × (1 - b + b × D / avgD)),
// where D is the row's word count and avgD the average. k1 is 1.2 and b 0.75,
// FTS5's own. The score handed back is the sum negated, so it is negative and
// lower is better, as bm25() was, and callers that turn it into a goodness by
// negating it keep working. A term that the parser splits into several words
// counts the least frequent of them.
//
// The field weights rank subject and sender matches above body matches. A
// shared subject term is a far stronger signal of "same kind of mail" than the
// same term buried in a footer, and the sender address is the single most
// predictive field for how mail gets filed — measured on this host's own email
// board, sender alone predicted the filed card for 93% of 846 linked messages.
// They are the FTS5 column weights: subject 8 (weight A), sender address 6
// (B), sender name 2 (C), body text 1 (D).
const fieldWeightOfHit = `CASE hit_weight WHEN 'A' THEN 8 WHEN 'B' THEN 6 WHEN 'C' THEN 2 ELSE 1 END`

// SearchMessages runs a full-text search over recorded payloads. The query is
// plain text: it is cut into words here, and only runs of letters and digits
// reach the database, so no operator syntax in the input can change the
// query's meaning. All terms must match (implicit AND). accountID narrows to
// one account when non-empty.
func (s *Store) SearchMessages(query, accountID string, limit int) ([]SearchHit, error) {
	terms := tokenizeForSearch(query)
	if len(terms) == 0 {
		return nil, fmt.Errorf("query %q contains no searchable terms", query)
	}
	return s.runRankedSearch(terms, " & ", accountID, "", "", limit)
}

// SimilarMessages finds recorded messages that read like the given text — a
// subject and body, typically of a message that is not stored yet. The text is
// reduced to its distinct terms joined with OR, and BM25 ranking does the
// discrimination: terms common across the corpus contribute almost nothing,
// distinctive shared terms dominate. The message identified by
// excludeAccountID/excludeProviderID is left out of the results, so a stored
// message can be asked about without matching itself.
func (s *Store) SimilarMessages(text string, excludeAccountID, excludeProviderID string, limit int) ([]SearchHit, error) {
	terms := tokenizeForSearch(text)
	if len(terms) > maxSimilarityTerms {
		terms = terms[:maxSimilarityTerms]
	}
	if len(terms) == 0 {
		return nil, fmt.Errorf("text contains no searchable terms")
	}
	return s.runRankedSearch(terms, " | ", "", excludeAccountID, excludeProviderID, limit)
}

// runRankedSearch finds the rows matching terms joined by operator (" & " or
// " | "), ranks them by BM25 and answers the best limit of them. Every term is
// a run of letters and digits (tokenizeForSearch), so the joined text is a
// valid tsquery with no operator the caller wrote.
func (s *Store) runRankedSearch(terms []string, operator, accountID, excludeAccountID, excludeProviderID string, limit int) ([]SearchHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	query := strings.Join(terms, operator)
	args := []any{terms, query}
	filters := ""
	if accountID != "" {
		filters += " AND b.account_id = ?"
		args = append(args, accountID)
	}
	if excludeAccountID != "" || excludeProviderID != "" {
		filters += " AND NOT (b.account_id = ? AND b.provider_id = ?)"
		args = append(args, excludeAccountID, excludeProviderID)
	}
	args = append(args, limit, query)
	rows, err := s.db.Query(`
		WITH query_terms AS (
			SELECT term.word, tsvector_to_array(to_tsvector('mail_search', term.word)) AS lexemes
			FROM unnest(?::text[]) WITH ORDINALITY AS term(word, position)
		),
		corpus AS (
			SELECT count(*)::double precision AS documents,
			       coalesce(avg(search_token_count), 0)::double precision AS average_tokens
			FROM message_bodies
		),
		term_rarity AS (
			SELECT query_terms.lexemes,
			       greatest(ln((corpus.documents - holding.documents + 0.5) / (holding.documents + 0.5)), 1e-6) AS rarity
			FROM query_terms CROSS JOIN corpus
			CROSS JOIN LATERAL (
				SELECT count(*)::double precision AS documents FROM message_bodies
				WHERE search_document @@ phraseto_tsquery('mail_search', query_terms.word)
			) AS holding
			WHERE cardinality(query_terms.lexemes) > 0
		),
		candidates AS (
			SELECT b.id, b.search_document, b.search_token_count FROM message_bodies b
			WHERE b.search_document @@ to_tsquery('mail_search', ?)`+filters+`
		),
		term_hits AS (
			SELECT candidates.id, entry.lexeme, sum(`+fieldWeightOfHit+`)::double precision AS weighted_hits
			FROM candidates
			CROSS JOIN LATERAL unnest(candidates.search_document) AS entry
			CROSS JOIN LATERAL unnest(entry.weights) AS hit_weight
			WHERE entry.lexeme IN (SELECT unnest(lexemes) FROM query_terms)
			GROUP BY candidates.id, entry.lexeme
		),
		scores AS (
			SELECT candidates.id,
			       sum(term_rarity.rarity * (frequency.weighted_hits * 2.2)
			           / (frequency.weighted_hits + 1.2 * (0.25 + 0.75 * candidates.search_token_count / corpus.average_tokens))) AS goodness
			FROM candidates CROSS JOIN corpus CROSS JOIN term_rarity
			CROSS JOIN LATERAL (
				SELECT coalesce(min(coalesce(term_hits.weighted_hits, 0)), 0) AS weighted_hits
				FROM unnest(term_rarity.lexemes) AS term_lexeme
				LEFT JOIN term_hits ON term_hits.id = candidates.id AND term_hits.lexeme = term_lexeme
			) AS frequency
			GROUP BY candidates.id
		),
		best AS (
			SELECT id, goodness FROM scores ORDER BY goodness DESC, id LIMIT ?
		)
		SELECT b.account_id, b.folder_scope, b.provider_id, b.message_id,
		       b.subject, b.from_name, b.from_email, b.date_ms,
		       -best.goodness,
		       ts_headline('mail_search', b.search_text, to_tsquery('mail_search', ?),
		           'StartSel="", StopSel="", MaxWords=12, MinWords=6, ShortWord=0, MaxFragments=1, FragmentDelimiter="…"')
		FROM best JOIN message_bodies b ON b.id = best.id
		ORDER BY best.goodness DESC, best.id
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
