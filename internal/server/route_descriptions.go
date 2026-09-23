package server

// routeDescription says what one route does, for the route table in the
// README. routeAccessRules says who may call a route; this says what it is
// for. Both are keyed by the exact registration pattern, and
// TestReadmeRouteTableMatchesTheRoutes fails when the two key sets differ or
// when the README's table is not the one these produce.
type routeDescription struct {
	group       routeGroup
	description string
}

// routeGroup is one heading of the README route table.
type routeGroup string

const (
	routeGroupSessions         routeGroup = "Sessions"
	routeGroupOneSession       routeGroup = "One session"
	routeGroupSignals          routeGroup = "Signals"
	routeGroupOperations       routeGroup = "Operations"
	routeGroupMachines         routeGroup = "Machines, instances and credentials"
	routeGroupHooks            routeGroup = "Hooks"
	routeGroupHarnessesModels  routeGroup = "Harnesses, models and settings"
	routeGroupFolders          routeGroup = "Folders, labels and housekeeping"
	routeGroupServices         routeGroup = "Services page"
	routeGroupLogin            routeGroup = "Login and store proxies"
	routeGroupRunners          routeGroup = "Runners and harness callbacks"
	routeGroupAgentStore       routeGroup = "agent-store, mounted in this process"
	routeGroupAgentStorePrompt routeGroup = "agent-store prompt source, mounted in this process"
	routeGroupMemoryStore      routeGroup = "memory-store, mounted in this process"
)

// routeGroupsInReadmeOrder is the order the README lists the groups in.
var routeGroupsInReadmeOrder = []routeGroup{
	routeGroupSessions,
	routeGroupOneSession,
	routeGroupSignals,
	routeGroupOperations,
	routeGroupMachines,
	routeGroupHooks,
	routeGroupHarnessesModels,
	routeGroupFolders,
	routeGroupServices,
	routeGroupLogin,
	routeGroupRunners,
	routeGroupAgentStore,
	routeGroupAgentStorePrompt,
	routeGroupMemoryStore,
}

var routeDescriptions = map[string]routeDescription{
	// Sessions.
	"POST /sessions":              {routeGroupSessions, "Create a session; `auto_start` starts it"},
	"GET /sessions":               {routeGroupSessions, "List sessions"},
	"GET /sessions/summary":       {routeGroupSessions, "The chat sidebar's list, projected small"},
	"POST /sessions/summary":      {routeGroupSessions, "The same, with the id lists in the body, since a long query string makes nginx drop the HTTP/2 connection"},
	"GET /sessions/recent-bundle": {routeGroupSessions, "Recent sessions, to warm the chat page"},
	"GET /sessions/validators":    {routeGroupSessions, "Cheap check of which cached sessions changed"},
	"POST /sessions/validators":   {routeGroupSessions, "The same, with the ids in the body"},
	"GET /session-events":         {routeGroupSessions, "SSE stream of changes to the session list"},
	"GET /sessions/search":        {routeGroupSessions, "Full-text search, answered by log-store"},
	"GET /sessions/aggregates":    {routeGroupSessions, "Totals across sessions, answered by log-store"},
	"GET /sessions/discover":      {routeGroupSessions, "Import sessions the harness CLIs have on this host's disk"},
	"GET /effective-config":       {routeGroupSessions, "Every setting a session would get, and which layer decides it, before it exists"},
	"GET /snapshots/blob/{sha}":   {routeGroupSessions, "One file snapshot's content (snapshot-store)"},

	// One session.
	"GET /sessions/{id}":                               {routeGroupOneSession, "The session record"},
	"POST /sessions/{id}/send":                         {routeGroupOneSession, "Send a message"},
	"GET /sessions/{id}/events":                        {routeGroupOneSession, "SSE stream of `msg.Event`; replays the current turn and honours `Last-Event-ID`"},
	"GET /sessions/{id}/attach":                        {routeGroupOneSession, "WebSocket onto a pty-mode session's terminal"},
	"GET /sessions/{id}/attach-token":                  {routeGroupOneSession, "The token the attach WebSocket needs"},
	"GET /sessions/{id}/messages":                      {routeGroupOneSession, "History, projected by log-store for reading"},
	"GET /sessions/{id}/messages/raw":                  {routeGroupOneSession, "History with nothing left out, about ten times larger"},
	"GET /sessions/{id}/entries/{eventId}":             {routeGroupOneSession, "One history entry with its tool input and output in full"},
	"POST /sessions/{id}/interrupt":                    {routeGroupOneSession, "Stop the current turn"},
	"POST /sessions/{id}/resume":                       {routeGroupOneSession, "Restart a stopped session with its history"},
	"POST /sessions/{id}/stop":                         {routeGroupOneSession, "End the session"},
	"POST /sessions/{id}/mode":                         {routeGroupOneSession, "Switch between events mode and pty mode"},
	"POST /sessions/{id}/compact":                      {routeGroupOneSession, "Compact the context"},
	"POST /sessions/{id}/fork":                         {routeGroupOneSession, "Branch a new session from this one"},
	"POST /sessions/{id}/rename":                       {routeGroupOneSession, "Set the title"},
	"POST /sessions/{id}/auto-rename":                  {routeGroupOneSession, "The renamer session posts the title it wrote"},
	"POST /sessions/{id}/config":                       {routeGroupOneSession, "Change model, effort, budget or disabled tools"},
	"POST /sessions/{id}/mark-done":                    {routeGroupOneSession, "Mark done and move to Archive"},
	"PUT /sessions/{id}/folder":                        {routeGroupOneSession, "Move into a folder"},
	"PUT /sessions/{id}/permission-mode":               {routeGroupOneSession, "`ask`, `auto` or `bypass` for this session"},
	"PUT /sessions/{id}/bypass-permissions":            {routeGroupOneSession, "Old boolean form of the above"},
	"GET /sessions/{id}/git/repos":                     {routeGroupOneSession, "Git repos in the session's directory"},
	"GET /sessions/{id}/git":                           {routeGroupOneSession, "Status and diff of one of them (`?repo=`)"},
	"GET /sessions/{id}/effective-config":              {routeGroupOneSession, "Every setting the session runs with, and which layer decided it"},
	"GET /sessions/{id}/hooks/pending":                 {routeGroupOneSession, "Hooks waiting on a human decision"},
	"POST /sessions/{id}/hooks/{request_id}/resolve":   {routeGroupOneSession, "Allow or deny one"},
	"GET /sessions/{id}/tools/{tool_use_id}/snapshots": {routeGroupOneSession, "File snapshots before and after an Edit or Write (snapshot-store)"},

	// Signals.
	"GET /signals": {routeGroupSignals, "The inbox across sessions (`?state=open`)"},

	"POST /operations":                            {routeGroupOperations, "Accept an operation intent: 202 and a new receipt, or 200 and the first one for a repeated idempotency key"},
	"GET /operations":                             {routeGroupOperations, "Receipts, newest first (`?organization_id=&principal_id=&type=&state=&created_after=&created_before=&limit=`)"},
	"GET /operations/{id}":                        {routeGroupOperations, "One operation's receipt"},
	"GET /operations/{id}/events":                 {routeGroupOperations, "SSE of an operation's events, resumable with `Last-Event-ID`, closed once it and its children finish"},
	"POST /operations/{id}/cancel":                {routeGroupOperations, "Ask an operation to stop; 409 with the receipt when it already finished"},
	"GET /operations/{id}/children":               {routeGroupOperations, "The receipts of an operation's children"},
	"GET /operation-types":                        {routeGroupOperations, "The operation types this server runs, with their attempt and time limits"},
	"GET /operation-budgets":                      {routeGroupOperations, "Every organization's monthly limit and what it has spent this UTC month"},
	"GET /operation-budgets/{organization_id}":    {routeGroupOperations, "One organization's monthly limit and spend"},
	"PUT /operation-budgets/{organization_id}":    {routeGroupOperations, "Set an organization's monthly limit (`{\"monthly_limit_usd\":50}`)"},
	"DELETE /operation-budgets/{organization_id}": {routeGroupOperations, "Remove an organization's monthly limit"},
	"GET /sessions/{id}/signals":                  {routeGroupSignals, "One session's signals"},
	"POST /sessions/{id}/signals":                 {routeGroupSignals, "A session raises a notice about itself"},
	"POST /signals/{id}/answer":                   {routeGroupSignals, "Answer a question, whether or not its session still runs"},
	"POST /signals/{id}/resolve":                  {routeGroupSignals, "Acknowledge or dismiss"},

	// Machines, instances and credentials.
	"GET /machines":                                {routeGroupMachines, "List machines (harness-store)"},
	"POST /machines":                               {routeGroupMachines, "Add a machine (harness-store)"},
	"GET /machines/{id}":                           {routeGroupMachines, "One machine (harness-store)"},
	"PUT /machines/{id}":                           {routeGroupMachines, "Change a machine (harness-store)"},
	"DELETE /machines/{id}":                        {routeGroupMachines, "Remove a machine (harness-store)"},
	"GET /instances":                               {routeGroupMachines, "List instances (harness-store)"},
	"POST /instances":                              {routeGroupMachines, "Add an instance (harness-store)"},
	"GET /instances/{id}":                          {routeGroupMachines, "One instance (harness-store)"},
	"PUT /instances/{id}":                          {routeGroupMachines, "Change an instance (harness-store)"},
	"DELETE /instances/{id}":                       {routeGroupMachines, "Remove an instance (harness-store)"},
	"GET /instances/{id}/status":                   {routeGroupMachines, "Live sessions and credential state (harness-store)"},
	"GET /instances/{id}/sessions":                 {routeGroupMachines, "Its sessions (harness-store)"},
	"POST /instances/{id}/oneshot":                 {routeGroupMachines, "One model call on the instance, with no session (harness-store)"},
	"GET /instances/{id}/credentials":              {routeGroupMachines, "Bound credentials (harness-store)"},
	"POST /instances/{id}/credentials":             {routeGroupMachines, "Bind a credential (harness-store)"},
	"DELETE /instances/{id}/credentials/{cred_id}": {routeGroupMachines, "Unbind one (harness-store)"},
	"GET /credentials":                             {routeGroupMachines, "List auth-store credentials, keys masked"},
	"POST /credentials":                            {routeGroupMachines, "Add a credential to auth-store"},
	"DELETE /credentials/{id}":                     {routeGroupMachines, "Delete one"},

	// Hooks.
	"GET /hooks":         {routeGroupHooks, "List hooks (hook-store)"},
	"POST /hooks":        {routeGroupHooks, "Create a hook (hook-store)"},
	"GET /hooks/{id}":    {routeGroupHooks, "One hook (hook-store)"},
	"PATCH /hooks/{id}":  {routeGroupHooks, "Change one (hook-store)"},
	"DELETE /hooks/{id}": {routeGroupHooks, "Delete one (hook-store)"},
	"GET /hook-options":  {routeGroupHooks, "Which harnesses take hooks, their events and scopes (hook-store)"},

	// Harnesses, models and settings.
	"GET /health":                        {routeGroupHarnessesModels, "Health, harnesses present, session counts"},
	"GET /harnesses":                     {routeGroupHarnessesModels, "Each harness's name, label, image, capabilities and hook events"},
	"GET /harnesses/{name}/capabilities": {routeGroupHarnessesModels, "One harness's capabilities"},
	"GET /harnesses/{name}/agents":       {routeGroupHarnessesModels, "Its named agents"},
	"/images/":                           {routeGroupHarnessesModels, "Harness images"},
	"GET /models":                        {routeGroupHarnessesModels, "Models there are credentials for"},
	"GET /bridge-prefs":                  {routeGroupHarnessesModels, "Stored preferences: per-harness defaults, default principal, …"},
	"PUT /bridge-prefs":                  {routeGroupHarnessesModels, "Change them"},
	"POST /bridge/permission-mode":       {routeGroupHarnessesModels, "Default permission mode for new sessions"},
	"POST /bridge/bypass-permissions":    {routeGroupHarnessesModels, "Old boolean form of the above"},
	"GET /settings":                      {routeGroupHarnessesModels, "Every server setting, its value and what decided it"},
	"PUT /settings/{key}":                {routeGroupHarnessesModels, "Change a stored setting without a restart"},
	"GET /conformance":                   {routeGroupHarnessesModels, "Latest capability matrix across harnesses"},
	"POST /conformance/run":              {routeGroupHarnessesModels, "Start a conformance run"},

	// Folders, labels and housekeeping.
	"GET /folders":                    {routeGroupFolders, "List folders"},
	"POST /folders":                   {routeGroupFolders, "Create one"},
	"PUT /folders/{name}":             {routeGroupFolders, "Rename one"},
	"DELETE /folders/{name}":          {routeGroupFolders, "Delete one"},
	"GET /source-folders":             {routeGroupFolders, "Which folder each session `source` files into"},
	"PUT /source-folders/{source}":    {routeGroupFolders, "Set one"},
	"DELETE /source-folders/{source}": {routeGroupFolders, "Clear one"},
	"GET /session-taxonomy":           {routeGroupFolders, "The types and purposes a session may carry"},
	"GET /session-taxonomy/report":    {routeGroupFolders, "Sessions whose labels break that vocabulary"},
	"POST /admin/file-inactive":       {routeGroupFolders, "File inactive sessions; a scheduler job calls it"},
	"POST /admin/archive-old":         {routeGroupFolders, "Archive old sessions; a scheduler job calls it"},

	// Services page.
	"GET /services":                  {routeGroupServices, "Each service healthcheck watches, its status, processes and open SQLite files"},
	"GET /services/databases/schema": {routeGroupServices, "Tables of one open file (`?path=`)"},
	"GET /services/databases/rows":   {routeGroupServices, "Newest rows of one table (`?path=&table=&filter=col:op:value`)"},

	// Login and store proxies.
	"POST /auth/demo-login":          {routeGroupLogin, "Sign in as a principal by id, with no password"},
	"GET /auth/principal":            {routeGroupLogin, "Who the login cookie names"},
	"POST /auth/logout":              {routeGroupLogin, "Clear the cookie"},
	kanbanProxyMountPrefix + "/":     {routeGroupLogin, "kanban-store `/api/…`, as the caller"},
	grantStoreProxyMountPrefix + "/": {routeGroupLogin, "grant-store `/…`, as the caller"},

	// Runners and harness callbacks.
	"GET /api/runner/ws":                         {routeGroupRunners, "A runner's WebSocket"},
	"POST /api/runner/enroll":                    {routeGroupRunners, "Trade a passphrase for a runner token"},
	"GET /api/runner/install.sh":                 {routeGroupRunners, "Runner install script"},
	"GET /api/runner/binary":                     {routeGroupRunners, "Runner and wrapper binaries (`?os=&arch=&name=`)"},
	"POST /api/runner/seed/broadcast":            {routeGroupRunners, "Tell every runner to re-sync agent and skill files"},
	"/api/agent-store/":                          {routeGroupRunners, "Runners read agent-store through this"},
	"/api/skill-store/":                          {routeGroupRunners, "Runners read skill-store through this"},
	"/api/harness-proxy/{harness}/{rest...}":     {routeGroupRunners, "Forward to a harness backend (inber, hermes) on this host. Checks no credential; nothing calls it"},
	"POST /permission/cc-prehook/{bridge_id}":    {routeGroupRunners, "Claude Code's permission check before each tool call"},
	"POST /permission/codex-prehook/{bridge_id}": {routeGroupRunners, "The same for codex"},
	"POST /sidecar/event/{bridge_id}":            {routeGroupRunners, "Events from a pty-mode session's sidecar"},
	"POST /hooks/exec/{id}":                      {routeGroupRunners, "A harness runs a registered hook (hook-store)"},

	// agent-store.
	"GET /agents":                                  {routeGroupAgentStore, "List agents"},
	"POST /agents":                                 {routeGroupAgentStore, "Create an agent"},
	"GET /agents/{slug}":                           {routeGroupAgentStore, "One agent"},
	"PUT /agents/{slug}":                           {routeGroupAgentStore, "Change one"},
	"DELETE /agents/{slug}":                        {routeGroupAgentStore, "Delete one"},
	"GET /agents/{slug}/harnesses":                 {routeGroupAgentStore, "Harnesses an agent runs on"},
	"POST /agents/{slug}/harnesses":                {routeGroupAgentStore, "Bind an agent to a harness"},
	"GET /agents/{slug}/config":                    {routeGroupAgentStore, "An agent's runtime config"},
	"GET /configs":                                 {routeGroupAgentStore, "Every runtime config"},
	"GET /reconcile":                               {routeGroupAgentStore, "Problems in each agent's harness bindings"},
	"GET /files":                                   {routeGroupAgentStore, "Tracked context files"},
	"GET /files/{id}":                              {routeGroupAgentStore, "One tracked file"},
	"GET /files/{id}/content":                      {routeGroupAgentStore, "Its content"},
	"PUT /files/{id}/content":                      {routeGroupAgentStore, "Write its content"},
	"GET /files/{id}/versions":                     {routeGroupAgentStore, "Its saved versions"},
	"GET /versions/{vid}/content":                  {routeGroupAgentStore, "One version's content"},
	"POST /files/{id}/enable":                      {routeGroupAgentStore, "Track a file"},
	"POST /files/{id}/disable":                     {routeGroupAgentStore, "Stop tracking it"},
	"POST /files/scan":                             {routeGroupAgentStore, "Scan disk for context files"},
	"GET /tracked-file-ignore-rules":               {routeGroupAgentStore, "Paths the scan skips"},
	"POST /tracked-file-ignore-rules":              {routeGroupAgentStore, "Add one"},
	"POST /tracked-file-ignore-rules/{id}/enable":  {routeGroupAgentStore, "Turn one on"},
	"POST /tracked-file-ignore-rules/{id}/disable": {routeGroupAgentStore, "Turn one off"},
	"GET /seed/profiles":                           {routeGroupAgentStore, "Runner seed profiles"},
	"GET /seed/profile":                            {routeGroupAgentStore, "One runner's seed profile"},
	"PUT /seed/profile":                            {routeGroupAgentStore, "Set it"},
	"GET /seed/manifest":                           {routeGroupAgentStore, "The files a runner should hold"},
	"POST /seed/observe":                           {routeGroupAgentStore, "A runner reports the hash of a file it holds"},
	"POST /seed/drift":                             {routeGroupAgentStore, "A runner saves its local edit before overwriting the file"},
	"GET /seed/state":                              {routeGroupAgentStore, "Seed state across runners"},
	"GET /context/resolve":                         {routeGroupAgentStore, "The prompt a harness gets for a directory and tags (`?harness=&work_dir=&tag=`)"},

	// agent-store prompt source.
	"GET /prompt-collections":                   {routeGroupAgentStorePrompt, "Prompt collections: the host one and one per repo"},
	"POST /prompt-collections":                  {routeGroupAgentStorePrompt, "Create one"},
	"GET /prompt-collections/{id}":              {routeGroupAgentStorePrompt, "One collection and its sections"},
	"POST /prompt-collections/{id}/sections":    {routeGroupAgentStorePrompt, "Add a section"},
	"POST /prompt-collections/{id}/render":      {routeGroupAgentStorePrompt, "Write the collection's files to disk"},
	"POST /prompt-collections/{id}/outputs":     {routeGroupAgentStorePrompt, "Add a file the collection renders to"},
	"GET /prompt-collections/{id}/revisions":    {routeGroupAgentStorePrompt, "Its history"},
	"POST /prompt-collections/import-untracked": {routeGroupAgentStorePrompt, "Import prompt files not yet in a collection"},
	"POST /prompt-outputs/{id}/enable":          {routeGroupAgentStorePrompt, "Turn a rendered file on"},
	"POST /prompt-outputs/{id}/disable":         {routeGroupAgentStorePrompt, "Turn it off"},
	"PUT /prompt-sections/{id}":                 {routeGroupAgentStorePrompt, "Change a section"},
	"DELETE /prompt-sections/{id}":              {routeGroupAgentStorePrompt, "Delete a section"},
	"GET /prompt-sections/{id}/revisions":       {routeGroupAgentStorePrompt, "A section's history"},
	"GET /prompt-drifts":                        {routeGroupAgentStorePrompt, "Edits found in rendered files"},
	"POST /prompt-drifts/reconcile":             {routeGroupAgentStorePrompt, "Compare rendered files with the sections now"},
	"GET /prompt-drifts/{id}":                   {routeGroupAgentStorePrompt, "One drift"},
	"GET /prompt-drifts/{id}/disk-content":      {routeGroupAgentStorePrompt, "The file as it is on disk"},
	"PUT /prompt-drifts/{id}/annotation":        {routeGroupAgentStorePrompt, "Note on a drift"},
	"POST /prompt-drifts/{id}/apply":            {routeGroupAgentStorePrompt, "Take the file's edit into the sections"},
	"POST /prompt-drifts/{id}/dismiss":          {routeGroupAgentStorePrompt, "Drop the edit"},
	"GET /prompt-harness-deliveries":            {routeGroupAgentStorePrompt, "How each harness gets the prompt"},
	"PUT /prompt-harness-deliveries/{harness}":  {routeGroupAgentStorePrompt, "Set it for one harness"},
	"GET /prompt-delivery-options":              {routeGroupAgentStorePrompt, "The ways a harness can get it"},

	// memory-store.
	"POST /memories":         {routeGroupMemoryStore, "Save a memory"},
	"GET /memories/{id}":     {routeGroupMemoryStore, "One memory"},
	"DELETE /memories/{id}":  {routeGroupMemoryStore, "Delete one"},
	"POST /memories/search":  {routeGroupMemoryStore, "Search"},
	"GET /memories/recent":   {routeGroupMemoryStore, "Recent memories"},
	"POST /memories/decay":   {routeGroupMemoryStore, "Lower importance with age"},
	"POST /memories/compact": {routeGroupMemoryStore, "Compact old memories"},
	"POST /memories/context": {routeGroupMemoryStore, "Memories to put in a prompt"},
}
