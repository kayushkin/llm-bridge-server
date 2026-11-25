package budgetstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
)

// maxImportBytes bounds one imported file. Ten years of a busy checking
// account is well under 5 MB.
const maxImportBytes = 32 << 20

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("write response: %v", err)
	}
}

func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, ErrPlaidOff):
		status = http.StatusServiceUnavailable
	}
	var plaidErr *PlaidError
	if errors.As(err, &plaidErr) {
		status = http.StatusBadGateway
	}
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
	}
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

// decodeBody reads one JSON object and refuses unknown keys, so a misspelled
// field is a 400 rather than a silent no-op.
func decodeBody(r *http.Request, into any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: body: %v", ErrInvalid, err)
	}
	return nil
}

// decodeLargeBody is decodeBody for the bulk routes, whose bodies carry a
// whole run of suggestions or decisions.
func decodeLargeBody(r *http.Request, into any) error {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 16<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%w: body: %v", ErrInvalid, err)
	}
	return nil
}

func optionalWholeNumber(r *http.Request, name string) (int, error) {
	value := r.URL.Query().Get(name)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("%w: %s %q is not a whole number", ErrInvalid, name, value)
	}
	return parsed, nil
}

func respond[T any](w http.ResponseWriter, status int, value T, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, status, value)
}

// RegisterHandlers mounts every route on mux. CONTRACT.md is the route table.
func RegisterHandlers(mux *http.ServeMux, s *Store) {
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /vocabulary", handleVocabulary)

	mux.HandleFunc("GET /institutions", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListInstitutions()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /institutions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name    string `json:"name"`
			Website string `json:"website"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateInstitution(Institution{Name: in.Name, Website: in.Website})
		respond(w, http.StatusCreated, out, err)
	})

	mux.HandleFunc("GET /accounts", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListAccounts()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /accounts", func(w http.ResponseWriter, r *http.Request) {
		var in AccountInput
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateAccount(in)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetAccount(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PATCH /accounts/{id}", func(w http.ResponseWriter, r *http.Request) {
		var patch AccountPatch
		if err := decodeBody(r, &patch); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.PatchAccount(r.PathValue("id"), patch)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /accounts/{id}/description-markers", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Marker string `json:"marker"`
			Note   string `json:"note"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.AddDescriptionMarker(r.PathValue("id"), in.Marker, in.Note)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("DELETE /accounts/{id}/description-markers/{marker_id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.DeleteDescriptionMarker(r.PathValue("id"), r.PathValue("marker_id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /accounts/{id}/loan-schedule", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetLoanSchedule(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /accounts/{id}/loan-schedule/apply", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.ApplyLoanSchedule(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /accounts/{id}/loan-payment-facts", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.ListLoanPaymentFacts(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /accounts/{id}/loan-payment-facts", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Facts []LoanPaymentFact `json:"facts"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.RecordLoanPaymentFacts(r.PathValue("id"), in.Facts)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /accounts/{id}/opening-balance", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AsOf         string `json:"as_of"`
			BalanceCents int64  `json:"balance_cents"`
			Source       string `json:"source"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetOpeningBalance(BalanceAssertionInput{AccountID: r.PathValue("id"), AsOf: in.AsOf, BalanceCents: in.BalanceCents, Source: in.Source})
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /balance-assertions", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.ListBalanceAssertions(r.URL.Query().Get("account_id"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /balance-assertions", func(w http.ResponseWriter, r *http.Request) {
		var in BalanceAssertionInput
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateBalanceAssertion(in)
		respond(w, http.StatusCreated, out, err)
	})

	mux.HandleFunc("GET /categories", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListCategories()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /categories", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ParentID string `json:"parent_id"`
			Name     string `json:"name"`
			Kind     string `json:"kind"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateCategory(Category{ParentID: in.ParentID, Name: in.Name, Kind: in.Kind})
		respond(w, http.StatusCreated, out, err)
	})

	mux.HandleFunc("GET /categories/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetCategory(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /payees/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetPayee(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /payees", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListPayees()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /payees", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name              string   `json:"name"`
			DefaultCategoryID string   `json:"default_category_id"`
			Patterns          []string `json:"patterns"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreatePayee(Payee{Name: in.Name, DefaultCategoryID: in.DefaultCategoryID, Patterns: in.Patterns})
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("POST /payees/apply-rules", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.ApplyPayeeRules()
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /transactions", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		filter := TransactionFilter{
			AccountID:            query.Get("account_id"),
			CategoryID:           query.Get("category_id"),
			Month:                query.Get("month"),
			Query:                query.Get("q"),
			Uncategorized:        query.Get("uncategorized") == "true",
			IncludeSubcategories: query.Get("include_subcategories") == "true",
		}
		for name, target := range map[string]*int{"limit": &filter.Limit, "offset": &filter.Offset} {
			if value := query.Get(name); value != "" {
				parsed, err := strconv.Atoi(value)
				if err != nil || parsed < 0 {
					writeError(w, fmt.Errorf("%w: %s %q is not a whole number", ErrInvalid, name, value))
					return
				}
				*target = parsed
			}
		}
		list, err := s.ListTransactions(filter)
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /transactions", func(w http.ResponseWriter, r *http.Request) {
		var in TransactionInput
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateTransaction(in)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET /transactions/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetTransaction(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PATCH /transactions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var patch TransactionPatch
		if err := decodeBody(r, &patch); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.PatchTransaction(r.PathValue("id"), patch)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PUT /transactions/{id}/categories", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Splits []CategorySplit `json:"splits"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetTransactionCategories(r.PathValue("id"), in.Splits)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /transactions/{id}/undo-transfer-without-partner", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.UndoTransferWithoutPartner(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /transactions/{id}/move-to-account", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccountID string `json:"account_id"`
			Reason    string `json:"reason"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.MoveCategoryPostingToAccount(r.PathValue("id"), in.AccountID, in.Reason)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /transfers/match", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.MatchTransfers()
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /import-batches", func(w http.ResponseWriter, _ *http.Request) {
		list, err := s.ListImportBatches()
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /import-batches", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		out, err := s.Import(ImportRequest{
			Source:    query.Get("source"),
			AccountID: query.Get("account_id"),
			FileName:  query.Get("file_name"),
			FileID:    query.Get("file_id"),
		}, http.MaxBytesReader(w, r.Body, maxImportBytes))
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET /import-batches/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetImportBatch(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /import-batches/{id}/undo", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.UndoImportBatch(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /reports/spending", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.SpendingByCategory(r.URL.Query().Get("month"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("GET /reports/monthly", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.SpendingByMonth(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("GET /reports/by-account", func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		list, err := s.SpendingByAccountAndMonth(query["category_id"], query.Get("from"), query.Get("to"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("GET /budget", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetBudget(r.URL.Query().Get("month"), r.URL.Query().Get("view"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PUT /budget-assignments", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			CategoryID  string `json:"category_id"`
			Month       string `json:"month"`
			AmountCents int64  `json:"amount_cents"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetBudgetAssignment(BudgetAssignment{CategoryID: in.CategoryID, Month: in.Month, AmountCents: in.AmountCents})
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /category-paths", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.CategoryPaths()
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /category-suggestions/candidates", func(w http.ResponseWriter, r *http.Request) {
		limit, err := optionalWholeNumber(r, "limit")
		if err != nil {
			writeError(w, err)
			return
		}
		out, err := s.ListSuggestionCandidates(limit, r.URL.Query().Get("before"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /category-suggestions", func(w http.ResponseWriter, r *http.Request) {
		limit, err := optionalWholeNumber(r, "limit")
		if err != nil {
			writeError(w, err)
			return
		}
		out, err := s.ListSuggestions(r.URL.Query().Get("status"), limit)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /category-suggestions", func(w http.ResponseWriter, r *http.Request) {
		var in SuggestionBatch
		if err := decodeLargeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		count, err := s.CreateSuggestions(in)
		respond(w, http.StatusCreated, map[string]int{"created": count}, err)
	})
	mux.HandleFunc("POST /category-suggestions/decisions", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			DecidedBy string               `json:"decided_by"`
			Decisions []SuggestionDecision `json:"decisions"`
		}
		if err := decodeLargeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.DecideSuggestions(in.DecidedBy, in.Decisions)
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("POST /recurring-bills/detect", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.DetectRecurringBills()
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /recurring-bills", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.ListRecurringBills(r.URL.Query().Get("status"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /recurring-bills", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TransactionID string `json:"transaction_id"`
			Cadence       string `json:"cadence"`
			Status        string `json:"status"`
			Name          string `json:"name"`
			DecidedBy     string `json:"decided_by"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.AddRecurringBill(in.TransactionID, in.Cadence, in.Status, in.Name, in.DecidedBy)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("GET /recurring-bills/totals", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.GetRecurringTotals()
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /recurring-bills/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetRecurringBill(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PATCH /recurring-bills/{id}", func(w http.ResponseWriter, r *http.Request) {
		var patch RecurringBillPatch
		if err := decodeBody(r, &patch); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.PatchRecurringBill(r.PathValue("id"), patch)
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /trips/totals", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.ListTripTotals()
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /trips/{trip_ref}/transactions", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.ListTripTransactions(r.PathValue("trip_ref"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /trips/{trip_ref}/suggest", func(w http.ResponseWriter, r *http.Request) {
		var in TripSuggestRequest
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		count, err := s.SuggestTripTransactions(r.PathValue("trip_ref"), in)
		respond(w, http.StatusOK, map[string]int{"suggested": count}, err)
	})
	mux.HandleFunc("PUT /trips/{trip_ref}/transactions/{transaction_id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Status    string `json:"status"`
			DecidedBy string `json:"decided_by"`
			Reason    string `json:"reason"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetTripTransaction(r.PathValue("trip_ref"), r.PathValue("transaction_id"), in.Status, in.DecidedBy, in.Reason)
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("POST /marketplace-orders", func(w http.ResponseWriter, r *http.Request) {
		var in MarketplaceOrder
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		order, err := s.CreateMarketplaceOrder(in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, order)
	})
	mux.HandleFunc("GET /marketplace-orders", func(w http.ResponseWriter, r *http.Request) {
		limit, err := optionalWholeNumber(r, "limit")
		if err != nil {
			writeError(w, err)
			return
		}
		offset, err := optionalWholeNumber(r, "offset")
		if err != nil {
			writeError(w, err)
			return
		}
		orders, err := s.ListMarketplaceOrders(MarketplaceOrderFilter{TransactionID: r.URL.Query().Get("transaction_id"),
			Unmatched: r.URL.Query().Get("unmatched") == "true", Limit: limit, Offset: offset})
		respond(w, http.StatusOK, orders, err)
	})
	mux.HandleFunc("POST /marketplace-orders/match-transactions", func(w http.ResponseWriter, _ *http.Request) {
		counts, err := s.MatchMarketplaceOrders()
		respond(w, http.StatusOK, counts, err)
	})
	mux.HandleFunc("GET /marketplace-orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		order, err := s.GetMarketplaceOrder(r.PathValue("id"))
		respond(w, http.StatusOK, order, err)
	})
	mux.HandleFunc("GET /marketplace-order-lines/{id}", func(w http.ResponseWriter, r *http.Request) {
		line, err := s.GetMarketplaceOrderLine(r.PathValue("id"))
		respond(w, http.StatusOK, line, err)
	})
	mux.HandleFunc("PATCH /marketplace-order-lines/{id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Disposition string `json:"disposition"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		line, err := s.SetOrderLineDisposition(r.PathValue("id"), in.Disposition)
		respond(w, http.StatusOK, line, err)
	})
	mux.HandleFunc("GET /interests", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.ListInterests()
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /interests", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateInterest(in.Name, in.Description)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("POST /interests/match", func(w http.ResponseWriter, _ *http.Request) {
		count, err := s.MatchInterests()
		respond(w, http.StatusOK, map[string]int{"added": count}, err)
	})
	mux.HandleFunc("GET /interests/{id}", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.GetInterest(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /interests/{id}/rules", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.AddInterestRule(r.PathValue("id"), in.Kind, in.Value)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /interests/{id}/history-sources", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Kind string `json:"kind"`
			Ref  string `json:"ref"`
			Note string `json:"note"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.AddInterestHistorySource(r.PathValue("id"), in.Kind, in.Ref, in.Note)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("GET /interests/{id}/transactions", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.ListInterestTransactions(r.PathValue("id"))
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("PUT /interests/{id}/transactions/{transaction_id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Status    string `json:"status"`
			DecidedBy string `json:"decided_by"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.SetInterestTransaction(r.PathValue("id"), r.PathValue("transaction_id"), in.Status, in.DecidedBy)
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("POST /plaid/link-token", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RedirectURI string `json:"redirect_uri"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		token, err := s.CreatePlaidLinkToken(r.Context(), in.RedirectURI)
		respond(w, http.StatusOK, map[string]string{"link_token": token}, err)
	})
	mux.HandleFunc("GET /plaid/items", func(w http.ResponseWriter, _ *http.Request) {
		out, err := s.ListPlaidItems()
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /plaid/items", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			PublicToken     string `json:"public_token"`
			InstitutionID   string `json:"institution_id"`
			InstitutionName string `json:"institution_name"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.LinkPlaidItem(r.Context(), in.PublicToken, in.InstitutionID, in.InstitutionName)
		respond(w, http.StatusCreated, out, err)
	})
	mux.HandleFunc("PUT /plaid/accounts/{plaid_account_id}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			AccountID  string `json:"account_id"`
			SyncFromOn string `json:"sync_from_on"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.MatchPlaidAccount(r.PathValue("plaid_account_id"), in.AccountID, in.SyncFromOn)
		respond(w, http.StatusOK, out, err)
	})
	mux.HandleFunc("POST /plaid/sync", func(w http.ResponseWriter, r *http.Request) {
		out, err := s.SyncPlaid(r.Context())
		respond(w, http.StatusOK, out, err)
	})

	mux.HandleFunc("GET /links", func(w http.ResponseWriter, r *http.Request) {
		list, err := s.ListLinks(r.URL.Query().Get("from_id"))
		respond(w, http.StatusOK, list, err)
	})
	mux.HandleFunc("POST /links", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			FromID     string `json:"from_id"`
			EntityType string `json:"entity_type"`
			EntityRef  string `json:"entity_ref"`
			Note       string `json:"note"`
		}
		if err := decodeBody(r, &in); err != nil {
			writeError(w, err)
			return
		}
		out, err := s.CreateLink(Link{FromID: in.FromID, EntityType: in.EntityType, EntityRef: in.EntityRef, Note: in.Note})
		respond(w, http.StatusCreated, out, err)
	})
}
