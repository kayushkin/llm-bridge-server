package server

import "fmt"

// The rules and descriptions of the routes usage_and_eval_store_routes.go
// mounts. They are added to routeAccessRules, routeDescriptions and the
// README's group order at start, so the shared tables gain them in one place.

// usageStoreRouteRule is the rule of every usage-store route.
var usageStoreRouteRule = operatorRoute("usage-store holds the subscription usage and API spend of this server's accounts, with no owner on a row; a refresh spends the providers' rate limits and a top-up changes the balance the usage page shows")

// evalStoreRouteRule is the rule of every eval-store route.
var evalStoreRouteRule = operatorRoute("eval-store's cases are copies of real model calls, which hold real mail, with no owner on a row; a run spends an organization's operation budget")

var usageAndEvalStoreRouteAccessRules = map[string]routeAccessRule{
	"GET /usage-store/api/usage":                                        usageStoreRouteRule,
	"GET /usage-store/api/usage/limits":                                 usageStoreRouteRule,
	"GET /usage-store/api/usage/limits/{provider}":                      usageStoreRouteRule,
	"GET /usage-store/api/usage/limits/{provider}/history":              usageStoreRouteRule,
	"POST /usage-store/api/usage/limits/refresh":                        usageStoreRouteRule,
	"POST /usage-store/api/usage/limits/watch":                          usageStoreRouteRule,
	"GET /usage-store/api/usage/spend/keys":                             usageStoreRouteRule,
	"GET /usage-store/api/usage/spend/keys/{provider}/{api_key_id}/raw": usageStoreRouteRule,
	"POST /usage-store/api/usage/spend/refresh":                         usageStoreRouteRule,
	"GET /usage-store/api/usage/spend/topups":                           usageStoreRouteRule,
	"POST /usage-store/api/usage/spend/topups":                          usageStoreRouteRule,
	"DELETE /usage-store/api/usage/spend/topups/{id}":                   usageStoreRouteRule,
	"GET /usage-store/health":                                           usageStoreRouteRule,

	"GET /eval-store/health":                               evalStoreRouteRule,
	"GET /eval-store/vocabulary":                           evalStoreRouteRule,
	"GET /eval-store/suites":                               evalStoreRouteRule,
	"POST /eval-store/suites":                              evalStoreRouteRule,
	"GET /eval-store/suites/{id}":                          evalStoreRouteRule,
	"PATCH /eval-store/suites/{id}":                        evalStoreRouteRule,
	"DELETE /eval-store/suites/{id}":                       evalStoreRouteRule,
	"GET /eval-store/suites/{id}/cases":                    evalStoreRouteRule,
	"POST /eval-store/suites/{id}/cases":                   evalStoreRouteRule,
	"POST /eval-store/suites/{id}/cases/import-operations": evalStoreRouteRule,
	"GET /eval-store/cases/{id}":                           evalStoreRouteRule,
	"PATCH /eval-store/cases/{id}":                         evalStoreRouteRule,
	"GET /eval-store/runs":                                 evalStoreRouteRule,
	"POST /eval-store/runs":                                evalStoreRouteRule,
	"GET /eval-store/runs/{id}":                            evalStoreRouteRule,
	"PATCH /eval-store/runs/{id}":                          evalStoreRouteRule,
	"DELETE /eval-store/runs/{id}":                         evalStoreRouteRule,
	"POST /eval-store/runs/{id}/start":                     evalStoreRouteRule,
	"POST /eval-store/runs/{id}/resume":                    evalStoreRouteRule,
	"POST /eval-store/runs/{id}/stop":                      evalStoreRouteRule,
	"GET /eval-store/runs/{id}/answers":                    evalStoreRouteRule,
	"GET /eval-store/runs/{id}/verdicts":                   evalStoreRouteRule,
	"GET /eval-store/runs/{id}/checks":                     evalStoreRouteRule,
	"POST /eval-store/runs/{id}/checks":                    evalStoreRouteRule,
	"GET /eval-store/runs/{id}/report":                     evalStoreRouteRule,
	"GET /eval-store/runs/{id}/cost-projection":            evalStoreRouteRule,
}

const (
	routeGroupUsageStore routeGroup = "usage-store, mounted in this process"
	routeGroupEvalStore  routeGroup = "eval-store, mounted in this process"
)

var usageAndEvalStoreRouteDescriptions = map[string]routeDescription{
	"GET /usage-store/api/usage":                                        {routeGroupUsageStore, "Token totals per agent, orchestrator and model over the last `day`, `week` and `month`, from model-store's old daily usage rows"},
	"GET /usage-store/api/usage/limits":                                 {routeGroupUsageStore, "The latest limits snapshot of `anthropic` and `codex`, each with `stale_after`"},
	"GET /usage-store/api/usage/limits/{provider}":                      {routeGroupUsageStore, "One provider's latest limits snapshot; 404 when it has none"},
	"GET /usage-store/api/usage/limits/{provider}/history":              {routeGroupUsageStore, "One provider's snapshot rows, oldest first: `window`, `since`, `until`, `limit`"},
	"POST /usage-store/api/usage/limits/refresh":                        {routeGroupUsageStore, "Read `?provider=anthropic|codex` now and keep the snapshot"},
	"POST /usage-store/api/usage/limits/watch":                          {routeGroupUsageStore, "The usage page is open: read limits on the watched interval for a while. 204"},
	"GET /usage-store/api/usage/spend/keys":                             {routeGroupUsageStore, "Each account's API keys with their 24h, 7d and 30d spend, top-ups and remaining credit"},
	"GET /usage-store/api/usage/spend/keys/{provider}/{api_key_id}/raw": {routeGroupUsageStore, "The provider's last answer about one key, as it came"},
	"POST /usage-store/api/usage/spend/refresh":                         {routeGroupUsageStore, "Read `?provider=anthropic` per-key spend now"},
	"GET /usage-store/api/usage/spend/topups":                           {routeGroupUsageStore, "A provider's top-ups, earliest first: `?provider=`"},
	"POST /usage-store/api/usage/spend/topups":                          {routeGroupUsageStore, "Record credit bought: `{\"provider\",\"amount_usd\",\"occurred_at\"|\"occurred_at_str\",\"note\"}`"},
	"DELETE /usage-store/api/usage/spend/topups/{id}":                   {routeGroupUsageStore, "Delete one top-up. 204"},
	"GET /usage-store/health":                                           {routeGroupUsageStore, "`{\"status\":\"ok\"}`"},

	"GET /eval-store/health":                               {routeGroupEvalStore, "Counts of each record"},
	"GET /eval-store/vocabulary":                           {routeGroupEvalStore, "Run statuses, source kinds, checkers, selection modes and answer origins"},
	"GET /eval-store/suites":                               {routeGroupEvalStore, "Every suite"},
	"POST /eval-store/suites":                              {routeGroupEvalStore, "Create a suite"},
	"GET /eval-store/suites/{id}":                          {routeGroupEvalStore, "One suite"},
	"PATCH /eval-store/suites/{id}":                        {routeGroupEvalStore, "Change the suite fields named"},
	"DELETE /eval-store/suites/{id}":                       {routeGroupEvalStore, "Delete a suite; 409 while it has runs"},
	"GET /eval-store/suites/{id}/cases":                    {routeGroupEvalStore, "A suite's cases without their prompts or answers"},
	"POST /eval-store/suites/{id}/cases":                   {routeGroupEvalStore, "Add one case"},
	"POST /eval-store/suites/{id}/cases/import-operations": {routeGroupEvalStore, "Import succeeded llm.completion operations as cases"},
	"GET /eval-store/cases/{id}":                           {routeGroupEvalStore, "One case with its prompts and original answer"},
	"PATCH /eval-store/cases/{id}":                         {routeGroupEvalStore, "Replace a case's `expected_answer`"},
	"GET /eval-store/runs":                                 {routeGroupEvalStore, "Runs, newest first: `?suite_id=`"},
	"POST /eval-store/runs":                                {routeGroupEvalStore, "Create a run, freezing its cases and model-store's prices"},
	"GET /eval-store/runs/{id}":                            {routeGroupEvalStore, "One run and its progress"},
	"PATCH /eval-store/runs/{id}":                          {routeGroupEvalStore, "Change a run's `prediction_id` or `note`"},
	"DELETE /eval-store/runs/{id}":                         {routeGroupEvalStore, "Delete a run with its answers, verdicts and checks; 409 while it is going"},
	"POST /eval-store/runs/{id}/start":                     {routeGroupEvalStore, "Start a created run (202); 503 while `eval_store.runs.enabled` is off"},
	"POST /eval-store/runs/{id}/resume":                    {routeGroupEvalStore, "Make what a run is missing, and with `retry_failed` what failed (202); 503 while runs are off"},
	"POST /eval-store/runs/{id}/stop":                      {routeGroupEvalStore, "Finish the calls in flight, then stop"},
	"GET /eval-store/runs/{id}/answers":                    {routeGroupEvalStore, "A run's answers; bodies only with `include_bodies=true`"},
	"GET /eval-store/runs/{id}/verdicts":                   {routeGroupEvalStore, "A run's verdicts; bodies and reasons only with `include_bodies=true`"},
	"GET /eval-store/runs/{id}/checks":                     {routeGroupEvalStore, "A run's check results"},
	"POST /eval-store/runs/{id}/checks":                    {routeGroupEvalStore, "Run the suite's checker over a run's answers again"},
	"GET /eval-store/runs/{id}/report":                     {routeGroupEvalStore, "A run's report: `format=json|markdown`, `calls_per_week`"},
	"GET /eval-store/runs/{id}/cost-projection":            {routeGroupEvalStore, "What each model would cost a week, and what reaches `target_usd`"},
}

func init() {
	for pattern, rule := range usageAndEvalStoreRouteAccessRules {
		if _, taken := routeAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		routeAccessRules[pattern] = rule
	}
	for pattern, description := range usageAndEvalStoreRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupUsageStore, routeGroupEvalStore)
}
