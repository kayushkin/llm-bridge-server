package tools

// Auto-registers the zero-argument tools into the in-process registry. Tools
// whose constructors require caller context (e.g. RepoMap, RecentFiles,
// ScratchpadTool, TaskPlanTool) are NOT auto-registered — consumers that need
// them call the factory with their own context and then Register the result.
// Browser, WebSearch and Scheduler are not here either: each reaches an outside
// service, and RegisterToolsThatReachOutsideServices takes where those are.
func init() {
	Register(Shell())
	Register(EndTurn())
	Register(ReadFile())
	Register(WriteFile())
	Register(EditFile())
	Register(ListFiles())
	Register(Grep())
	Register(WebFetch())
}

// OutsideServiceConnections is what the three tools that reach an outside
// service need to be told. This package reads no environment variable for
// them: the program that builds the tools owns that configuration.
type OutsideServiceConnections struct {
	Pinchtab    PinchtabConnection
	BraveAPIKey string
	Scheduler   SchedulerConnection
}

// RegisterToolsThatReachOutsideServices registers Browser, WebSearch and
// Scheduler, built with connections. A program that lists or seeds the
// registry calls it first, or those three are absent from the list.
func RegisterToolsThatReachOutsideServices(connections OutsideServiceConnections) {
	Register(Browser(connections.Pinchtab))
	Register(WebSearch(connections.BraveAPIKey))
	Register(Scheduler(connections.Scheduler))
}
