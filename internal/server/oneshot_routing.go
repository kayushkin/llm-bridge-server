package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/oneshotrouting"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
	modelstore "github.com/kayushkin/model-store"
)

// Every background model call this server makes names a model-store role and
// goes through one router (internal/oneshotrouting): the role's models in
// order, each on the instance oneshot.instance_by_provider names for its
// provider, skipping one whose subscription usage-store reports full, until
// one answers. The Caller on each request says which feature asked.
const (
	oneShotCallerSignalClassifier     = "signal-classifier"
	oneShotCallerQuestionTriage       = "question-triage"
	oneShotCallerPromptDriftTagger    = "prompt-drift-tagger"
	oneShotCallerSessionRenamer       = "session-renamer"
	oneShotCallerSessionActionsReview = "session-actions-review"
	oneShotCallerSessionActionModel   = "session-action-model-call"
)

// newOneShotRouter builds the router from the server's stores and settings.
// A store the server was built without stays nil in the router, so a call
// fails naming it rather than on a nil pointer.
func (s *Server) newOneShotRouter() *oneshotrouting.Router {
	router := &oneshotrouting.Router{
		InstanceByProvider: func() map[string]string {
			return s.settings.StringMap(config.SettingOneShotInstanceByProvider)
		},
		Call: s.callInstanceOneShot,
		Logf: log.Printf,
	}
	if s.modelStore != nil {
		router.Models = s.modelStore
	}
	if s.harnessStore != nil {
		router.Instances = s.harnessStore
	}
	if s.cfg.UsageStoreURL != "" {
		router.Limits = oneshotrouting.NewUsageStoreLimits(s.cfg.UsageStoreURL)
	} else {
		log.Printf("[oneshot] LLMBRIDGE_USAGE_STORE_URL is empty: routed calls are never skipped for subscription limits")
	}
	return router
}

// callInstanceOneShot runs one request on one instance's harness and reads
// its answer. Anything but a 200 with a one-shot response is an error.
func (s *Server) callInstanceOneShot(ctx context.Context, instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error) {
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

// routedOneShotJSON runs a request that names a role and returns the answer
// encoded, for the classifier and triage, which read the bytes.
func (s *Server) routedOneShotJSON(ctx context.Context, request msg.OneShotRequest) ([]byte, error) {
	response, err := s.oneShotRouter.RunRole(ctx, request)
	if err != nil {
		return nil, err
	}
	return json.Marshal(response)
}

// OneShotRoute implements executors.OneShotCaller. An input's model — a
// role, an id or an alias — or operations.completion_model_role when it names
// none, read at the time of use.
func (s *Server) OneShotRoute(requestedModel string) (oneshotrouting.Route, error) {
	if requestedModel == "" {
		requestedModel = s.settings.ModelRole(config.SettingOperationsCompletionModelRole)
	}
	if requestedModel == "" {
		return oneshotrouting.Route{}, &executors.TargetError{Code: "no_model",
			Message: "the input names no model and operations.completion_model_role is empty"}
	}
	route, err := s.oneShotRouter.Resolve(requestedModel)
	if resolution, ok := oneshotrouting.IsResolutionError(err); ok {
		return oneshotrouting.Route{}, &executors.TargetError{Code: resolution.Code, Message: resolution.Message}
	}
	return route, err
}

// RunOneShotRoute implements executors.OneShotCaller.
func (s *Server) RunOneShotRoute(ctx context.Context, route oneshotrouting.Route, request msg.OneShotRequest, admit func(oneshotrouting.Candidate) string) (msg.OneShotResponse, error) {
	return s.oneShotRouter.Run(ctx, request.Caller, route, request, admit)
}

// handleRoutedOneShot is POST /oneshot: one stateless model call for a
// model-store role, routed and fallen back by the server. The body is a
// msg.OneShotRequest naming model_role (never model) and, for the logs,
// caller; the answer is a msg.OneShotResponse with model, instance_id and
// attempts filled.
func (s *Server) handleRoutedOneShot(w http.ResponseWriter, r *http.Request) {
	var request msg.OneShotRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_body", "invalid request body: "+err.Error())
		return
	}
	if strings.TrimSpace(request.Prompt) == "" {
		writeJSONError(w, http.StatusBadRequest, "prompt_required", "prompt is required")
		return
	}
	if request.Model != "" {
		writeJSONError(w, http.StatusBadRequest, "model_not_accepted",
			"name a model_role ("+strings.Join(modelstore.CanonicalRoles, ", ")+"), not a model: the server picks the model, the instance and the fallbacks. POST /instances/{id}/oneshot takes a model")
		return
	}
	route, err := s.oneShotRouter.ResolveRole(request.ModelRole)
	if err != nil {
		status := http.StatusServiceUnavailable
		code := "model_role_unresolvable"
		if resolution, ok := oneshotrouting.IsResolutionError(err); ok {
			code = resolution.Code
			if code == oneshotrouting.CodeNoModelRole || code == oneshotrouting.CodeUnknownModelRole {
				status = http.StatusBadRequest
			}
		}
		writeJSONError(w, status, code, err.Error())
		return
	}
	response, err := s.oneShotRouter.Run(r.Context(), request.Caller, route, request, nil)
	if err != nil {
		var allFailed *oneshotrouting.AllModelsFailedError
		if errors.As(err, &allFailed) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":    map[string]string{"code": "all_models_failed", "message": err.Error()},
				"attempts": allFailed.Attempts,
			})
			return
		}
		writeJSONError(w, http.StatusBadGateway, "oneshot_failed", err.Error())
		return
	}
	writeJSON(w, response)
}

// checkSettingNamesAnAssignedModelRole is the write check on every model_role
// setting: the role must be one of model-store's, and model-store must have
// models for it, or every call on it would fail.
func (s *Server) checkSettingNamesAnAssignedModelRole(role string) error {
	if !modelstore.IsCanonicalRole(role) {
		return fmt.Errorf("%q is not a model role; the roles are %s", role, strings.Join(modelstore.CanonicalRoles, ", "))
	}
	if s.modelStore == nil {
		return fmt.Errorf("%w: this server has no model-store to check role %q with", servicesettings.ErrSettingOwnerUnavailable, role)
	}
	assigned, err := s.modelStore.RoleModelLists()
	if err != nil {
		return fmt.Errorf("%w: model-store: %v", servicesettings.ErrSettingOwnerUnavailable, err)
	}
	if len(assigned[role]) == 0 {
		return fmt.Errorf("model-store has no models for role %q", role)
	}
	return nil
}
