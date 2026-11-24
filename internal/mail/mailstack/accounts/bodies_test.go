package accounts

import (
	"path/filepath"
	"strings"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustUpsertBody(t *testing.T, s *Store, b MessageBody) {
	t.Helper()
	if b.SearchText == "" {
		b.SearchText = SearchableText(b.BodyText, b.BodyHTML)
	}
	if err := s.UpsertMessageBody(b); err != nil {
		t.Fatalf("upsert body %s:%s: %v", b.AccountID, b.ProviderID, err)
	}
}

func TestSearchFindsAMessageByABodyTermAndReturnsItsLocator(t *testing.T) {
	s := openTestStore(t)
	mustUpsertBody(t, s, MessageBody{
		AccountID: "gmail-personal", ProviderID: "m1", MessageID: "id-1@example.com",
		Subject: "Your order has shipped", FromEmail: "orders@shop.example",
		BodyText: "Your ceramic teapot is on the way, tracking number ZX123456789.",
	})
	mustUpsertBody(t, s, MessageBody{
		AccountID: "gmail-personal", ProviderID: "m2",
		Subject: "Weekly newsletter", FromEmail: "news@letter.example",
		BodyText: "This week in ceramics: glazing techniques.",
	})

	hits, err := s.SearchMessages("teapot tracking", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("want exactly the shipping mail, got %d hits: %+v", len(hits), hits)
	}
	h := hits[0]
	if h.AccountID != "gmail-personal" || h.ProviderID != "m1" {
		t.Errorf("locator = %s:%s, want gmail-personal:m1", h.AccountID, h.ProviderID)
	}
	if h.MessageID != "id-1@example.com" {
		t.Errorf("message_id = %q, want the stored RFC 5322 id", h.MessageID)
	}
	if h.Score >= 0 {
		t.Errorf("bm25 score = %v, want negative (lower is better)", h.Score)
	}
}

func TestSearchTermsAreQuotedSoFTSOperatorSyntaxCannotInject(t *testing.T) {
	s := openTestStore(t)
	mustUpsertBody(t, s, MessageBody{
		AccountID: "a", ProviderID: "m1", Subject: "hello", BodyText: "plain message",
	})
	// Every one of these would be an FTS5 syntax error or an operator if the
	// query were passed through raw. Tokenization must reduce them to plain
	// terms (or reject the query as empty), never error at the FTS layer.
	for _, q := range []string{`"unbalanced`, `NEAR(a b)`, `subject:hello`, `hello*`, `a AND`, `(((`} {
		_, err := s.SearchMessages(q, "", 10)
		if err != nil && !strings.Contains(err.Error(), "no searchable terms") {
			t.Errorf("query %q: unexpected error %v", q, err)
		}
	}
}

func TestSearchRequiresEveryTermWhereSimilarRequiresAny(t *testing.T) {
	s := openTestStore(t)
	mustUpsertBody(t, s, MessageBody{
		AccountID: "a", ProviderID: "m1", Subject: "invoice", BodyText: "payment due for consulting",
	})
	mustUpsertBody(t, s, MessageBody{
		AccountID: "a", ProviderID: "m2", Subject: "picnic", BodyText: "bring a blanket",
	})

	// AND: a term from each message matches neither.
	hits, err := s.SearchMessages("invoice blanket", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("AND search matched %d messages, want 0", len(hits))
	}

	// OR: the same terms match both.
	hits, err = s.SimilarMessages("invoice blanket", "", "", 10)
	if err != nil {
		t.Fatalf("similar: %v", err)
	}
	if len(hits) != 2 {
		t.Errorf("OR similarity matched %d messages, want 2", len(hits))
	}
}

func TestSimilarRanksTheNearMatchFirstAndExcludesTheMessageItself(t *testing.T) {
	s := openTestStore(t)
	mustUpsertBody(t, s, MessageBody{
		AccountID: "a", ProviderID: "uber1", FromEmail: "noreply@uber.com",
		Subject:  "Your Friday morning trip receipt",
		BodyText: "Thanks for riding. Trip fare 11.94 from Market Street.",
	})
	mustUpsertBody(t, s, MessageBody{
		AccountID: "a", ProviderID: "zillow1", FromEmail: "alerts@zillow.com",
		Subject:  "New listings in your saved search",
		BodyText: "3 new homes match your search in Reno.",
	})

	// A new Uber receipt: similar should surface the stored one first.
	hits, err := s.SimilarMessages("Your Tuesday evening trip receipt. Thanks for riding. Trip fare 8.10.", "", "", 10)
	if err != nil {
		t.Fatalf("similar: %v", err)
	}
	if len(hits) == 0 || hits[0].ProviderID != "uber1" {
		t.Fatalf("want uber1 ranked first, got %+v", hits)
	}

	// Asking about a stored message must not return that message.
	hits, err = s.SimilarMessages("Trip fare 11.94 from Market Street", "a", "uber1", 10)
	if err != nil {
		t.Fatalf("similar with exclusion: %v", err)
	}
	for _, h := range hits {
		if h.AccountID == "a" && h.ProviderID == "uber1" {
			t.Errorf("excluded message came back in its own results")
		}
	}
}

func TestReUpsertReplacesRatherThanDuplicatesAndTheIndexFollows(t *testing.T) {
	s := openTestStore(t)
	body := MessageBody{
		AccountID: "a", ProviderID: "m1", Subject: "draft agenda", BodyText: "quarterly planning zebra",
	}
	mustUpsertBody(t, s, body)
	body.Subject = "final agenda"
	body.BodyText = "quarterly planning giraffe"
	mustUpsertBody(t, s, body)

	// The old body term no longer matches: the update trigger re-indexed.
	hits, err := s.SearchMessages("zebra", "", 10)
	if err != nil {
		t.Fatalf("search old term: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("old body text still matches after re-upsert: %+v", hits)
	}
	hits, err = s.SearchMessages("giraffe", "", 10)
	if err != nil {
		t.Fatalf("search new term: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("new body text matches %d rows, want 1", len(hits))
	}

	st, err := s.IndexStats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.BodiesStored != 1 {
		t.Errorf("bodies_stored = %d after two upserts of one message, want 1", st.BodiesStored)
	}
}

func TestAnEmptyRefetchDoesNotBlankAStoredBody(t *testing.T) {
	s := openTestStore(t)
	mustUpsertBody(t, s, MessageBody{
		AccountID: "a", ProviderID: "m1", Subject: "receipt", BodyText: "total 42.00 for the workshop",
	})
	// A later fetch that failed to decode parts arrives with everything empty.
	if err := s.UpsertMessageBody(MessageBody{AccountID: "a", ProviderID: "m1"}); err != nil {
		t.Fatalf("empty re-upsert: %v", err)
	}
	hits, err := s.SearchMessages("workshop", "", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("stored body was blanked by an empty re-upsert")
	}
	if hits[0].Subject != "receipt" {
		t.Errorf("subject = %q, want the stored one preserved", hits[0].Subject)
	}
}

func TestAccountFilterNarrowsSearch(t *testing.T) {
	s := openTestStore(t)
	mustUpsertBody(t, s, MessageBody{AccountID: "personal", ProviderID: "m1", BodyText: "shared keyword aurora"})
	mustUpsertBody(t, s, MessageBody{AccountID: "work", ProviderID: "m2", BodyText: "shared keyword aurora"})

	hits, err := s.SearchMessages("aurora", "work", 10)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].AccountID != "work" {
		t.Fatalf("account filter leaked: %+v", hits)
	}
}

func TestSearchableTextPrefersTheRealPartOverTheStub(t *testing.T) {
	longProse := strings.Repeat("This is the actual content of the message body. ", 10)
	html := "<html><head><style>p{color:red}</style></head><body><p>" + longProse + "</p></body></html>"

	// A stub text part loses to a substantial HTML part.
	got := SearchableText("View this email in your browser", html)
	if !strings.Contains(got, "actual content") {
		t.Errorf("stub text part won over substantial HTML: %q", got)
	}
	if strings.Contains(got, "color:red") || strings.Contains(got, "<p>") {
		t.Errorf("HTML leaked into search text: %q", got)
	}

	// A substantial text part wins even when HTML exists.
	got = SearchableText(longProse, html)
	if !strings.HasPrefix(got, "This is the actual content") {
		t.Errorf("substantial text part lost: %q", got)
	}

	// No usable text at all is an empty answer, not an invented one.
	if got := SearchableText("", ""); got != "" {
		t.Errorf("empty message produced search text %q", got)
	}
}

func TestSearchableTextIsCappedOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("é", searchTextCap) // 2 bytes per rune
	got := SearchableText(long, "")
	if len(got) > searchTextCap {
		t.Errorf("search text %d bytes, cap is %d", len(got), searchTextCap)
	}
	for _, r := range got {
		if r != 'é' {
			t.Fatalf("cap split a rune: found %q", r)
		}
	}
}

func TestTokenizeForSearchDropsNoiseAndKeepsOrder(t *testing.T) {
	got := tokenizeForSearch("The Quick-Brown FOX! the fox and a 42nd item, hi")
	want := []string{"quick", "brown", "fox", "42nd", "item"}
	if len(got) != len(want) {
		t.Fatalf("terms = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("terms = %v, want %v", got, want)
		}
	}
}
