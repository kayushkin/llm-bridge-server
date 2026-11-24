package journalstore

// Wire types: what the routes take and answer. ./generate-ts.sh renders this
// file to ts/model.ts; keep a type here (not in store.go, registry.go or the
// other excluded files) when a route needs it in TypeScript.

// Entry is one piece of the user's writing. All timestamps are epoch seconds,
// and 0 means unset.
type Entry struct {
	// ID is entry_000001: what anything else joins on.
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Title may be empty only for kind journal.
	Title string `json:"title"`
	// Body is markdown. A [text](ref:<id>) link in it is a mention of <id>.
	Body string `json:"body"`
	// WrittenAt is the day the entry is about or was written. It is now only
	// when the creating call sent none; an import must send it.
	WrittenAt  int64  `json:"written_at"`
	Visibility string `json:"visibility"`
	// PublishedAt is the first time the entry became published. Unpublishing
	// keeps it, so a post that goes back to draft for a fix keeps its date.
	PublishedAt int64    `json:"published_at"`
	Slug        string   `json:"slug"`
	Tags        []string `json:"tags"`
	// AboutRef is, for kind about, the id of what the entry is about
	// (person_000012). A timeline may name the person it follows here; every
	// other kind leaves it empty.
	AboutRef string `json:"about_ref"`
	// Source, SourceRef and SourceHash are set by POST /imports only.
	Source     string `json:"source"`
	SourceRef  string `json:"source_ref"`
	SourceHash string `json:"source_hash"`
	CreatedAt  int64  `json:"created_at"`
	UpdatedAt  int64  `json:"updated_at"`
	// DeletedAt is 0 for a live entry. DELETE sets it; restore clears it.
	DeletedAt int64 `json:"deleted_at"`
	// Mentions is filled by GET /entries/{id} only.
	Mentions []Mention `json:"mentions,omitempty"`
}

// Mention is one ref: link in an entry's body, or in one of a timeline's items.
type Mention struct {
	EntryID string `json:"entry_id"`
	// ItemID is set when the link is in a timeline item rather than in the
	// entry's own body; EntryID is then the timeline.
	ItemID string `json:"item_id,omitempty"`
	// Position is the 0-based order of the link among the entry's mentions.
	Position int `json:"position"`
	// Line is the 1-based line of the body the link starts on.
	Line int `json:"line"`
	// EntityRef is the id the link points at, and the only thing to join on.
	EntityRef string `json:"entity_ref"`
	// EntityType is the reference-store type whose id pattern alone
	// matches EntityRef. Empty when none or several match, or when the
	// registry has not been read yet.
	EntityType string `json:"entity_type"`
	// Label is the link text as written. Display only.
	Label string `json:"label"`
}

// EntryInput is the body of POST /entries. source fields are refused here:
// only POST /imports writes them.
type EntryInput struct {
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	WrittenAt  int64    `json:"written_at"`
	Visibility string   `json:"visibility"`
	Slug       string   `json:"slug"`
	Tags       []string `json:"tags"`
	AboutRef   string   `json:"about_ref"`
}

// EntryPatch is the body of PATCH /entries/{id}. A nil field is left alone. A
// key that is not one of these is a 400.
type EntryPatch struct {
	Kind       *string   `json:"kind,omitempty"`
	Title      *string   `json:"title,omitempty"`
	Body       *string   `json:"body,omitempty"`
	WrittenAt  *int64    `json:"written_at,omitempty"`
	Visibility *string   `json:"visibility,omitempty"`
	Slug       *string   `json:"slug,omitempty"`
	Tags       *[]string `json:"tags,omitempty"`
	AboutRef   *string   `json:"about_ref,omitempty"`
}

// Revision is the whole state of an entry just before one change to it.
type Revision struct {
	RevisionID int64 `json:"revision_id"`
	// Reason names the change that replaced this state: update, delete,
	// restore, import or promote-todo.
	Reason     string `json:"reason"`
	ReplacedAt int64  `json:"replaced_at"`
	Entry      Entry  `json:"entry"`
}

// ImportEntry is one entry in POST /imports. Source, SourceRef, SourceHash and
// WrittenAt are required.
type ImportEntry struct {
	Source     string   `json:"source"`
	SourceRef  string   `json:"source_ref"`
	SourceHash string   `json:"source_hash"`
	Kind       string   `json:"kind"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	WrittenAt  int64    `json:"written_at"`
	Visibility string   `json:"visibility"`
	Slug       string   `json:"slug"`
	Tags       []string `json:"tags"`
	AboutRef   string   `json:"about_ref"`
}

// ImportRequest is the body of POST /imports.
type ImportRequest struct {
	Entries []ImportEntry `json:"entries"`
}

// The outcomes of one imported entry.
const (
	ImportCreated = "created"
	ImportUpdated = "updated"
	ImportSkipped = "skipped"
)

// ImportOutcome says what POST /imports did with one entry.
type ImportOutcome struct {
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
	EntryID   string `json:"entry_id"`
	Outcome   string `json:"outcome"`
}

// ImportResult is the answer to POST /imports.
type ImportResult struct {
	Created  int             `json:"created"`
	Updated  int             `json:"updated"`
	Skipped  int             `json:"skipped"`
	Outcomes []ImportOutcome `json:"outcomes"`
}

// Backlink is one entry that mentions a ref, with the mentions of that ref in
// it. The answer to GET /mentions?ref=.
type Backlink struct {
	Entry    Entry     `json:"entry"`
	Mentions []Mention `json:"mentions"`
}

// PromoteTodoRequest is the body of POST /entries/{id}/promote-todo. Line is
// 1-based and must hold a markdown checklist item.
type PromoteTodoRequest struct {
	Line int `json:"line"`
}

// PromoteTodoResult is the answer to POST /entries/{id}/promote-todo: the
// noteboard todo's id and the entry with its line rewritten to link it.
type PromoteTodoResult struct {
	TodoID string `json:"todo_id"`
	Entry  Entry  `json:"entry"`
}

// TimelineItem is one dated thing on a timeline: what happened, when, and
// what it rests on.
type TimelineItem struct {
	// ID is timeline_item_000001.
	ID string `json:"id"`
	// TimelineID is the entry, of kind timeline, the item is on.
	TimelineID string `json:"timeline_id"`
	// OccurredAt is when it happened, epoch seconds; EndsAt is when it
	// stopped, or 0 for a moment.
	OccurredAt int64 `json:"occurred_at"`
	EndsAt     int64 `json:"ends_at"`
	// Precision is one of GET /time-precisions.
	Precision string `json:"precision"`
	Title     string `json:"title"`
	// Body is markdown; its [text](ref:<id>) links are mentions, like an entry's.
	Body string `json:"body"`
	// Sources are the ids the item rests on: a message, a note, an entry.
	// Each is recorded as a mention too, so the source's backlinks find it.
	Sources   []string `json:"sources"`
	CreatedAt int64    `json:"created_at"`
	UpdatedAt int64    `json:"updated_at"`
	DeletedAt int64    `json:"deleted_at"`
}

// TimelineItemInput is one item in POST /timelines/{id}/items.
type TimelineItemInput struct {
	OccurredAt int64    `json:"occurred_at"`
	EndsAt     int64    `json:"ends_at"`
	Precision  string   `json:"precision"`
	Title      string   `json:"title"`
	Body       string   `json:"body"`
	Sources    []string `json:"sources"`
}

// TimelineItemsRequest is the body of POST /timelines/{id}/items. The items
// are written in one transaction: one bad item writes none.
type TimelineItemsRequest struct {
	Items []TimelineItemInput `json:"items"`
}

// TimelineItemsResult is the answer to POST /timelines/{id}/items.
type TimelineItemsResult struct {
	Items []TimelineItem `json:"items"`
}

// TimelineItemPatch is the body of PATCH /timeline-items/{id}. A nil field is
// left alone; a key that is not one of these is a 400.
type TimelineItemPatch struct {
	OccurredAt *int64    `json:"occurred_at,omitempty"`
	EndsAt     *int64    `json:"ends_at,omitempty"`
	Precision  *string   `json:"precision,omitempty"`
	Title      *string   `json:"title,omitempty"`
	Body       *string   `json:"body,omitempty"`
	Sources    *[]string `json:"sources,omitempty"`
}

// TimelineItemRevision is the whole state of an item just before one change.
type TimelineItemRevision struct {
	RevisionID int64        `json:"revision_id"`
	Reason     string       `json:"reason"`
	ReplacedAt int64        `json:"replaced_at"`
	Item       TimelineItem `json:"item"`
}

// Counts is what GET /health counts.
type Counts struct {
	Entries       int `json:"entries"`
	Deleted       int `json:"deleted"`
	Published     int `json:"published"`
	Mentions      int `json:"mentions"`
	Revisions     int `json:"revisions"`
	TimelineItems int `json:"timeline_items"`
}

// EntityRegistryStatus says how fresh this process's copy of reference-store's
// list of reference types is.
type EntityRegistryStatus struct {
	Types     int    `json:"types"`
	FetchedAt int64  `json:"fetched_at"`
	Error     string `json:"error,omitempty"`
}

// Health is the answer to GET /health. Status is "degraded" when the
// entity-type registry could not be read: writes still work, and new
// mentions are stored with an empty type until a later read succeeds.
type Health struct {
	Status         string               `json:"status"`
	Counts         Counts               `json:"counts"`
	EntityRegistry EntityRegistryStatus `json:"entity_registry"`
}
