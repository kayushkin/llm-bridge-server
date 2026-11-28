package routeaccess

import "fmt"

// The rules and descriptions of the routes usage_and_eval_store_routes.go
// mounts. They are added to RouteAccessRules, routeDescriptions and the
// README's group order at start, so the shared tables gain them in one place.

// usageStoreRouteRule is the rule of every usage-store route.
var usageStoreRouteRule = operatorRoute("usage-store holds the subscription usage and API spend of this server's accounts, with no owner on a row; a refresh spends the providers' rate limits and a top-up changes the balance the usage page shows")

// evalStoreRouteRule is the rule of every eval-store route.
var evalStoreRouteRule = operatorRoute("eval-store's cases are copies of real model calls, which hold real mail, with no owner on a row; a run spends an organization's operation budget")

var UsageAndEvalStoreRouteAccessRules = map[string]RouteAccessRule{
	"GET /usage/api/usage":                                        usageStoreRouteRule,
	"GET /usage/api/usage/limits":                                 usageStoreRouteRule,
	"GET /usage/api/usage/limits/{provider}":                      usageStoreRouteRule,
	"GET /usage/api/usage/limits/{provider}/history":              usageStoreRouteRule,
	"POST /usage/api/usage/limits/refresh":                        usageStoreRouteRule,
	"POST /usage/api/usage/limits/watch":                          usageStoreRouteRule,
	"GET /usage/api/usage/spend/keys":                             usageStoreRouteRule,
	"GET /usage/api/usage/spend/keys/{provider}/{api_key_id}/raw": usageStoreRouteRule,
	"POST /usage/api/usage/spend/refresh":                         usageStoreRouteRule,
	"GET /usage/api/usage/spend/topups":                           usageStoreRouteRule,
	"POST /usage/api/usage/spend/topups":                          usageStoreRouteRule,
	"DELETE /usage/api/usage/spend/topups/{id}":                   usageStoreRouteRule,
	"GET /usage/health":                                           usageStoreRouteRule,

	"GET /evaluations/health":                               evalStoreRouteRule,
	"GET /evaluations/vocabulary":                           evalStoreRouteRule,
	"GET /evaluations/suites":                               evalStoreRouteRule,
	"POST /evaluations/suites":                              evalStoreRouteRule,
	"GET /evaluations/suites/{id}":                          evalStoreRouteRule,
	"PATCH /evaluations/suites/{id}":                        evalStoreRouteRule,
	"DELETE /evaluations/suites/{id}":                       evalStoreRouteRule,
	"GET /evaluations/suites/{id}/cases":                    evalStoreRouteRule,
	"POST /evaluations/suites/{id}/cases":                   evalStoreRouteRule,
	"POST /evaluations/suites/{id}/cases/import-operations": evalStoreRouteRule,
	"GET /evaluations/cases/{id}":                           evalStoreRouteRule,
	"PATCH /evaluations/cases/{id}":                         evalStoreRouteRule,
	"GET /evaluations/runs":                                 evalStoreRouteRule,
	"POST /evaluations/runs":                                evalStoreRouteRule,
	"GET /evaluations/runs/{id}":                            evalStoreRouteRule,
	"PATCH /evaluations/runs/{id}":                          evalStoreRouteRule,
	"DELETE /evaluations/runs/{id}":                         evalStoreRouteRule,
	"POST /evaluations/runs/{id}/start":                     evalStoreRouteRule,
	"POST /evaluations/runs/{id}/resume":                    evalStoreRouteRule,
	"POST /evaluations/runs/{id}/stop":                      evalStoreRouteRule,
	"GET /evaluations/runs/{id}/answers":                    evalStoreRouteRule,
	"GET /evaluations/runs/{id}/verdicts":                   evalStoreRouteRule,
	"GET /evaluations/runs/{id}/checks":                     evalStoreRouteRule,
	"POST /evaluations/runs/{id}/checks":                    evalStoreRouteRule,
	"GET /evaluations/runs/{id}/report":                     evalStoreRouteRule,
	"GET /evaluations/runs/{id}/cost-projection":            evalStoreRouteRule,
}

const (
	routeGroupUsageStore routeGroup = "usage-store, mounted in this process"
	routeGroupEvalStore  routeGroup = "eval-store, mounted in this process"
)

var usageAndEvalStoreRouteDescriptions = map[string]routeDescription{
	"GET /usage/api/usage":                                        {routeGroupUsageStore, "Token totals per agent, orchestrator and model over the last `day`, `week` and `month`, from model-store's old daily usage rows"},
	"GET /usage/api/usage/limits":                                 {routeGroupUsageStore, "The latest limits snapshot of `anthropic` and `codex`, each with `stale_after`"},
	"GET /usage/api/usage/limits/{provider}":                      {routeGroupUsageStore, "One provider's latest limits snapshot; 404 when it has none"},
	"GET /usage/api/usage/limits/{provider}/history":              {routeGroupUsageStore, "One provider's snapshot rows, oldest first: `window`, `since`, `until`, `limit`"},
	"POST /usage/api/usage/limits/refresh":                        {routeGroupUsageStore, "Read `?provider=anthropic|codex` now and keep the snapshot"},
	"POST /usage/api/usage/limits/watch":                          {routeGroupUsageStore, "The usage page is open: read limits on the watched interval for a while. 204"},
	"GET /usage/api/usage/spend/keys":                             {routeGroupUsageStore, "Each account's API keys with their 24h, 7d and 30d spend, top-ups and remaining credit"},
	"GET /usage/api/usage/spend/keys/{provider}/{api_key_id}/raw": {routeGroupUsageStore, "The provider's last answer about one key, as it came"},
	"POST /usage/api/usage/spend/refresh":                         {routeGroupUsageStore, "Read `?provider=anthropic` per-key spend now"},
	"GET /usage/api/usage/spend/topups":                           {routeGroupUsageStore, "A provider's top-ups, earliest first: `?provider=`"},
	"POST /usage/api/usage/spend/topups":                          {routeGroupUsageStore, "Record credit bought: `{\"provider\",\"amount_usd\",\"occurred_at\"|\"occurred_at_str\",\"note\"}`"},
	"DELETE /usage/api/usage/spend/topups/{id}":                   {routeGroupUsageStore, "Delete one top-up. 204"},
	"GET /usage/health":                                           {routeGroupUsageStore, "`{\"status\":\"ok\"}`"},

	"GET /evaluations/health":                               {routeGroupEvalStore, "Counts of each record"},
	"GET /evaluations/vocabulary":                           {routeGroupEvalStore, "Run statuses, source kinds, checkers, selection modes and answer origins"},
	"GET /evaluations/suites":                               {routeGroupEvalStore, "Every suite"},
	"POST /evaluations/suites":                              {routeGroupEvalStore, "Create a suite"},
	"GET /evaluations/suites/{id}":                          {routeGroupEvalStore, "One suite"},
	"PATCH /evaluations/suites/{id}":                        {routeGroupEvalStore, "Change the suite fields named"},
	"DELETE /evaluations/suites/{id}":                       {routeGroupEvalStore, "Delete a suite; 409 while it has runs"},
	"GET /evaluations/suites/{id}/cases":                    {routeGroupEvalStore, "A suite's cases without their prompts or answers"},
	"POST /evaluations/suites/{id}/cases":                   {routeGroupEvalStore, "Add one case"},
	"POST /evaluations/suites/{id}/cases/import-operations": {routeGroupEvalStore, "Import succeeded llm.completion operations as cases"},
	"GET /evaluations/cases/{id}":                           {routeGroupEvalStore, "One case with its prompts and original answer"},
	"PATCH /evaluations/cases/{id}":                         {routeGroupEvalStore, "Replace a case's `expected_answer`"},
	"GET /evaluations/runs":                                 {routeGroupEvalStore, "Runs, newest first: `?suite_id=`"},
	"POST /evaluations/runs":                                {routeGroupEvalStore, "Create a run, freezing its cases and model-store's prices"},
	"GET /evaluations/runs/{id}":                            {routeGroupEvalStore, "One run and its progress"},
	"PATCH /evaluations/runs/{id}":                          {routeGroupEvalStore, "Change a run's `prediction_id` or `note`"},
	"DELETE /evaluations/runs/{id}":                         {routeGroupEvalStore, "Delete a run with its answers, verdicts and checks; 409 while it is going"},
	"POST /evaluations/runs/{id}/start":                     {routeGroupEvalStore, "Start a created run (202); 503 while `evaluations.runs.enabled` is off"},
	"POST /evaluations/runs/{id}/resume":                    {routeGroupEvalStore, "Make what a run is missing, and with `retry_failed` what failed (202); 503 while runs are off"},
	"POST /evaluations/runs/{id}/stop":                      {routeGroupEvalStore, "Finish the calls in flight, then stop"},
	"GET /evaluations/runs/{id}/answers":                    {routeGroupEvalStore, "A run's answers; bodies only with `include_bodies=true`"},
	"GET /evaluations/runs/{id}/verdicts":                   {routeGroupEvalStore, "A run's verdicts; bodies and reasons only with `include_bodies=true`"},
	"GET /evaluations/runs/{id}/checks":                     {routeGroupEvalStore, "A run's check results"},
	"POST /evaluations/runs/{id}/checks":                    {routeGroupEvalStore, "Run the suite's checker over a run's answers again"},
	"GET /evaluations/runs/{id}/report":                     {routeGroupEvalStore, "A run's report: `format=json|markdown`, `calls_per_week`"},
	"GET /evaluations/runs/{id}/cost-projection":            {routeGroupEvalStore, "What each model would cost a week, and what reaches `target_usd`"},
}

func init() {
	for pattern, rule := range UsageAndEvalStoreRouteAccessRules {
		if _, taken := RouteAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		RouteAccessRules[pattern] = rule
	}
	for pattern, description := range usageAndEvalStoreRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupUsageStore, routeGroupEvalStore)
}
