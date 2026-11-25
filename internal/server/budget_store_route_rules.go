package server

import "fmt"

// The rule and descriptions of the budget-store routes
// (budget_store_routes.go). They are kept here and added to routeAccessRules,
// routeDescriptions and the README's group order at start, so the shared
// tables gain them in one place.

// budgetStoreRouteRule is the rule of every budget-store route.
var budgetStoreRouteRule = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "budget-store as the caller's principal: every row has an owner and each person keeps books of their own, so the store works in the books of whoever keeps the record a path names (404 for anyone else) or in the caller's own; an administrator, and the internal service acting as itself, reach any owner's record by its id and any owner's books by owner_principal_id"}

const routeGroupBudgetStore routeGroup = "budget-store, mounted in this process"

var budgetStoreRouteDescriptions = map[string]routeDescription{
	"GET /budget-store/health":                                           {routeGroupBudgetStore, "`{\"status\":\"ok\"}`"},
	"GET /budget-store/vocabulary":                                       {routeGroupBudgetStore, "Every closed set of values budget-store accepts"},
	"GET /budget-store/institutions":                                     {routeGroupBudgetStore, "The banks and issuers in the books"},
	"POST /budget-store/institutions":                                    {routeGroupBudgetStore, "Add a bank or issuer; 201"},
	"GET /budget-store/accounts":                                         {routeGroupBudgetStore, "The accounts in the books, each with its balance and details"},
	"POST /budget-store/accounts":                                        {routeGroupBudgetStore, "Add an account; 201"},
	"GET /budget-store/accounts/{id}":                                    {routeGroupBudgetStore, "One account with its balance and details"},
	"PATCH /budget-store/accounts/{id}":                                  {routeGroupBudgetStore, "Change an account's fields; a detail block replaces the stored one"},
	"POST /budget-store/accounts/{id}/description-markers":               {routeGroupBudgetStore, "Add text that names this account in other accounts' rows"},
	"DELETE /budget-store/accounts/{id}/description-markers/{marker_id}": {routeGroupBudgetStore, "Remove a description marker"},
	"GET /budget-store/accounts/{id}/loan-schedule":                      {routeGroupBudgetStore, "A loan's payments split into interest and principal, and the projection to payoff"},
	"POST /budget-store/accounts/{id}/loan-schedule/apply":               {routeGroupBudgetStore, "Write the loan schedule onto the loan's payments"},
	"GET /budget-store/accounts/{id}/loan-payment-facts":                 {routeGroupBudgetStore, "What statements say about a loan's payments"},
	"POST /budget-store/accounts/{id}/loan-payment-facts":                {routeGroupBudgetStore, "Record what a statement says about payments, replacing one on the same date"},
	"POST /budget-store/accounts/{id}/opening-balance":                   {routeGroupBudgetStore, "Make the ledger agree with a reported balance with one opening-balance transaction"},
	"GET /budget-store/balance-assertions":                               {routeGroupBudgetStore, "Reported balances, each with the ledger's balance and the difference"},
	"POST /budget-store/balance-assertions":                              {routeGroupBudgetStore, "Record a reported balance; 201"},
	"GET /budget-store/categories":                                       {routeGroupBudgetStore, "The categories in the books"},
	"POST /budget-store/categories":                                      {routeGroupBudgetStore, "Add a category; 201"},
	"GET /budget-store/categories/{id}":                                  {routeGroupBudgetStore, "One category"},
	"GET /budget-store/payees/{id}":                                      {routeGroupBudgetStore, "One payee with its patterns"},
	"GET /budget-store/payees":                                           {routeGroupBudgetStore, "The payees in the books"},
	"POST /budget-store/payees":                                          {routeGroupBudgetStore, "Add a payee and the bank texts that name it; 201"},
	"POST /budget-store/payees/apply-rules":                              {routeGroupBudgetStore, "Name each transaction without a payee by the payee rules, and move a wholly Uncategorized one to its payee's category"},
	"GET /budget-store/transactions":                                     {routeGroupBudgetStore, "Transactions newest first, by `account_id`, `category_id`, `month`, `q`, `uncategorized`, `limit` and `offset`"},
	"POST /budget-store/transactions":                                    {routeGroupBudgetStore, "Record a balanced transaction by hand; 201"},
	"GET /budget-store/transactions/{id}":                                {routeGroupBudgetStore, "One transaction with its postings, source rows, category decision, orders and links"},
	"PATCH /budget-store/transactions/{id}":                              {routeGroupBudgetStore, "Change a transaction's status, payee or memo"},
	"PUT /budget-store/transactions/{id}/categories":                     {routeGroupBudgetStore, "Replace a transaction's category postings with splits summing to the same"},
	"POST /budget-store/transactions/{id}/undo-transfer-without-partner": {routeGroupBudgetStore, "Put a posting the transfer matcher moved without a partner back on Uncategorized"},
	"POST /budget-store/transactions/{id}/move-to-account":               {routeGroupBudgetStore, "Turn a transaction's one category posting into a posting on another of the owner's accounts"},
	"POST /budget-store/transfers/match":                                 {routeGroupBudgetStore, "Pair transfers between the owner's own accounts"},
	"GET /budget-store/import-batches":                                   {routeGroupBudgetStore, "Imported files, newest first, with what became of their rows"},
	"POST /budget-store/import-batches":                                  {routeGroupBudgetStore, "Import one bank file into an account, all or nothing; 201"},
	"GET /budget-store/import-batches/{id}":                              {routeGroupBudgetStore, "One imported file"},
	"POST /budget-store/import-batches/{id}/undo":                        {routeGroupBudgetStore, "Delete what an imported file made, keeping its rows"},
	"GET /budget-store/reports/spending":                                 {routeGroupBudgetStore, "Each category's total in one `month`"},
	"GET /budget-store/reports/monthly":                                  {routeGroupBudgetStore, "Each category's total in each month from `from` to `to`"},
	"GET /budget-store/reports/by-account":                               {routeGroupBudgetStore, "The named categories and those under them, per month, category and account"},
	"GET /budget-store/budget":                                           {routeGroupBudgetStore, "One `month`'s budget in the limit or envelope `view`"},
	"PUT /budget-store/budget-assignments":                               {routeGroupBudgetStore, "Set one category's amount for one month"},
	"GET /budget-store/category-paths":                                   {routeGroupBudgetStore, "Every category's path from the top of its tree"},
	"GET /budget-store/category-suggestions/candidates":                  {routeGroupBudgetStore, "Wholly Uncategorized transactions with no pending suggestion, for the categorizer"},
	"GET /budget-store/category-suggestions":                             {routeGroupBudgetStore, "Category suggestions by `status`, with each transaction's facts"},
	"POST /budget-store/category-suggestions":                            {routeGroupBudgetStore, "Record a model's category suggestions; it writes no category"},
	"POST /budget-store/category-suggestions/decisions":                  {routeGroupBudgetStore, "A person accepting, changing or rejecting suggestions, all or nothing"},
	"POST /budget-store/recurring-bills/detect":                          {routeGroupBudgetStore, "Find payments that come on a steady rhythm and refresh the known bills"},
	"GET /budget-store/recurring-bills":                                  {routeGroupBudgetStore, "Recurring bills by `status`, soonest due first"},
	"POST /budget-store/recurring-bills":                                 {routeGroupBudgetStore, "Name a bill from a transaction's payments, with the caller's cadence; 201"},
	"GET /budget-store/recurring-bills/totals":                           {routeGroupBudgetStore, "Confirmed bills: expected and actual per month, six months and year"},
	"GET /budget-store/recurring-bills/{id}":                             {routeGroupBudgetStore, "One bill with every payment"},
	"PATCH /budget-store/recurring-bills/{id}":                           {routeGroupBudgetStore, "A person confirming, rejecting or renaming a bill"},
	"GET /budget-store/trips/totals":                                     {routeGroupBudgetStore, "Confirmed and suggested spending per location-store trip"},
	"GET /budget-store/trips/{trip_ref}/transactions":                    {routeGroupBudgetStore, "The transactions linked to one trip, removed ones included"},
	"POST /budget-store/trips/{trip_ref}/suggest":                        {routeGroupBudgetStore, "Suggest the spending dated during a trip"},
	"PUT /budget-store/trips/{trip_ref}/transactions/{transaction_id}":   {routeGroupBudgetStore, "A person confirming, removing or adding a transaction on a trip"},
	"POST /budget-store/marketplace-orders":                              {routeGroupBudgetStore, "Record a marketplace order and its lines, finding its charge; 201"},
	"GET /budget-store/marketplace-orders":                               {routeGroupBudgetStore, "Marketplace orders newest first, by `transaction_id` and `unmatched`"},
	"POST /budget-store/marketplace-orders/match-transactions":           {routeGroupBudgetStore, "Look again for the charge of every order without one"},
	"GET /budget-store/marketplace-orders/{id}":                          {routeGroupBudgetStore, "One marketplace order with its lines"},
	"GET /budget-store/marketplace-order-lines/{id}":                     {routeGroupBudgetStore, "One line of a marketplace order"},
	"PATCH /budget-store/marketplace-order-lines/{id}":                   {routeGroupBudgetStore, "Change what became of an order line"},
	"GET /budget-store/interests":                                        {routeGroupBudgetStore, "The interests followed in the books, each with its rules and total"},
	"POST /budget-store/interests":                                       {routeGroupBudgetStore, "Add an interest; 201"},
	"POST /budget-store/interests/match":                                 {routeGroupBudgetStore, "Run every interest's rules again for new transactions"},
	"GET /budget-store/interests/{id}":                                   {routeGroupBudgetStore, "One interest with its rules, history sources and total"},
	"POST /budget-store/interests/{id}/rules":                            {routeGroupBudgetStore, "Add a rule to an interest and confirm what it matches"},
	"POST /budget-store/interests/{id}/history-sources":                  {routeGroupBudgetStore, "Point an interest at another store's record of it"},
	"GET /budget-store/interests/{id}/transactions":                      {routeGroupBudgetStore, "An interest's transactions, removed ones included"},
	"PUT /budget-store/interests/{id}/transactions/{transaction_id}":     {routeGroupBudgetStore, "A person confirming or removing a transaction on an interest"},
	"POST /budget-store/plaid/link-token":                                {routeGroupBudgetStore, "A Plaid Link token for linking one more bank"},
	"GET /budget-store/plaid/items":                                      {routeGroupBudgetStore, "Every bank linked through Plaid, with its accounts"},
	"POST /budget-store/plaid/items":                                     {routeGroupBudgetStore, "Link a bank from Plaid Link's public token, keeping its access token in auth-store; 201"},
	"PUT /budget-store/plaid/accounts/{plaid_account_id}":                {routeGroupBudgetStore, "Match a Plaid account to one of the owner's accounts, or unmatch it"},
	"POST /budget-store/plaid/sync":                                      {routeGroupBudgetStore, "Sync every linked bank's transactions from its stored cursor"},
	"GET /budget-store/links":                                            {routeGroupBudgetStore, "Links from one of the store's records (`from_id`) to other stores' records"},
	"POST /budget-store/links":                                           {routeGroupBudgetStore, "Link one of the store's records to another store's record by its id; 201"},
}

func init() {
	for pattern, description := range budgetStoreRouteDescriptions {
		if _, taken := routeAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		routeAccessRules[pattern] = budgetStoreRouteRule
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupBudgetStore)
}
