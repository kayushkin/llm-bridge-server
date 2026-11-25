package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/agents/evalstore"
	"github.com/kayushkin/llm-bridge-server/internal/operationstore"
	"github.com/kayushkin/llm-bridge/msg"
)

// evalStoreInThisServer is this server as eval-store calls it: the
// operations coordinator, model-store and usage-store, each called in process
// as the internal service acting as no principal, through the same functions
// their routes answer from. The internal service is unrestricted on those
// routes, so no check a principal meets is left out here: an operation still
// needs an active organization and passes the coordinator's own budget and
// intent checks.
type evalStoreInThisServer struct {
	server *Server
}

var (
	_ evalstore.Operations   = evalStoreInThisServer{}
	_ evalstore.ModelCatalog = evalStoreInThisServer{}
	_ evalstore.UsageReader  = evalStoreInThisServer{}
)

// errOperationsOff is a use of the operations coordinator on a server that
// was started without one.
var errOperationsOff = errors.New("this server was started without an operations coordinator")

// SubmitOperation holds an intent to the size POST /operations takes, then
// submits it as the internal service.
func (thisServer evalStoreInThisServer) SubmitOperation(ctx context.Context, intent msg.OperationIntent) (msg.OperationReceipt, error) {
	encoded, err := json.Marshal(intent)
	if err != nil {
		return msg.OperationReceipt{}, err
	}
	if len(encoded) > maximumOperationIntentBytes {
		return msg.OperationReceipt{}, &operationRefusal{http.StatusBadRequest, "invalid_intent",
			fmt.Sprintf("the intent is %d bytes; an operation intent is at most %d", len(encoded), maximumOperationIntentBytes)}
	}
	receipt, _, refusal := thisServer.server.submitOperation(ctx, requestCaller{}, intent)
	if refusal == nil {
		return receipt, nil
	}
	if refusal.status == http.StatusPaymentRequired {
		return msg.OperationReceipt{}, fmt.Errorf("%w: %v", evalstore.ErrBridgeBudgetRefused, refusal)
	}
	return msg.OperationReceipt{}, refusal
}

func (thisServer evalStoreInThisServer) ReadOperation(_ context.Context, id string) (msg.OperationReceipt, error) {
	coordinator := thisServer.server.operations
	if coordinator == nil {
		return msg.OperationReceipt{}, errOperationsOff
	}
	operation, err := coordinator.Store().Get(id)
	if err != nil {
		return msg.OperationReceipt{}, err
	}
	return operation.Receipt, nil
}

func (thisServer evalStoreInThisServer) CancelOperation(ctx context.Context, id string) error {
	coordinator := thisServer.server.operations
	if coordinator == nil {
		return errOperationsOff
	}
	_, err := coordinator.Cancel(ctx, id)
	return err
}

func (thisServer evalStoreInThisServer) ListSucceededOperations(_ context.Context, query evalstore.OperationQuery) ([]msg.OperationReceipt, error) {
	coordinator := thisServer.server.operations
	if coordinator == nil {
		return nil, errOperationsOff
	}
	if query.Limit < 1 || query.Limit > maximumOperationListLimit {
		return nil, fmt.Errorf("a page of operations holds 1 to %d, not %d", maximumOperationListLimit, query.Limit)
	}
	return coordinator.Store().List(operationstore.Filter{
		OrganizationID: query.OrganizationID,
		PrincipalID:    query.PrincipalID,
		Type:           query.Type,
		State:          msg.OperationStateSucceeded,
		CreatedAfter:   query.CreatedAfter,
		CreatedBefore:  query.CreatedBefore,
		Limit:          query.Limit,
	})
}

// ReadOperationIntent refuses: no route of this server hands out the intent
// behind an operation (its prompts can hold anything a caller sent), and
// eval-store reached it over HTTP before, where it found no such route.
// Serving intents is a decision of its own, not part of moving the call.
func (thisServer evalStoreInThisServer) ReadOperationIntent(context.Context, string) (msg.OperationIntent, error) {
	return msg.OperationIntent{}, evalstore.ErrOperationIntentsNotServed
}

// Models reads every model's provider and list price from model-store, the
// records GET /model-store/api/models answers.
func (thisServer evalStoreInThisServer) Models(context.Context) (map[string]evalstore.ModelPrice, error) {
	if thisServer.server.modelStore == nil {
		return nil, errors.New("model-store: this server was built without a model registry")
	}
	statuses, err := thisServer.server.modelStore.AllModelsWithStatus()
	if err != nil {
		return nil, fmt.Errorf("model-store: %w", err)
	}
	models := make(map[string]evalstore.ModelPrice, len(statuses))
	for _, status := range statuses {
		// model-store keeps a price for every model, so every price is known.
		models[status.ID] = evalstore.ModelPrice{Name: status.Name, Provider: status.Provider,
			InputCostPerMillion: status.InputCost, OutputCostPerMillion: status.OutputCost, Known: true}
	}
	return models, nil
}

// Limits reads usage-store's latest limits, the answer of its
// GET /api/usage/limits. A provider usage-store has no snapshot of is listed
// with no windows, as that route's null was read.
func (thisServer evalStoreInThisServer) Limits(context.Context) (evalstore.UsageLimits, error) {
	handlers := thisServer.server.usageAndEvalStores.usage.Load()
	if handlers == nil {
		return nil, fmt.Errorf("usage-store: %w", errUsageStoreOff)
	}
	latest, err := handlers.LatestLimits()
	if err != nil {
		return nil, fmt.Errorf("usage-store: %w", err)
	}
	limits := make(evalstore.UsageLimits, len(latest))
	for provider, snapshot := range latest {
		var subscription evalstore.UsageSubscription
		if snapshot != nil {
			subscription.SnapshotAt = snapshot.SnapshotAt
			if snapshot.StaleAfter != nil {
				subscription.StaleAfter = *snapshot.StaleAfter
			}
			subscription.Windows = make(map[string]evalstore.UsageWindow, len(snapshot.Windows))
			for name, window := range snapshot.Windows {
				var read evalstore.UsageWindow
				if window != nil {
					read.UsedPercent = window.UsedPercent
					if window.ResetsAt != nil {
						read.ResetsAt = *window.ResetsAt
					}
				}
				subscription.Windows[name] = read
			}
		}
		limits[provider] = subscription
	}
	return limits, nil
}
