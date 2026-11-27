// Package issueitems is where an issue's content is kept: its title, body,
// status, priority, tags, due date, recurrence rule, hold, spend ceiling,
// links and revisions. They are the items and item_revisions tables of the
// issues module's own schema (migration 0002 in issues/db), read and written
// by the items package, the same code notes keeps its notes with. Where an
// issue sits on boards, and what happened to it, are the rest of the issues
// module (issues/db).
//
// An issue is an item of type todo, so every reader that checks an item's
// type still finds "todo". Its id is the id the todo had in noteboard, and a
// card's id is its issue's id.
package issueitems

import (
	"database/sql"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
)

// Holding is what the issues module keeps as items: todos, which are issues.
// An item made without a type is one. Ranked lists are notes: an issue joins
// one through a rank entry in notes whose links hold the issue's id.
var Holding = items.Holding{
	Name:        "issues",
	Types:       []string{model.TypeTodo},
	DefaultType: model.TypeTodo,
	TypesKeptElsewhere: map[string]string{
		model.TypeNote:      "notes are kept by notes: write it to /notes/items",
		model.TypeRank:      "ranked lists are kept by notes: write it to /notes/items",
		model.TypeWorkspace: "workspaces are kept by notes: write it to /notes/items",
	},
	ListMembershipKeptElsewhere: "ranked lists are notes: add a rank entry to /notes/items whose links hold this issue's id",
}

// New returns the store of issues over db, the issues module's pool. The
// schema must already be migrated.
func New(db *sql.DB) *items.Store { return items.New(db, Holding) }
