package operationroutes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"

	"github.com/kayushkin/llm-bridge-server/internal/serversentevents"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/executors"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationstore"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
)

// The operation routes. The coordinator (internal/automation/operations) does the work;
// these handlers decide who is asking, check the organization with
// principal-store, and translate the coordinator's answers to HTTP. The
// contract is docs/OPERATIONS.md in llm-bridge.

func (s *Service) operationsOrRefuse(w http.ResponseWriter) (*operations.Coordinator, bool) {
	if s.Operations() == nil {
		httpjson.WriteJSONError(w, http.StatusServiceUnavailable, "operations_not_configured", "this server was started without an operations coordinator")
		return nil, false
	}
	return s.Operations(), true
}

// MaximumOperationIntentBytes bounds one intent. Content a store owns travels
// by reference, so an intent is small.
const MaximumOperationIntentBytes = 4 << 20

func (s *Service) HandleCreateOperation(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.operationsOrRefuse(w); !ok {
		return
	}
	var intent msg.OperationIntent
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaximumOperationIntentBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_intent", "body is not an operation intent: "+err.Error())
		return
	}
	caller, _ := requestcaller.FromRequest(r)
	receipt, created, refusal := s.SubmitOperation(r.Context(), caller, intent)
	if refusal != nil {
		httpjson.WriteJSONError(w, refusal.Status, refusal.Code, refusal.Message)
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

// Refusal is an operation call refused: the status, code and
// message the operation routes answer it with.
type Refusal struct {
	Status  int
	Code    string
	Message string
}

func (refusal *Refusal) Error() string {
	return fmt.Sprintf("%d %s: %s", refusal.Status, refusal.Code, refusal.Message)
}

// refusalOf turns a check's zero-or-status answer into a refusal.
func refusalOf(status int, code, message string) *Refusal {
	if status == 0 {
		return nil
	}
	return &Refusal{Status: status, Code: code, Message: message}
}

// SubmitOperation checks and submits an intent as caller: the operation is
// the caller's whatever the intent says, the organization must be an active
// group the caller belongs to, and the caller must hold can_run_operation on
// the type. The zero caller is this server's internal service, acting as no
// principal, which belongs to every organization and needs no grant. Both
// POST /operations and the callers inside this server go through here.
func (s *Service) SubmitOperation(ctx context.Context, caller requestcaller.Caller, intent msg.OperationIntent) (msg.OperationReceipt, bool, *Refusal) {
	if s.Operations() == nil {
		return msg.OperationReceipt{}, false, &Refusal{http.StatusServiceUnavailable, "operations_not_configured", "this server was started without an operations coordinator"}
	}
	intent.PrincipalID = caller.PrincipalID
	if refusal := refusalOf(s.checkOperationOrganization(ctx, caller, intent.OrganizationID)); refusal != nil {
		return msg.OperationReceipt{}, false, refusal
	}
	if refusal := refusalOf(s.checkOperationGrant(ctx, caller, intent.Type)); refusal != nil {
		return msg.OperationReceipt{}, false, refusal
	}
	receipt, created, err := s.Operations().Submit(intent)
	var intentError *operations.IntentError
	var budgetError *operations.BudgetError
	switch {
	case errors.As(err, &intentError):
		return msg.OperationReceipt{}, false, &Refusal{http.StatusBadRequest, intentError.Code, intentError.Message}
	case errors.As(err, &budgetError):
		return msg.OperationReceipt{}, false, &Refusal{http.StatusPaymentRequired, budgetError.Code, budgetError.Message}
	case errors.Is(err, operationstore.ErrIdempotencyKeyReused):
		return msg.OperationReceipt{}, false, &Refusal{http.StatusConflict, "idempotency_key_reused", err.Error()}
	case err != nil:
		log.Printf("operations: submit: %v", err)
		return msg.OperationReceipt{}, false, &Refusal{http.StatusInternalServerError, "operation_not_stored", err.Error()}
	}
	return receipt, created, nil
}

// checkOperationOrganization makes sure the organization is an active
// principal-store group and, for a principal who is not an administrator,
// one they belong to. A zero status means it passed.
func (s *Service) checkOperationOrganization(ctx context.Context, caller requestcaller.Caller, organizationID string) (status int, code, message string) {
	if status, code, message := s.checkOrganizationIsAnActiveGroup(ctx, organizationID); status != 0 {
		return status, code, message
	}
	if !caller.NarrowsToOnePrincipal() {
		return 0, "", ""
	}
	principalID := caller.PrincipalID
	groups, err := s.PrincipalStore().ListGroups(ctx, principalID, false)
	if err != nil {
		return http.StatusBadGateway, "principal_store_unavailable", fmt.Sprintf("principal-store: the groups of %s: %v", principalID, err)
	}
	for _, group := range groups {
		if group.ID == organizationID {
			return 0, "", ""
		}
	}
	return http.StatusForbidden, "not_a_member_of_organization", fmt.Sprintf("%s is not a member of %s", principalID, organizationID)
}

// checkOrganizationIsAnActiveGroup makes sure principal-store has the
// organization as an active group. A zero status means it passed.
func (s *Service) checkOrganizationIsAnActiveGroup(ctx context.Context, organizationID string) (status int, code, message string) {
	if organizationID == "" {
		return http.StatusBadRequest, "organization_required", "organization_id is required: the principal-store group the operation runs for"
	}
	organization, err := s.PrincipalStore().Get(ctx, organizationID, false)
	switch {
	case errors.Is(err, principals.ErrNotFound):
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

func (s *Service) HandleGetOperation(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	operation, err := coordinator.Store().Get(r.PathValue("id"))
	if writeOperationStoreError(w, err) {
		return
	}
	httpjson.WriteJSON(w, operation.Receipt)
}

func (s *Service) HandleCancelOperation(w http.ResponseWriter, r *http.Request) {
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
	httpjson.WriteJSON(w, receipt)
}

// MaximumOperationListLimit bounds GET /operations and the children route.
const MaximumOperationListLimit = 500

func (s *Service) HandleListOperationChildren(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	parentID := r.PathValue("id")
	if _, err := coordinator.Store().Get(parentID); writeOperationStoreError(w, err) {
		return
	}
	children, err := coordinator.Store().List(operationstore.Filter{ParentOperationID: parentID, Limit: MaximumOperationListLimit})
	if writeOperationStoreError(w, err) {
		return
	}
	httpjson.WriteJSON(w, children)
}

func (s *Service) HandleListOperations(w http.ResponseWriter, r *http.Request) {
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
		httpjson.WriteJSONError(w, http.StatusBadRequest, "unknown_state", fmt.Sprintf("state %q is not one of %v", filter.State, msg.AllOperationStates))
		return
	}
	if principalID, restricted := requestcaller.PrincipalRestrictingRequest(r); restricted {
		if filter.PrincipalID != "" && filter.PrincipalID != principalID {
			httpjson.WriteJSON(w, []msg.OperationReceipt{})
			return
		}
		filter.PrincipalID = principalID
	}
	for name, target := range map[string]*time.Time{"created_after": &filter.CreatedAfter, "created_before": &filter.CreatedBefore} {
		if value := query.Get(name); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_"+name, fmt.Sprintf("%s must be an RFC 3339 time: %v", name, err))
				return
			}
			*target = parsed
		}
	}
	if value := query.Get("limit"); value != "" {
		limit, err := strconv.Atoi(value)
		if err != nil || limit < 1 || limit > MaximumOperationListLimit {
			httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_limit", fmt.Sprintf("limit must be between 1 and %d", MaximumOperationListLimit))
			return
		}
		filter.Limit = limit
	}
	receipts, err := coordinator.Store().List(filter)
	if writeOperationStoreError(w, err) {
		return
	}
	httpjson.WriteJSON(w, receipts)
}

func (s *Service) HandleOperationTypes(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	httpjson.WriteJSON(w, coordinator.Types())
}

// HandleOperationEvents streams an operation's events: every stored one after
// Last-Event-ID, then each new one, and closes once the operation and all its
// children have finished and every event has been sent. The
// SSE id of each frame is its sequence, which is the receipt's revision.
func (s *Service) HandleOperationEvents(w http.ResponseWriter, r *http.Request) {
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
			httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_last_event_id", "Last-Event-ID must be an event sequence number")
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

	keepalive := time.NewTicker(serversentevents.SseKeepaliveInterval)
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
		httpjson.WriteJSONError(w, http.StatusInternalServerError, "operation_store_failed", err.Error())
	}
	return true
}

// OperationIsOwnedByPrincipal reports whether the operation exists and was
// started as principalID.
func (s *Service) OperationIsOwnedByPrincipal(operationID, principalID string) bool {
	if s.Operations() == nil || principalID == "" {
		return false
	}
	operation, err := s.Operations().Store().Get(operationID)
	return err == nil && operation.Receipt.PrincipalID == principalID
}

// BoardTaxonomy implements executors.TaxonomyReader with kanban-store.
func (s *Service) BoardTaxonomy(ctx context.Context, boardID, principalID string) (msg.ClassificationTaxonomy, string, error) {
	if s.IssueReads() == nil {
		return msg.ClassificationTaxonomy{}, "", errors.New("kanban-store is not configured, so no board taxonomy can be read")
	}
	taxonomy, revision, err := s.IssueReads().BoardTaxonomy(ctx, boardID, principalID)
	if errors.Is(err, issuereads.ErrNotFound) {
		return msg.ClassificationTaxonomy{}, "", fmt.Errorf("%w: %w", executors.ErrTaxonomyBoardNotFound, err)
	}
	return taxonomy, revision, err
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
func (s *Service) checkOperationGrant(ctx context.Context, caller requestcaller.Caller, operationType msg.OperationType) (status int, code, message string) {
	if !caller.NarrowsToOnePrincipal() {
		return 0, "", ""
	}
	principalID := caller.PrincipalID
	enforcement := s.Settings.String(config.SettingOperationsGrantEnforcement)
	if enforcement != config.OperationsGrantEnforcementLenient && enforcement != config.OperationsGrantEnforcementStrict {
		return http.StatusInternalServerError, "grant_enforcement_misconfigured", fmt.Sprintf(
			"operations.grant_enforcement is %q, not one of %v", enforcement, config.OperationsGrantEnforcementValues)
	}
	granted, err := s.GrantChecker.GrantedResourcesOf(ctx, grantcheck.SubjectOfCaller(caller), operationRunGrantRelation, operationTypeResourceType)
	switch {
	case errors.Is(err, grants.ErrNotFound):
		return http.StatusBadRequest, "unknown_principal", err.Error()
	case err != nil:
		return http.StatusBadGateway, "grant_store_unavailable", err.Error()
	}
	// strict refuses a principal holding no grant whatever its kind; lenient
	// leaves that to the kind, as every other relation does.
	if enforcement == config.OperationsGrantEnforcementStrict {
		granted.RestrictedToItsGrants = true
	}
	if granted.Permits(string(operationType)) {
		return 0, "", ""
	}
	return http.StatusForbidden, "not_granted", fmt.Sprintf("%s may not start %s %s (enforcement %s): %s",
		principalID, operationTypeResourceType, operationType, enforcement, granted.Describe())
}

func (s *Service) HandleListOperationBudgets(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	budgets, err := coordinator.Store().ListOrganizationBudgets(time.Now())
	if writeOperationStoreError(w, err) {
		return
	}
	httpjson.WriteJSON(w, budgets)
}

func (s *Service) HandleGetOperationBudget(w http.ResponseWriter, r *http.Request) {
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
	httpjson.WriteJSON(w, budget)
}

func (s *Service) HandlePutOperationBudget(w http.ResponseWriter, r *http.Request) {
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
		httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_budget", `body must be {"monthly_limit_usd": <dollars, 0 or more>}`)
		return
	}
	organizationID := r.PathValue("organization_id")
	if status, code, message := s.checkOrganizationIsAnActiveGroup(r.Context(), organizationID); status != 0 {
		httpjson.WriteJSONError(w, status, code, message)
		return
	}
	updatedBy, _ := requestcaller.PrincipalIdentityOfRequest(r)
	if err := coordinator.Store().SetOrganizationBudget(organizationID, *body.MonthlyLimitUSD, updatedBy, time.Now()); writeOperationStoreError(w, err) {
		return
	}
	s.HandleGetOperationBudget(w, r)
}

func (s *Service) HandleDeleteOperationBudget(w http.ResponseWriter, r *http.Request) {
	coordinator, ok := s.operationsOrRefuse(w)
	if !ok {
		return
	}
	if writeOperationStoreError(w, coordinator.Store().DeleteOrganizationBudget(r.PathValue("organization_id"))) {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// CheckOperationGrantEnforcementValue is the write check on
// operations.grant_enforcement.
func CheckOperationGrantEnforcementValue(value string) error {
	for _, allowed := range config.OperationsGrantEnforcementValues {
		if value == allowed {
			return nil
		}
	}
	return fmt.Errorf("must be one of %v", config.OperationsGrantEnforcementValues)
}
