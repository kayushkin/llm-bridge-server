package accounts

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/kayushkin/llm-bridge-server/internal/mail/mailstack/mailmodule"
)

// heldText is text as Postgres can hold it; see mailmodule.TextPostgresCanHold.
func heldText(text string) string {
	held, _ := mailmodule.TextPostgresCanHold(text)
	return held
}

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
	// A provider can hand back a body in a charset nobody declared; Postgres
	// holds only UTF-8, and the JSON served has always shown such bytes as
	// U+FFFD.
	for _, field := range []*string{&b.AccountID, &b.FolderScope, &b.ProviderID, &b.MessageID, &b.Subject,
		&b.FromName, &b.FromEmail, &b.BodyText, &b.BodyHTML, &b.SearchText} {
		*field = heldText(*field)
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

// Search ranks with BM25 as SQLite's FTS5 bm25() did, computed in SQL from
// message_body_words, the module's table of each recorded body's words and
// their weighted counts (see the migration that makes it).
//
// For each query term: its rarity is ln((N - n + 0.5) / (n + 0.5)), at least
// 1e-6, where N is every recorded body and n those holding the term; its
// frequency in a row is the row's hits of it, each weighted by the field it is
// in; and it adds rarity × f × (k1 + 1) / (f + k1 × (1 - b + b × D / avgD)),
// where D is the row's word count and avgD the average. k1 is 1.2 and b 0.75,
// FTS5's own. The score handed back is the sum negated, so it is negative and
// lower is better, as bm25() was, and callers that turn it into a goodness by
// negating it keep working. A term the parser splits into several words
// counts as held when all of them are, as often as the least frequent; FTS5
// read such a term as a phrase, in order.
//
// The field weights rank subject and sender matches above body matches. A
// shared subject term is a far stronger signal of "same kind of mail" than the
// same term buried in a footer, and the sender address is the single most
// predictive field for how mail gets filed — measured on this host's own email
// board, sender alone predicted the filed card for 93% of 846 linked messages.
// They are the FTS5 column weights: subject 8, sender address 6, sender name
// 2, body text 1.

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
	return s.runRankedSearch(terms, true, accountID, "", "", limit)
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
	return s.runRankedSearch(terms, false, "", excludeAccountID, excludeProviderID, limit)
}

// runRankedSearch finds the rows holding every term (everyTerm) or any of
// them, ranks them by BM25 and answers the best limit of them. Every term is
// a run of letters and digits (tokenizeForSearch), so the joined text the
// snippet is drawn with is a valid tsquery with no operator the caller wrote.
func (s *Store) runRankedSearch(terms []string, everyTerm bool, accountID, excludeAccountID, excludeProviderID string, limit int) ([]SearchHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	operator := " | "
	if everyTerm {
		operator = " & "
	}
	query := strings.Join(terms, operator)
	args := []any{terms, everyTerm}
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
	rows, err := s.db.Query(rankedSearchStatement(filters), args...)
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

// rankedSearchStatement is the ranking query, with filters (each " AND …",
// its values after the query text in the arguments) narrowing the rows.
func rankedSearchStatement(filters string) string {
	return `
		WITH query_terms AS (
			SELECT term.position, tsvector_to_array(to_tsvector('mail_search', term.word)) AS words
			FROM unnest(?::text[]) WITH ORDINALITY AS term(word, position)
		),
		query_words AS (
			SELECT DISTINCT query_terms.position, cardinality(query_terms.words) AS word_count, word
			FROM query_terms CROSS JOIN LATERAL unnest(query_terms.words) AS word
		),
		corpus AS (
			SELECT count(*)::double precision AS documents,
			       coalesce(avg(search_token_count), 0)::double precision AS average_tokens
			FROM message_bodies
		),
		term_frequencies AS (
			SELECT held.body_id, query_words.position,
			       CASE WHEN count(*) = max(query_words.word_count) THEN min(held.weighted_hits) ELSE 0 END::double precision AS weighted_hits
			FROM message_body_words held JOIN query_words ON query_words.word = held.word
			GROUP BY held.body_id, query_words.position
		),
		term_rarity AS (
			SELECT holding.position,
			       greatest(ln((corpus.documents - holding.documents + 0.5) / (holding.documents + 0.5)), 1e-6) AS rarity
			FROM (SELECT position, count(*)::double precision AS documents FROM term_frequencies
			      WHERE weighted_hits > 0 GROUP BY position) AS holding
			CROSS JOIN corpus
		),
		matched AS (
			SELECT body_id, count(*) AS terms_held FROM term_frequencies WHERE weighted_hits > 0 GROUP BY body_id
		),
		candidates AS (
			SELECT b.id, b.search_token_count FROM message_bodies b JOIN matched ON matched.body_id = b.id
			WHERE matched.terms_held >= CASE WHEN ?::boolean
			      THEN (SELECT count(*) FROM query_terms WHERE cardinality(words) > 0) ELSE 1 END` + filters + `
		),
		scores AS (
			SELECT candidates.id,
			       sum(term_rarity.rarity * (term_frequencies.weighted_hits * 2.2)
			           / (term_frequencies.weighted_hits + 1.2 * (0.25 + 0.75 * candidates.search_token_count / corpus.average_tokens))) AS goodness
			FROM candidates
			JOIN term_frequencies ON term_frequencies.body_id = candidates.id AND term_frequencies.weighted_hits > 0
			JOIN term_rarity ON term_rarity.position = term_frequencies.position
			CROSS JOIN corpus
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
	`
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
