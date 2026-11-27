package model

import (
	"encoding/json"
	"fmt"
	"time"
)

// ItemType values. `workspace` is an agent's durable working memory: a
// timestamped markdown document a recurring job rewrites each run. It is a
// distinct type rather than a tagged `note` on purpose — a workspace is
// rewritten forever and must never be mistakable for work to do (the todo queue
// has already been flooded once) nor clutter the notes list. Being a type also
// lets the schema enforce one workspace per job, which a convention cannot.
const (
	TypeNote      = "note"
	TypeTodo      = "todo"
	TypeRank      = "rank"
	TypeWorkspace = "workspace"
)

func ValidType(t string) bool {
	switch t {
	case TypeNote, TypeTodo, TypeRank, TypeWorkspace:
		return true
	}
	return false
}

type Item struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Tags     []string   `json:"tags"`
	Priority int        `json:"priority"`
	Rank     float64    `json:"rank"`
	Status   string     `json:"status"`
	ListID   string     `json:"list_id"`
	DueAt    *time.Time `json:"due_at,omitempty"`
	ParentID *string    `json:"parent_id,omitempty"`
	Links    []string   `json:"links"`
	// Schedule is the recurrence RULE (see schedule.go). DueAt above stays the
	// single source of truth for when this item is next due; Schedule is what
	// generates that answer, not a second copy of it.
	Schedule *Schedule `json:"schedule,omitempty"`
	// DeletedAt is the reversible delete. It is deliberately NOT the `archived`
	// status: archived is a state the user chose for a live item, deletion is
	// the item being taken away. Overloading one onto the other (which DELETE
	// used to do) means a restore cannot tell them apart.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// HeldAt is the agent gate: work the user has parked. A held item stays
	// `open` and stays visible to humans — it is withheld from agent read paths,
	// which exclude held items by default (see db.ListParams.IncludeHeld).
	//
	// Hold is deliberately NOT a `status`. Status is the work's lifecycle; hold
	// is who is allowed to act on it, and an item can legitimately be open AND
	// held. Collapsing them would hide parked work from the user's own open-todo
	// list, which is the opposite of what parking it means.
	//
	// It is also deliberately not a kanban column. A column gates only the one
	// consumer that reads that column, so a card must be pausable in ANY stage
	// rather than only by being dragged into a designated gate column.
	HeldAt     *time.Time `json:"held_at,omitempty"`
	HoldReason string     `json:"hold_reason,omitempty"`
	// AutoHoldAtUSD is the spend ceiling for this item: once the cumulative cost
	// of the agent sessions that worked it reaches this many dollars, it is held
	// automatically. Nil = no ceiling (the default; this is opt-in).
	//
	// The dollars spent are deliberately NOT stored here. Cost lives in
	// llm-bridge's session aggregates and the item's session links are the join
	// — a copy kept on the item would be a second source of truth that goes stale
	// the moment a session bills another turn.
	//
	// This one number drives two enforcement points, which is why it is one number:
	// the dispatcher hands the harness a per-session cap of (ceiling - already
	// spent), so no single session can push the item past the ceiling, and the
	// curator holds the item once the cumulative total reaches it. The per-session
	// cap is what makes the polled cumulative check safe: without it a fast burner
	// could blow far past the ceiling between two polls.
	AutoHoldAtUSD *float64  `json:"auto_hold_at_usd,omitempty"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Held reports whether an item is withheld from agents.
func (i *Item) Held() bool { return i.HeldAt != nil }

// Revision is the prior state of an item, snapshotted before every mutation.
// Workspaces are rewritten on every job run, so an agent that corrupts its own
// memory would otherwise destroy the accumulated judgment with no undo.
type Revision struct {
	ID        int64      `json:"id"`
	ItemID    string     `json:"item_id"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	Tags      []string   `json:"tags"`
	Status    string     `json:"status"`
	Priority  int        `json:"priority"`
	ListID    string     `json:"list_id"`
	ParentID  *string    `json:"parent_id,omitempty"`
	Links     []string   `json:"links"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// Reason names the mutation that produced this snapshot: update, delete,
	// purge, restore, hold or unhold.
	//
	// delete and purge are the two that must not be read as one. A delete
	// leaves the row and can be undone; a purge is the only mutation in this
	// store that destroys it, so its snapshot is the sole surviving copy of
	// the item and this column is the only thing that says so.
	Reason     string    `json:"reason"`
	ReplacedAt time.Time `json:"replaced_at"`
}

type CreateItemRequest struct {
	Type      string     `json:"type"`
	Title     string     `json:"title"`
	Body      *string    `json:"body,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	Priority  *int       `json:"priority,omitempty"`
	Rank      *float64   `json:"rank,omitempty"`
	Status    *string    `json:"status,omitempty"`
	ListID    *string    `json:"list_id,omitempty"`
	DueAt     *time.Time `json:"due_at,omitempty"`
	ParentID  *string    `json:"parent_id,omitempty"`
	Links     []string   `json:"links,omitempty"`
	Schedule  *Schedule  `json:"schedule,omitempty"`
	CreatedBy *string    `json:"created_by,omitempty"`
	// Hold creates the item already parked, so there is no window between
	// creation and the gate closing in which an agent could pick it up. Opt-in:
	// the caller that knows the work is sensitive asks for it.
	Hold       bool   `json:"hold,omitempty"`
	HoldReason string `json:"hold_reason,omitempty"`
	// AutoHoldAtUSD arms the spend ceiling at creation. Nil = no ceiling.
	AutoHoldAtUSD *float64 `json:"auto_hold_at_usd,omitempty"`
}

func (r *CreateItemRequest) Validate() error {
	if r.Type == "" {
		return fmt.Errorf("type is required")
	}
	if !ValidType(r.Type) {
		return fmt.Errorf("type must be note, todo, rank, or workspace")
	}
	if r.Title == "" {
		return fmt.Errorf("title is required")
	}
	if err := r.Schedule.Validate(); err != nil {
		return err
	}
	// A rule with nothing to anchor on expands to nothing and would silently
	// remind no one, so reject it here rather than at expansion time.
	if r.Schedule != nil {
		if _, err := r.Schedule.Anchor(r.DueAt); err != nil {
			return err
		}
	}
	// A negative ceiling is already breached the moment it is set, so it would
	// arm a gate that can never open. Reject it rather than store nonsense.
	if r.AutoHoldAtUSD != nil && *r.AutoHoldAtUSD < 0 {
		return fmt.Errorf("auto_hold_at_usd must not be negative (got %v)", *r.AutoHoldAtUSD)
	}
	return nil
}

type UpdateItemRequest struct {
	Title    *string    `json:"title,omitempty"`
	Body     *string    `json:"body,omitempty"`
	Tags     []string   `json:"tags,omitempty"`
	Priority *int       `json:"priority,omitempty"`
	Rank     *float64   `json:"rank,omitempty"`
	Status   *string    `json:"status,omitempty"`
	ListID   *string    `json:"list_id,omitempty"`
	DueAt    *time.Time `json:"due_at,omitempty"`
	ParentID *string    `json:"parent_id,omitempty"`
	Links    []string   `json:"links,omitempty"`
	Schedule *Schedule  `json:"schedule,omitempty"`
	// AutoHoldAtUSD is the spend ceiling. `null` clears it (see HasAutoHoldAtUSD).
	AutoHoldAtUSD *float64 `json:"auto_hold_at_usd,omitempty"`
	// Track which fields were explicitly set
	HasTags          bool `json:"-"`
	HasLinks         bool `json:"-"`
	HasDueAt         bool `json:"-"`
	HasSchedule      bool `json:"-"`
	HasAutoHoldAtUSD bool `json:"-"`

	// ExpectedUpdatedAt makes the update conditional: it is applied only if the
	// stored item's updated_at is exactly this, and refused otherwise. It comes
	// from the If-Match header, never from the body. Nil applies the update
	// whatever the item's version, as every update did before this existed.
	ExpectedUpdatedAt *time.Time `json:"-"`
}

func (r *UpdateItemRequest) UnmarshalJSON(data []byte) error {
	type Alias UpdateItemRequest
	aux := &struct{ *Alias }{Alias: (*Alias)(r)}
	if err := json.Unmarshal(data, aux); err != nil {
		return err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if _, ok := raw["tags"]; ok {
		r.HasTags = true
	}
	if _, ok := raw["links"]; ok {
		r.HasLinks = true
	}
	// due_at and schedule need presence tracking, not just non-nil, because
	// `null` is a meaningful value for both: it is how you clear a due date or
	// take a recurrence off an item. Without this, "unset it" is indistinguishable
	// from "don't touch it" and a schedule could never be removed.
	if _, ok := raw["due_at"]; ok {
		r.HasDueAt = true
	}
	if _, ok := raw["schedule"]; ok {
		r.HasSchedule = true
	}
	// Same reason as due_at and schedule: `null` is how you REMOVE a spend
	// ceiling, and it must not read as "leave it alone". A ceiling that cannot
	// be taken off is a ceiling you can only escape by deleting the item.
	if _, ok := raw["auto_hold_at_usd"]; ok {
		r.HasAutoHoldAtUSD = true
	}
	return nil
}

// Validate checks the shape of a schedule on update. The anchor cannot be checked
// here — on a PATCH the due date it may anchor to lives on the stored item, not
// in the request — so the store re-checks it against the merged item.
func (r *UpdateItemRequest) Validate() error {
	return r.Schedule.Validate()
}

type RerankRequest struct {
	Items []RerankItem `json:"items"`
}

type RerankItem struct {
	ID   string  `json:"id"`
	Rank float64 `json:"rank"`
}

type ListInfo struct {
	ListID string `json:"list_id"`
	Count  int    `json:"count"`
}

type TagInfo struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}
