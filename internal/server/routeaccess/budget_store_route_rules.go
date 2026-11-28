package routeaccess

import "fmt"

// The rule and descriptions of the budget-store routes
// (budget_store_routes.go). They are kept here and added to RouteAccessRules,
// routeDescriptions and the README's group order at start, so the shared
// tables gain them in one place.

// budgetStoreRouteRule is the rule of every budget-store route.
var budgetStoreRouteRule = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "budget-store as the caller's principal: every row has an owner and each person keeps books of their own, so the store works in the books of whoever keeps the record a path names (404 for anyone else) or in the caller's own; an administrator, and the internal service acting as itself, reach any owner's record by its id and any owner's books by owner_principal_id"}

const routeGroupBudgetStore routeGroup = "budget-store, mounted in this process"

var budgetStoreRouteDescriptions = map[string]routeDescription{
	"GET /budget/health":                                           {routeGroupBudgetStore, "`{\"status\":\"ok\"}`"},
	"GET /budget/vocabulary":                                       {routeGroupBudgetStore, "Every closed set of values budget-store accepts"},
	"GET /budget/institutions":                                     {routeGroupBudgetStore, "The banks and issuers in the books"},
	"POST /budget/institutions":                                    {routeGroupBudgetStore, "Add a bank or issuer; 201"},
	"GET /budget/accounts":                                         {routeGroupBudgetStore, "The accounts in the books, each with its balance and details"},
	"POST /budget/accounts":                                        {routeGroupBudgetStore, "Add an account; 201"},
	"GET /budget/accounts/{id}":                                    {routeGroupBudgetStore, "One account with its balance and details"},
	"PATCH /budget/accounts/{id}":                                  {routeGroupBudgetStore, "Change an account's fields; a detail block replaces the stored one"},
	"POST /budget/accounts/{id}/description-markers":               {routeGroupBudgetStore, "Add text that names this account in other accounts' rows"},
	"DELETE /budget/accounts/{id}/description-markers/{marker_id}": {routeGroupBudgetStore, "Remove a description marker"},
	"GET /budget/accounts/{id}/loan-schedule":                      {routeGroupBudgetStore, "A loan's payments split into interest and principal, and the projection to payoff"},
	"POST /budget/accounts/{id}/loan-schedule/apply":               {routeGroupBudgetStore, "Write the loan schedule onto the loan's payments"},
	"GET /budget/accounts/{id}/loan-payment-facts":                 {routeGroupBudgetStore, "What statements say about a loan's payments"},
	"POST /budget/accounts/{id}/loan-payment-facts":                {routeGroupBudgetStore, "Record what a statement says about payments, replacing one on the same date"},
	"POST /budget/accounts/{id}/opening-balance":                   {routeGroupBudgetStore, "Make the ledger agree with a reported balance with one opening-balance transaction"},
	"GET /budget/balance-assertions":                               {routeGroupBudgetStore, "Reported balances, each with the ledger's balance and the difference"},
	"POST /budget/balance-assertions":                              {routeGroupBudgetStore, "Record a reported balance; 201"},
	"GET /budget/categories":                                       {routeGroupBudgetStore, "The categories in the books"},
	"POST /budget/categories":                                      {routeGroupBudgetStore, "Add a category; 201"},
	"GET /budget/categories/{id}":                                  {routeGroupBudgetStore, "One category"},
	"GET /budget/payees/{id}":                                      {routeGroupBudgetStore, "One payee with its patterns"},
	"GET /budget/payees":                                           {routeGroupBudgetStore, "The payees in the books"},
	"POST /budget/payees":                                          {routeGroupBudgetStore, "Add a payee and the bank texts that name it; 201"},
	"POST /budget/payees/apply-rules":                              {routeGroupBudgetStore, "Name each transaction without a payee by the payee rules, and move a wholly Uncategorized one to its payee's category"},
	"GET /budget/transactions":                                     {routeGroupBudgetStore, "Transactions newest first, by `account_id`, `category_id`, `month`, `q`, `uncategorized`, `limit` and `offset`"},
	"POST /budget/transactions":                                    {routeGroupBudgetStore, "Record a balanced transaction by hand; 201"},
	"GET /budget/transactions/{id}":                                {routeGroupBudgetStore, "One transaction with its postings, source rows, category decision, orders and links"},
	"PATCH /budget/transactions/{id}":                              {routeGroupBudgetStore, "Change a transaction's status, payee or memo"},
	"PUT /budget/transactions/{id}/categories":                     {routeGroupBudgetStore, "Replace a transaction's category postings with splits summing to the same"},
	"POST /budget/transactions/{id}/undo-transfer-without-partner": {routeGroupBudgetStore, "Put a posting the transfer matcher moved without a partner back on Uncategorized"},
	"POST /budget/transactions/{id}/move-to-account":               {routeGroupBudgetStore, "Turn a transaction's one category posting into a posting on another of the owner's accounts"},
	"POST /budget/transfers/match":                                 {routeGroupBudgetStore, "Pair transfers between the owner's own accounts"},
	"GET /budget/import-batches":                                   {routeGroupBudgetStore, "Imported files, newest first, with what became of their rows"},
	"POST /budget/import-batches":                                  {routeGroupBudgetStore, "Import one bank file into an account, all or nothing; 201"},
	"GET /budget/import-batches/{id}":                              {routeGroupBudgetStore, "One imported file"},
	"POST /budget/import-batches/{id}/undo":                        {routeGroupBudgetStore, "Delete what an imported file made, keeping its rows"},
	"GET /budget/reports/spending":                                 {routeGroupBudgetStore, "Each category's total in one `month`"},
	"GET /budget/reports/monthly":                                  {routeGroupBudgetStore, "Each category's total in each month from `from` to `to`"},
	"GET /budget/reports/by-account":                               {routeGroupBudgetStore, "The named categories and those under them, per month, category and account"},
	"GET /budget/budget":                                           {routeGroupBudgetStore, "One `month`'s budget in the limit or envelope `view`"},
	"PUT /budget/budget-assignments":                               {routeGroupBudgetStore, "Set one category's amount for one month"},
	"GET /budget/category-paths":                                   {routeGroupBudgetStore, "Every category's path from the top of its tree"},
	"GET /budget/category-suggestions/candidates":                  {routeGroupBudgetStore, "Wholly Uncategorized transactions with no pending suggestion, for the categorizer"},
	"GET /budget/category-suggestions":                             {routeGroupBudgetStore, "Category suggestions by `status`, with each transaction's facts"},
	"POST /budget/category-suggestions":                            {routeGroupBudgetStore, "Record a model's category suggestions; it writes no category"},
	"POST /budget/category-suggestions/decisions":                  {routeGroupBudgetStore, "A person accepting, changing or rejecting suggestions, all or nothing"},
	"POST /budget/recurring-bills/detect":                          {routeGroupBudgetStore, "Find payments that come on a steady rhythm and refresh the known bills"},
	"GET /budget/recurring-bills":                                  {routeGroupBudgetStore, "Recurring bills by `status`, soonest due first"},
	"POST /budget/recurring-bills":                                 {routeGroupBudgetStore, "Name a bill from a transaction's payments, with the caller's cadence; 201"},
	"GET /budget/recurring-bills/totals":                           {routeGroupBudgetStore, "Confirmed bills: expected and actual per month, six months and year"},
	"GET /budget/recurring-bills/{id}":                             {routeGroupBudgetStore, "One bill with every payment"},
	"PATCH /budget/recurring-bills/{id}":                           {routeGroupBudgetStore, "A person confirming, rejecting or renaming a bill"},
	"GET /budget/trips/totals":                                     {routeGroupBudgetStore, "Confirmed and suggested spending per location-store trip"},
	"GET /budget/trips/{trip_ref}/transactions":                    {routeGroupBudgetStore, "The transactions linked to one trip, removed ones included"},
	"POST /budget/trips/{trip_ref}/suggest":                        {routeGroupBudgetStore, "Suggest the spending dated during a trip"},
	"PUT /budget/trips/{trip_ref}/transactions/{transaction_id}":   {routeGroupBudgetStore, "A person confirming, removing or adding a transaction on a trip"},
	"POST /budget/marketplace-orders":                              {routeGroupBudgetStore, "Record a marketplace order and its lines, finding its charge; 201"},
	"GET /budget/marketplace-orders":                               {routeGroupBudgetStore, "Marketplace orders newest first, by `transaction_id` and `unmatched`"},
	"POST /budget/marketplace-orders/match-transactions":           {routeGroupBudgetStore, "Look again for the charge of every order without one"},
	"GET /budget/marketplace-orders/{id}":                          {routeGroupBudgetStore, "One marketplace order with its lines"},
	"GET /budget/marketplace-order-lines/{id}":                     {routeGroupBudgetStore, "One line of a marketplace order"},
	"PATCH /budget/marketplace-order-lines/{id}":                   {routeGroupBudgetStore, "Change what became of an order line"},
	"GET /budget/interests":                                        {routeGroupBudgetStore, "The interests followed in the books, each with its rules and total"},
	"POST /budget/interests":                                       {routeGroupBudgetStore, "Add an interest; 201"},
	"POST /budget/interests/match":                                 {routeGroupBudgetStore, "Run every interest's rules again for new transactions"},
	"GET /budget/interests/{id}":                                   {routeGroupBudgetStore, "One interest with its rules, history sources and total"},
	"POST /budget/interests/{id}/rules":                            {routeGroupBudgetStore, "Add a rule to an interest and confirm what it matches"},
	"POST /budget/interests/{id}/history-sources":                  {routeGroupBudgetStore, "Point an interest at another store's record of it"},
	"GET /budget/interests/{id}/transactions":                      {routeGroupBudgetStore, "An interest's transactions, removed ones included"},
	"PUT /budget/interests/{id}/transactions/{transaction_id}":     {routeGroupBudgetStore, "A person confirming or removing a transaction on an interest"},
	"POST /budget/plaid/link-token":                                {routeGroupBudgetStore, "A Plaid Link token for linking one more bank"},
	"GET /budget/plaid/items":                                      {routeGroupBudgetStore, "Every bank linked through Plaid, with its accounts"},
	"POST /budget/plaid/items":                                     {routeGroupBudgetStore, "Link a bank from Plaid Link's public token, keeping its access token in auth-store; 201"},
	"PUT /budget/plaid/accounts/{plaid_account_id}":                {routeGroupBudgetStore, "Match a Plaid account to one of the owner's accounts, or unmatch it"},
	"POST /budget/plaid/sync":                                      {routeGroupBudgetStore, "Sync every linked bank's transactions from its stored cursor"},
	"GET /budget/links":                                            {routeGroupBudgetStore, "Links from one of the store's records (`from_id`) to other stores' records"},
	"POST /budget/links":                                           {routeGroupBudgetStore, "Link one of the store's records to another store's record by its id; 201"},
}

func init() {
	for pattern, description := range budgetStoreRouteDescriptions {
		if _, taken := RouteAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		RouteAccessRules[pattern] = budgetStoreRouteRule
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupBudgetStore)
}
