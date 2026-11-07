package possessionstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type fakeArticles struct {
	saved []string
	text  string
}

func (articles *fakeArticles) SaveListing(ctx context.Context, pageURL string, tags []string, addedBy string) (string, error) {
	articles.saved = append(articles.saved, pageURL)
	return "article_000007", nil
}

func (articles *fakeArticles) ReadListing(ctx context.Context, articleID string) (ListingPage, error) {
	return ListingPage{ArticleID: articleID, Title: "Metronome Watch", Text: articles.text}, nil
}

func openProductTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir(), fakeReferences{known: map[string]bool{"photo_000001": true, "article_000007": true}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestAProductIsReadFromItsListingAndServesItsPossessions(t *testing.T) {
	store := openProductTestStore(t)
	ctx := context.Background()
	watch, err := store.CreatePossession(ctx, fields(t, `{"name":"metronome wristwatch","brand":"Acme"}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	models := &scriptedModels{respond: func(role, step, prompt string, pictures int) (any, bool) {
		if !strings.Contains(prompt, "Read this product's description") || !strings.Contains(prompt, "Tempo range: 40-304") {
			return nil, false
		}
		return map[string]any{"is_this_product": true, "name": "Metronome Watch", "description": "A watch that keeps tempo.",
			"specs": []map[string]string{{"name": "Tempo range", "value": "40-304"}}, "list_price_cents": -1, "list_price_currency": "", "why": "the spec table"}, true
	}}
	articles := &fakeArticles{text: "A watch that keeps tempo.\nTempo range: 40-304\nMW-6 open price"}
	reviewer := &Reviewer{Store: store, Models: models, Photos: fakePhotos{}, Articles: articles, FirstReaderRole: "first", SecondReaderRole: "second"}
	product, err := reviewer.CreateProduct(ctx, ProductRequest{Actor: "principal_000001", Brand: "Acme", ModelNumber: "MW-6", ListingURL: "https://maker.example/mw",
		ListingSource: "manufacturer", ListingPublisher: "Acme Instruments", FoundBy: "web search", PossessionIDs: []string{watch["id"].(string)}})
	if err != nil {
		t.Fatal(err)
	}
	if product.ID != "product_000001" || product.ArticleID != "article_000007" || product.Name != "Metronome Watch" || product.ListPriceCents != nil ||
		len(product.Specs) != 1 || product.ExtractedBy != "first-model" || len(product.PossessionIDs) != 1 {
		t.Fatalf("product = %+v", product)
	}
	if len(articles.saved) != 1 || articles.saved[0] != "https://maker.example/mw" {
		t.Fatalf("saved = %v", articles.saved)
	}
	read, err := store.GetPossession(watch["id"].(string), false)
	if err != nil || fmt.Sprint(read["product_id"]) != "product_000001" {
		t.Fatalf("possession product_id = %v %v", read["product_id"], err)
	}
	_, err = reviewer.CreateProduct(ctx, ProductRequest{Actor: "principal_000001", Brand: "acme", ModelNumber: "mw-6", Name: "again", ListingURL: "https://maker.example/mw",
		ListingSource: "manufacturer", ArticleID: "article_000007", Description: "given"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("a second product of one brand and model = %v, want a conflict", err)
	}
	if _, err := store.PatchPossession(ctx, watch["id"].(string), fields(t, `{"product_id":"product_000099"}`), "principal_000001"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pointing at no product = %v, want invalid", err)
	}
	patched, err := store.PatchProduct(ctx, product.ID, fields(t, `{"list_price_cents":26800,"list_price_currency":"USD"}`), "principal_000001")
	if err != nil || patched.ListPriceCents == nil || *patched.ListPriceCents != 26800 {
		t.Fatalf("patched = %+v %v", patched, err)
	}
}

type fakeMail struct{ messages map[string]MailMessage }

func (mail fakeMail) Search(ctx context.Context, account, query, cursor string) ([]string, string, error) {
	ids := []string{}
	for id := range mail.messages {
		ids = append(ids, id)
	}
	return ids, "", nil
}

func (mail fakeMail) Message(ctx context.Context, account, id string) (MailMessage, error) {
	return mail.messages[id], nil
}

func TestEbayConfirmationsBecomePurchasesOnceAndMatchTheirPossessions(t *testing.T) {
	store := openProductTestStore(t)
	ctx := context.Background()
	watch, err := store.CreatePossession(ctx, fields(t, `{"name":"metronome wristwatch","brand":"Acme","watch":{}}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	ordered := time.Date(2026, 7, 25, 23, 6, 0, 0, time.UTC)
	mail := fakeMail{messages: map[string]MailMessage{
		"m1": {ID: "m1", Subject: "your order is confirmed", From: "ebay@ebay.com", Date: ordered,
			BodyText: "Acme Metronome Watch MW-6 NEW\nPrice: $268.00\nItem ID: 206000000001\nOrder number: 19-00000-00001\nSeller: seller_one\nTotal $290.15"},
		"m2": {ID: "m2", Subject: "Order confirmed: Gift card", From: "ebay@ebay.com", Date: ordered.Add(time.Hour),
			BodyText: "Gift card\nItem ID: 206000000002"},
		"m3": {ID: "m3", Subject: "Order confirmed: socks", From: "ebay@ebay.com", Date: ordered.Add(2 * time.Hour), BodyText: "Wool socks\nItem ID: 206000000003"},
	}}
	models := &scriptedModels{respond: func(role, step, prompt string, pictures int) (any, bool) {
		switch {
		case strings.Contains(prompt, "Acme Metronome Watch MW-6 NEW") && strings.Contains(prompt, "What order does it confirm?"):
			return map[string]any{"is_order_confirmation": true, "order_number": "19-00000-00001", "seller": "seller_one", "total_cents": 29015, "currency": "usd",
				"lines": []map[string]any{{"title": "Acme Metronome Watch MW-6 NEW", "item_id": "206000000001", "quantity": 1, "unit_price_cents": 26800}}, "why": ""}, true
		case strings.Contains(prompt, "Gift card"):
			return map[string]any{"is_order_confirmation": false, "order_number": "", "seller": "", "total_cents": -1, "currency": "USD", "lines": []any{}, "why": "an ad"}, true
		case strings.Contains(prompt, "Wool socks"):
			// An item number the mail does not hold must not be recorded.
			return map[string]any{"is_order_confirmation": true, "order_number": "", "seller": "s", "total_cents": -1, "currency": "USD",
				"lines": []map[string]any{{"title": "Wool socks", "item_id": "999999999999", "quantity": 1, "unit_price_cents": -1}}, "why": ""}, true
		case strings.Contains(prompt, "Which entry, if any, did this order line buy?"):
			if !strings.Contains(prompt, "C1: metronome wristwatch [watch]") {
				return nil, false
			}
			return map[string]any{"match": "C1", "sure": true, "why": "same maker and model"}, true
		}
		return nil, false
	}}
	reviewer := &Reviewer{Store: store, Models: models, Photos: fakePhotos{}, Mail: mail, FirstReaderRole: "first", SecondReaderRole: "second"}
	run, err := reviewer.ImportEbayOrders(ctx, EbayImportRequest{Actor: "principal_000001", Account: "mail-account"})
	if err != nil {
		t.Fatal(err)
	}
	if run.Looked != 3 || run.Counts["orders"] != 1 || run.Counts["lines"] != 1 || run.Counts["not_an_order"] != 1 || run.Counts["failed"] != 1 {
		t.Fatalf("run = %+v", run)
	}
	purchase, err := store.GetPurchase(run.Actions[0].Records[0])
	if err != nil {
		t.Fatal(err)
	}
	if purchase.Seller != "seller_one" || purchase.Marketplace != "ebay" || purchase.Source != "email_receipt" || purchase.Currency != "USD" ||
		purchase.MailAccountID != "mail-account" || purchase.MailMessageID != "m1" || purchase.OrderedAt != ordered.Unix() || *purchase.TotalCents != 29015 ||
		purchase.Lines[0].ListingURL != "https://www.ebay.com/itm/206000000001" || purchase.Lines[0].ProductReference != "206000000001" {
		t.Fatalf("purchase = %+v", purchase)
	}
	again, err := reviewer.ImportEbayOrders(ctx, EbayImportRequest{Actor: "principal_000001", Account: "mail-account"})
	if err != nil || again.Counts["already_imported"] != 1 || again.Counts["orders"] != 0 {
		t.Fatalf("a second import = %+v %v, want the order skipped", again, err)
	}

	matched, err := reviewer.MatchPurchaseLines(ctx, "principal_000001", "")
	if err != nil {
		t.Fatal(err)
	}
	if matched.Counts["matched"] != 1 {
		t.Fatalf("match = %+v", matched)
	}
	linked, err := store.GetPossession(watch["id"].(string), false)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(linked["purchase_line_id"]) != purchase.Lines[0].ID || linked["acquired_at"] != ordered.Unix() || linked["acquired_how"] != "bought" ||
		linked["price_cents"] != int64(26800) || linked["price_currency"] != "USD" {
		t.Fatalf("linked possession = %v", linked)
	}
	purchase, _ = store.GetPurchase(purchase.ID)
	if purchase.Lines[0].Disposition != "possession" || len(purchase.Lines[0].PossessionIDs) != 1 {
		t.Fatalf("line after the match = %+v", purchase.Lines[0])
	}
	if rerun, err := reviewer.MatchPurchaseLines(ctx, "principal_000001", ""); err != nil || rerun.Looked != 0 {
		t.Fatalf("a second match = %+v %v, want nothing left to look at", rerun, err)
	}
}

func TestAPurchaseLineAnswerLinksThePossession(t *testing.T) {
	store := openProductTestStore(t)
	ctx := context.Background()
	lamp, err := store.CreatePossession(ctx, fields(t, `{"name":"desk lamp","acquired_at":5}`), "principal_000001")
	if err != nil {
		t.Fatal(err)
	}
	purchase, err := store.CreatePurchase(ctx, Purchase{Seller: "s", Marketplace: "ebay", OrderReference: "1", OrderedAt: 100, Source: "manual",
		Lines: []PurchaseLine{{Title: "Lamp"}}})
	if err != nil {
		t.Fatal(err)
	}
	transaction, err := store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.applyAuditResolution(transaction, OwnerQuestion{ID: "possession_question_000001"},
		Resolution{Action: "purchase_line_link", PossessionID: lamp["id"].(string), PurchaseLineID: purchase.Lines[0].ID}, "principal_000001", 200); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	read, _ := store.GetPossession(lamp["id"].(string), false)
	if fmt.Sprint(read["purchase_line_id"]) != purchase.Lines[0].ID || read["acquired_at"] != int64(5) || read["price_cents"] != nil {
		t.Fatalf("answered possession = %v; acquired_at kept and no price invented", read)
	}
}
