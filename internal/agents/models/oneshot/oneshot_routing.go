package oneshot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelrouting"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/executors"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Every background model call this server makes names a model-store role and
// goes through one router (internal/agents/models/modelrouting): the role's models in
// order, each on the instance oneshot.instance_by_provider names for its
// provider, skipping one whose subscription usage-store reports full, until
// one answers. The Caller on each request says which feature asked.
const (
	OneShotCallerSignalClassifier     = "signal-classifier"
	OneShotCallerQuestionTriage       = "question-triage"
	OneShotCallerPromptDriftTagger    = "prompt-drift-tagger"
	OneShotCallerSessionRenamer       = "session-renamer"
	OneShotCallerSessionActionsReview = "session-actions-review"
	OneShotCallerSessionActionModel   = "session-action-model-call"
)

// NewOneShotRouter builds the router from the server's stores and settings.
// A store the server was built without stays nil in the router, so a call
// fails naming it rather than on a nil pointer.
func (s *Service) NewOneShotRouter() *modelrouting.Router {
	router := &modelrouting.Router{
		InstanceByProvider: func() map[string]string {
			return s.Settings.StringMap(config.SettingOneShotInstanceByProvider)
		},
		Call: s.callInstanceOneShot,
		Logf: log.Printf,
	}
	if s.ModelStore() != nil {
		router.Models = s.ModelStore()
	}
	if s.HarnessStore() != nil {
		router.Instances = s.HarnessStore()
	}
	// usage-store runs in this process. Until its module is open a read is
	// an error, which the router logs and calls anyway.
	router.Limits = modelrouting.NewUsageStoreLimits(s.Server.ReadUsageStoreLimits)
	return router
}

// callInstanceOneShot runs one request on one instance's harness and reads
// its answer. Anything but a 200 with a one-shot response is an error.
func (s *Service) callInstanceOneShot(ctx context.Context, instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	raw, status, err := s.runOneShot(ctx, instance, request)
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	if status != http.StatusOK {
		return msg.OneShotResponse{}, fmt.Errorf("instance %s answered %d: %s", instance.ID, status, strings.TrimSpace(string(raw)))
	}
	var response msg.OneShotResponse
	if err := json.Unmarshal(raw, &response); err != nil {
		return msg.OneShotResponse{}, fmt.Errorf("instance %s answered with something that is not a one-shot response: %w", instance.ID, err)
	}
	return response, nil
}

// RoutedOneShotJSON runs a request that names a role and returns the answer
// encoded, for the classifier and triage, which read the bytes.
func (s *Service) RoutedOneShotJSON(ctx context.Context, request msg.OneShotRequest) ([]byte, error) {
	response, err := s.OneShotRouter().RunRole(ctx, request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

// OneShotRoute implements executors.OneShotCaller. An input's model — a
// role, an id or an alias — or operations.completion_model_role when it names
// none, read at the time of use.
func (s *Service) OneShotRoute(requestedModel string) (modelrouting.Route, error) {
	if requestedModel == "" {
		requestedModel = s.Settings.ModelRole(config.SettingOperationsCompletionModelRole)
	}
	if requestedModel == "" {
		return modelrouting.Route{}, &executors.TargetError{Code: "no_model",
			Message: "the input names no model and operations.completion_model_role is empty"}
	}
	route, err := s.OneShotRouter().Resolve(requestedModel)
	if resolution, ok := modelrouting.IsResolutionError(err); ok {
		return modelrouting.Route{}, &executors.TargetError{Code: resolution.Code, Message: resolution.Message}
	}
	return route, err
}

// RunOneShotRoute implements executors.OneShotCaller.
func (s *Service) RunOneShotRoute(ctx context.Context, route modelrouting.Route, request msg.OneShotRequest, admit func(modelrouting.Candidate) string) (msg.OneShotResponse, error) {
	return s.OneShotRouter().Run(ctx, request.Caller, route, request, admit)
}

// HandleRoutedOneShot is POST /oneshot: one stateless model call for a
// model-store role, routed and fallen back by the server. The body is a
// msg.OneShotRequest naming model_role (never model) and, for the logs,
// caller; the answer is a msg.OneShotResponse with model, instance_id and
// attempts filled.
func (s *Service) HandleRoutedOneShot(w http.ResponseWriter, r *http.Request) {
	var request msg.OneShotRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		httpjson.WriteJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	response, refusal := s.RoutedOneShot(r.Context(), request)
	if refusal != nil {
		if refusal.attempts != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(refusal.status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    map[string]string{"code": refusal.code, "message": refusal.message},
				"attempts": refusal.attempts,
			})
			return
		}
		httpjson.WriteJSONError(w, refusal.status, refusal.code, refusal.message)
		return
	}
	httpjson.WriteJSON(w, response)
}

// oneShotRefusal is the one-shot router refusing a call, with the status and
// code POST /oneshot answers. attempts is set when every model failed.
type oneShotRefusal struct {
	status   int
	code     string
	message  string
	attempts any
}

func (refusal *oneShotRefusal) Error() string {
	return fmt.Sprintf("%s (%d): %s", refusal.code, refusal.status, refusal.message)
}

// RoutedOneShot is one stateless model call for a model-store role, routed
// and fallen back by the server, with the checks POST /oneshot makes: a
// prompt is required, and the request names a model_role, never a model.
func (s *Service) RoutedOneShot(ctx context.Context, request msg.OneShotRequest) (msg.OneShotResponse, *oneShotRefusal) {
	if strings.TrimSpace(request.Prompt) == "" {
		return msg.OneShotResponse{}, &oneShotRefusal{status: http.StatusBadRequest, code: "prompt_required", message: "prompt is required"}
	}
	if request.Model != "" {
		return msg.OneShotResponse{}, &oneShotRefusal{status: http.StatusBadRequest, code: "model_not_accepted",
			message: "name a model_role (" + strings.Join(models.CanonicalRoles, ", ") + "), not a model: the server picks the model, the instance and the fallbacks. POST /instances/{id}/oneshot takes a model"}
	}
	route, err := s.OneShotRouter().ResolveRole(request.ModelRole)
	if err != nil {
		status := http.StatusServiceUnavailable
		code := "model_role_unresolvable"
		if resolution, ok := modelrouting.IsResolutionError(err); ok {
			code = resolution.Code
			if code == modelrouting.CodeNoModelRole || code == modelrouting.CodeUnknownModelRole {
				status = http.StatusBadRequest
			}
		}
		return msg.OneShotResponse{}, &oneShotRefusal{status: status, code: code, message: err.Error()}
	}
	response, err := s.OneShotRouter().Run(ctx, request.Caller, route, request, nil)
	if err != nil {
		var allFailed *modelrouting.AllModelsFailedError
		if errors.As(err, &allFailed) {
			return msg.OneShotResponse{}, &oneShotRefusal{status: http.StatusBadGateway, code: "all_models_failed", message: err.Error(), attempts: allFailed.Attempts}
		}
		return msg.OneShotResponse{}, &oneShotRefusal{status: http.StatusBadGateway, code: "oneshot_failed", message: err.Error()}
	}
	return response, nil
}

// CheckSettingNamesAnAssignedModelRole is the write check on every model_role
// setting: the role must be one of model-store's, and model-store must have
// models for it, or every call on it would fail.
func (s *Service) CheckSettingNamesAnAssignedModelRole(role string) error {
	if !models.IsCanonicalRole(role) {
		return fmt.Errorf("%q is not a model role; the roles are %s", role, strings.Join(models.CanonicalRoles, ", "))
	}
	if s.ModelStore() == nil {
		return fmt.Errorf("%w: this server has no model-store to check role %q with", servicesettings.ErrSettingOwnerUnavailable, role)
	}
	assigned, err := s.ModelStore().RoleModelLists()
	if err != nil {
		return fmt.Errorf("%w: model-store: %v", servicesettings.ErrSettingOwnerUnavailable, err)
	}
	if len(assigned[role]) == 0 {
		return fmt.Errorf("model-store has no models for role %q", role)
	}
	return nil
}

// OperationExecutors is every executor this server runs operations with,
// wired to its own one-shot path. main hands them to the coordinator.
func (s *Service) OperationExecutors() []operations.Executor {
	var boardStore executors.BoardClassificationStore
	if s.IssueReads() != nil {
		boardStore = s.IssueReads()
	}
	return []operations.Executor{executors.ModelClassifier{Caller: s, Taxonomies: s.OperationRoutes}, executors.BoardClassifier{Caller: s, Store: boardStore}, executors.LLMCompletion{Caller: s}}
}

// CheckOneShotInstanceByProvider is the write check on
// oneshot.instance_by_provider: each provider must be one model-store knows,
// and each instance one harness-store has enabled.
func (s *Service) CheckOneShotInstanceByProvider(value string) error {
	pairs := map[string]string{}
	for _, pair := range strings.Split(value, ",") {
		provider, instanceID, found := strings.Cut(strings.TrimSpace(pair), ":")
		if strings.TrimSpace(pair) == "" {
			continue
		}
		if !found || provider == "" || instanceID == "" {
			return fmt.Errorf("%q is not provider:instance", pair)
		}
		pairs[provider] = instanceID
	}
	if s.ModelStore() != nil {
		providers, err := s.ModelStore().Providers()
		if err != nil {
			return fmt.Errorf("%w: model-store: %v", servicesettings.ErrSettingOwnerUnavailable, err)
		}
		known := map[string]bool{}
		for _, provider := range providers {
			known[provider.ID] = true
		}
		for provider := range pairs {
			if !known[provider] {
				return fmt.Errorf("model-store has no provider %q", provider)
			}
		}
	}
	for _, instanceID := range pairs {
		if err := s.Server.CheckSettingNamesAnEnabledInstance(instanceID); err != nil {
			return err
		}
	}
	return nil
}
