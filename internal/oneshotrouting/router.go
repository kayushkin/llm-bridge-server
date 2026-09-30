// Package oneshotrouting decides where a one-shot model call goes and tries
// the next model when one fails.
//
// Every background call this server makes — the signal classifier, question
// triage, the prompt drift tagger, the session renamer, the session-action
// reviewer, the operation executors, and POST /oneshot — names a model-store
// role ("balanced") rather than a model and an instance. model-store keeps an
// ordered list of models per role: the first model, then its fallbacks. The
// router walks that list. For each model it takes the harness instance that
// oneshot.instance_by_provider names for the model's provider, skips the model
// when usage-store says the subscription that instance bills is used up, and
// otherwise calls it. A failure is recorded and the next model is tried; the
// first answer wins. When every model fails, the error names every attempt.
//
// Nothing here guesses. A provider with no instance is an attempt that failed
// ("no instance configured for provider X"), not a hunt for some instance
// that might do. A usage-store that cannot be read is logged and the model is
// called anyway: the call itself fails if the limit is real, and the next
// model is tried.
package oneshotrouting

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
)

// UsageProviderByHarness names the usage-store provider whose subscription a
// harness's calls are billed to. It is the whole list: a harness not in it is
// never skipped for limits, because nothing says which limits apply to it.
var UsageProviderByHarness = map[msg.Harness]string{
	msg.HarnessClaudeCode: "anthropic",
	msg.HarnessCodex:      "codex",
}

// Candidate is one model a route may call, with the provider that decides
// which instance calls it.
type Candidate struct {
	ModelID  string
	Provider string
}

// Route is the models to try for one call, in order.
type Route struct {
	// Requested is what the caller named: a role, a model id or an alias.
	Requested string
	// Role is the model-store role Requested named; empty when it named one
	// model.
	Role       string
	Candidates []Candidate
}

// Describe says what the route was asked for, for logs and errors.
func (route Route) Describe() string {
	if route.Role != "" {
		return "role=" + route.Role
	}
	return "model=" + route.Requested
}

// ResolutionError is a route that cannot be built: no role named, a role
// model-store does not have, a model it does not know. Retrying cannot fix it.
type ResolutionError struct {
	Code    string
	Message string
}

func (e *ResolutionError) Error() string { return e.Code + ": " + e.Message }

// Codes a ResolutionError carries.
const (
	CodeNoModelRole          = "no_model_role"
	CodeUnknownModelRole     = "unknown_model_role"
	CodeModelRoleUnresolved  = "model_role_unresolvable"
	CodeUnknownModel         = "unknown_model"
	CodeModelStoreNotPresent = "no_model_store"
)

// AllModelsFailedError is a route every model of which failed or was skipped.
type AllModelsFailedError struct {
	Caller   string
	Route    Route
	Attempts []msg.OneShotAttempt
}

func (e *AllModelsFailedError) Error() string {
	parts := make([]string, 0, len(e.Attempts))
	for _, attempt := range e.Attempts {
		where := attempt.Model
		if attempt.InstanceID != "" {
			where += " on " + attempt.InstanceID
		}
		if attempt.Skipped != "" {
			parts = append(parts, where+" skipped: "+attempt.Skipped)
		} else {
			parts = append(parts, where+" failed: "+attempt.Error)
		}
	}
	return fmt.Sprintf("%s caller=%s: every model failed: %s", e.Route.Describe(), e.Caller, strings.Join(parts, "; "))
}

// ModelRegistry is the part of model-store the router reads.
type ModelRegistry interface {
	RoleModels(role string) ([]*modelstore.Model, error)
	ResolveModel(idOrAliasOrRole string) (*modelstore.Model, error)
}

// InstanceRegistry is the part of harness-store the router reads.
type InstanceRegistry interface {
	GetInstance(id string) (*msg.Instance, error)
}

// LimitReader says whether a usage-store provider's subscription is used up.
// exhausted is a description of the full window ("seven_day at 100%"), empty
// when none is full or the snapshot is stale.
type LimitReader interface {
	ExhaustedWindow(ctx context.Context, usageProvider string) (exhausted string, err error)
}

// Router routes one-shot calls. Every field but Limits must be set.
type Router struct {
	Models    ModelRegistry
	Instances InstanceRegistry
	// InstanceByProvider reads oneshot.instance_by_provider at the time of
	// use: model-store provider → harness instance id.
	InstanceByProvider func() map[string]string
	// Limits is usage-store. Nil switches the limit check off.
	Limits LimitReader
	// Call runs one request on one instance's harness.
	Call func(ctx context.Context, instance *msg.Instance, request msg.OneShotRequest) (msg.OneShotResponse, error)
	// Logf writes the fallback lines.
	Logf func(format string, arguments ...any)
}

// ResolveRole builds the route for a model-store role.
func (router *Router) ResolveRole(role string) (Route, error) {
	if role == "" {
		return Route{}, &ResolutionError{CodeNoModelRole, "no model role named"}
	}
	if !modelstore.IsCanonicalRole(role) {
		return Route{}, &ResolutionError{CodeUnknownModelRole, fmt.Sprintf("%q is not a model role; the roles are %s", role, strings.Join(modelstore.CanonicalRoles, ", "))}
	}
	if router.Models == nil {
		return Route{}, &ResolutionError{CodeModelStoreNotPresent, "this server has no model-store, so no role can be resolved"}
	}
	models, err := router.Models.RoleModels(role)
	if err != nil {
		return Route{}, &ResolutionError{CodeModelRoleUnresolved, fmt.Sprintf("model-store cannot resolve role %q: %v", role, err)}
	}
	route := Route{Requested: role, Role: role}
	for _, model := range models {
		route.Candidates = append(route.Candidates, Candidate{ModelID: model.ID, Provider: model.Provider})
	}
	if len(route.Candidates) == 0 {
		return Route{}, &ResolutionError{CodeModelRoleUnresolved, fmt.Sprintf("role %q has no models", role)}
	}
	return route, nil
}

// Resolve builds the route for a role, a model id or an alias. A role name is
// a role: its whole list is tried. Anything else is one model, tried alone.
func (router *Router) Resolve(requested string) (Route, error) {
	if modelstore.IsCanonicalRole(requested) || requested == "" {
		return router.ResolveRole(requested)
	}
	if router.Models == nil {
		return Route{}, &ResolutionError{CodeModelStoreNotPresent, "this server has no model-store, so no model can be resolved"}
	}
	model, err := router.Models.ResolveModel(requested)
	if err != nil {
		return Route{}, &ResolutionError{CodeUnknownModel, fmt.Sprintf("model-store cannot resolve %q to a model: %v", requested, err)}
	}
	return Route{Requested: requested, Candidates: []Candidate{{ModelID: model.ID, Provider: model.Provider}}}, nil
}

// RunRole resolves request.ModelRole and runs the request on its route.
func (router *Router) RunRole(ctx context.Context, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	if request.Model != "" {
		return msg.OneShotResponse{}, &ResolutionError{"model_and_role", "a routed request names model_role, not model"}
	}
	route, err := router.ResolveRole(request.ModelRole)
	if err != nil {
		return msg.OneShotResponse{}, err
	}
	return router.Run(ctx, request.Caller, route, request, nil)
}

// Run tries the route's models in order and returns the first answer, with
// Model, InstanceID and the attempts before it filled in. admit, when set, is
// asked first about each model; a non-empty answer skips it with that reason.
// The request's Model is set to each model in turn and its ModelRole cleared:
// a harness is told a model, never a role.
func (router *Router) Run(ctx context.Context, caller string, route Route, request msg.OneShotRequest, admit func(Candidate) string) (msg.OneShotResponse, error) {
	var attempts []msg.OneShotAttempt
	for index, candidate := range route.Candidates {
		if ctx.Err() != nil {
			attempts = append(attempts, msg.OneShotAttempt{Model: candidate.ModelID, Error: ctx.Err().Error()})
			break
		}
		attempt, response, answered := router.try(ctx, candidate, request, admit)
		if answered {
			response.InstanceID = attempt.InstanceID
			if response.Model == "" {
				response.Model = candidate.ModelID
			}
			response.Attempts = attempts
			return response, nil
		}
		attempts = append(attempts, attempt)
		router.logFallback(caller, route, attempt, route.Candidates, index)
	}
	return msg.OneShotResponse{}, &AllModelsFailedError{Caller: caller, Route: route, Attempts: attempts}
}

// try makes one attempt: admit, instance, limits, call.
func (router *Router) try(ctx context.Context, candidate Candidate, request msg.OneShotRequest, admit func(Candidate) string) (msg.OneShotAttempt, msg.OneShotResponse, bool) {
	attempt := msg.OneShotAttempt{Model: candidate.ModelID}
	if admit != nil {
		if reason := admit(candidate); reason != "" {
			attempt.Skipped = reason
			return attempt, msg.OneShotResponse{}, false
		}
	}
	instanceID := router.InstanceByProvider()[candidate.Provider]
	if instanceID == "" {
		attempt.Error = fmt.Sprintf("no instance configured for provider %s (oneshot.instance_by_provider)", candidate.Provider)
		return attempt, msg.OneShotResponse{}, false
	}
	attempt.InstanceID = instanceID
	if router.Instances == nil {
		attempt.Error = "this server has no harness-store, so it has no instances to call"
		return attempt, msg.OneShotResponse{}, false
	}
	instance, err := router.Instances.GetInstance(instanceID)
	if err != nil {
		attempt.Error = fmt.Sprintf("instance %s: %v", instanceID, err)
		return attempt, msg.OneShotResponse{}, false
	}
	if !instance.Enabled {
		attempt.Error = fmt.Sprintf("instance %s is disabled", instanceID)
		return attempt, msg.OneShotResponse{}, false
	}
	if reason := router.exhaustedLimit(ctx, instance); reason != "" {
		attempt.Skipped = reason
		return attempt, msg.OneShotResponse{}, false
	}
	outgoing := request
	outgoing.Model = candidate.ModelID
	outgoing.ModelRole = ""
	response, err := router.Call(ctx, instance, outgoing)
	if err != nil {
		attempt.Error = err.Error()
		return attempt, msg.OneShotResponse{}, false
	}
	return attempt, response, true
}

// exhaustedLimit is why a model on instance must be skipped for limits, or
// empty. A usage-store that cannot be read skips nothing.
func (router *Router) exhaustedLimit(ctx context.Context, instance *msg.Instance) string {
	if router.Limits == nil {
		return ""
	}
	usageProvider, billed := UsageProviderByHarness[instance.HarnessType]
	if !billed {
		return ""
	}
	exhausted, err := router.Limits.ExhaustedWindow(ctx, usageProvider)
	if err != nil {
		router.Logf("[oneshot] usage-store limits for %s could not be read, calling %s anyway: %v", usageProvider, instance.ID, err)
		return ""
	}
	if exhausted == "" {
		return ""
	}
	return usageProvider + " " + exhausted
}

func (router *Router) logFallback(caller string, route Route, attempt msg.OneShotAttempt, candidates []Candidate, index int) {
	what := "failed: " + attempt.Error
	if attempt.Skipped != "" {
		what = "skipped: " + attempt.Skipped
	}
	next := "no model left"
	if index+1 < len(candidates) {
		next = "trying " + candidates[index+1].ModelID
	}
	router.Logf("[oneshot] %s caller=%s model %s %s; %s", route.Describe(), caller, attempt.Model, what, next)
}

// IsResolutionError reports whether err is a route that could not be built.
func IsResolutionError(err error) (*ResolutionError, bool) {
	var resolution *ResolutionError
	if errors.As(err, &resolution) {
		return resolution, true
	}
	return nil, false
}
