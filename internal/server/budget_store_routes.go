package server

// budget-store moved in from the standalone service: the same routes under
// /budget-store/, answered from this process. It is a connected records
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

// budgetStoreMountPrefix is where budget-store is mounted.
const budgetStoreMountPrefix = "/budget-store"

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
	return s.connectedRecordsRoute(budgetStoreMountPrefix, "budget-store", "budget_store_off",
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
	s.mux.HandleFunc("GET /budget-store/health", budget)
	s.mux.HandleFunc("GET /budget-store/vocabulary", budget)
	s.mux.HandleFunc("GET /budget-store/institutions", budget)
	s.mux.HandleFunc("POST /budget-store/institutions", budget)
	s.mux.HandleFunc("GET /budget-store/accounts", budget)
	s.mux.HandleFunc("POST /budget-store/accounts", budget)
	s.mux.HandleFunc("GET /budget-store/accounts/{id}", budget)
	s.mux.HandleFunc("PATCH /budget-store/accounts/{id}", budget)
	s.mux.HandleFunc("POST /budget-store/accounts/{id}/description-markers", budget)
	s.mux.HandleFunc("DELETE /budget-store/accounts/{id}/description-markers/{marker_id}", budget)
	s.mux.HandleFunc("GET /budget-store/accounts/{id}/loan-schedule", budget)
	s.mux.HandleFunc("POST /budget-store/accounts/{id}/loan-schedule/apply", budget)
	s.mux.HandleFunc("GET /budget-store/accounts/{id}/loan-payment-facts", budget)
	s.mux.HandleFunc("POST /budget-store/accounts/{id}/loan-payment-facts", budget)
	s.mux.HandleFunc("POST /budget-store/accounts/{id}/opening-balance", budget)
	s.mux.HandleFunc("GET /budget-store/balance-assertions", budget)
	s.mux.HandleFunc("POST /budget-store/balance-assertions", budget)
	s.mux.HandleFunc("GET /budget-store/categories", budget)
	s.mux.HandleFunc("POST /budget-store/categories", budget)
	s.mux.HandleFunc("GET /budget-store/categories/{id}", budget)
	s.mux.HandleFunc("GET /budget-store/payees/{id}", budget)
	s.mux.HandleFunc("GET /budget-store/payees", budget)
	s.mux.HandleFunc("POST /budget-store/payees", budget)
	s.mux.HandleFunc("POST /budget-store/payees/apply-rules", budget)
	s.mux.HandleFunc("GET /budget-store/transactions", budget)
	s.mux.HandleFunc("POST /budget-store/transactions", budget)
	s.mux.HandleFunc("GET /budget-store/transactions/{id}", budget)
	s.mux.HandleFunc("PATCH /budget-store/transactions/{id}", budget)
	s.mux.HandleFunc("PUT /budget-store/transactions/{id}/categories", budget)
	s.mux.HandleFunc("POST /budget-store/transactions/{id}/undo-transfer-without-partner", budget)
	s.mux.HandleFunc("POST /budget-store/transactions/{id}/move-to-account", budget)
	s.mux.HandleFunc("POST /budget-store/transfers/match", budget)
	s.mux.HandleFunc("GET /budget-store/import-batches", budget)
	s.mux.HandleFunc("POST /budget-store/import-batches", budget)
	s.mux.HandleFunc("GET /budget-store/import-batches/{id}", budget)
	s.mux.HandleFunc("POST /budget-store/import-batches/{id}/undo", budget)
	s.mux.HandleFunc("GET /budget-store/reports/spending", budget)
	s.mux.HandleFunc("GET /budget-store/reports/monthly", budget)
	s.mux.HandleFunc("GET /budget-store/reports/by-account", budget)
	s.mux.HandleFunc("GET /budget-store/budget", budget)
	s.mux.HandleFunc("PUT /budget-store/budget-assignments", budget)
	s.mux.HandleFunc("GET /budget-store/category-paths", budget)
	s.mux.HandleFunc("GET /budget-store/category-suggestions/candidates", budget)
	s.mux.HandleFunc("GET /budget-store/category-suggestions", budget)
	s.mux.HandleFunc("POST /budget-store/category-suggestions", budget)
	s.mux.HandleFunc("POST /budget-store/category-suggestions/decisions", budget)
	s.mux.HandleFunc("POST /budget-store/recurring-bills/detect", budget)
	s.mux.HandleFunc("GET /budget-store/recurring-bills", budget)
	s.mux.HandleFunc("POST /budget-store/recurring-bills", budget)
	s.mux.HandleFunc("GET /budget-store/recurring-bills/totals", budget)
	s.mux.HandleFunc("GET /budget-store/recurring-bills/{id}", budget)
	s.mux.HandleFunc("PATCH /budget-store/recurring-bills/{id}", budget)
	s.mux.HandleFunc("GET /budget-store/trips/totals", budget)
	s.mux.HandleFunc("GET /budget-store/trips/{trip_ref}/transactions", budget)
	s.mux.HandleFunc("POST /budget-store/trips/{trip_ref}/suggest", budget)
	s.mux.HandleFunc("PUT /budget-store/trips/{trip_ref}/transactions/{transaction_id}", budget)
	s.mux.HandleFunc("POST /budget-store/marketplace-orders", budget)
	s.mux.HandleFunc("GET /budget-store/marketplace-orders", budget)
	s.mux.HandleFunc("POST /budget-store/marketplace-orders/match-transactions", budget)
	s.mux.HandleFunc("GET /budget-store/marketplace-orders/{id}", budget)
	s.mux.HandleFunc("GET /budget-store/marketplace-order-lines/{id}", budget)
	s.mux.HandleFunc("PATCH /budget-store/marketplace-order-lines/{id}", budget)
	s.mux.HandleFunc("GET /budget-store/interests", budget)
	s.mux.HandleFunc("POST /budget-store/interests", budget)
	s.mux.HandleFunc("POST /budget-store/interests/match", budget)
	s.mux.HandleFunc("GET /budget-store/interests/{id}", budget)
	s.mux.HandleFunc("POST /budget-store/interests/{id}/rules", budget)
	s.mux.HandleFunc("POST /budget-store/interests/{id}/history-sources", budget)
	s.mux.HandleFunc("GET /budget-store/interests/{id}/transactions", budget)
	s.mux.HandleFunc("PUT /budget-store/interests/{id}/transactions/{transaction_id}", budget)
	s.mux.HandleFunc("POST /budget-store/plaid/link-token", budget)
	s.mux.HandleFunc("GET /budget-store/plaid/items", budget)
	s.mux.HandleFunc("POST /budget-store/plaid/items", budget)
	s.mux.HandleFunc("PUT /budget-store/plaid/accounts/{plaid_account_id}", budget)
	s.mux.HandleFunc("POST /budget-store/plaid/sync", budget)
	s.mux.HandleFunc("GET /budget-store/links", budget)
	s.mux.HandleFunc("POST /budget-store/links", budget)
}
