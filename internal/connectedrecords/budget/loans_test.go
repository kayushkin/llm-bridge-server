package budget

import (
	"errors"
	"testing"
)

func TestLoanScheduleSplitsPaymentsAndMeetsTheStatement(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	noFeed := false
	interest, _ := s.CreateCategory(Category{Name: "Loan interest", Kind: "expense"})
	loan := mustAccount(t, s, AccountInput{Name: "Loan", Kind: "loan", HasOwnFeed: &noFeed,
		Loan: &LoanDetails{APRBasisPoints: 1200, FirstPaymentOn: "2026-02-10", ScheduledPaymentCents: 50000, InterestCategoryID: interest.ID}})
	if _, err := s.AddDescriptionMarker(loan.ID, "LOANREF0001", "autopay"); err != nil {
		t.Fatal(err)
	}
	mustImport(t, s, "wells_fargo_csv", checking.ID, wellsFargoHeader+
		`"01/01/2026","LOAN PAYOUT","3000.00","","Posted"`+"\n"+
		`"02/11/2026","AUTO PAY LOANREF0001","-500.00","","Posted"`+"\n"+
		`"03/10/2026","AUTO PAY LOANREF0001","-500.00","","Posted"`+"\n"+
		`"04/13/2026","AUTO PAY LOANREF0001","-500.00","","Posted"`+"\n")
	if _, err := s.MatchTransfers(); err != nil {
		t.Fatal(err)
	}
	payout, _ := s.ListTransactions(TransactionFilter{Query: "PAYOUT"})
	if _, err := s.MoveCategoryPostingToAccount(payout[0].ID, loan.ID, "the loan paid out"); err != nil {
		t.Fatal(err)
	}
	schedule, err := s.GetLoanSchedule(loan.ID)
	if err != nil {
		t.Fatal(err)
	}
	// First payment, effective on the due day (Feb 10), 40 days after the
	// payout: 300000 × 12% × 40 / 365 = 3945.2 → 3945.
	first := schedule.Payments[0]
	if first.EffectiveOn != "2026-02-10" || first.InterestCents != 3945 || first.PrincipalCents != 46055 || first.Source != "computed" {
		t.Fatalf("first payment = %+v", first)
	}
	if schedule.ScheduleAppliedToAll {
		t.Fatal("the schedule claims to be applied before it was")
	}
	// A statement for the April payment says 2026.00 interest... a made-up
	// split one cent off from the arithmetic, so the March payment must
	// take up the cent.
	third := schedule.Payments[2]
	prior := third.BalanceAfterCents + third.PrincipalCents
	if _, err := s.RecordLoanPaymentFacts(loan.ID, []LoanPaymentFact{{EffectiveOn: third.EffectiveOn, InterestCents: third.InterestCents, PrincipalCents: third.PrincipalCents,
		PrincipalBalanceAfterCents: prior - third.PrincipalCents - 1, Source: "statement 04/20/26"}}); err != nil {
		t.Fatal(err)
	}
	schedule, err = s.GetLoanSchedule(loan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if schedule.Payments[1].Source != "computed, adjusted to meet a statement" || schedule.Payments[2].Source != "statement" {
		t.Fatalf("sources = %q, %q", schedule.Payments[1].Source, schedule.Payments[2].Source)
	}
	if schedule.Payments[2].BalanceAfterCents != prior-third.PrincipalCents-1 {
		t.Fatalf("balance after the statement payment = %d, want the statement's %d", schedule.Payments[2].BalanceAfterCents, prior-third.PrincipalCents-1)
	}
	if len(schedule.Projected) == 0 || schedule.Projected[len(schedule.Projected)-1].BalanceAfterCents != 0 {
		t.Fatalf("the projection does not pay the loan off: %+v", schedule.Projected)
	}
	applied, err := s.ApplyLoanSchedule(loan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.ScheduleAppliedToAll {
		t.Fatal("after apply the schedule is not on the postings")
	}
	if got := balance(t, s, loan.ID); got != -applied.PrincipalOwedCents {
		t.Fatalf("loan balance %d, schedule says %d owed", got, applied.PrincipalOwedCents)
	}
	// Applying twice changes nothing.
	again, err := s.ApplyLoanSchedule(loan.ID)
	if err != nil || again.PaidInterestCents != applied.PaidInterestCents || balance(t, s, loan.ID) != -applied.PrincipalOwedCents {
		t.Fatalf("a second apply moved something: %+v, %v", again, err)
	}
	spending, _ := s.SpendingByAccountAndMonth([]string{interest.ID}, "2026-01", "2026-12")
	var total int64
	for _, row := range spending {
		total += row.TotalCents
		if row.AccountID != checking.ID {
			t.Fatalf("interest attributed to %s, want the paying account %s", row.AccountID, checking.ID)
		}
	}
	if total != applied.PaidInterestCents {
		t.Fatalf("interest by month sums to %d, schedule says %d", total, applied.PaidInterestCents)
	}
}

func TestLoanScheduleRefusesWhatItCannotDo(t *testing.T) {
	s := openTestStore(t)
	checking := mustAccount(t, s, AccountInput{Name: "Checking", Kind: "checking"})
	if _, err := s.GetLoanSchedule(checking.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("a schedule for a checking account = %v, want invalid", err)
	}
	loan := mustAccount(t, s, AccountInput{Name: "Loan", Kind: "loan", Loan: &LoanDetails{APRBasisPoints: 500}})
	if _, err := s.GetLoanSchedule(loan.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("a schedule with no payout = %v, want a conflict", err)
	}
	if _, err := s.RecordLoanPaymentFacts(loan.ID, []LoanPaymentFact{{EffectiveOn: "2026-01-10", InterestCents: 1, PrincipalCents: 1}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a fact with no source = %v, want invalid", err)
	}
}
