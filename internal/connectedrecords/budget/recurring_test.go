package budget

import (
	"errors"
	"testing"
	"time"
)

func TestBillsAreFoundByRhythmAndWaitForTheOperator(t *testing.T) {
	s := openTestStore(t)
	s.now = func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card", LastFour: "2222"})
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		// A utility, monthly, the amount moving a little.
		`"08/15/2026","POWER CO PYMT 260815 ACCT1234","-90.10","","Posted"`+"\n"+
		`"09/15/2026","POWER CO PYMT 260915 ACCT1234","-95.20","","Posted"`+"\n"+
		`"10/15/2026","POWER CO PYMT 261015 ACCT1234","-101.30","","Posted"`+"\n"+
		// Rent through a portal that adds a fee the same day: one payment.
		`"08/01/2026","PROPERTY CO RENT WEB PMTS","-1000.00","","Posted"`+"\n"+
		`"08/01/2026","PROPERTY CO RENT WEB PMTS FEE","-2.49","","Posted"`+"\n"+
		`"09/01/2026","PROPERTY CO RENT WEB PMTS","-1000.00","","Posted"`+"\n"+
		`"09/01/2026","PROPERTY CO RENT WEB PMTS FEE","-2.49","","Posted"`+"\n"+
		`"10/01/2026","PROPERTY CO RENT WEB PMTS","-1000.00","","Posted"`+"\n"+
		`"10/01/2026","PROPERTY CO RENT WEB PMTS FEE","-2.49","","Posted"`+"\n"+
		// Snacks several times a month: not a bill.
		`"10/01/2026","PURCHASE AUTHORIZED ON 10/01 SNACK BAR S123456789","-2.00","","Posted"`+"\n"+
		`"10/03/2026","PURCHASE AUTHORIZED ON 10/03 SNACK BAR S123456790","-2.50","","Posted"`+"\n"+
		`"10/04/2026","PURCHASE AUTHORIZED ON 10/04 SNACK BAR S123456791","-1.50","","Posted"`+"\n"+
		// A gym that stopped in spring: a pattern, but no longer paid.
		`"02/01/2026","GYM DUES","-30.00","","Posted"`+"\n"+
		`"03/01/2026","GYM DUES","-30.00","","Posted"`+"\n"+
		`"04/01/2026","GYM DUES","-30.00","","Posted"`+"\n"+
		// Monthly card payments: transfers, never bills.
		`"08/20/2026","ONLINE TRANSFER TO CARD XXXX2222","-200.00","","Posted"`+"\n"+
		`"09/20/2026","ONLINE TRANSFER TO CARD XXXX2222","-200.00","","Posted"`+"\n"+
		`"10/20/2026","ONLINE TRANSFER TO CARD XXXX2222","-200.00","","Posted"`+"\n")
	mustImport(t, s, "wells_fargo_csv", card.ID, wellsFargoHeader+
		`"08/20/2026","ONLINE PAYMENT THANK YOU","200.00",,"Posted"`+"\n"+
		`"09/20/2026","ONLINE PAYMENT THANK YOU","200.00",,"Posted"`+"\n"+
		`"10/20/2026","ONLINE PAYMENT THANK YOU","200.00",,"Posted"`+"\n")
	if _, err := s.MatchTransfers(); err != nil {
		t.Fatal(err)
	}
	result, err := s.DetectRecurringBills()
	if err != nil {
		t.Fatal(err)
	}
	if result.Suggested != 2 {
		t.Fatalf("result = %+v, want the utility and the rent suggested", result)
	}
	bills, _ := s.ListRecurringBills("suggested")
	var bill, rent RecurringBill
	for _, b := range bills {
		switch b.Name {
		case "POWER CO PYMT":
			bill = b
		case "PROPERTY CO RENT WEB":
			rent = b
		}
	}
	if rent.ExpectedCents != 100249 || rent.Cadence != "monthly" || rent.PaymentCount != 6 {
		t.Fatalf("rent = %+v, want one monthly payment of 1002.49 a month over six transactions", rent)
	}
	if bill.Cadence != "monthly" || bill.ExpectedCents != 9520 || bill.DayOfMonth != 15 || bill.NextDueOn != "2026-11-15" || bill.PaymentCount != 3 {
		t.Fatalf("bill = %+v", bill)
	}
	full, err := s.GetRecurringBill(bill.ID)
	if err != nil || len(full.Payments) != 3 || full.Payments[2].ChangedFromPreviousCents != 610 {
		t.Fatalf("payments = %+v, %v", full.Payments, err)
	}
	if totals, _ := s.GetRecurringTotals(); totals.Bills != 0 {
		t.Fatalf("an unconfirmed bill counted in the totals: %+v", totals)
	}
	if _, err := s.PatchRecurringBill(bill.ID, RecurringBillPatch{Status: ptr("confirmed")}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("confirming with no decided_by = %v, want invalid", err)
	}
	if _, err := s.PatchRecurringBill(rent.ID, RecurringBillPatch{Status: ptr("rejected"), DecidedBy: "the operator, in a test"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PatchRecurringBill(bill.ID, RecurringBillPatch{Status: ptr("confirmed"), DecidedBy: "the operator, in a test", Name: ptr("Power company")}); err != nil {
		t.Fatal(err)
	}
	totals, err := s.GetRecurringTotals()
	if err != nil || totals.Bills != 1 || totals.ExpectedPerMonthCents != 9520 || totals.ExpectedPerYearCents != 9520*12 || totals.PaidLastSixMonthsCents != 28660 {
		t.Fatalf("totals = %+v, %v", totals, err)
	}
	// Run again later with no new payment: the confirmed bill has stopped.
	s.now = func() time.Time { return time.Date(2027, 1, 20, 12, 0, 0, 0, time.UTC) }
	again, err := s.DetectRecurringBills()
	if err != nil || again.Ended != 1 {
		t.Fatalf("second detect = %+v, %v; want the bill ended", again, err)
	}
	ended, _ := s.GetRecurringBill(bill.ID)
	if ended.Status != "ended" || ended.Name != "Power company" {
		t.Fatalf("after it stopped: %+v", ended)
	}
}

func TestBillTextKeysDropReferenceNumbersAndDates(t *testing.T) {
	for in, want := range map[string]string{
		"PURCHASE AUTHORIZED ON 10/08 MAPLE FUEL #704 SPRINGFIELD ST S586000000000001 CARD 0000": "MAPLE FUEL SPRINGFIELD ST",
		"CITY POWER NORTH SPPC PYMT 261015 3000000000000 JSMITH":                                 "CITY POWER NORTH SPPC",
		"RECURRING PAYMENT AUTHORIZED ON 08/27 NEIGHBOR: 833-416- NEIGHBOR.COM UT":               "NEIGHBOR: NEIGHBOR.COM UT",
	} {
		if got := billTextKey(in); got != want {
			t.Errorf("billTextKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheOperatorCanNameABillTheRhythmTestMissed(t *testing.T) {
	s := openTestStore(t)
	s.now = func() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }
	card := mustAccount(t, s, AccountInput{Name: "Card", Kind: "credit_card"})
	// A server host that sometimes charges twice in a month.
	mustImport(t, s, "wells_fargo_csv", card.ID, wellsFargoHeader+
		`"07/01/2026","HOSTING CO SERVERS","-40.00",,"Posted"`+"\n"+
		`"08/01/2026","HOSTING CO SERVERS","-40.00",,"Posted"`+"\n"+
		`"08/15/2026","HOSTING CO SERVERS","-12.00",,"Posted"`+"\n"+
		`"09/01/2026","HOSTING CO SERVERS","-40.00",,"Posted"`+"\n"+
		`"09/03/2026","HOSTING CO SERVERS","-12.00",,"Posted"`+"\n"+
		`"10/01/2026","HOSTING CO SERVERS","-52.00",,"Posted"`+"\n")
	if result, _ := s.DetectRecurringBills(); result.Suggested != 0 {
		t.Fatalf("the rhythm test found %+v; this case is meant to be one it misses", result)
	}
	list, _ := s.ListTransactions(TransactionFilter{})
	if _, err := s.AddRecurringBill(list[0].ID, "monthly", "confirmed", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a bill with no decided_by = %v, want invalid", err)
	}
	bill, err := s.AddRecurringBill(list[0].ID, "monthly", "confirmed", "Servers", "the operator, in a test")
	if err != nil {
		t.Fatal(err)
	}
	// One payment a month, the two August charges summed: 40, 52, 52, 52.
	if bill.Status != "confirmed" || bill.PaymentCount != 6 || bill.ExpectedCents != 5200 || bill.Cadence != "monthly" || bill.LastPaidOn != "2026-10-01" {
		t.Fatalf("bill = %+v", bill)
	}
	if _, err := s.AddRecurringBill(list[1].ID, "monthly", "confirmed", "", "the operator, in a test"); !errors.Is(err, ErrConflict) {
		t.Fatalf("naming the same payments twice = %v, want a conflict", err)
	}
	// Detecting again keeps the operator's cadence and decision.
	if _, err := s.DetectRecurringBills(); err != nil {
		t.Fatal(err)
	}
	again, _ := s.GetRecurringBill(bill.ID)
	if again.Status != "confirmed" || again.ExpectedCents != 5200 || again.Name != "Servers" {
		t.Fatalf("after detect: %+v", again)
	}
}
