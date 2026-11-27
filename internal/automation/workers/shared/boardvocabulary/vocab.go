// Package boardvocabulary holds the strings the kanban binaries exchange through
// kanban-store: entity types, card-link types, column names, and the tags that
// make up the task-completion-loop protocol.
//
// These are wire formats, not local preferences. kanban-scoper writes a
// "group:" tag that kanban-dispatcher reads back; kanban-classifier writes a
// card into "In Progress" that kanban-curator later moves to "Done". Both
// sides used to spell the string separately, so a one-character drift in
// either would have produced no error and no empty result that looked wrong —
// just sub-tasks silently never grouping, or cards silently never closing.
//
// # Column names are shared, column SETS are not
//
// A column name is only ever used to look up a column id on one specific
// board (issuesclient.Client.ColumnsByName). The name is therefore safe to
// share across boards; the set of columns a board has is not. There are four
// live boards and they do not agree:
//
//	Task completion loop   Goals, Backlog, In Progress, Done, Failed
//	Agent runs             Queued, In Progress, Blocked, Done, Failed
//	Autoworker queue       Needs approval, Queued, In Progress, Blocked, Done, Failed
//	Projects               Init, In Progress, Done
//
// So "Done" on Task completion loop and "Done" on Agent runs are two
// different rows with two different ids that happen to share a name, and
// merging the two boards' column *lists* into one constant would be a
// name-based join across boards. The lifecycle triple below is shared because
// kanban-curator genuinely runs against three of these boards from one binary
// and needs the same three names on each; the per-board blocks are kept apart
// on purpose.
package boardvocabulary

// Entity types. kanban-store stamps both entity tags and card links with an
// entity_type naming what kind of thing the ref points at, and the two tables
// share one namespace — "session" means a bridge session id in either.
const (
	// EntityTypeNote is the entity type for a card. It is not a misnomer:
	// kanban-store owns placement only and noteboard owns card content, so a
	// card id *is* a noteboard item id.
	EntityTypeNote = "note"

	// EntityTypeSession refers to an llm-bridge session id.
	EntityTypeSession = "session"
)

// Card-link types. A card link records that a card relates to some entity;
// LinkTypeSession shares its value with EntityTypeSession because it names the
// same kind of thing.
const (
	LinkTypeSession = EntityTypeSession

	// LinkTypeParentCard links a task-completion-loop sub-task card back to
	// the parent card it was decomposed from.
	LinkTypeParentCard = "parent_card"
)

// Lifecycle columns. Every board that takes part in the agent workflow carries
// these three names, which is what lets one kanban-curator binary be pointed at
// any of them.
const (
	ColumnInProgress = "In Progress"
	ColumnDone       = "Done"
	ColumnFailed     = "Failed"
)

// --- Task completion loop board ---

// BoardTaskCompletionLoop is the board kanban-scoper decomposes onto and
// kanban-dispatcher dispatches from.
const BoardTaskCompletionLoop = "Task completion loop"

const (
	// ColumnGoals holds parent cards awaiting decomposition.
	ColumnGoals = "Goals"
	// ColumnBacklog holds the sub-task cards a parent was decomposed into.
	ColumnBacklog = "Backlog"
)

const (
	// TagTrigger marks a Goals card as eligible for decomposition.
	TagTrigger = "task-completion-loop"
	// TagSubtask marks a card as a sub-task produced by kanban-scoper.
	TagSubtask = "task-completion-loop-subtask"
	// TagScoped is stamped on a parent (as an entity tag of EntityTypeNote)
	// once it has been decomposed, so it is not decomposed twice.
	TagScoped = "scoped"
	// TagGroupPrefix prefixes the sub-task grouping tag, e.g. "group:build".
	// kanban-scoper writes it and kanban-dispatcher reads it back to decide
	// which sub-tasks share one bridge session. This is the single most
	// drift-sensitive string in the package: the two binaries agreeing on it
	// is the entire mechanism.
	TagGroupPrefix = "group:"
)

// TaskCompletionLoopColumns returns the column set for BoardTaskCompletionLoop,
// in board order. Callers get a fresh slice; it is not shared state.
func TaskCompletionLoopColumns() []string {
	return []string{ColumnGoals, ColumnBacklog, ColumnInProgress, ColumnDone, ColumnFailed}
}

// --- Agent runs board ---

// BoardAgentRuns is the board kanban-classifier writes session cards onto.
const BoardAgentRuns = "Agent runs"

const (
	// ColumnQueued holds cards for sessions that have not started work.
	ColumnQueued = "Queued"
	// ColumnBlocked holds cards for sessions waiting on something external.
	ColumnBlocked = "Blocked"
)

// AgentRunsColumns returns the column set for BoardAgentRuns, in board order.
// Callers get a fresh slice; it is not shared state.
func AgentRunsColumns() []string {
	return []string{ColumnQueued, ColumnInProgress, ColumnBlocked, ColumnDone, ColumnFailed}
}

// --- Autoworker queue board ---

// BoardAutoworkerQueue is the board a human (or kanban-curator) drops work onto
// for cmd/autoworker to pick up.
const BoardAutoworkerQueue = "Autoworker queue"

// AutoworkerQueueColumns returns the column set for BoardAutoworkerQueue, in
// board order. Callers get a fresh slice; it is not shared state.
//
// This currently returns the same names as AgentRunsColumns, and it is a
// separate function on purpose. cmd/autoworker used to pass one shared slice to
// EnsureBoard for both boards, which is the hazard the package doc describes:
// the moment either board's shape needs to change, a single shared set changes
// both, and EnsureBoard's additive behavior means the mistake shows up as an
// extra column rather than an error.
//
// ⚠️ The live board carries a sixth column, "Needs approval", which no code in
// this repo creates — EnsureBoard only adds missing columns and never removes,
// so it has survived since somebody added it by hand. It is named in a doc
// comment in cmd/ask as the column an approval reply moves a card out of, and
// it holds no cards. On a fresh box this board would come up without it. Adding
// it here would change what EnsureBoard creates, so it is deliberately left out
// pending a decision about whether the approval flow is real.
func AutoworkerQueueColumns() []string {
	return []string{ColumnQueued, ColumnInProgress, ColumnBlocked, ColumnDone, ColumnFailed}
}
