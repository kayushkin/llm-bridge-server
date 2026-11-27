package operationroutes

import "github.com/kayushkin/llm-bridge-server/internal/httproutes"

// Mount registers the operation routes on the server's mux: asynchronous work with a durable receipt (docs/OPERATIONS.md in llm-bridge).
func (s *Service) Mount(mux httproutes.Mux) {
	mux.HandleFunc("POST /operations", s.HandleCreateOperation)
	mux.HandleFunc("GET /operations", s.HandleListOperations)
	mux.HandleFunc("GET /operations/{id}", s.HandleGetOperation)
	mux.HandleFunc("GET /operations/{id}/events", s.HandleOperationEvents)
	mux.HandleFunc("POST /operations/{id}/cancel", s.HandleCancelOperation)
	mux.HandleFunc("GET /operations/{id}/children", s.HandleListOperationChildren)
	mux.HandleFunc("GET /operation-types", s.HandleOperationTypes)
	mux.HandleFunc("GET /operation-budgets", s.HandleListOperationBudgets)
	mux.HandleFunc("GET /operation-budgets/{organization_id}", s.HandleGetOperationBudget)
	mux.HandleFunc("PUT /operation-budgets/{organization_id}", s.HandlePutOperationBudget)
	mux.HandleFunc("DELETE /operation-budgets/{organization_id}", s.HandleDeleteOperationBudget)
}
