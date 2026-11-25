package budgetstore

import (
	"errors"
	"testing"
)

func TestAnInterestGathersSpendingAcrossCategoriesByItsRules(t *testing.T) {
	s := openTestStore(t)
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card"})
	recreation, _ := s.CreateCategory(Category{Name: "Recreation", Kind: "expense"})
	skating, _ := s.CreateCategory(Category{Name: "Skating", Kind: "expense", ParentID: recreation.ID})
	shopping, _ := s.CreateCategory(Category{Name: "Shopping", Kind: "expense"})
	rink, _ := s.CreatePayee(Payee{Name: "The rink", DefaultCategoryID: skating.ID, Patterns: []string{"ICE RINK"}})
	mustImport(t, s, "wells_fargo_csv", card.ID, wellsFargoHeader+
		`"10/01/2026","ICE RINK PUBLIC SESSION","-13.00",,"Posted"`+"\n"+
		`"10/05/2026","SKATE SHOP BLADES","-180.00",,"Posted"`+"\n"+
		`"10/07/2026","FIGURE SKATING CLUB DUES","-60.00",,"Posted"`+"\n"+
		`"10/09/2026","GROCERY","-50.00",,"Posted"`+"\n")
	all, _ := s.ListTransactions(TransactionFilter{})
	byText := map[string]string{}
	for _, tr := range all {
		byText[tr.Description] = tr.ID
	}
	if _, err := s.SetTransactionCategories(byText["SKATE SHOP BLADES"], []CategorySplit{{CategoryID: shopping.ID, AmountCents: 18000}}); err != nil {
		t.Fatal(err)
	}
	interest, err := s.CreateInterest("Skating", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddInterestRule(interest.ID, "category", recreation.ID); err != nil { // the parent: Skating sits under it
		t.Fatal(err)
	}
	if _, err := s.AddInterestRule(interest.ID, "payee", rink.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.AddInterestRule(interest.ID, "description", "figure skating")
	if err != nil {
		t.Fatal(err)
	}
	if got.Transactions != 2 || got.TotalCents != 7300 {
		t.Fatalf("interest = %+v, want the rink session and the club dues", got)
	}
	// The skates were filed under Shopping, so no rule caught them: add by hand.
	if _, err := s.SetInterestTransaction(interest.ID, byText["SKATE SHOP BLADES"], "confirmed", "the operator, in a test"); err != nil {
		t.Fatal(err)
	}
	// Remove the club dues; matching again must not bring them back.
	if _, err := s.SetInterestTransaction(interest.ID, byText["FIGURE SKATING CLUB DUES"], "removed", "the operator, in a test"); err != nil {
		t.Fatal(err)
	}
	if added, err := s.MatchInterests(); err != nil || added != 0 {
		t.Fatalf("matching again added %d, %v", added, err)
	}
	final, _ := s.GetInterest(interest.ID)
	if final.Transactions != 2 || final.TotalCents != 19300 {
		t.Fatalf("final = %+v, want the session and the skates", final)
	}
	if _, err := s.AddInterestHistorySource(interest.ID, "location_place", "place_000002", "the rink"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddInterestHistorySource(interest.ID, "location_place", "Reno Ice", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a place given by name = %v, want invalid", err)
	}
	if _, err := s.SetInterestTransaction(interest.ID, byText["GROCERY"], "confirmed", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a change with no decided_by = %v, want invalid", err)
	}
}
