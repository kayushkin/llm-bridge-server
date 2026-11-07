package budgetstore

import (
	"fmt"
	"net/http"
	"slices"
)

// Each closed set of values the store accepts, in one place. GET /vocabulary
// serves them so a caller never has to copy a list.

var accountKinds = []string{"checking", "savings", "cash", "credit_card", "loan", "retirement", "brokerage", "payment_service"}

// liabilityAccountKinds hold a debt: their balance is negative when money is owed.
var liabilityAccountKinds = []string{"credit_card", "loan"}

var retirementPlanKinds = []string{"401k", "ira", "hsa", "other"}

var taxTreatments = []string{"traditional", "roth", "mixed"}

// categoryKinds: expense and income are the budget's categories; equity holds
// opening balances and corrections; uncategorized holds what nobody sorted yet.
var categoryKinds = []string{"expense", "income", "equity", "uncategorized"}

// categoryRoles name the few categories the store itself posts to. Each role
// is held by at most one category, created on first boot.
var categoryRoles = []string{"uncategorized", "opening_balances"}

var transactionStatuses = []string{"pending", "posted", "void"}

// importSources are the file formats the store can read. importing.go has one
// parser per source.
var importSources = []string{"wells_fargo_csv", "inspira_csv"}

// importRowOutcomes say what became of each source row.
var importRowOutcomes = []string{
	"created",          // a new transaction
	"duplicate",        // an earlier, still-live batch already holds this row
	"skipped_internal", // a move inside the account that changes no balance (a cash sweep)
}

// billCadences are the rhythms the bill detector recognizes, with the days
// one period spans and how many periods make a year.
var billCadences = []string{"weekly", "monthly", "quarterly", "yearly"}

// billStatuses: suggested waits for the operator; confirmed is a bill;
// rejected is not one; ended was a bill and has stopped.
var billStatuses = []string{"suggested", "confirmed", "rejected", "ended"}

// tripTransactionStatuses: suggested waits for the operator; confirmed
// belongs to the trip; removed does not, and is never suggested again.
var tripTransactionStatuses = []string{"suggested", "confirmed", "removed"}

// interestRuleKinds say what an interest rule matches: a category and every
// category under it, a payee, or upper-cased text in the description.
var interestRuleKinds = []string{"category", "payee", "description"}

// interestHistoryKinds name where an interest's history lives elsewhere.
var interestHistoryKinds = []string{"location_place", "calendar_search"}

func requireOneOf(field, value string, allowed []string) error {
	if !slices.Contains(allowed, value) {
		return fmt.Errorf("%w: %s %q is not one of %v", ErrInvalid, field, value, allowed)
	}
	return nil
}

func handleVocabulary(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"account_kinds":             accountKinds,
		"liability_account_kinds":   liabilityAccountKinds,
		"retirement_plan_kinds":     retirementPlanKinds,
		"tax_treatments":            taxTreatments,
		"category_kinds":            categoryKinds,
		"category_roles":            categoryRoles,
		"transaction_statuses":      transactionStatuses,
		"import_sources":            importSources,
		"import_row_outcomes":       importRowOutcomes,
		"budget_views":              budgetViews,
		"suggestion_statuses":       suggestionStatuses,
		"bill_cadences":             billCadences,
		"bill_statuses":             billStatuses,
		"trip_transaction_statuses": tripTransactionStatuses,
		"interest_rule_kinds":       interestRuleKinds,
		"interest_history_kinds":    interestHistoryKinds,
	})
}
