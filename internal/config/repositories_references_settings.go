package config

import (
	"github.com/kayushkin/llm-bridge-server/internal/filestorage/filestore"
	"github.com/kayushkin/llm-bridge-server/internal/productiondefaults"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/workgraphstore"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Keys of the settings of the repositories, file_storage and references
// modules: repo-store, work-graph-store, file-store and reference-store.
const (
	SettingRepoStorePullRequestSyncEnabled  = "repo_store.pull_request_sync.enabled"
	SettingRepoStorePullRequestSyncInterval = "repo_store.pull_request_sync.interval"
	SettingRepoStoreGitHubCommandLine       = "repo_store.github_command_line_program"
	SettingWorkGraphStoreIngestEnabled      = "work_graph_store.ingest.enabled"
	SettingWorkGraphStoreSpoolPath          = "work_graph_store.spool_path"
	SettingFileStoreDataDirectory           = "file_store.data_directory"
	SettingFileStoreMaximumFileBytes        = "file_store.maximum_file_bytes"
	SettingFileStoreInlineContentTypes      = "file_store.inline_content_types"
	SettingReferenceStoreServiceURLs        = "reference_store.service_urls"
	SettingReferenceStoreSiteURLs           = "reference_store.site_urls"
	SettingReferenceStoreAuthStoreProviders = "reference_store.auth_store_providers"
	SettingReferenceStoreRecordCacheSeconds = "reference_store.record_cache_seconds"
	SettingReferenceStoreMissCacheSeconds   = "reference_store.miss_cache_seconds"
	SettingReferenceStoreAccessCacheSeconds = "reference_store.access_cache_seconds"
	SettingReferenceStoreLookupTimeout      = "reference_store.lookup_timeout_seconds"
	SettingReferenceStoreLookupConcurrency  = "reference_store.lookup_concurrency_per_ask"
)

// repositoriesAndReferencesSettingDefinitions declares the settings above.
func repositoriesAndReferencesSettingDefinitions() []servicesettings.Definition {
	const (
		wiring   = msg.ServiceSettingKindWiring
		path     = msg.ServiceSettingKindPath
		behavior = msg.ServiceSettingKindBehavior
		text     = msg.ServiceSettingValueTypeString
		pairs    = msg.ServiceSettingValueTypeStringMap
		integer  = msg.ServiceSettingValueTypeInteger
		boolean  = msg.ServiceSettingValueTypeBoolean
	)
	return []servicesettings.Definition{
		{Key: SettingRepoStorePullRequestSyncEnabled, EnvironmentVariable: "LLMBRIDGE_REPO_STORE_PULL_REQUEST_SYNC_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Read every repo's GitHub pull requests into repo-store in this server, every repo_store.pull_request_sync.interval. Off by default, while the standalone repo-store still runs the sync; POST /repo-store/repos/{id}/pull-requests/sync syncs one repo either way. Read at start."},
		{Key: SettingRepoStorePullRequestSyncInterval, EnvironmentVariable: "LLMBRIDGE_REPO_STORE_PULL_REQUEST_SYNC_INTERVAL", Kind: behavior, ValueType: msg.ServiceSettingValueTypeDuration, Default: "10m",
			Description: "How long the pull-request sync waits between passes when it is on. Shorter makes GET /repo-store/pull-requests fresher and spends more of the GitHub account's hourly allowance: each pass makes at least one request per repo with a github_url. Read at start."},
		{Key: SettingRepoStoreGitHubCommandLine, EnvironmentVariable: "LLMBRIDGE_REPO_STORE_GITHUB_CLI", Kind: path, ValueType: text, Default: "gh",
			Description: "The gh program repo-store runs (`gh api …`) to read pull requests, as whatever account gh is logged in to under this server's HOME. A full path, or a name on PATH; the server will not start if it cannot find it. Changing it may change which GitHub account the pull requests are read as."},
		{Key: SettingWorkGraphStoreIngestEnabled, EnvironmentVariable: "LLMBRIDGE_WORK_GRAPH_STORE_INGEST_ENABLED", Kind: behavior, ValueType: boolean, Default: "false",
			Description: "Move the git hook's spool into work-graph-store in this server, every two seconds. The ingest renames the spool aside and deletes it once stored, so only one reader may run: off by default, while the standalone work-graph-store still reads it. Read at start."},
		{Key: SettingWorkGraphStoreSpoolPath, EnvironmentVariable: "LLMBRIDGE_WORK_GRAPH_STORE_SPOOL_PATH", Kind: path, ValueType: text, Default: workgraphstore.DefaultSpoolPath(),
			Description: "The file the git hook (work-graph-hook) appends ref updates to, which the ingest reads. The hook always writes to the default, so changing this makes the ingest read a file nobody writes."},
		{Key: SettingFileStoreDataDirectory, EnvironmentVariable: "LLMBRIDGE_FILE_STORE_DATA_DIRECTORY", Kind: path, ValueType: text, Default: productiondefaults.FileStoreDataDirectory(),
			Description: "The directory the file-store module keeps every file's bytes in (blobs/<aa>/<bb>/<sha256>); the records are in Postgres. Moving it leaves the bytes behind: move blobs/ along, or every file reads as damaged. Only this server may write it."},
		{Key: SettingFileStoreMaximumFileBytes, EnvironmentVariable: "LLMBRIDGE_FILE_STORE_MAXIMUM_FILE_BYTES", Kind: behavior, ValueType: integer, Editable: true, Default: filestore.DefaultMaximumFileBytes,
			Description: "The largest file file-store takes, in bytes; a larger upload is refused with 413 and nothing is kept. Lowering it does not touch files already stored."},
		{Key: SettingFileStoreInlineContentTypes, EnvironmentVariable: "LLMBRIDGE_FILE_STORE_INLINE_CONTENT_TYPES", Kind: behavior, ValueType: msg.ServiceSettingValueTypeStringList, Editable: true, Default: filestore.DefaultInlineContentTypes,
			Description: "The declared content types file-store serves for display when a download asks ?inline=true; every other file is served as an attachment. A type a browser runs as a page or a script (text/html, image/svg+xml, XML, JavaScript) is refused."},
		{Key: SettingReferenceStoreServiceURLs, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_SERVICE_URLS", Kind: wiring, ValueType: pairs,
			Description: "service:url pairs for the owning services reference-store asks over HTTP: every service a reference type names that does not run in this server. A lookup or search in a service that is in neither this list nor this server answers an error naming it. Naming a service that runs in this server stops the start."},
		{Key: SettingReferenceStoreSiteURLs, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_SITE_URLS", Kind: wiring, ValueType: pairs,
			Description: "site:url pairs for record pages served somewhere other than the page that draws the chip, e.g. music:https://example.com. A type whose page names a site missing here has no page link."},
		{Key: SettingReferenceStoreAuthStoreProviders, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_AUTH_STORE_PROVIDERS", Kind: wiring, ValueType: pairs,
			Description: "service:provider pairs: the auth-store provider whose api_key reference-store sends as a bearer token to that service, e.g. multichat:multichat. auth-store is auth_store.url, asked with auth_store.token."},
		{Key: SettingReferenceStoreRecordCacheSeconds, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_RECORD_CACHE_SECONDS", Kind: behavior, ValueType: integer, Default: "60",
			Description: "How long a record a store answered is reused before the store is asked again. Read at start."},
		{Key: SettingReferenceStoreMissCacheSeconds, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_MISS_CACHE_SECONDS", Kind: behavior, ValueType: integer, Default: "15",
			Description: "How long an id no store knows is remembered as unknown. Read at start."},
		{Key: SettingReferenceStoreAccessCacheSeconds, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_ACCESS_CACHE_SECONDS", Kind: behavior, ValueType: integer, Default: "30",
			Description: "How long a caller's administrator flag and can_see_references grants are reused before principal-store and grant-store are read again. Read at start."},
		{Key: SettingReferenceStoreLookupTimeout, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_LOOKUP_TIMEOUT_SECONDS", Kind: behavior, ValueType: integer, Default: "5",
			Description: "How long one store is given to answer for one id. Read at start."},
		{Key: SettingReferenceStoreLookupConcurrency, EnvironmentVariable: "LLMBRIDGE_REFERENCE_STORE_LOOKUP_CONCURRENCY_PER_ASK", Kind: behavior, ValueType: integer, Default: "8",
			Description: "How many stores one POST /reference-store/resolve asks at once. Read at start."},
	}
}
