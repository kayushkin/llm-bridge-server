package server

import "fmt"

// The rules and descriptions of the routes automation_routes.go mounts. They
// are added to routeAccessRules, routeDescriptions and the README's group
// order at start, so the shared tables gain them in one place.

// schedulerRouteRule is the rule of every scheduler route.
var schedulerRouteRule = operatorRoute("the scheduler's jobs are shell commands and agent prompts this server's OS user runs, with no owner on a row: creating or editing one is running code as the server")

// ruleStoreRouteRule is the rule of every rule-store route.
var ruleStoreRouteRule = operatorRoute("rule-store's rules act on any session and card as the internal service, with no owner on a row, and its subject copies hold every session's state")

var automationRouteAccessRules = map[string]routeAccessRule{
	"GET /scheduler/health":                         schedulerRouteRule,
	"GET /scheduler/api/jobs":                       schedulerRouteRule,
	"POST /scheduler/api/jobs":                      schedulerRouteRule,
	"GET /scheduler/api/jobs/{id}":                  schedulerRouteRule,
	"PATCH /scheduler/api/jobs/{id}":                schedulerRouteRule,
	"DELETE /scheduler/api/jobs/{id}":               schedulerRouteRule,
	"POST /scheduler/api/jobs/{id}/hold":            schedulerRouteRule,
	"DELETE /scheduler/api/jobs/{id}/hold":          schedulerRouteRule,
	"GET /scheduler/api/jobs/{id}/runs":             schedulerRouteRule,
	"POST /scheduler/api/jobs/{id}/run":             schedulerRouteRule,
	"GET /scheduler/api/runs/latest":                schedulerRouteRule,
	"GET /scheduler/api/job-references/{reference}": schedulerRouteRule,
	"GET /scheduler/api/event-trigger-subjects":     schedulerRouteRule,
	"GET /scheduler/api/calendar/events":            schedulerRouteRule,
	"GET /scheduler/settings":                       schedulerRouteRule,
	"PUT /scheduler/settings/{key}":                 schedulerRouteRule,

	"GET /rules/health":                ruleStoreRouteRule,
	"GET /rules/vocabulary":            ruleStoreRouteRule,
	"GET /rules/rules":                 ruleStoreRouteRule,
	"POST /rules/rules":                ruleStoreRouteRule,
	"GET /rules/rules/{id}":            ruleStoreRouteRule,
	"POST /rules/rules/{id}/revisions": ruleStoreRouteRule,
	"POST /rules/rules/{id}/activate":  ruleStoreRouteRule,
	"POST /rules/rules/{id}/pause":     ruleStoreRouteRule,
	"POST /rules/rules/{id}/retire":    ruleStoreRouteRule,
	"POST /rules/rules/{id}/preview":   ruleStoreRouteRule,
	"GET /rules/rules/{id}/comparison": ruleStoreRouteRule,
	"GET /rules/runs":                  ruleStoreRouteRule,
	"GET /rules/subjects/{type}/{id}":  ruleStoreRouteRule,
	"GET /rules/activity":              ruleStoreRouteRule,
	"GET /rules/summary":               ruleStoreRouteRule,
}

const (
	routeGroupScheduler routeGroup = "scheduler, mounted in this process"
	routeGroupRuleStore routeGroup = "rule-store, mounted in this process"
)

var automationRouteDescriptions = map[string]routeDescription{
	"GET /scheduler/health":                         {routeGroupScheduler, "`{\"status\",\"uptime_s\",\"started_at\"}`"},
	"GET /scheduler/api/jobs":                       {routeGroupScheduler, "Every job, by id"},
	"POST /scheduler/api/jobs":                      {routeGroupScheduler, "Create a shell or agent job; `\"enabled\":false` creates it switched off. 201"},
	"GET /scheduler/api/jobs/{id}":                  {routeGroupScheduler, "One job"},
	"PATCH /scheduler/api/jobs/{id}":                {routeGroupScheduler, "Change any field; an unknown field, a bad cron expression or an unrunnable job is 400"},
	"DELETE /scheduler/api/jobs/{id}":               {routeGroupScheduler, "Delete a job; its runs stay. 204"},
	"POST /scheduler/api/jobs/{id}/hold":            {routeGroupScheduler, "Hold a job: `{\"days\"}` or `{\"until\"}` (RFC3339)"},
	"DELETE /scheduler/api/jobs/{id}/hold":          {routeGroupScheduler, "Clear a job's hold"},
	"GET /scheduler/api/jobs/{id}/runs":             {routeGroupScheduler, "A job's newest runs with output, newest first: `limit` (1 to 100)"},
	"POST /scheduler/api/jobs/{id}/run":             {routeGroupScheduler, "Run a job now; 503 while `scheduler.runs.enabled` is off, 409 for a triggered job"},
	"GET /scheduler/api/runs/latest":                {routeGroupScheduler, "Every job's newest run, without output"},
	"GET /scheduler/api/job-references/{reference}": {routeGroupScheduler, "A job by its reference id (`scheduler_job_000076`), with `reference_id` and `last_run`"},
	"GET /scheduler/api/event-trigger-subjects":     {routeGroupScheduler, "The bus subjects a job's event trigger may name, with each one's fields"},
	"GET /scheduler/api/calendar/events":            {routeGroupScheduler, "Every enabled job's cron occurrences between `from` and `to` (RFC3339)"},
	"GET /scheduler/settings":                       {routeGroupScheduler, "The model role each job binary and agent job asks for, stored here"},
	"PUT /scheduler/settings/{key}":                 {routeGroupScheduler, "Store a model role, checked with model-store: `{\"value\"}`"},

	"GET /rules/health":                {routeGroupRuleStore, "Counts of each record, and the reactor's state"},
	"GET /rules/vocabulary":            {routeGroupRuleStore, "Sources, condition kinds, session fields, effect kinds, modes and rule states"},
	"GET /rules/rules":                 {routeGroupRuleStore, "Every rule, oldest first: `state`"},
	"POST /rules/rules":                {routeGroupRuleStore, "Create a draft rule: `{name, description?, source, created_by}`. 201"},
	"GET /rules/rules/{id}":            {routeGroupRuleStore, "A rule with its revisions and activations"},
	"POST /rules/rules/{id}/revisions": {routeGroupRuleStore, "Add a validated, immutable revision. 201"},
	"POST /rules/rules/{id}/activate":  {routeGroupRuleStore, "Activate a revision in `shadow` or `live` mode"},
	"POST /rules/rules/{id}/pause":     {routeGroupRuleStore, "Pause an active rule"},
	"POST /rules/rules/{id}/retire":    {routeGroupRuleStore, "Retire a rule, for good"},
	"POST /rules/rules/{id}/preview":   {routeGroupRuleStore, "What a revision or definition would decide about every session held now; records nothing"},
	"GET /rules/rules/{id}/comparison": {routeGroupRuleStore, "Whether each run's planned effects are visible at their owners now"},
	"GET /rules/runs":                  {routeGroupRuleStore, "Decisions, newest first: `rule_id`, `subject_id`, `since`, `limit`"},
	"GET /rules/subjects/{type}/{id}":  {routeGroupRuleStore, "rule-store's copy of a subject and its state episode"},
	"GET /rules/activity":              {routeGroupRuleStore, "The reactor's recent activity, newest first: `after`, `kind`, `limit`"},
	"GET /rules/summary":               {routeGroupRuleStore, "Counts, the reactor's state and every rule with its run counts"},
}

func init() {
	for pattern, rule := range automationRouteAccessRules {
		if _, taken := routeAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		routeAccessRules[pattern] = rule
	}
	for pattern, description := range automationRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupScheduler, routeGroupRuleStore)
}
