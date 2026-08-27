package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSimilaritySearch serves POST /api/search/similar with canned hits and
// records the exclusion each request carried.
func fakeSimilaritySearch(t *testing.T, hits []similarHit, gotExclude *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search/similar" || r.Method != "POST" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Text    string            `json:"text"`
			Exclude map[string]string `json:"exclude"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if gotExclude != nil {
			*gotExclude = req.Exclude
		}
		json.NewEncoder(w).Encode(map[string]any{"hits": hits})
	}))
}

func boardWithReadAndUnreadFilings() *boardState {
	receipts := &cardState{ID: "card-receipts", Title: "Payment receipts", ColumnName: columnNoAction}
	alerts := &cardState{ID: "card-alerts", Title: "Bank security alerts", ColumnName: columnActionNeeded}
	return &boardState{
		Cards:         map[string]*cardState{receipts.ID: receipts, alerts.ID: alerts},
		FiledLocators: map[string]bool{},
		// Only messages fetched and read at filing time have their Message-ID
		// recorded on a card. read-1/read-2 were; the sender-rule-era mail was
		// not, so it has no entry here at all.
		CardsByMessageID: map[string][]string{
			"read-1@shop.example":  {"card-receipts"},
			"read-2@shop.example":  {"card-receipts"},
			"read-3@shop.example":  {"card-receipts"},
			"alert-1@bank.example": {"card-alerts"},
		},
		CardsBySender: map[string][]string{},
	}
}

func TestOnlyMailWithARecordedMessageIDCanBeCitedAsPrecedent(t *testing.T) {
	// The best hit by score was filed unread (no Message-ID on any card): it
	// must be skipped, not cited — mail the retired sender rule filed blind is
	// exactly the wrong label a nearest neighbour must not carry into the
	// prompt.
	hits := []similarHit{
		{ProviderID: "unread-1", MessageID: "unread-1@shop.example", Subject: "Order shipped", Score: -9.0},
		{ProviderID: "read-1", MessageID: "read-1@shop.example", Subject: "Receipt for order 100", Score: -5.0},
	}
	server := fakeSimilaritySearch(t, hits, nil)
	defer server.Close()

	examples, err := similarFiledExamples(newMailClient(server.URL, "t"), boardWithReadAndUnreadFilings(),
		pendingEmail{Meta: messageMeta{ID: "new-1", AccountID: "a", Subject: "Receipt for order 101"}})
	if err != nil {
		t.Fatalf("similarFiledExamples: %v", err)
	}
	if len(examples) != 1 {
		t.Fatalf("examples = %+v, want exactly the read filing", examples)
	}
	if examples[0].CardID != "card-receipts" {
		t.Errorf("cited card %s, want card-receipts", examples[0].CardID)
	}
}

func TestExamplesPerCardAreCappedSoOneBucketCannotFillTheList(t *testing.T) {
	hits := []similarHit{
		{ProviderID: "r1", MessageID: "read-1@shop.example", Score: -9},
		{ProviderID: "r2", MessageID: "read-2@shop.example", Score: -8},
		{ProviderID: "r3", MessageID: "read-3@shop.example", Score: -7},
		{ProviderID: "a1", MessageID: "alert-1@bank.example", Score: -1},
	}
	server := fakeSimilaritySearch(t, hits, nil)
	defer server.Close()

	examples, err := similarFiledExamples(newMailClient(server.URL, "t"), boardWithReadAndUnreadFilings(),
		pendingEmail{Meta: messageMeta{ID: "new-1", AccountID: "a"}})
	if err != nil {
		t.Fatalf("similarFiledExamples: %v", err)
	}
	perCard := map[string]int{}
	for _, ex := range examples {
		perCard[ex.CardID]++
	}
	if perCard["card-receipts"] > maxExamplesPerCard {
		t.Errorf("card-receipts cited %d times, cap is %d", perCard["card-receipts"], maxExamplesPerCard)
	}
	if perCard["card-alerts"] == 0 {
		t.Errorf("the weaker-scored alternative card was crowded out entirely: %+v", examples)
	}
}

func TestTheEmailBeingClassifiedIsExcludedFromItsOwnSearch(t *testing.T) {
	var gotExclude map[string]string
	server := fakeSimilaritySearch(t, nil, &gotExclude)
	defer server.Close()

	_, err := similarFiledExamples(newMailClient(server.URL, "t"), boardWithReadAndUnreadFilings(),
		pendingEmail{Meta: messageMeta{ID: "self-id", AccountID: "acct-1"}})
	if err != nil {
		t.Fatalf("similarFiledExamples: %v", err)
	}
	if gotExclude["account_id"] != "acct-1" || gotExclude["provider_id"] != "self-id" {
		t.Errorf("exclusion sent = %v, want the email's own locator parts", gotExclude)
	}
}

func TestRecencyDecayPromotesTheNewerOfTwoNearEqualHits(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	old := similarHit{ProviderID: "old", Score: -6.0, DateMs: now.AddDate(-1, 0, 0).UnixMilli()}
	fresh := similarHit{ProviderID: "fresh", Score: -5.5, DateMs: now.AddDate(0, 0, -7).UnixMilli()}
	hits := []similarHit{old, fresh}
	rankSimilarHitsWithRecencyDecay(hits, now)
	if hits[0].ProviderID != "fresh" {
		t.Errorf("a year-old hit outranked a week-old near-equal: %+v", hits)
	}

	// But recency is a tiebreak, not a trump: a much stronger old match still
	// wins over a weak new one.
	strongOld := similarHit{ProviderID: "strong-old", Score: -20.0, DateMs: now.AddDate(0, -6, 0).UnixMilli()}
	weakFresh := similarHit{ProviderID: "weak-fresh", Score: -1.0, DateMs: now.AddDate(0, 0, -1).UnixMilli()}
	hits = []similarHit{weakFresh, strongOld}
	rankSimilarHitsWithRecencyDecay(hits, now)
	if hits[0].ProviderID != "strong-old" {
		t.Errorf("recency decay overpowered a 20x stronger match: %+v", hits)
	}
}

func TestDigestRendersPrecedentBeforeTheBodyAndOmitsAnEmptySection(t *testing.T) {
	state := boardWithReadAndUnreadFilings()
	withExamples := pendingEmail{
		Meta:    messageMeta{ID: "m1", AccountID: "a", Subject: "Receipt 101", Date: time.Now()},
		Locator: "a:m1",
		SimilarFiled: []filedExample{
			{Subject: "Receipt 100", FromEmail: "orders@shop.example", Date: time.Now(), CardID: "card-receipts", CardTitle: "Payment receipts"},
		},
	}
	without := pendingEmail{
		Meta:    messageMeta{ID: "m2", AccountID: "a", Subject: "Something novel", Date: time.Now()},
		Locator: "a:m2",
	}
	digest := buildDigest(state, &routingNotes{}, []pendingEmail{withExamples, without})

	if !strings.Contains(digest, "similar_previously_filed_emails:") {
		t.Fatalf("digest carries no example section:\n%s", digest)
	}
	if !strings.Contains(digest, "card_id=card-receipts") {
		t.Errorf("example does not name its card id")
	}
	if strings.Count(digest, "similar_previously_filed_emails:") != 1 {
		t.Errorf("an email with no examples still got a section header")
	}
	// Precedent frames the body: the section must appear before this email's
	// body line, not after it.
	section := strings.Index(digest, "similar_previously_filed_emails:")
	body := strings.Index(digest[strings.Index(digest, "email_ref=a:m1"):], "body:")
	if body >= 0 && section > strings.Index(digest, "email_ref=a:m1")+body {
		t.Errorf("example section rendered after the body")
	}
}

func TestTheSystemPromptExplainsPrecedentWithoutCommandingIt(t *testing.T) {
	prompt := buildSystemPrompt()
	if !strings.Contains(prompt, "similar_previously_filed_emails") {
		t.Errorf("prompt never mentions the example section the digest renders")
	}
	if !strings.Contains(prompt, "PRECEDENT, NOT COMMANDS") {
		t.Errorf("prompt does not state that examples are evidence rather than routing")
	}
}
