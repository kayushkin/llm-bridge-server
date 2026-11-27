// Package noteboardsplit splits what noteboard kept between the two modules
// that own it since 2026-11-27: todos, which are issues, go to the issues
// module, and notes, ranked list entries and workspaces stay in notes. It
// does this twice, once each way the records can arrive:
//
//   - MoveTodosOutOfNotes, at the server's start, for an install whose
//     noteboard module already holds todos in Postgres. notes is that module,
//     renamed; the move takes its todos and their revisions to issues.
//   - ImportStandaloneDatabase, for llm-bridge-server import-noteboard, which
//     copies a standalone noteboard's SQLite file into both modules at once.
//
// Both keep every id, time and revision as it was, and both run in one
// transaction on a login that may act as both modules' roles, so the records
// are in one module or the other and never in both or neither.
//
// A revision goes with its item. A revision whose item is gone (the item was
// purged, and the revision is all that is left of it) names no type, so it
// goes to issues when the issues module holds a card's history under that id
// (a placement or a card event), and stays in notes otherwise.
package noteboardsplit

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// Modules are the two modules the split writes to, as the server provisions
// them: their schemas and roles.
type Modules struct {
	Notes  postgresstorage.Module
	Issues postgresstorage.Module
}

// itemColumns are the columns of an items table a copy carries, the
// generated search document aside, which Postgres writes itself.
const itemColumns = "id, type, title, body, tags, priority, rank, status, list_id, due_at, due_at_offset_seconds, parent_id, links, " +
	"created_by, created_at, updated_at, deleted_at, schedule, held_at, hold_reason, auto_hold_at_usd"

// revisionColumns are the columns of an item_revisions table.
const revisionColumns = "id, item_id, title, body, tags, status, priority, list_id, parent_id, links, deleted_at, reason, replaced_at"

// splitLock makes two servers that start together move the todos once.
const splitLock = "noteboardsplit"

// MoveCounts says what MoveTodosOutOfNotes moved.
type MoveCounts struct {
	// Issues is the todos moved, soft-deleted ones included.
	Issues int64 `json:"issues"`
	// Revisions is every revision moved: the todos' own and those of purged
	// items the issues module holds a card's history for.
	Revisions int64 `json:"revisions"`
	// RevisionsOfPurgedCards is the part of Revisions whose item is gone.
	RevisionsOfPurgedCards int64 `json:"revisions_of_purged_cards"`
}

// revisionsThatAreIssues is the condition on a notes revision r that it goes
// to issues: its item is a todo in notes, or its item is gone from notes and
// issues holds a card's history under its id.
func revisionsThatAreIssues(modules Modules) string {
	notes, issues := modules.Notes.Schema, modules.Issues.Schema
	return `(r.item_id IN (SELECT id FROM ` + notes + `.items WHERE type = '` + model.TypeTodo + `')
		OR (NOT EXISTS (SELECT 1 FROM ` + notes + `.items i WHERE i.id = r.item_id)
			AND r.item_id IN (SELECT card_id FROM ` + issues + `.placements UNION SELECT card_id FROM ` + issues + `.card_events)))`
}

// MoveTodosOutOfNotes moves every todo notes holds, with its revisions, into
// the issues module, keeping each id, time and revision number, and moves the
// revisions of purged items whose ids are cards. It runs in one transaction
// on administrator, a login that may act as both modules' roles (the one that
// provisioned them), after both modules are migrated. An install with no
// todos in notes, which is every install after the first start, moves nothing.
//
// An id already in issues is a failure that moves nothing: the rows would be
// two copies of one issue, and a person has to say which is right.
// Afterwards issues numbers revisions on after the highest number notes has
// handed out, so a revision number names one revision in either module.
func MoveTodosOutOfNotes(ctx context.Context, administrator *sql.DB, modules Modules) (MoveCounts, error) {
	var counts MoveCounts
	notes, issues := modules.Notes.Schema, modules.Issues.Schema
	err := postgresstorage.InTransaction(ctx, administrator, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", splitLock); err != nil {
			return fmt.Errorf("taking the split lock: %w", err)
		}
		var todos, revisions, orphanRevisions int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+notes+`.items WHERE type = $1`, model.TypeTodo).Scan(&todos); err != nil {
			return fmt.Errorf("counting the todos in %s: %w", notes, err)
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(*) FILTER (WHERE NOT EXISTS (SELECT 1 FROM `+notes+`.items i WHERE i.id = r.item_id))
			FROM `+notes+`.item_revisions r WHERE `+revisionsThatAreIssues(modules)).Scan(&revisions, &orphanRevisions); err != nil {
			return fmt.Errorf("counting the revisions to move: %w", err)
		}
		if todos == 0 && revisions == 0 {
			return nil
		}
		var clashes int64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+notes+`.items n JOIN `+issues+`.items i ON i.id = n.id WHERE n.type = $1`, model.TypeTodo).Scan(&clashes); err != nil {
			return fmt.Errorf("looking for todos issues already holds: %w", err)
		}
		if clashes > 0 {
			return fmt.Errorf("%d of the %d todos in %s are already issues in %s, so moving them would make two copies of each; nothing was moved", clashes, todos, notes, issues)
		}

		// The revisions first, while the condition can still see which items
		// are todos.
		moved, err := tx.ExecContext(ctx, `INSERT INTO `+issues+`.item_revisions (`+revisionColumns+`) OVERRIDING SYSTEM VALUE
			SELECT `+revisionColumns+` FROM `+notes+`.item_revisions r WHERE `+revisionsThatAreIssues(modules)+` ORDER BY r.id`)
		if err != nil {
			return fmt.Errorf("copying the revisions to %s: %w", issues, err)
		}
		if err := expectRows(moved, revisions, "revisions copied"); err != nil {
			return err
		}
		deleted, err := tx.ExecContext(ctx, `DELETE FROM `+notes+`.item_revisions r WHERE `+revisionsThatAreIssues(modules))
		if err != nil {
			return fmt.Errorf("removing the moved revisions from %s: %w", notes, err)
		}
		if err := expectRows(deleted, revisions, "revisions removed from notes"); err != nil {
			return err
		}

		moved, err = tx.ExecContext(ctx, `INSERT INTO `+issues+`.items (`+itemColumns+`)
			SELECT `+itemColumns+` FROM `+notes+`.items WHERE type = $1 ORDER BY created_at, id`, model.TypeTodo)
		if err != nil {
			return fmt.Errorf("copying the todos to %s: %w", issues, err)
		}
		if err := expectRows(moved, todos, "todos copied"); err != nil {
			return err
		}
		deleted, err = tx.ExecContext(ctx, `DELETE FROM `+notes+`.items WHERE type = $1`, model.TypeTodo)
		if err != nil {
			return fmt.Errorf("removing the moved todos from %s: %w", notes, err)
		}
		if err := expectRows(deleted, todos, "todos removed from notes"); err != nil {
			return err
		}

		if err := continueRevisionNumbering(ctx, tx, modules); err != nil {
			return err
		}
		counts = MoveCounts{Issues: todos, Revisions: revisions, RevisionsOfPurgedCards: orphanRevisions}
		return nil
	})
	if err != nil {
		return MoveCounts{}, fmt.Errorf("moving the todos out of %s into %s: %w", notes, issues, err)
	}
	return counts, nil
}

func expectRows(result sql.Result, want int64, what string) error {
	got, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%d %s, but %d were counted; nothing was moved", got, what, want)
	}
	return nil
}

// continueRevisionNumbering makes each module's next revision number follow
// the highest number either has handed out, so a moved revision's number is
// never handed out again in the module it left or the one it joined.
func continueRevisionNumbering(ctx context.Context, tx *sql.Tx, modules Modules) error {
	var highest int64
	for _, schema := range []string{modules.Notes.Schema, modules.Issues.Schema} {
		var last int64
		// A sequence that has handed out nothing reads as its start, 1, with
		// is_called false; then the highest number handed out is 0.
		if err := tx.QueryRowContext(ctx, `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM `+schema+`.item_revisions_id_seq`).Scan(&last); err != nil {
			return fmt.Errorf("reading the revision numbering of %s: %w", schema, err)
		}
		var highestKept sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(id) FROM `+schema+`.item_revisions`).Scan(&highestKept); err != nil {
			return fmt.Errorf("reading the highest revision of %s: %w", schema, err)
		}
		highest = max(highest, last, highestKept.Int64)
	}
	for _, schema := range []string{modules.Notes.Schema, modules.Issues.Schema} {
		if _, err := tx.ExecContext(ctx, "SELECT setval($1::regclass, $2, false)", schema+".item_revisions_id_seq", highest+1); err != nil {
			return fmt.Errorf("continuing the revision numbering of %s after %d: %w", schema, highest, err)
		}
	}
	return nil
}

// ImportCounts says what ImportStandaloneDatabase copied into each module.
type ImportCounts struct {
	Notes  items.StandaloneCopyCounts `json:"notes"`
	Issues items.StandaloneCopyCounts `json:"issues"`
	// RevisionsOfPurgedItemsInIssues and RevisionsOfPurgedItemsInNotes split
	// the revisions whose item the file no longer holds: to issues when the
	// issues module holds a card's history under the id, to notes otherwise.
	RevisionsOfPurgedItemsInIssues int `json:"revisions_of_purged_items_in_issues"`
	RevisionsOfPurgedItemsInNotes  int `json:"revisions_of_purged_items_in_notes"`
	// LastRevisionNumberHandedOut is the highest revision number the file
	// handed out; both modules number on after it.
	LastRevisionNumberHandedOut int64 `json:"last_revision_number_handed_out"`
}

// ImportStandaloneDatabase copies every item and revision of a standalone
// noteboard's SQLite file into the two modules: todos and their revisions to
// issues, the rest to notes, each id, time, hold, rule and soft-deleted row as
// it was. It runs in one transaction on administrator, a login that may act as
// both modules' roles, so it copies everything or nothing. Run import-issues
// first: a revision of a purged item goes to issues only when issues already
// holds that card's history.
//
// It refuses modules whose items or revisions tables hold any row, so it can
// never mix two noteboards or run twice, and a file holding an item of a type
// neither module keeps. Afterwards both modules number revisions on from the
// highest number the file handed out.
func ImportStandaloneDatabase(ctx context.Context, administrator *sql.DB, modules Modules, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	source, err := items.OpenStandaloneDatabase(ctx, sqlitePath)
	if err != nil {
		return counts, err
	}
	defer source.Close()
	itemTypes, err := items.StandaloneItemTypes(ctx, source)
	if err != nil {
		return counts, err
	}
	for id, itemType := range itemTypes {
		if !model.ValidType(itemType) {
			return counts, fmt.Errorf("item %s has type %q, which neither notes nor issues keeps; change it in the standalone store before importing", id, itemType)
		}
	}
	revisionItemIDs, err := items.StandaloneRevisionItemIDs(ctx, source)
	if err != nil {
		return counts, err
	}
	lastHandedOut, err := items.LastStandaloneRevisionNumberHandedOut(ctx, source)
	if err != nil {
		return counts, err
	}

	err = postgresstorage.InTransaction(ctx, administrator, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(hashtext($1))", splitLock); err != nil {
			return fmt.Errorf("taking the split lock: %w", err)
		}
		for _, module := range []postgresstorage.Module{modules.Notes, modules.Issues} {
			for _, table := range []string{"items", "item_revisions"} {
				var held int
				if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+module.Schema+"."+table).Scan(&held); err != nil {
					return fmt.Errorf("counting %s.%s: %w", module.Schema, table, err)
				}
				if held > 0 {
					return fmt.Errorf("module %s already holds %d rows in %s; the import copies into empty modules only", module.Schema, held, table)
				}
			}
		}
		cardIDs, err := idsWithCardHistory(ctx, tx, modules.Issues.Schema, revisionItemIDs, itemTypes)
		if err != nil {
			return err
		}
		revisionIsAnIssue := func(itemID string) bool {
			if itemType, kept := itemTypes[itemID]; kept {
				return itemType == model.TypeTodo
			}
			return cardIDs[itemID]
		}

		for _, destination := range []struct {
			module  postgresstorage.Module
			issues  bool
			counted *items.StandaloneCopyCounts
		}{{modules.Notes, false, &counts.Notes}, {modules.Issues, true, &counts.Issues}} {
			if err := actAsModule(ctx, tx, destination.module); err != nil {
				return err
			}
			copied, err := items.CopyStandaloneRows(ctx, source, tx,
				func(_, itemType string) bool { return (itemType == model.TypeTodo) == destination.issues },
				func(_ int64, itemID string) bool {
					if revisionIsAnIssue(itemID) != destination.issues {
						return false
					}
					if _, kept := itemTypes[itemID]; !kept {
						if destination.issues {
							counts.RevisionsOfPurgedItemsInIssues++
						} else {
							counts.RevisionsOfPurgedItemsInNotes++
						}
					}
					return true
				})
			if err != nil {
				return fmt.Errorf("copying into %s: %w", destination.module.Schema, err)
			}
			*destination.counted = copied
			lastHandedOut = max(lastHandedOut, copied.LastRevisionID)
		}
		if _, err := tx.ExecContext(ctx, "RESET ROLE"); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "RESET search_path"); err != nil {
			return err
		}
		for _, schema := range []string{modules.Notes.Schema, modules.Issues.Schema} {
			if _, err := tx.ExecContext(ctx, "SELECT setval($1::regclass, $2, false)", schema+".item_revisions_id_seq", lastHandedOut+1); err != nil {
				return fmt.Errorf("continuing the revision numbering of %s after %d: %w", schema, lastHandedOut, err)
			}
		}
		counts.LastRevisionNumberHandedOut = lastHandedOut
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

// actAsModule makes the rest of tx act as module's role and resolve bare
// table names in its schema, as the module's own pool does, so a row is
// written with the module's rights and nowhere else.
func actAsModule(ctx context.Context, tx *sql.Tx, module postgresstorage.Module) error {
	if _, err := tx.ExecContext(ctx, "RESET ROLE"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "SELECT set_config('search_path', $1, true)", module.Schema); err != nil {
		return fmt.Errorf("resolving names in %s: %w", module.Schema, err)
	}
	if module.Role != "" {
		if _, err := tx.ExecContext(ctx, "SET LOCAL ROLE "+module.Role); err != nil {
			return fmt.Errorf("acting as %s: %w", module.Role, err)
		}
	}
	return nil
}

// idsWithCardHistory is the ids, of those revisions name whose item the file
// no longer holds, under which the issues module holds a placement or a card
// event.
func idsWithCardHistory(ctx context.Context, tx *sql.Tx, issuesSchema string, revisionItemIDs []string, itemTypes map[string]string) (map[string]bool, error) {
	var purged []string
	for _, id := range revisionItemIDs {
		if _, kept := itemTypes[id]; !kept {
			purged = append(purged, id)
		}
	}
	cards := map[string]bool{}
	if len(purged) == 0 {
		return cards, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT card_id FROM `+issuesSchema+`.placements WHERE card_id = ANY($1)
		UNION SELECT card_id FROM `+issuesSchema+`.card_events WHERE card_id = ANY($1)`, purged)
	if err != nil {
		return nil, fmt.Errorf("looking for the cards of purged items in %s: %w", issuesSchema, err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		cards[id] = true
	}
	return cards, rows.Err()
}
