package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"

	_ "modernc.org/sqlite"
)

// standaloneColumns are the columns of each table of the SQLite database the
// standalone project-store kept. The import refuses a file whose tables have
// any other columns: a column it does not know is data it would drop.
var standaloneColumns = map[string][]string{
	"id_sequences": {"name", "next"},
	"projects": {"id", "name", "kind", "parent_id", "goal", "done_when", "stage", "spend_target_share", "created_by",
		"created_at", "updated_at", "archived_at"},
	"project_links":   {"id", "project_id", "entity_type", "entity_ref", "label", "note", "created_by", "created_at"},
	"project_changes": {"id", "project_id", "field", "old_value", "new_value", "changed_by", "changed_at"},
	"suggestions": {"id", "project_id", "title", "why", "status", "model", "requested_by", "created_at", "decided_by",
		"decided_at", "note_id"},
	"session_filings": {"id", "session_id", "session_label", "evidence", "goal", "repo_store_ids", "project_id", "confidence",
		"why", "model", "status", "link_id", "filed_project_id", "created_by", "created_at", "decided_by", "decided_at"},
	"goals": {"id", "project_id", "parent_goal_id", "statement", "done_when", "state", "priority", "target_at", "created_by",
		"created_at", "updated_at", "closed_by", "closed_at", "closed_because"},
	"goal_changes": {"id", "goal_id", "field", "old_value", "new_value", "changed_by", "changed_at"},
	"goal_links":   {"id", "goal_id", "entity_type", "entity_ref", "label", "note", "created_by", "created_at"},
}

// ImportCounts says how many rows of each table an import copied, and the
// number each table goes on from.
type ImportCounts struct {
	Projects                int   `json:"projects"`
	Links                   int   `json:"links"`
	Changes                 int   `json:"changes"`
	Suggestions             int   `json:"suggestions"`
	SessionFilings          int   `json:"session_filings"`
	Goals                   int   `json:"goals"`
	GoalChanges             int   `json:"goal_changes"`
	GoalLinks               int   `json:"goal_links"`
	LastGoalNumber          int64 `json:"last_goal_number"`
	LastGoalChangeID        int64 `json:"last_goal_change_id"`
	LastGoalLinkID          int64 `json:"last_goal_link_id"`
	LastProjectNumber       int64 `json:"last_project_number"`
	LastSuggestionNumber    int64 `json:"last_suggestion_number"`
	LastSessionFilingNumber int64 `json:"last_session_filing_number"`
	LastLinkID              int64 `json:"last_link_id"`
	LastChangeID            int64 `json:"last_change_id"`
}

// ImportStandaloneDatabase copies every project, link, change, suggestion and
// session filing from the SQLite file of a standalone project-store into the
// module behind db, as they are: ids, numbers and times. It runs in one
// transaction, so it copies everything or nothing.
//
// It refuses a module that already holds any row, and a file it cannot copy
// whole: a table with a column it does not know, an id that is not exactly the
// form its number makes, repo ids that are not a list of numbers, or a row
// pointing at a project the file does not hold. Afterwards each table numbers
// on from the highest number the file handed out, so no id already written on
// a card, a link or a chat is handed out again.
func ImportStandaloneDatabase(ctx context.Context, db *sql.DB, sqlitePath string) (ImportCounts, error) {
	var counts ImportCounts
	if _, err := os.Stat(sqlitePath); err != nil {
		return counts, fmt.Errorf("the standalone database: %w", err)
	}
	source, err := sql.Open("sqlite", "file:"+sqlitePath+"?mode=ro")
	if err != nil {
		return counts, fmt.Errorf("opening %s: %w", sqlitePath, err)
	}
	defer source.Close()
	for table, want := range standaloneColumns {
		if err := checkStandaloneColumns(ctx, source, table, want); err != nil {
			return counts, fmt.Errorf("%s: %w", sqlitePath, err)
		}
	}
	handedOut, err := standaloneSequences(ctx, source)
	if err != nil {
		return counts, err
	}

	err = postgresstorage.InTransaction(ctx, db, func(tx *sql.Tx) error {
		for _, table := range []string{"projects", "project_links", "project_changes", "suggestions", "session_filings", "goals", "goal_changes", "goal_links"} {
			var held int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&held); err != nil {
				return fmt.Errorf("counting %s: %w", table, err)
			}
			if held > 0 {
				return fmt.Errorf("the projects module already holds %d rows in %s; the import copies into an empty module only", held, table)
			}
		}
		projects, err := importProjects(ctx, source, tx, &counts)
		if err != nil {
			return err
		}
		if err := importLinks(ctx, source, tx, projects, &counts); err != nil {
			return err
		}
		if err := importChanges(ctx, source, tx, projects, &counts); err != nil {
			return err
		}
		if err := importSuggestions(ctx, source, tx, projects, &counts); err != nil {
			return err
		}
		if err := importSessionFilings(ctx, source, tx, projects, &counts); err != nil {
			return err
		}
		goals, err := importGoals(ctx, source, tx, projects, &counts)
		if err != nil {
			return err
		}
		if err := importGoalChanges(ctx, source, tx, goals, &counts); err != nil {
			return err
		}
		if err := importGoalLinks(ctx, source, tx, goals, &counts); err != nil {
			return err
		}
		counts.LastGoalNumber = max(counts.LastGoalNumber, handedOut[GoalIDPrefix])
		counts.LastProjectNumber = max(counts.LastProjectNumber, handedOut[ProjectIDPrefix])
		counts.LastSuggestionNumber = max(counts.LastSuggestionNumber, handedOut[SuggestionIDPrefix])
		counts.LastSessionFilingNumber = max(counts.LastSessionFilingNumber, handedOut[SessionFilingIDPrefix])
		for _, numbering := range []struct {
			table, column string
			last          int64
		}{
			{"projects", "number", counts.LastProjectNumber},
			{"suggestions", "number", counts.LastSuggestionNumber},
			{"session_filings", "number", counts.LastSessionFilingNumber},
			{"project_links", "id", counts.LastLinkID},
			{"project_changes", "id", counts.LastChangeID},
			{"goals", "number", counts.LastGoalNumber},
			{"goal_changes", "id", counts.LastGoalChangeID},
			{"goal_links", "id", counts.LastGoalLinkID},
		} {
			if err := postgresstorage.ContinueNumberingAfter(ctx, tx, numbering.table, numbering.column, numbering.last); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return ImportCounts{}, err
	}
	return counts, nil
}

func checkStandaloneColumns(ctx context.Context, source *sql.DB, table string, want []string) error {
	rows, err := source.QueryContext(ctx, "SELECT name FROM pragma_table_info(?)", table)
	if err != nil {
		return fmt.Errorf("reading the columns of %s: %w", table, err)
	}
	defer rows.Close()
	var have []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		have = append(have, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	sortedWant := append([]string(nil), want...)
	sort.Strings(have)
	sort.Strings(sortedWant)
	if strings.Join(have, ",") != strings.Join(sortedWant, ",") {
		return fmt.Errorf("table %s has columns %v; the import copies exactly %v", table, have, sortedWant)
	}
	return nil
}

// standaloneSequences is the last number the standalone store handed out of
// each id type: its id_sequences row holds the next one.
func standaloneSequences(ctx context.Context, source *sql.DB) (map[string]int64, error) {
	rows, err := source.QueryContext(ctx, "SELECT name, next FROM id_sequences")
	if err != nil {
		return nil, fmt.Errorf("reading id_sequences: %w", err)
	}
	defer rows.Close()
	handedOut := map[string]int64{}
	for rows.Next() {
		var name string
		var next int64
		if err := rows.Scan(&name, &next); err != nil {
			return nil, err
		}
		switch name {
		case ProjectIDPrefix, SuggestionIDPrefix, SessionFilingIDPrefix, GoalIDPrefix:
			handedOut[name] = next - 1
		default:
			return nil, fmt.Errorf("id_sequences numbers %q, which is not an id type this store has", name)
		}
	}
	return handedOut, rows.Err()
}

// importedNumber is the number of a stored id, refused unless the id is
// exactly the form its number makes.
func importedNumber(prefix, id, rowName string) (int64, error) {
	number, ok := recordNumberOf(prefix, id)
	if !ok {
		return 0, fmt.Errorf("%s: %q is not a %s id the store writes", rowName, id, prefix)
	}
	return number, nil
}

// importedProjectReference is a stored reference to a project: NULL for ""
// or NULL, and otherwise the number of a project the file holds.
func importedProjectReference(id sql.NullString, projects map[int64]bool, rowName string) (any, error) {
	if !id.Valid || id.String == "" {
		return nil, nil
	}
	number, err := importedNumber(ProjectIDPrefix, id.String, rowName)
	if err != nil {
		return nil, err
	}
	if !projects[number] {
		return nil, fmt.Errorf("%s points at %s, which the file does not hold", rowName, id.String)
	}
	return number, nil
}

// importProjects copies the projects and answers the numbers it copied, for
// the rows that point at them.
func importProjects(ctx context.Context, source *sql.DB, tx *sql.Tx, counts *ImportCounts) (map[int64]bool, error) {
	rows, err := source.QueryContext(ctx, `SELECT id, name, kind, parent_id, goal, done_when, stage, spend_target_share, created_by,
		created_at, updated_at, archived_at FROM projects ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading projects: %w", err)
	}
	defer rows.Close()
	parents := map[int64]sql.NullString{}
	for rows.Next() {
		var id, name, kind, goal, doneWhen, stage, createdBy string
		var parentID sql.NullString
		var share sql.NullFloat64
		var createdAt, updatedAt, archivedAt int64
		if err := rows.Scan(&id, &name, &kind, &parentID, &goal, &doneWhen, &stage, &share, &createdBy, &createdAt, &updatedAt, &archivedAt); err != nil {
			return nil, fmt.Errorf("reading the project after %d: %w", counts.LastProjectNumber, err)
		}
		number, err := importedNumber(ProjectIDPrefix, id, "project "+id)
		if err != nil {
			return nil, err
		}
		// Parents are set once every project is in, since a project may name
		// a parent made after it.
		if _, err := tx.ExecContext(ctx, `INSERT INTO projects (number, name, kind, goal, done_when, stage, spend_target_share, created_by, created_at, updated_at, archived_at)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			number, name, kind, goal, doneWhen, stage, share, createdBy, createdAt, updatedAt, archivedAt); err != nil {
			return nil, fmt.Errorf("copying project %s: %w", id, err)
		}
		parents[number] = parentID
		counts.Projects++
		counts.LastProjectNumber = max(counts.LastProjectNumber, number)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	projects := map[int64]bool{}
	for number := range parents {
		projects[number] = true
	}
	for number, parentID := range parents {
		parent, err := importedProjectReference(parentID, projects, "project "+formatProjectID(number))
		if err != nil {
			return nil, err
		}
		if parent == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE projects SET parent_number = $1 WHERE number = $2`, parent, number); err != nil {
			return nil, fmt.Errorf("setting the parent of %s: %w", formatProjectID(number), err)
		}
	}
	return projects, nil
}

func importLinks(ctx context.Context, source *sql.DB, tx *sql.Tx, projects map[int64]bool, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, project_id, entity_type, entity_ref, label, note, created_by, created_at FROM project_links ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading project_links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, createdAt int64
		var projectID sql.NullString
		var entityType, entityRef, label, note, createdBy string
		if err := rows.Scan(&id, &projectID, &entityType, &entityRef, &label, &note, &createdBy, &createdAt); err != nil {
			return fmt.Errorf("reading the link after %d: %w", counts.LastLinkID, err)
		}
		rowName := fmt.Sprintf("link %d", id)
		project, err := importedProjectReference(projectID, projects, rowName)
		if err != nil {
			return err
		}
		if project == nil {
			return fmt.Errorf("%s names no project", rowName)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_links (id, project_number, entity_type, entity_ref, label, note, created_by, created_at)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, project, entityType, entityRef, label, note, createdBy, createdAt); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.Links++
		counts.LastLinkID = id
	}
	return rows.Err()
}

func importChanges(ctx context.Context, source *sql.DB, tx *sql.Tx, projects map[int64]bool, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, project_id, field, old_value, new_value, changed_by, changed_at FROM project_changes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading project_changes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, changedAt int64
		var projectID sql.NullString
		var field, oldValue, newValue, changedBy string
		if err := rows.Scan(&id, &projectID, &field, &oldValue, &newValue, &changedBy, &changedAt); err != nil {
			return fmt.Errorf("reading the change after %d: %w", counts.LastChangeID, err)
		}
		rowName := fmt.Sprintf("change %d", id)
		project, err := importedProjectReference(projectID, projects, rowName)
		if err != nil {
			return err
		}
		if project == nil {
			return fmt.Errorf("%s names no project", rowName)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_changes (id, project_number, field, old_value, new_value, changed_by, changed_at)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, project, field, oldValue, newValue, changedBy, changedAt); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.Changes++
		counts.LastChangeID = id
	}
	return rows.Err()
}

func importSuggestions(ctx context.Context, source *sql.DB, tx *sql.Tx, projects map[int64]bool, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, project_id, title, why, status, model, requested_by, created_at, decided_by, decided_at, note_id FROM suggestions ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading suggestions: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, why, status, model, requestedBy, decidedBy, noteID string
		var projectID sql.NullString
		var createdAt, decidedAt int64
		if err := rows.Scan(&id, &projectID, &title, &why, &status, &model, &requestedBy, &createdAt, &decidedBy, &decidedAt, &noteID); err != nil {
			return fmt.Errorf("reading the suggestion after %d: %w", counts.LastSuggestionNumber, err)
		}
		rowName := "suggestion " + id
		number, err := importedNumber(SuggestionIDPrefix, id, rowName)
		if err != nil {
			return err
		}
		project, err := importedProjectReference(projectID, projects, rowName)
		if err != nil {
			return err
		}
		if project == nil {
			return fmt.Errorf("%s names no project", rowName)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO suggestions (number, project_number, title, why, status, model, requested_by, created_at, decided_by, decided_at, note_id)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			number, project, title, why, status, model, requestedBy, createdAt, decidedBy, decidedAt, noteID); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.Suggestions++
		counts.LastSuggestionNumber = max(counts.LastSuggestionNumber, number)
	}
	return rows.Err()
}

func importSessionFilings(ctx context.Context, source *sql.DB, tx *sql.Tx, projects map[int64]bool, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, session_id, session_label, evidence, goal, repo_store_ids, project_id, confidence, why, model,
		status, link_id, filed_project_id, created_by, created_at, decided_by, decided_at FROM session_filings ORDER BY rowid`)
	if err != nil {
		return fmt.Errorf("reading session_filings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, sessionID, sessionLabel, evidence, goal, repoStoreIDs, why, model, status, createdBy, decidedBy string
		var projectID, filedProjectID sql.NullString
		var confidence float64
		var linkID, createdAt, decidedAt int64
		if err := rows.Scan(&id, &sessionID, &sessionLabel, &evidence, &goal, &repoStoreIDs, &projectID, &confidence, &why, &model,
			&status, &linkID, &filedProjectID, &createdBy, &createdAt, &decidedBy, &decidedAt); err != nil {
			return fmt.Errorf("reading the session filing after %d: %w", counts.LastSessionFilingNumber, err)
		}
		rowName := "session filing " + id
		number, err := importedNumber(SessionFilingIDPrefix, id, rowName)
		if err != nil {
			return err
		}
		var repos []int64
		if err := json.Unmarshal([]byte(repoStoreIDs), &repos); err != nil {
			return fmt.Errorf("%s: repo_store_ids %q is not a list of numbers: %w", rowName, repoStoreIDs, err)
		}
		if repos == nil {
			repos = []int64{}
		}
		encodedRepos, err := json.Marshal(repos)
		if err != nil {
			return err
		}
		project, err := importedProjectReference(projectID, projects, rowName)
		if err != nil {
			return err
		}
		filedProject, err := importedProjectReference(filedProjectID, projects, rowName)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_filings (number, session_id, session_label, evidence, goal, repo_store_ids, project_number, confidence, why, model,
			status, link_id, filed_project_number, created_by, created_at, decided_by, decided_at)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			number, sessionID, sessionLabel, evidence, goal, string(encodedRepos), project, confidence, why, model,
			status, linkID, filedProject, createdBy, createdAt, decidedBy, decidedAt); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.SessionFilings++
		counts.LastSessionFilingNumber = max(counts.LastSessionFilingNumber, number)
	}
	return rows.Err()
}

// importGoals copies the goals and answers the numbers it copied. Parents are
// set once every goal is in, since a goal may name a parent made after it.
func importGoals(ctx context.Context, source *sql.DB, tx *sql.Tx, projects map[int64]bool, counts *ImportCounts) (map[int64]bool, error) {
	rows, err := source.QueryContext(ctx, `SELECT id, project_id, parent_goal_id, statement, done_when, state, priority, target_at, created_by,
		created_at, updated_at, closed_by, closed_at, closed_because FROM goals ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("reading goals: %w", err)
	}
	defer rows.Close()
	parents := map[int64]sql.NullString{}
	for rows.Next() {
		var id, statement, doneWhen, state, createdBy, closedBy, closedBecause string
		var projectID, parentGoalID sql.NullString
		var priority int
		var targetAt, createdAt, updatedAt, closedAt int64
		if err := rows.Scan(&id, &projectID, &parentGoalID, &statement, &doneWhen, &state, &priority, &targetAt, &createdBy,
			&createdAt, &updatedAt, &closedBy, &closedAt, &closedBecause); err != nil {
			return nil, fmt.Errorf("reading the goal after %d: %w", counts.LastGoalNumber, err)
		}
		rowName := "goal " + id
		number, err := importedNumber(GoalIDPrefix, id, rowName)
		if err != nil {
			return nil, err
		}
		project, err := importedProjectReference(projectID, projects, rowName)
		if err != nil {
			return nil, err
		}
		if project == nil {
			return nil, fmt.Errorf("%s names no project", rowName)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO goals (number, project_number, statement, done_when, state, priority, target_at, created_by, created_at, updated_at, closed_by, closed_at, closed_because)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
			number, project, statement, doneWhen, state, priority, targetAt, createdBy, createdAt, updatedAt, closedBy, closedAt, closedBecause); err != nil {
			return nil, fmt.Errorf("copying %s: %w", rowName, err)
		}
		parents[number] = parentGoalID
		counts.Goals++
		counts.LastGoalNumber = max(counts.LastGoalNumber, number)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	goals := map[int64]bool{}
	for number := range parents {
		goals[number] = true
	}
	for number, parentGoalID := range parents {
		parent, err := importedGoalReference(parentGoalID, goals, "goal "+formatGoalID(number))
		if err != nil {
			return nil, err
		}
		if parent == nil {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE goals SET parent_goal_number = $1 WHERE number = $2`, parent, number); err != nil {
			return nil, fmt.Errorf("setting the parent of %s: %w", formatGoalID(number), err)
		}
	}
	return goals, nil
}

// importedGoalReference is a stored reference to a goal: NULL for "" or NULL,
// and otherwise the number of a goal the file holds.
func importedGoalReference(id sql.NullString, goals map[int64]bool, rowName string) (any, error) {
	if !id.Valid || id.String == "" {
		return nil, nil
	}
	number, err := importedNumber(GoalIDPrefix, id.String, rowName)
	if err != nil {
		return nil, err
	}
	if !goals[number] {
		return nil, fmt.Errorf("%s points at %s, which the file does not hold", rowName, id.String)
	}
	return number, nil
}

func importGoalChanges(ctx context.Context, source *sql.DB, tx *sql.Tx, goals map[int64]bool, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, goal_id, field, old_value, new_value, changed_by, changed_at FROM goal_changes ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading goal_changes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, changedAt int64
		var goalID sql.NullString
		var field, oldValue, newValue, changedBy string
		if err := rows.Scan(&id, &goalID, &field, &oldValue, &newValue, &changedBy, &changedAt); err != nil {
			return fmt.Errorf("reading the goal change after %d: %w", counts.LastGoalChangeID, err)
		}
		rowName := fmt.Sprintf("goal change %d", id)
		goal, err := importedGoalReference(goalID, goals, rowName)
		if err != nil {
			return err
		}
		if goal == nil {
			return fmt.Errorf("%s names no goal", rowName)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_changes (id, goal_number, field, old_value, new_value, changed_by, changed_at)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, goal, field, oldValue, newValue, changedBy, changedAt); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.GoalChanges++
		counts.LastGoalChangeID = id
	}
	return rows.Err()
}

func importGoalLinks(ctx context.Context, source *sql.DB, tx *sql.Tx, goals map[int64]bool, counts *ImportCounts) error {
	rows, err := source.QueryContext(ctx, `SELECT id, goal_id, entity_type, entity_ref, label, note, created_by, created_at FROM goal_links ORDER BY id`)
	if err != nil {
		return fmt.Errorf("reading goal_links: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, createdAt int64
		var goalID sql.NullString
		var entityType, entityRef, label, note, createdBy string
		if err := rows.Scan(&id, &goalID, &entityType, &entityRef, &label, &note, &createdBy, &createdAt); err != nil {
			return fmt.Errorf("reading the goal link after %d: %w", counts.LastGoalLinkID, err)
		}
		rowName := fmt.Sprintf("goal link %d", id)
		goal, err := importedGoalReference(goalID, goals, rowName)
		if err != nil {
			return err
		}
		if goal == nil {
			return fmt.Errorf("%s names no goal", rowName)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO goal_links (id, goal_number, entity_type, entity_ref, label, note, created_by, created_at)
			OVERRIDING SYSTEM VALUE VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, goal, entityType, entityRef, label, note, createdBy, createdAt); err != nil {
			return fmt.Errorf("copying %s: %w", rowName, err)
		}
		counts.GoalLinks++
		counts.LastGoalLinkID = id
	}
	return rows.Err()
}
