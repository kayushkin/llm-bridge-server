package server

import "fmt"

// The rules and descriptions of the routes agents_registry_routes.go mounts.
// They are kept here and added to routeAccessRules, routeDescriptions and the
// README's group order at start, so the shared tables gain them in one place.
// agent-store's and memory-store's own routes keep their rules in
// request_authorization.go, where they were before the move.

// modelStoreRouteRule is the rule of every model-store route.
var modelStoreRouteRule = operatorRoute("model-store is one registry with no owner on a row: a role or a toggle written here moves the model of every session and background call on this server")

// agentStoreRunnerSeedRouteRule is the rule of agent-store's seed routes for
// runners.
var agentStoreRunnerSeedRouteRule = runnerRoute("agent-store seed route for runners; the handler authenticates the runner's bearer token")

var agentsRegistryRouteAccessRules = map[string]routeAccessRule{
	"GET /model-store/api/models":              modelStoreRouteRule,
	"POST /model-store/api/models/toggle":      modelStoreRouteRule,
	"GET /model-store/api/roles":               modelStoreRouteRule,
	"POST /model-store/api/roles":              modelStoreRouteRule,
	"GET /model-store/api/roles/{role}":        modelStoreRouteRule,
	"GET /model-store/api/roles/{role}/models": modelStoreRouteRule,
	"GET /model-store/api/health":              modelStoreRouteRule,

	"GET /api/agent-store/seed/manifest":          agentStoreRunnerSeedRouteRule,
	"GET /api/agent-store/versions/{vid}/content": agentStoreRunnerSeedRouteRule,
	"POST /api/agent-store/seed/observe":          agentStoreRunnerSeedRouteRule,
	"POST /api/agent-store/seed/drift":            agentStoreRunnerSeedRouteRule,
}

const routeGroupModelStore routeGroup = "model-store, mounted in this process"

var agentsRegistryRouteDescriptions = map[string]routeDescription{
	"GET /model-store/api/models":              {routeGroupModelStore, "Every model with its health and aliases, by priority then id"},
	"POST /model-store/api/models/toggle":      {routeGroupModelStore, "Switch a model on or off: `{\"model\",\"enabled\"}`"},
	"GET /model-store/api/roles":               {routeGroupModelStore, "Each role's first model, each role's list, and the canonical roles"},
	"POST /model-store/api/roles":              {routeGroupModelStore, "Set a role's first model (`model`) or its whole list (`models`)"},
	"GET /model-store/api/roles/{role}":        {routeGroupModelStore, "A role's first model, resolved"},
	"GET /model-store/api/roles/{role}/models": {routeGroupModelStore, "Every model in a role's list, in the order to try them"},
	"GET /model-store/api/health":              {routeGroupModelStore, "`{\"ok\":true}`"},

	"GET /api/agent-store/seed/manifest":          {routeGroupRunners, "A runner's agent file seed manifest"},
	"GET /api/agent-store/versions/{vid}/content": {routeGroupRunners, "A runner fetches one agent file version"},
	"POST /api/agent-store/seed/observe":          {routeGroupRunners, "A runner reports the agent file hashes it holds"},
	"POST /api/agent-store/seed/drift":            {routeGroupRunners, "A runner sends an agent file it changed"},
}

func init() {
	for pattern, rule := range agentsRegistryRouteAccessRules {
		if _, taken := routeAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		routeAccessRules[pattern] = rule
	}
	for pattern, description := range agentsRegistryRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupModelStore)
}
