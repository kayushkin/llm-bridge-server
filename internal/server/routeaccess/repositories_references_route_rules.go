package routeaccess

import "fmt"

// The rules and descriptions of the repositories, file_storage and
// references routes (repositories_references_routes.go). They are kept here
// and added to RouteAccessRules, routeDescriptions and the README's group
// order at start, so the shared tables gain them in one place.

// repoStoreRoute is the rule of every repo-store route.
var repoStoreRoute = operatorRoute("repo-store's repos, deploy ledger and pull requests have no owner on a row, so any caller could rename or delete a repo, or write a deploy into the ledger that never happened")

// workGraphStoreRoute is the rule of every work-graph-store route.
var workGraphStoreRoute = operatorRoute("work-graph-store records which session moved which git ref across every session and repo on this host, which no one principal may see whole")

// fileStoreRoute is the rule of every file-store route.
var fileStoreRoute = operatorRoute("file-store decides nothing about who may read a file, so whoever reaches it reads every file; only callers that check access themselves may")

// referenceStoreCatalogRoute is the rule of reference-store's catalog, which
// the standalone store served to any caller.
var referenceStoreCatalogRoute = OpenRoute("names the reference types, their id patterns and vocabulary, and nothing about any record; kanban-store and the chat surfaces read it without a credential")

// referenceStoreLookupRoute is the rule of reference-store's lookups.
var referenceStoreLookupRoute = RouteAccessRule{Class: RoutePrincipalOrSessionAgentThroughStoreProxy, Reason: "reference-store as the caller's principal: it narrows every record to what that principal may see, and the internal service acting as itself looks records up for nobody, unchecked"}

var repositoriesAndReferencesRouteAccessRules = map[string]RouteAccessRule{
	"GET /repos/health":                         repoStoreRoute,
	"GET /repos/repos":                          repoStoreRoute,
	"POST /repos/repos":                         repoStoreRoute,
	"GET /repos/repos/{id}":                     repoStoreRoute,
	"DELETE /repos/repos/{id}":                  repoStoreRoute,
	"POST /repos/repos/{id}/detect":             repoStoreRoute,
	"PUT /repos/repos/{id}/provenance":          repoStoreRoute,
	"PUT /repos/repos/{id}/identity":            repoStoreRoute,
	"GET /repos/repos/by-name/{name}":           repoStoreRoute,
	"GET /repos/repos/by-mention/{mention}":     repoStoreRoute,
	"GET /repos/by-path":                        repoStoreRoute,
	"POST /repos/detect":                        repoStoreRoute,
	"POST /repos/repos/{id}/deployments":        repoStoreRoute,
	"GET /repos/deployments":                    repoStoreRoute,
	"GET /repos/deployments/latest":             repoStoreRoute,
	"GET /repos/pull-requests":                  repoStoreRoute,
	"POST /repos/repos/{id}/pull-requests/sync": repoStoreRoute,

	"GET /git-activity/health":                      workGraphStoreRoute,
	"GET /git-activity/ref-updates":                 workGraphStoreRoute,
	"GET /git-activity/repos/{repo_store_id}/graph": workGraphStoreRoute,
	"GET /git-activity/activity":                    workGraphStoreRoute,
	"GET /git-activity/session-repos":               workGraphStoreRoute,
	"GET /git-activity/commits/{mention...}":        workGraphStoreRoute,

	"GET /file-storage/health":              fileStoreRoute,
	"GET /file-storage/limits":              fileStoreRoute,
	"POST /file-storage/files":              fileStoreRoute,
	"GET /file-storage/files":               fileStoreRoute,
	"GET /file-storage/files/{id}":          fileStoreRoute,
	"GET /file-storage/files/{id}/content":  fileStoreRoute,
	"DELETE /file-storage/files/{id}":       fileStoreRoute,
	"POST /file-storage/files/{id}/restore": fileStoreRoute,

	"GET /references/health":           referenceStoreCatalogRoute,
	"GET /references/types":            referenceStoreCatalogRoute,
	"GET /references/types/{type}":     referenceStoreCatalogRoute,
	"GET /references/vocabulary":       referenceStoreCatalogRoute,
	"POST /references/resolve":         referenceStoreLookupRoute,
	"GET /references/search":           referenceStoreLookupRoute,
	"GET /references/attachments/{id}": referenceStoreLookupRoute,
}

const (
	routeGroupRepoStore      routeGroup = "repo-store, mounted in this process"
	routeGroupWorkGraphStore routeGroup = "work-graph-store, mounted in this process"
	routeGroupFileStore      routeGroup = "file-store, mounted in this process"
	routeGroupReferenceStore routeGroup = "reference-store, mounted in this process"
)

var repositoriesAndReferencesRouteDescriptions = map[string]routeDescription{
	"GET /repos/health":                         {routeGroupRepoStore, "`{\"status\":\"ok\"}`"},
	"GET /repos/repos":                          {routeGroupRepoStore, "List repos by `tag`, `q`, `provenance` (`ours`, `vendored`, `unstated`) and `limit`, by name"},
	"POST /repos/repos":                         {routeGroupRepoStore, "Make or refresh a repo by name, its tags detected from `path`; 201 when new"},
	"GET /repos/repos/{id}":                     {routeGroupRepoStore, "One repo, with `github_url` and `effective_tags`"},
	"DELETE /repos/repos/{id}":                  {routeGroupRepoStore, "Delete a repo, its deploys and pull requests"},
	"POST /repos/repos/{id}/detect":             {routeGroupRepoStore, "Detect its tags from its path again"},
	"PUT /repos/repos/{id}/provenance":          {routeGroupRepoStore, "Say who authors it: `ours`, `vendored` or `\"\"`"},
	"PUT /repos/repos/{id}/identity":            {routeGroupRepoStore, "Rename it and move its path and remote, keeping its id; 409 when another repo holds the name or path"},
	"GET /repos/repos/by-name/{name}":           {routeGroupRepoStore, "One repo by name"},
	"GET /repos/repos/by-mention/{mention}":     {routeGroupRepoStore, "One repo by a `repo:<name>` mention"},
	"GET /repos/by-path":                        {routeGroupRepoStore, "One repo by `path`"},
	"POST /repos/detect":                        {routeGroupRepoStore, "Detect the tags of a `path`, storing nothing"},
	"POST /repos/repos/{id}/deployments":        {routeGroupRepoStore, "Append a deploy to the ledger, as deploy-gate does"},
	"GET /repos/deployments":                    {routeGroupRepoStore, "One repo's deploys (`repo_id`, `limit`), newest first"},
	"GET /repos/deployments/latest":             {routeGroupRepoStore, "The newest deploy of every repo"},
	"GET /repos/pull-requests":                  {routeGroupRepoStore, "Pull requests by `repo_id`, `state` (`open`, `closed`, `merged`, `all`) and `limit`"},
	"POST /repos/repos/{id}/pull-requests/sync": {routeGroupRepoStore, "Read the repo's pull requests from GitHub now; 503 when this server cannot run gh"},

	"GET /git-activity/health":                      {routeGroupWorkGraphStore, "`{\"status\":\"ok\"}`"},
	"GET /git-activity/ref-updates":                 {routeGroupWorkGraphStore, "Ref updates by `repo_store_id`, `session_id`, `ref`, `since_unix_nano` and `limit`, newest first"},
	"GET /git-activity/repos/{repo_store_id}/graph": {routeGroupWorkGraphStore, "A repo's commit graph with each commit's maker and each branch's sessions"},
	"GET /git-activity/activity":                    {routeGroupWorkGraphStore, "The branches sessions touched in the last `hours`; `format=text` for an agent"},
	"GET /git-activity/session-repos":               {routeGroupWorkGraphStore, "How many refs each session moved in each repo since `since_unix_nano`"},
	"GET /git-activity/commits/{mention...}":        {routeGroupWorkGraphStore, "One commit by a `<repo>@<sha>` mention, with its repo and maker"},

	"GET /file-storage/health":              {routeGroupFileStore, "`{\"status\":\"ok\",\"files\":…,\"blobs\":…,\"blob_bytes\":…}`"},
	"GET /file-storage/limits":              {routeGroupFileStore, "The size limit and the inline content types in force"},
	"POST /file-storage/files":              {routeGroupFileStore, "Keep the body as a file of `owner_service` and `owner_ref`, named `filename`; 201 and the record"},
	"GET /file-storage/files":               {routeGroupFileStore, "Files by `owner_service`, `owner_ref`, `sha256`, `include_deleted`, `limit` and `offset`, oldest first"},
	"GET /file-storage/files/{id}":          {routeGroupFileStore, "One file's record; `include_deleted=true` for a deleted one"},
	"GET /file-storage/files/{id}/content":  {routeGroupFileStore, "The bytes, as a download unless `inline=true` and the type is on the inline list; ranges work"},
	"DELETE /file-storage/files/{id}":       {routeGroupFileStore, "Delete, reversibly; `hard=true` purges the row and, with the last row naming them, the bytes"},
	"POST /file-storage/files/{id}/restore": {routeGroupFileStore, "Undo a delete"},

	"GET /references/health":           {routeGroupReferenceStore, "`{\"status\":\"ok\"}`"},
	"GET /references/types":            {routeGroupReferenceStore, "Every reference type: id patterns, owning service, label and summary fields, page, access"},
	"GET /references/types/{type}":     {routeGroupReferenceStore, "One reference type"},
	"GET /references/vocabulary":       {routeGroupReferenceStore, "The summary formats, the access rules and the grant names the types use"},
	"POST /references/resolve":         {routeGroupReferenceStore, "What each of `ids` (at most 128) names, narrowed to what the caller may see"},
	"GET /references/search":           {routeGroupReferenceStore, "Records matching `q` in every mentionable type the caller may see whole"},
	"GET /references/attachments/{id}": {routeGroupReferenceStore, "A picture of the record, when the caller may see it"},
}

func init() {
	for pattern, rule := range repositoriesAndReferencesRouteAccessRules {
		if _, taken := RouteAccessRules[pattern]; taken {
			panic(fmt.Sprintf("route %s has two access rules", pattern))
		}
		RouteAccessRules[pattern] = rule
	}
	for pattern, description := range repositoriesAndReferencesRouteDescriptions {
		if _, taken := routeDescriptions[pattern]; taken {
			panic(fmt.Sprintf("route %s has two descriptions", pattern))
		}
		routeDescriptions[pattern] = description
	}
	routeGroupsInReadmeOrder = append(routeGroupsInReadmeOrder, routeGroupRepoStore, routeGroupWorkGraphStore, routeGroupFileStore, routeGroupReferenceStore)
}
