package server

// budget-store moved in from the standalone service: the same routes under
// /budget/, answered from this process. It is a connected records
// store like people-store: every row has an owner, and each person keeps
// books of their own. Every route works in one owner's books — the books of
// whoever keeps the record its path names, or the caller's own (for an
// administrator or this server's own service, the books owner_principal_id
// names) — and another owner's record answers 404. The caller reaches the
// store in the request context, set by connectedRecordsRoute from the
// credential request authorization verified.

import (
	"errors"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/budget"
)

// budgetMountPrefix is where budget-store is mounted.
const budgetMountPrefix = "/budget"

// budgetStoreModule is budget-store as this server serves it.
type budgetStoreModule struct {
	store   *budget.Store
	handler http.Handler
}

// EnableBudgetStore serves budget-store from store. main calls it once,
// after EnablePhotosAndLocations (trip matching reads location-store's
// journeys) and before the server listens. Until then its routes answer 503.
func (s *Server) EnableBudgetStore(store *budget.Store) error {
	if store == nil {
		return errors.New("budget-store: the store is required")
	}
	mux := http.NewServeMux()
	budget.RegisterHandlers(mux, store)
	s.budgetStore.Store(&budgetStoreModule{store: store, handler: mux})
	return nil
}

func (s *Server) budgetStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(budgetMountPrefix, "budget-store", "budget_store_off",
		func(*connectedRecordsStores) http.Handler {
			module := s.budgetStore.Load()
			if module == nil {
				return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					writeJSONError(w, http.StatusServiceUnavailable, "budget_store_off", "budget-store is not open on this server: EnableBudgetStore was never called")
				})
			}
			return module.handler
		}, false)
}

// registerBudgetStoreRoutes mounts budget-store.
func (s *Server) registerBudgetStoreRoutes() {
	budget := s.budgetStoreRoute()
	s.mux.HandleFunc("GET /budget/health", budget)
	s.mux.HandleFunc("GET /budget/vocabulary", budget)
	s.mux.HandleFunc("GET /budget/institutions", budget)
	s.mux.HandleFunc("POST /budget/institutions", budget)
	s.mux.HandleFunc("GET /budget/accounts", budget)
	s.mux.HandleFunc("POST /budget/accounts", budget)
	s.mux.HandleFunc("GET /budget/accounts/{id}", budget)
	s.mux.HandleFunc("PATCH /budget/accounts/{id}", budget)
	s.mux.HandleFunc("POST /budget/accounts/{id}/description-markers", budget)
	s.mux.HandleFunc("DELETE /budget/accounts/{id}/description-markers/{marker_id}", budget)
	s.mux.HandleFunc("GET /budget/accounts/{id}/loan-schedule", budget)
	s.mux.HandleFunc("POST /budget/accounts/{id}/loan-schedule/apply", budget)
	s.mux.HandleFunc("GET /budget/accounts/{id}/loan-payment-facts", budget)
	s.mux.HandleFunc("POST /budget/accounts/{id}/loan-payment-facts", budget)
	s.mux.HandleFunc("POST /budget/accounts/{id}/opening-balance", budget)
	s.mux.HandleFunc("GET /budget/balance-assertions", budget)
	s.mux.HandleFunc("POST /budget/balance-assertions", budget)
	s.mux.HandleFunc("GET /budget/categories", budget)
	s.mux.HandleFunc("POST /budget/categories", budget)
	s.mux.HandleFunc("GET /budget/categories/{id}", budget)
	s.mux.HandleFunc("GET /budget/payees/{id}", budget)
	s.mux.HandleFunc("GET /budget/payees", budget)
	s.mux.HandleFunc("POST /budget/payees", budget)
	s.mux.HandleFunc("POST /budget/payees/apply-rules", budget)
	s.mux.HandleFunc("GET /budget/transactions", budget)
	s.mux.HandleFunc("POST /budget/transactions", budget)
	s.mux.HandleFunc("GET /budget/transactions/{id}", budget)
	s.mux.HandleFunc("PATCH /budget/transactions/{id}", budget)
	s.mux.HandleFunc("PUT /budget/transactions/{id}/categories", budget)
	s.mux.HandleFunc("POST /budget/transactions/{id}/undo-transfer-without-partner", budget)
	s.mux.HandleFunc("POST /budget/transactions/{id}/move-to-account", budget)
	s.mux.HandleFunc("POST /budget/transfers/match", budget)
	s.mux.HandleFunc("GET /budget/import-batches", budget)
	s.mux.HandleFunc("POST /budget/import-batches", budget)
	s.mux.HandleFunc("GET /budget/import-batches/{id}", budget)
	s.mux.HandleFunc("POST /budget/import-batches/{id}/undo", budget)
	s.mux.HandleFunc("GET /budget/reports/spending", budget)
	s.mux.HandleFunc("GET /budget/reports/monthly", budget)
	s.mux.HandleFunc("GET /budget/reports/by-account", budget)
	s.mux.HandleFunc("GET /budget/budget", budget)
	s.mux.HandleFunc("PUT /budget/budget-assignments", budget)
	s.mux.HandleFunc("GET /budget/category-paths", budget)
	s.mux.HandleFunc("GET /budget/category-suggestions/candidates", budget)
	s.mux.HandleFunc("GET /budget/category-suggestions", budget)
	s.mux.HandleFunc("POST /budget/category-suggestions", budget)
	s.mux.HandleFunc("POST /budget/category-suggestions/decisions", budget)
	s.mux.HandleFunc("POST /budget/recurring-bills/detect", budget)
	s.mux.HandleFunc("GET /budget/recurring-bills", budget)
	s.mux.HandleFunc("POST /budget/recurring-bills", budget)
	s.mux.HandleFunc("GET /budget/recurring-bills/totals", budget)
	s.mux.HandleFunc("GET /budget/recurring-bills/{id}", budget)
	s.mux.HandleFunc("PATCH /budget/recurring-bills/{id}", budget)
	s.mux.HandleFunc("GET /budget/trips/totals", budget)
	s.mux.HandleFunc("GET /budget/trips/{trip_ref}/transactions", budget)
	s.mux.HandleFunc("POST /budget/trips/{trip_ref}/suggest", budget)
	s.mux.HandleFunc("PUT /budget/trips/{trip_ref}/transactions/{transaction_id}", budget)
	s.mux.HandleFunc("POST /budget/marketplace-orders", budget)
	s.mux.HandleFunc("GET /budget/marketplace-orders", budget)
	s.mux.HandleFunc("POST /budget/marketplace-orders/match-transactions", budget)
	s.mux.HandleFunc("GET /budget/marketplace-orders/{id}", budget)
	s.mux.HandleFunc("GET /budget/marketplace-order-lines/{id}", budget)
	s.mux.HandleFunc("PATCH /budget/marketplace-order-lines/{id}", budget)
	s.mux.HandleFunc("GET /budget/interests", budget)
	s.mux.HandleFunc("POST /budget/interests", budget)
	s.mux.HandleFunc("POST /budget/interests/match", budget)
	s.mux.HandleFunc("GET /budget/interests/{id}", budget)
	s.mux.HandleFunc("POST /budget/interests/{id}/rules", budget)
	s.mux.HandleFunc("POST /budget/interests/{id}/history-sources", budget)
	s.mux.HandleFunc("GET /budget/interests/{id}/transactions", budget)
	s.mux.HandleFunc("PUT /budget/interests/{id}/transactions/{transaction_id}", budget)
	s.mux.HandleFunc("POST /budget/plaid/link-token", budget)
	s.mux.HandleFunc("GET /budget/plaid/items", budget)
	s.mux.HandleFunc("POST /budget/plaid/items", budget)
	s.mux.HandleFunc("PUT /budget/plaid/accounts/{plaid_account_id}", budget)
	s.mux.HandleFunc("POST /budget/plaid/sync", budget)
	s.mux.HandleFunc("GET /budget/links", budget)
	s.mux.HandleFunc("POST /budget/links", budget)
}
