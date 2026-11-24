// Package projectstore keeps projects: what each is for, and what it owns in
// other stores, by those stores' ids. What happens in a project — its cards,
// sessions, branches, deploys — is not stored here but worked out on request
// from what it owns.
package projectstore

// Kinds of project. A project can be finished; a stream never is.
const (
	KindProject = "project"
	KindStream  = "stream"
)

// Kinds lists every kind.
var Kinds = []string{KindProject, KindStream}

// Stages a project moves through. A stream sits in keeping_up or parked.
var Stages = []string{"idea", "shaping", "building", "shipping", "keeping_up", "parked", "done"}

// DefaultStage is a new project's stage when none is given.
const DefaultStage = "idea"

// Entity types a project can own, and the store each id belongs to.
const (
	EntityRepo         = "repo"          // repo-store's numeric id
	EntityBoard        = "board"         // kanban-store board uuid
	EntitySchedulerJob = "scheduler_job" // scheduler's numeric job id
	EntityService      = "service"       // healthcheck's check name, its key there
	EntityPrincipal    = "principal"     // principal-store id, principal_000001
	EntityNote         = "note"          // noteboard item uuid: a decision, a design, a next step
	EntitySession      = "session"       // llm-bridge-server session id, br_…
)

// EntityTypeInfo describes a linkable entity type.
type EntityTypeInfo struct {
	Type    string `json:"type"`
	Service string `json:"service"`
	Meaning string `json:"meaning"`
}

// EntityTypes lists what a project can own.
var EntityTypes = []EntityTypeInfo{
	{EntityRepo, "repo-store", "A repo the project's code lives in; repo-store's numeric id. A repo may belong to several projects."},
	{EntityBoard, "kanban-store", "A board that shows the project's cards; the board's uuid. A board may show cards from several projects."},
	{EntitySchedulerJob, "scheduler", "A scheduled job that does the project's work; the scheduler's numeric job id."},
	{EntityService, "healthcheck", "A running service the project keeps up; healthcheck's check name."},
	{EntityPrincipal, "principal-store", "A person or group the project serves or works with; principal-store's id."},
	{EntityNote, "noteboard", "A note that records a decision or a design, or a todo that is one of the project's next steps; the noteboard item's uuid."},
	{EntitySession, "llm-bridge-server", "A session filed under the project: a chat about it, or a worker on it; llm-bridge-server's id. Filing is for sessions no card or branch ties to the project already."},
}

func entityTypeKnown(entityType string) bool {
	for _, info := range EntityTypes {
		if info.Type == entityType {
			return true
		}
	}
	return false
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
