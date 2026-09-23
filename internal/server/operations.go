package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// The operation routes. The coordinator (internal/operations) does the work;
// these handlers decide who is asking, check the organization with
// principal-store, and translate the coordinator's answers to HTTP. The
// contract is docs/OPERATIONS.md in llm-bridge.

// EnableOperations attaches the coordinator the operation routes use. Until
// it is called they answer 503 operations_not_configured.
func (s *Server) EnableOperations(coordinator *operations.Coordinator) {
	s.operations = coordinator
}

func (s *Server) operationsOrRefuse(w http.ResponseWriter) (*operations.Coordinator, bool) {
	if s.operations == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "operations_not_configured", "this server was started without an operations coordinator")
		return nil, false
	}
	return s.operations, true
}

// maximumOperationIntentBytes bounds one intent. Content a store owns travels
// by reference, so an intent is small.
const maximumOperationIntentBytes = 4 << 20

func (s *Server) handleCreateOperation(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	var intent msg.OperationIntent
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maximumOperationIntentBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_intent", "body is not an operation intent: "+err.Error())
		return
	}
	// Whatever the body says, the operation is the verified caller's.
	intent.PrincipalID, _ = principalIdentityOfRequest(r)
	if status, code, message := s.checkOperationOrganization(r, intent.OrganizationID); status != 0 {
		writeJSONError(w, status, code, message)
		return
	}
	if status, code, message := s.checkOperationGrant(r, intent.Type); status != 0 {
		writeJSONError(w, status, code, message)
		return
	}
	receipt, created, err := coordinator.Submit(intent)
	var intentError *operations.IntentError
	var budgetError *operations.BudgetError
	switch {
	case errors.As(err, &intentError):
		writeJSONError(w, http.StatusBadRequest, intentError.Code, intentError.Message)
		return
	case errors.As(err, &budgetError):
		writeJSONError(w, http.StatusPaymentRequired, budgetError.Code, budgetError.Message)
		return
	case errors.Is(err, operationstore.ErrIdempotencyKeyReused):
		writeJSONError(w, http.StatusConflict, "idempotency_key_reused", err.Error())
		return
	case err != nil:
		log.Printf("operations: submit: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "operation_not_stored", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Location", "/operations/"+receipt.ID)
	if created {
		w.WriteHeader(http.StatusAccepted)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	json.NewEncoder(w).Encode(receipt)
}

// checkOperationOrganization makes sure the organization is an active
// principal-store group and, for a principal who is not an administrator,
// one they belong to. A zero status means it passed.
func (s *Server) checkOperationOrganization(r *http.Request, organizationID string) (status int, code, message string) {
	if status, code, message := s.checkOrganizationIsAnActiveGroup(r, organizationID); status != 0 {
		return status, code, message
	}
	principalID, restricted := principalRestrictingRequest(r)
	if !restricted {
		return 0, "", ""
	}
	groupIDs, err := s.principalClient.GroupIDsOf(r.Context(), principalID)
	if err != nil {
		return http.StatusBadGateway, "principal_store_unavailable", err.Error()
	}
	for _, groupID := range groupIDs {
		if groupID == organizationID {
			return 0, "", ""
		}
	}
	return http.StatusForbidden, "not_a_member_of_organization", fmt.Sprintf("%s is not a member of %s", principalID, organizationID)
}

// checkOrganizationIsAnActiveGroup makes sure principal-store has the
// organization as an active group. A zero status means it passed.
func (s *Server) checkOrganizationIsAnActiveGroup(r *http.Request, organizationID string) (status int, code, message string) {
	if organizationID == "" {
		return http.StatusBadRequest, "organization_required", "organization_id is required: the principal-store group the operation runs for"
	}
	organization, err := s.principalClient.Get(r.Context(), organizationID)
	switch {
	case errors.Is(err, principalclient.ErrNotFound):
		return http.StatusBadRequest, "unknown_organization", fmt.Sprintf("principal-store has no principal %s", organizationID)
	case err != nil:
		return http.StatusBadGateway, "principal_store_unavailable", err.Error()
	case organization.Kind != "group":
		return http.StatusBadRequest, "organization_not_group", fmt.Sprintf("%s is a %s; an organization is a principal-store group", organizationID, organization.Kind)
	case organization.DisabledAt != 0:
		return http.StatusBadRequest, "organization_disabled", fmt.Sprintf("%s is disabled", organizationID)
	}
	return 0, "", ""
}

func (s *Server) handleGetOperation(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	operation, err := coordinator.Store().Get(r.PathValue("id"))
	if writeOperationStoreError(w, err) {
		return
	}
	writeJSON(w, operation.Receipt)
}

func (s *Server) handleCancelOperation(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	receipt, err := coordinator.Cancel(r.Context(), r.PathValue("id"))
	if errors.Is(err, operationstore.ErrTerminal) {
		operation, readError := coordinator.Store().Get(r.PathValue("id"))
		if writeOperationStoreError(w, readError) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		json.NewEncoder(w).Encode(map[string]any{
			"error":   map[string]string{"code": "operation_finished", "message": fmt.Sprintf("the operation already finished as %s", operation.Receipt.State)},
			"receipt": operation.Receipt,
		})
		return
	}
	if writeOperationStoreError(w, err) {
		return
	}
	writeJSON(w, receipt)
}

// maximumOperationListLimit bounds GET /operations and the children route.
const maximumOperationListLimit = 500

func (s *Server) handleListOperationChildren(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	parentID := r.PathValue("id")
	if _, err := coordinator.Store().Get(parentID); writeOperationStoreError(w, err) {
		return
	}
	children, err := coordinator.Store().List(operationstore.Filter{ParentOperationID: parentID, Limit: maximumOperationListLimit})
	if writeOperationStoreError(w, err) {
		return
	}
	writeJSON(w, children)
}

func (s *Server) handleListOperations(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	query := r.URL.Query()
	filter := operationstore.Filter{
		OrganizationID: query.Get("organization_id"),
		PrincipalID:    query.Get("principal_id"),
		Type:           msg.OperationType(query.Get("type")),
		State:          msg.OperationState(query.Get("state")),
		Limit:          100,
	}
	if filter.State != "" && !filter.State.IsKnown() {
		writeJSONError(w, http.StatusBadRequest, "unknown_state", fmt.Sprintf("state %q is not one of %v", filter.State, msg.AllOperationStates))
		return
	}
	if principalID, restricted := principalRestrictingRequest(r); restricted {
		if filter.PrincipalID != "" && filter.PrincipalID != principalID {
			writeJSON(w, []msg.OperationReceipt{})
			return
		}
		filter.PrincipalID = principalID
	}
	for name, target := range map[string]*time.Time{"created_after": &filter.CreatedAfter, "created_before": &filter.CreatedBefore} {
		if value := query.Get(name); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				writeJSONError(w, http.StatusBadRequest, "invalid_"+name, fmt.Sprintf("%s must be an RFC 3339 time: %v", name, err))
				return
			}
			*target = parsed
		}
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > maximumOperationListLimit {
			writeJSONError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", maximumOperationListLimit))
			return
		}
		filter.Limit = limit
	}
	receipts, err := coordinator.Store().List(filter)
	if writeOperationStoreError(w, err) {
		return
	}
	writeJSON(w, receipts)
}

func (s *Server) handleOperationTypes(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	writeJSON(w, coordinator.Types())
}

// handleOperationEvents streams an operation's events: every stored one after
// Last-Event-ID, then each new one, and closes once the operation and all its
// children have finished and every event has been sent. The
// SSE id of each frame is its sequence, which is the receipt's revision.
func (s *Server) handleOperationEvents(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	operationID := r.PathValue("id")
	if _, err := coordinator.Store().Get(operationID); writeOperationStoreError(w, err) {
		return
	}
	var lastSequence int64
	if value := r.Header.Get("Last-Event-ID"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed < 0 {
			writeJSONError(w, http.StatusBadRequest, "invalid_last_event_id", "Last-Event-ID must be an event sequence number")
			return
		}
		lastSequence = parsed
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	// Subscribe before the first read, so a change between the read and the
	// wait still wakes the loop.
	changed, unsubscribe := coordinator.Changes().Subscribe(operationID)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()
	for {
		events, err := coordinator.Store().EventsAfter(operationID, lastSequence)
		if err != nil {
			log.Printf("operations: %s: read events for stream: %v", operationID, err)
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", strconv.Quote(err.Error()))
			flusher.Flush()
			return
		}
		for _, event := range events {
			frame, err := json.Marshal(event)
			if err != nil {
				log.Printf("operations: %s: encode event %d: %v", operationID, event.Sequence, err)
				return
			}
			if _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", event.Sequence, event.Kind, frame); err != nil {
				return
			}
			lastSequence = event.Sequence
		}
		if len(events) > 0 {
			flusher.Flush()
		}
		operation, err := coordinator.Store().Get(operationID)
		if err != nil {
			log.Printf("operations: %s: read receipt for stream: %v", operationID, err)
			return
		}
		if operation.Receipt.Revision == lastSequence && operationAndChildrenFinished(operation.Receipt) {
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-changed:
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// operationAndChildrenFinished reports whether nothing more will happen to an
// operation: it is terminal and none of its children is still queued or
// running.
func operationAndChildrenFinished(receipt msg.OperationReceipt) bool {
	if !receipt.State.IsTerminal() {
		return false
	}
	if receipt.Children == nil {
		return true
	}
	return receipt.Children.ByState[msg.OperationStateQueued] == 0 && receipt.Children.ByState[msg.OperationStateRunning] == 0
}

// writeOperationStoreError writes the answer for a store error and reports
// whether there was one.
func writeOperationStoreError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, operationstore.ErrNotFound):
		http.Error(w, "operation not found", http.StatusNotFound)
	default:
		log.Printf("operations: %v", err)
		writeJSONError(w, http.StatusInternalServerError, "operation_store_failed", err.Error())
	}
	return true
}

// operationIsOwnedByPrincipal reports whether the operation exists and was
// started as principalID.
func (s *Server) operationIsOwnedByPrincipal(operationID, principalID string) bool {
	if s.operations == nil || principalID == "" {
		return false
	}
	operation, err := s.operations.Store().Get(operationID)
	return err == nil && operation.Receipt.PrincipalID == principalID
}

// OperationExecutors is every executor this server runs operations with,
// wired to its own one-shot path. main hands them to the coordinator.
func (s *Server) OperationExecutors() []operations.Executor {
	return []operations.Executor{executors.ModelClassifier{Caller: s, Taxonomies: s}, executors.LLMCompletion{Caller: s}}
}

// CompletionTarget implements executors.OneShotCaller from the stored
// settings, read at the time of use.
func (s *Server) CompletionTarget() (instanceID, defaultModel string) {
	return s.settings.String(config.SettingOperationsCompletionInstance), s.settings.String(config.SettingOperationsCompletionModel)
}

// RunOneShot implements executors.OneShotCaller with the same one-shot path
// the signal classifier uses: the instance's harness binary, its own login,
// no credential in this process.
func (s *Server) RunOneShot(ctx context.Context, instanceID string, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	if s.harnessStore == nil {
		return msg.OneShotResponse{}, errors.New("this server has no harness-store, so it has no instances to call")
	}
	instance, err := s.harnessStore.GetInstance(instanceID)
	if err != nil {
		return msg.OneShotResponse{}, fmt.Errorf("completion instance %q: %w", instanceID, err)
	}
	if !instance.Enabled {
		return msg.OneShotResponse{}, fmt.Errorf("completion instance %q is disabled", instanceID)
	}
	raw, status, err := s.runOneShot(ctx, instance, request)
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	if status != http.StatusOK {
		return msg.OneShotResponse{}, fmt.Errorf("instance %s answered the one-shot call with %d: %s", instanceID, status, strings.TrimSpace(string(raw)))
	}
	var response msg.OneShotResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return msg.OneShotResponse{}, fmt.Errorf("instance %s answered with something that is not a one-shot response: %w", instanceID, err)
	}
	return response, nil
}

// BoardTaxonomy implements executors.TaxonomyReader with kanban-store.
func (s *Server) BoardTaxonomy(ctx context.Context, boardID, principalID string) (msg.ClassificationTaxonomy, string, error) {
	if s.kanbanClient == nil {
		return msg.ClassificationTaxonomy{}, "", errors.New("kanban-store is not configured, so no board taxonomy can be read")
	}
	return s.kanbanClient.BoardTaxonomy(ctx, boardID, principalID)
}

// ModelListPrice reads a model's list price from model-store. A model it
// does not know, or one whose prices are both zero, has no known price: a
// zero there means nobody entered one, and budgets must not treat it as free.
func (s *Server) ModelListPrice(model string) (inputPerMillion, outputPerMillion float64, known bool) {
	if s.modelStore == nil || model == "" {
		return 0, 0, false
	}
	resolved, err := s.modelStore.ResolveModel(model)
	if err != nil || resolved == nil || (resolved.InputCost <= 0 && resolved.OutputCost <= 0) {
		return 0, 0, false
	}
	return resolved.InputCost, resolved.OutputCost, true
}

// operationRunGrantRelation and operationTypeResourceType are how grant-store
// spells "may start operations of this type".
const (
	operationRunGrantRelation = "can_run_operation"
	operationTypeResourceType = "operation_type"
)

// checkOperationGrant applies grant-store's can_run_operation grants to a
// principal starting an operation, as operations.grant_enforcement says. An
// administrator and the service token are not checked. A zero status means
// it passed.
func (s *Server) checkOperationGrant(r *http.Request, operationType msg.OperationType) (status int, code, message string) {
	principalID, restricted := principalRestrictingRequest(r)
	if !restricted {
		return 0, "", ""
	}
	enforcement := s.settings.String(config.SettingOperationsGrantEnforcement)
	if enforcement != config.OperationsGrantEnforcementLenient && enforcement != config.OperationsGrantEnforcementStrict {
		return http.StatusInternalServerError, "grant_enforcement_misconfigured", fmt.Sprintf(
			"operations.grant_enforcement is %q, not one of %v", enforcement, config.OperationsGrantEnforcementValues)
	}
	granted, err := s.grantClient.EffectiveResourceIDs(r.Context(), principalID, operationRunGrantRelation, operationTypeResourceType)
	switch {
	case errors.Is(err, grantclient.ErrPrincipalUnknown):
		return http.StatusBadRequest, "unknown_principal", err.Error()
	case err != nil:
		return http.StatusBadGateway, "grant_store_unavailable", err.Error()
	}
	if len(granted) == 0 && enforcement == config.OperationsGrantEnforcementLenient {
		return 0, "", ""
	}
	for _, grantedType := range granted {
		if grantedType == string(operationType) {
			return 0, "", ""
		}
	}
	return http.StatusForbidden, "not_granted", fmt.Sprintf("%s holds no %s grant on %s %s (enforcement %s; granted: %v)",
		principalID, operationRunGrantRelation, operationTypeResourceType, operationType, enforcement, granted)
}

func (s *Server) handleListOperationBudgets(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	budgets, err := coordinator.Store().ListOrganizationBudgets(time.Now())
	if writeOperationStoreError(w, err) {
		return
	}
	writeJSON(w, budgets)
}

func (s *Server) handleGetOperationBudget(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	budget, found, err := coordinator.Store().OrganizationBudget(r.PathValue("organization_id"), time.Now())
	if writeOperationStoreError(w, err) {
		return
	}
	if !found {
		http.Error(w, "organization has no budget", http.StatusNotFound)
		return
	}
	writeJSON(w, budget)
}

func (s *Server) handlePutOperationBudget(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	var body struct {
		MonthlyLimitUSD *float64 `json:"monthly_limit_usd"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil || body.MonthlyLimitUSD == nil || *body.MonthlyLimitUSD < 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_budget", `body must be {"monthly_limit_usd": <dollars, 0 or more>}`)
		return
	}
	organizationID := r.PathValue("organization_id")
	if status, code, message := s.checkOrganizationIsAnActiveGroup(r, organizationID); status != 0 {
		writeJSONError(w, status, code, message)
		return
	}
	updatedBy, _ := principalIdentityOfRequest(r)
	if err := coordinator.Store().SetOrganizationBudget(organizationID, *body.MonthlyLimitUSD, updatedBy, time.Now()); writeOperationStoreError(w, err) {
		return
	}
	s.handleGetOperationBudget(w, r)
}

func (s *Server) handleDeleteOperationBudget(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	if writeOperationStoreError(w, coordinator.Store().DeleteOrganizationBudget(r.PathValue("organization_id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// checkOperationGrantEnforcementValue is the write check on
// operations.grant_enforcement.
func checkOperationGrantEnforcementValue(value string) error {
	for _, allowed := range config.OperationsGrantEnforcementValues {
		if value == allowed {
			return nil
		}
	}
	return fmt.Errorf("must be one of %v", config.OperationsGrantEnforcementValues)
}
