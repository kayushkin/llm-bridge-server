package server

import "fmt"

// The rules and descriptions of the repositories, file_storage and
// references routes (repositories_references_routes.go). They are kept here
// and added to routeAccessRules, routeDescriptions and the README's group
// order at start, so the shared tables gain them in one place.

// repoStoreRoute is the rule of every repo-store route.
var repoStoreRoute = operatorRoute("repo-store's repos, deploy ledger and pull requests have no owner on a row, so any caller could rename or delete a repo, or write a deploy into the ledger that never happened")

// workGraphStoreRoute is the rule of every work-graph-store route.
var workGraphStoreRoute = operatorRoute("work-graph-store records which session moved which git ref across every session and repo on this host, which no one principal may see whole")

// fileStoreRoute is the rule of every file-store route.
var fileStoreRoute = operatorRoute("file-store decides nothing about who may read a file, so whoever reaches it reads every file; only callers that check access themselves may")

// referenceStoreCatalogRoute is the rule of reference-store's catalog, which
// the standalone store served to any caller.
var referenceStoreCatalogRoute = openRoute("names the reference types, their id patterns and vocabulary, and nothing about any record; kanban-store and the chat surfaces read it without a credential")

// referenceStoreLookupRoute is the rule of reference-store's lookups.
var referenceStoreLookupRoute = routeAccessRule{class: routePrincipalOrSessionAgentThroughStoreProxy, reason: "reference-store as the caller's principal: it narrows every record to what that principal may see, and the internal service acting as itself looks records up for nobody, unchecked"}

var repositoriesAndReferencesRouteAccessRules = map[string]routeAccessRule{
	"GET /repo-store/health":                         repoStoreRoute,
	"GET /repo-store/repos":                          repoStoreRoute,
	"POST /repo-store/repos":                         repoStoreRoute,
	"GET /repo-store/repos/{id}":                     repoStoreRoute,
	"DELETE /repo-store/repos/{id}":                  repoStoreRoute,
	"POST /repo-store/repos/{id}/detect":             repoStoreRoute,
	"PUT /repo-store/repos/{id}/provenance":          repoStoreRoute,
	"PUT /repo-store/repos/{id}/identity":            repoStoreRoute,
	"GET /repo-store/repos/by-name/{name}":           repoStoreRoute,
	"GET /repo-store/repos/by-mention/{mention}":     repoStoreRoute,
	"GET /repo-store/by-path":                        repoStoreRoute,
	"POST /repo-store/detect":                        repoStoreRoute,
	"POST /repo-store/repos/{id}/deployments":        repoStoreRoute,
	"GET /repo-store/deployments":                    repoStoreRoute,
	"GET /repo-store/deployments/latest":             repoStoreRoute,
	"GET /repo-store/pull-requests":                  repoStoreRoute,
	"POST /repo-store/repos/{id}/pull-requests/sync": repoStoreRoute,

	"GET /work-graph-store/health":                      workGraphStoreRoute,
	"GET /work-graph-store/ref-updates":                 workGraphStoreRoute,
	"GET /work-graph-store/repos/{repo_store_id}/graph": workGraphStoreRoute,
	"GET /work-graph-store/activity":                    workGraphStoreRoute,
	"GET /work-graph-store/session-repos":               workGraphStoreRoute,
	"GET /work-graph-store/commits/{mention...}":        workGraphStoreRoute,

	"GET /file-store/health":              fileStoreRoute,
	"GET /file-store/limits":              fileStoreRoute,
	"POST /file-store/files":              fileStoreRoute,
	"GET /file-store/files":               fileStoreRoute,
	"GET /file-store/files/{id}":          fileStoreRoute,
	"GET /file-store/files/{id}/content":  fileStoreRoute,
	"DELETE /file-store/files/{id}":       fileStoreRoute,
	"POST /file-store/files/{id}/restore": fileStoreRoute,

	"GET /reference-store/health":           referenceStoreCatalogRoute,
	"GET /reference-store/types":            referenceStoreCatalogRoute,
	"GET /reference-store/types/{type}":     referenceStoreCatalogRoute,
	"GET /reference-store/vocabulary":       referenceStoreCatalogRoute,
	"POST /reference-store/resolve":         referenceStoreLookupRoute,
	"GET /reference-store/search":           referenceStoreLookupRoute,
	"GET /reference-store/attachments/{id}": referenceStoreLookupRoute,
}

const (
	routeGroupRepoStore      routeGroup = "repo-store, mounted in this process"
	routeGroupWorkGraphStore routeGroup = "work-graph-store, mounted in this process"
	routeGroupFileStore      routeGroup = "file-store, mounted in this process"
	routeGroupReferenceStore routeGroup = "reference-store, mounted in this process"
)

var repositoriesAndReferencesRouteDescriptions = map[string]routeDescription{
	"GET /repo-store/health":                         {routeGroupRepoStore, "`{\"status\":\"ok\"}`"},
	"GET /repo-store/repos":                          {routeGroupRepoStore, "List repos by `tag`, `q`, `provenance` (`ours`, `vendored`, `unstated`) and `limit`, by name"},
	"POST /repo-store/repos":                         {routeGroupRepoStore, "Make or refresh a repo by name, its tags detected from `path`; 201 when new"},
	"GET /repo-store/repos/{id}":                     {routeGroupRepoStore, "One repo, with `github_url` and `effective_tags`"},
	"DELETE /repo-store/repos/{id}":                  {routeGroupRepoStore, "Delete a repo, its deploys and pull requests"},
	"POST /repo-store/repos/{id}/detect":             {routeGroupRepoStore, "Detect its tags from its path again"},
	"PUT /repo-store/repos/{id}/provenance":          {routeGroupRepoStore, "Say who authors it: `ours`, `vendored` or `\"\"`"},
	"PUT /repo-store/repos/{id}/identity":            {routeGroupRepoStore, "Rename it and move its path and remote, keeping its id; 409 when another repo holds the name or path"},
	"GET /repo-store/repos/by-name/{name}":           {routeGroupRepoStore, "One repo by name"},
	"GET /repo-store/repos/by-mention/{mention}":     {routeGroupRepoStore, "One repo by a `repo:<name>` mention"},
	"GET /repo-store/by-path":                        {routeGroupRepoStore, "One repo by `path`"},
	"POST /repo-store/detect":                        {routeGroupRepoStore, "Detect the tags of a `path`, storing nothing"},
	"POST /repo-store/repos/{id}/deployments":        {routeGroupRepoStore, "Append a deploy to the ledger, as deploy-gate does"},
	"GET /repo-store/deployments":                    {routeGroupRepoStore, "One repo's deploys (`repo_id`, `limit`), newest first"},
	"GET /repo-store/deployments/latest":             {routeGroupRepoStore, "The newest deploy of every repo"},
	"GET /repo-store/pull-requests":                  {routeGroupRepoStore, "Pull requests by `repo_id`, `state` (`open`, `closed`, `merged`, `all`) and `limit`"},
	"POST /repo-store/repos/{id}/pull-requests/sync": {routeGroupRepoStore, "Read the repo's pull requests from GitHub now; 503 when this server cannot run gh"},

	"GET /work-graph-store/health":                      {routeGroupWorkGraphStore, "`{\"status\":\"ok\"}`"},
	"GET /work-graph-store/ref-updates":                 {routeGroupWorkGraphStore, "Ref updates by `repo_store_id`, `session_id`, `ref`, `since_unix_nano` and `limit`, newest first"},
	"GET /work-graph-store/repos/{repo_store_id}/graph": {routeGroupWorkGraphStore, "A repo's commit graph with each commit's maker and each branch's sessions"},
	"GET /work-graph-store/activity":                    {routeGroupWorkGraphStore, "The branches sessions touched in the last `hours`; `format=text` for an agent"},
	"GET /work-graph-store/session-repos":               {routeGroupWorkGraphStore, "How many refs each session moved in each repo since `since_unix_nano`"},
	"GET /work-graph-store/commits/{mention...}":        {routeGroupWorkGraphStore, "One commit by a `<repo>@<sha>` mention, with its repo and maker"},

	"GET /file-store/health":              {routeGroupFileStore, "`{\"status\":\"ok\",\"files\":…,\"blobs\":…,\"blob_bytes\":…}`"},
	"GET /file-store/limits":              {routeGroupFileStore, "The size limit and the inline content types in force"},
	"POST /file-store/files":              {routeGroupFileStore, "Keep the body as a file of `owner_service` and `owner_ref`, named `filename`; 201 and the record"},
	"GET /file-store/files":               {routeGroupFileStore, "Files by `owner_service`, `owner_ref`, `sha256`, `include_deleted`, `limit` and `offset`, oldest first"},
	"GET /file-store/files/{id}":          {routeGroupFileStore, "One file's record; `include_deleted=true` for a deleted one"},
	"GET /file-store/files/{id}/content":  {routeGroupFileStore, "The bytes, as a download unless `inline=true` and the type is on the inline list; ranges work"},
	"DELETE /file-store/files/{id}":       {routeGroupFileStore, "Delete, reversibly; `hard=true` purges the row and, with the last row naming them, the bytes"},
	"POST /file-store/files/{id}/restore": {routeGroupFileStore, "Undo a delete"},

	"GET /reference-store/health":           {routeGroupReferenceStore, "`{\"status\":\"ok\"}`"},
	"GET /reference-store/types":            {routeGroupReferenceStore, "Every reference type: id patterns, owning service, label and summary fields, page, access"},
	"GET /reference-store/types/{type}":     {routeGroupReferenceStore, "One reference type"},
	"GET /reference-store/vocabulary":       {routeGroupReferenceStore, "The summary formats, the access rules and the grant names the types use"},
	"POST /reference-store/resolve":         {routeGroupReferenceStore, "What each of `ids` (at most 128) names, narrowed to what the caller may see"},
	"GET /reference-store/search":           {routeGroupReferenceStore, "Records matching `q` in every mentionable type the caller may see whole"},
	"GET /reference-store/attachments/{id}": {routeGroupReferenceStore, "A picture of the record, when the caller may see it"},
}

func init() {
	for pattern, rule := range repositoriesAndReferencesRouteAccessRules {
		if _, taken := routeAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		routeAccessRules[pattern] = rule
	}
	for pattern, description := range repositoriesAndReferencesRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupRepoStore, routeGroupWorkGraphStore, routeGroupFileStore, routeGroupReferenceStore)
}
