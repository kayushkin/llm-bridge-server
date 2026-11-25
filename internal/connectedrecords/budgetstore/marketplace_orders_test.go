package budgetstore

import (
	"errors"
	"testing"
	"time"
)

func TestAMarketplaceOrderFindsItsChargeByOrderNumberElseByAmountAndDate(t *testing.T) {
	s := openTestStore(t)
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card"})
	categories, _ := s.ListCategories()
	charge := func(day, description string, cents int64) Transaction {
		t.Helper()
		transaction, err := s.CreateTransaction(TransactionInput{OccurredOn: day, Description: description, Postings: []Posting{
			{AccountID: card.ID, AmountCents: -cents}, {CategoryID: categories[0].ID, AmountCents: cents}}})
		if err != nil {
			t.Fatal(err)
		}
		return transaction
	}
	byNumber := charge("2026-07-26", "EBAY O*19-00000-00001 SAN JOSE CA", 29015)
	byAmount := charge("2026-02-23", "PURCHASE AUTHORIZED ON 02/21 Ebay Commerce Inc. CARD 0000", 7520)
	charge("2026-02-23", "COFFEE SHOP", 7520)
	ordered := func(day string) int64 {
		parsed, _ := time.Parse("2006-01-02 15:04", day+" 20:00")
		return parsed.Unix()
	}

	first, err := s.CreateMarketplaceOrder(MarketplaceOrder{Marketplace: "ebay", OrderNumber: "19-00000-00001", Seller: "seller_one", OrderedAt: ordered("2026-07-25"),
		TotalCents: ptr(int64(29015)), Currency: "USD", MailAccountID: "mail-account", MailMessageID: "m1", ExtractedBy: "model", CreatedBy: "principal_000001",
		Lines: []MarketplaceOrderLine{{Title: "Metronome watch", ItemNumber: "206000000001", ListingURL: "https://www.ebay.com/itm/206000000001", UnitPriceCents: ptr(int64(26800))}}})
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "marketplace_order_000001" || first.TransactionID != byNumber.ID || first.MatchedBy != "order_number" || first.Lines[0].ID != "marketplace_order_line_000001" ||
		first.Lines[0].Disposition != "unreviewed" || first.Lines[0].Quantity != 1 {
		t.Fatalf("first = %+v", first)
	}
	second, err := s.CreateMarketplaceOrder(MarketplaceOrder{Marketplace: "ebay", OrderNumber: "19-00000-00002", OrderedAt: ordered("2026-02-21"),
		TotalCents: ptr(int64(7520)), Currency: "USD", CreatedBy: "principal_000001", Lines: []MarketplaceOrderLine{{Title: "Jeans"}}})
	if err != nil {
		t.Fatal(err)
	}
	if second.TransactionID != byAmount.ID || second.MatchedBy != "amount_and_date" {
		t.Fatalf("second = %+v, want the eBay charge of its total, not the coffee", second)
	}
	old, err := s.CreateMarketplaceOrder(MarketplaceOrder{Marketplace: "ebay", OrderNumber: "", OrderedAt: ordered("2019-03-01"), Currency: "GBP",
		CreatedBy: "principal_000001", MailAccountID: "mail-account", MailMessageID: "m3", Lines: []MarketplaceOrderLine{{Title: "Jacket"}}})
	if err != nil || old.TransactionID != "" || old.MatchedBy != "" {
		t.Fatalf("an order older than the ledger = %+v %v, want no transaction", old, err)
	}
	if _, err := s.CreateMarketplaceOrder(MarketplaceOrder{Marketplace: "ebay", OrderNumber: "19-00000-00009", OrderedAt: 1, Currency: "USD", CreatedBy: "x",
		MailAccountID: "mail-account", MailMessageID: "m1", Lines: []MarketplaceOrderLine{{Title: "again"}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second order from one email = %v, want a conflict", err)
	}
	if _, err := s.CreateMarketplaceOrder(MarketplaceOrder{Marketplace: "ebay", OrderNumber: "19-00000-00001", OrderedAt: 1, Currency: "USD", CreatedBy: "x",
		Lines: []MarketplaceOrderLine{{Title: "again"}}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("an order number recorded twice = %v, want a conflict", err)
	}

	if _, err := s.CreateLink(Link{FromID: first.Lines[0].ID, EntityType: "possession", EntityRef: "possession_000315"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateLink(Link{FromID: byNumber.ID, EntityType: "possession", EntityRef: "possession_000315"}); err != nil {
		t.Fatal(err)
	}
	read, err := s.GetTransaction(byNumber.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.MarketplaceOrders) != 1 || read.MarketplaceOrders[0].Lines[0].PossessionIDs[0] != "possession_000315" || len(read.Links) != 1 {
		t.Fatalf("the transaction read = %+v", read)
	}
	line, err := s.SetOrderLineDisposition(first.Lines[0].ID, "possession")
	if err != nil || line.Disposition != "possession" {
		t.Fatalf("disposition = %+v %v", line, err)
	}
	if _, err := s.SetOrderLineDisposition(first.Lines[0].ID, "lost"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a disposition outside the vocabulary = %v", err)
	}

	newer := charge("2019-03-04", "EBAY O*00-OLD JACKET", 1)
	_ = newer
	counts, err := s.MatchMarketplaceOrders()
	if err != nil || counts["unmatched"] != 1 {
		t.Fatalf("match again = %v %v", counts, err)
	}
}
