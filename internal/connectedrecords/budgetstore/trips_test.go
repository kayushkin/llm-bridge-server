package budgetstore

import (
	"errors"
	"testing"
)

func TestTripsSuggestSpendingDuringTheTripButNotHomeChargesOrBills(t *testing.T) {
	s := openTestStore(t)
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card"})
	mustImport(t, s, "wells_fargo_csv", card.ID, wellsFargoHeader+
		`"09/29/2026","DINER HOMETOWN ST","-12.00",,"Posted"`+"\n"+ // before the trip
		`"09/30/2026","AIRPORT CAFE ELSEWHERE NY","-9.00",,"Posted"`+"\n"+
		`"10/02/2026","MUSEUM ELSEWHERE NY","-30.00",,"Posted"`+"\n"+
		`"10/03/2026","GROCER HOMETOWN ST","-40.00",,"Posted"`+"\n"+ // at home, though dated during the trip
		`"10/08/2026","HOTEL ELSEWHERE NY","-400.00",,"Posted"`+"\n"+ // posted three days after
		`"10/09/2026","LATE CHARGE ELSEWHERE NY","-5.00",,"Posted"`+"\n") // four days after: too late
	count, err := s.SuggestTripTransactions("trip_002596", TripSuggestRequest{FirstDay: "2026-09-30", LastDay: "2026-10-05", HomeMarkers: []string{"hometown st"}})
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("suggested %d, want the cafe, the museum and the hotel", count)
	}
	// Suggesting again adds nothing.
	if again, _ := s.SuggestTripTransactions("trip_002596", TripSuggestRequest{FirstDay: "2026-09-30", LastDay: "2026-10-05", HomeMarkers: []string{"hometown st"}}); again != 0 {
		t.Fatalf("a second suggest added %d", again)
	}
	list, _ := s.ListTripTransactions("trip_002596")
	byText := map[string]string{}
	for _, tt := range list {
		byText[tt.Description] = tt.TransactionID
	}
	if _, err := s.SetTripTransaction("trip_002596", byText["MUSEUM ELSEWHERE NY"], "removed", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a decision with no decided_by = %v, want invalid", err)
	}
	if _, err := s.SetTripTransaction("trip_002596", byText["MUSEUM ELSEWHERE NY"], "removed", "the operator, in a test", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetTripTransaction("trip_002596", byText["HOTEL ELSEWHERE NY"], "confirmed", "the operator, in a test", ""); err != nil {
		t.Fatal(err)
	}
	totals, _ := s.ListTripTotals()
	if len(totals) != 1 || totals[0].ConfirmedCents != 40000 || totals[0].SuggestedCents != 900 || totals[0].Confirmed != 1 || totals[0].Suggested != 1 {
		t.Fatalf("totals = %+v", totals)
	}
	// A trip a day later suggests the same late charges; confirming one on the
	// first trip clears it from the second.
	if n, _ := s.SuggestTripTransactions("trip_002597", TripSuggestRequest{FirstDay: "2026-10-06", LastDay: "2026-10-07"}); n != 1 {
		t.Fatalf("the second trip suggested %d, want only the late charge (the hotel is confirmed elsewhere)", n)
	}
	second, _ := s.ListTripTransactions("trip_002597")
	if _, err := s.SetTripTransaction("trip_002596", second[0].TransactionID, "confirmed", "the operator, in a test", "posted late"); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.ListTripTransactions("trip_002597"); len(left) != 0 {
		t.Fatalf("the second trip still lists %+v", left)
	}
	// A transaction confirmed on one trip cannot be confirmed on another.
	if _, err := s.SetTripTransaction("trip_002597", byText["HOTEL ELSEWHERE NY"], "confirmed", "the operator, in a test", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("confirming on a second trip = %v, want a conflict", err)
	}
	if _, err := s.SuggestTripTransactions("not-a-trip", TripSuggestRequest{FirstDay: "2026-01-01", LastDay: "2026-01-02"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bad trip ref = %v, want invalid", err)
	}
}
