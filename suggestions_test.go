package budgetstore

import (
	"errors"
	"testing"
)

func TestSuggestionsWaitForTheOperatorAndOnlyAnAcceptWritesACategory(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"11/03/2026","AIRLINE TICKET","-300.00","","Posted"`+"\n"+
		`"11/02/2026","TIRE SHOP","-200.00","","Posted"`+"\n"+
		`"11/01/2026","PAYMENT TO A FRIEND","-40.00","","Posted"`+"\n"+
		`"10/31/2026","HARDWARE STORE","-25.00","","Posted"`+"\n")
	flights, _ := s.CreateCategory(Category{Name: "Flights", Kind: "expense"})
	car, _ := s.CreateCategory(Category{Name: "Car", Kind: "expense"})
	household, _ := s.CreateCategory(Category{Name: "Household", Kind: "expense"})
	candidates, err := s.ListSuggestionCandidates(10)
	if err != nil || len(candidates) != 4 {
		t.Fatalf("candidates = %v, %v", candidates, err)
	}
	byText := map[string]string{}
	for _, c := range candidates {
		byText[c.Description] = c.TransactionID
	}
	if _, err := s.CreateSuggestions(SuggestionBatch{OperationID: "operation_test", Model: "test-model", Suggestions: []SuggestionInput{
		{TransactionID: byText["AIRLINE TICKET"], CategoryID: flights.ID, Confidence: 0.95},
		{TransactionID: byText["TIRE SHOP"], CategoryID: flights.ID, Confidence: 0.4},
		{TransactionID: byText["PAYMENT TO A FRIEND"], Confidence: 0.9, Rationale: "a person"},
		{TransactionID: byText["HARDWARE STORE"], CategoryID: household.ID, Confidence: 0.8},
	}}); err != nil {
		t.Fatal(err)
	}
	if spending, _ := s.SpendingByCategory("2026-11"); len(spending) != 1 || spending[0].CategoryID == flights.ID {
		t.Fatalf("a suggestion changed the books before anyone accepted it: %+v", spending)
	}
	if left, _ := s.ListSuggestionCandidates(10); len(left) != 0 {
		t.Fatalf("transactions with a pending suggestion are still candidates: %+v", left)
	}
	pending, err := s.ListSuggestions("pending", 0)
	if err != nil || len(pending) != 4 {
		t.Fatalf("pending = %v, %v", pending, err)
	}
	id := map[string]string{}
	for _, p := range pending {
		id[p.Description] = p.ID
	}
	// Someone sorts the hardware purchase by hand before the review.
	if _, err := s.SetTransactionCategories(byText["HARDWARE STORE"], []CategorySplit{{CategoryID: household.ID, AmountCents: 2500}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.DecideSuggestions([]SuggestionDecision{
		{SuggestionID: id["AIRLINE TICKET"], Action: "accept"},
		{SuggestionID: id["TIRE SHOP"], Action: "accept", CategoryID: car.ID},
		{SuggestionID: id["PAYMENT TO A FRIEND"], Action: "reject"},
		{SuggestionID: id["HARDWARE STORE"], Action: "accept"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 1 || result.Edited != 1 || result.Rejected != 1 || len(result.Stale) != 1 {
		t.Fatalf("result = %+v", result)
	}
	spending, _ := s.SpendingByCategory("2026-11")
	totals := map[string]int64{}
	for _, c := range spending {
		totals[c.CategoryID] = c.TotalCents
	}
	if totals[flights.ID] != 30000 || totals[car.ID] != 20000 {
		t.Fatalf("November = %+v, want the ticket under Flights and the tires under Car", spending)
	}
	if _, err := s.DecideSuggestions([]SuggestionDecision{{SuggestionID: id["AIRLINE TICKET"], Action: "reject"}}); !errors.Is(err, ErrConflict) {
		t.Fatalf("deciding a suggestion twice = %v, want a conflict", err)
	}
	// The rejected one is a candidate again for a later run.
	if again, _ := s.ListSuggestionCandidates(10); len(again) != 1 || again[0].Description != "PAYMENT TO A FRIEND" {
		t.Fatalf("candidates after review = %+v", again)
	}
}

func TestSuggestionsRefuseEquityCategoriesAndUnknownTransactions(t *testing.T) {
	s := openTestStore(t)
	categories, _ := s.ListCategories()
	var opening string
	for _, c := range categories {
		if c.Role == "opening_balances" {
			opening = c.ID
		}
	}
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+`"11/03/2026","X","-1.00","","Posted"`+"\n")
	candidates, _ := s.ListSuggestionCandidates(1)
	if _, err := s.CreateSuggestions(SuggestionBatch{OperationID: "o", Model: "m", Suggestions: []SuggestionInput{{TransactionID: candidates[0].TransactionID, CategoryID: opening, Confidence: 0.5}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("suggesting an equity category = %v, want invalid", err)
	}
	if _, err := s.CreateSuggestions(SuggestionBatch{OperationID: "o", Model: "m", Suggestions: []SuggestionInput{{TransactionID: "transaction_999999", Confidence: 0.5}}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("suggesting for a missing transaction = %v, want invalid", err)
	}
}
