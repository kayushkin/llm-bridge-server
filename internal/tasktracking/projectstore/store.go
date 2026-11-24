package projectstore

import (
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

//go:embed schema.sql
var schemaSQL string

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("not found")

// ErrInvalid marks a request the caller got wrong, so the HTTP layer answers 400.
var ErrInvalid = errors.New("invalid")

// ErrConflict marks a write that clashes with a row already there.
var ErrConflict = errors.New("conflict")

// Project is a project's own fields.
type Project struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	ParentID string `json:"parent_id"`
	Goal     string `json:"goal"`
	DoneWhen string `json:"done_when"`
	Stage    string `json:"stage"`
	// SpendTargetShare is nil when nobody set a target.
	SpendTargetShare *float64 `json:"spend_target_share"`
	CreatedBy        string   `json:"created_by"`
	CreatedAt        int64    `json:"created_at"`
	UpdatedAt        int64    `json:"updated_at"`
	ArchivedAt       int64    `json:"archived_at"`
}

// Link is one thing a project owns.
type Link struct {
	ID         int64  `json:"id"`
	ProjectID  string `json:"project_id"`
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Label      string `json:"label"`
	Note       string `json:"note"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
}

// Change is one recorded change to a project's field.
type Change struct {
	ID        int64  `json:"id"`
	ProjectID string `json:"project_id"`
	Field     string `json:"field"`
	OldValue  string `json:"old_value"`
	NewValue  string `json:"new_value"`
	ChangedBy string `json:"changed_by"`
	ChangedAt int64  `json:"changed_at"`
}

// Store is the SQLite database.
type Store struct {
	database *sql.DB
	now      func() time.Time
}

// Open opens or creates project-store.db in dataDirectory.
func Open(dataDirectory string) (*Store, error) {
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return nil, err
	}
	database, err := sql.Open("sqlite3", filepath.Join(dataDirectory, "project-store.db")+"?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	database.SetMaxOpenConns(1)
	if _, err := database.Exec(schemaSQL); err != nil {
		database.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{database: database, now: time.Now}, nil
}

// Close closes the database.
func (store *Store) Close() error { return store.database.Close() }

const projectColumns = `id, name, kind, COALESCE(parent_id, ''), goal, done_when, stage, spend_target_share, created_by, created_at, updated_at, archived_at`

type scanner interface{ Scan(...any) error }

func scanProject(row scanner) (Project, error) {
	var project Project
	var share sql.NullFloat64
	err := row.Scan(&project.ID, &project.Name, &project.Kind, &project.ParentID, &project.Goal, &project.DoneWhen, &project.Stage,
		&share, &project.CreatedBy, &project.CreatedAt, &project.UpdatedAt, &project.ArchivedAt)
	if share.Valid {
		project.SpendTargetShare = &share.Float64
	}
	return project, err
}

// ProjectFields are the fields a caller sets on create or patch. A nil field
// is left as it is (on create: its default).
type ProjectFields struct {
	Name             *string
	Kind             *string
	ParentID         *string
	Goal             *string
	DoneWhen         *string
	Stage            *string
	SpendTargetShare **float64
}

func (store *Store) validate(transaction *sql.Tx, projectID string, fields ProjectFields) error {
	if fields.Name != nil && strings.TrimSpace(*fields.Name) == "" {
		return fmt.Errorf("%w: name must not be empty", ErrInvalid)
	}
	if fields.Kind != nil && !contains(Kinds, *fields.Kind) {
		return fmt.Errorf("%w: kind %q is not one of %s", ErrInvalid, *fields.Kind, strings.Join(Kinds, ", "))
	}
	if fields.Stage != nil && !contains(Stages, *fields.Stage) {
		return fmt.Errorf("%w: stage %q is not one of %s", ErrInvalid, *fields.Stage, strings.Join(Stages, ", "))
	}
	if fields.SpendTargetShare != nil && *fields.SpendTargetShare != nil {
		if share := **fields.SpendTargetShare; share < 0 || share > 1 {
			return fmt.Errorf("%w: spend_target_share %v is not between 0 and 1", ErrInvalid, share)
		}
	}
	if fields.ParentID != nil && *fields.ParentID != "" {
		// The parent must exist, and must not be the project or below it.
		for ancestor := *fields.ParentID; ancestor != ""; {
			if ancestor == projectID {
				return fmt.Errorf("%w: parent_id %s would make the project its own ancestor", ErrInvalid, *fields.ParentID)
			}
			var next sql.NullString
			if err := transaction.QueryRow(`SELECT parent_id FROM projects WHERE id = ?`, ancestor).Scan(&next); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: parent_id %s names no project", ErrInvalid, ancestor)
				}
				return err
			}
			ancestor = next.String
		}
	}
	return nil
}

func nextID(transaction *sql.Tx, prefix string) (string, error) {
	var next int64
	err := transaction.QueryRow(`SELECT next FROM id_sequences WHERE name = ?`, prefix).Scan(&next)
	if errors.Is(err, sql.ErrNoRows) {
		next = 1
		if _, err := transaction.Exec(`INSERT INTO id_sequences (name, next) VALUES (?, 2)`, prefix); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	} else if _, err := transaction.Exec(`UPDATE id_sequences SET next = next + 1 WHERE name = ?`, prefix); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s_%06d", prefix, next), nil
}

// CreateProject creates a project. Name is required; kind defaults to
// project and stage to idea.
func (store *Store) CreateProject(fields ProjectFields, createdBy string) (Project, error) {
	if fields.Name == nil {
		return Project{}, fmt.Errorf("%w: name is required", ErrInvalid)
	}
	kind, stage := KindProject, DefaultStage
	if fields.Kind == nil {
		fields.Kind = &kind
	}
	if fields.Stage == nil {
		fields.Stage = &stage
	}
	transaction, err := store.database.Begin()
	if err != nil {
		return Project{}, err
	}
	defer transaction.Rollback()
	if err := store.validate(transaction, "", fields); err != nil {
		return Project{}, err
	}
	id, err := nextID(transaction, "project")
	if err != nil {
		return Project{}, err
	}
	now := store.now().Unix()
	var parent, goal, doneWhen sql.NullString
	if fields.ParentID != nil && *fields.ParentID != "" {
		parent = sql.NullString{String: *fields.ParentID, Valid: true}
	}
	if fields.Goal != nil {
		goal.String = *fields.Goal
	}
	if fields.DoneWhen != nil {
		doneWhen.String = *fields.DoneWhen
	}
	var share sql.NullFloat64
	if fields.SpendTargetShare != nil && *fields.SpendTargetShare != nil {
		share = sql.NullFloat64{Float64: **fields.SpendTargetShare, Valid: true}
	}
	if _, err := transaction.Exec(`INSERT INTO projects (id, name, kind, parent_id, goal, done_when, stage, spend_target_share, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, strings.TrimSpace(*fields.Name), *fields.Kind, parent, goal.String, doneWhen.String, *fields.Stage, share, createdBy, now, now); err != nil {
		return Project{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Project{}, err
	}
	return store.Project(id)
}

// Project returns one project.
func (store *Store) Project(id string) (Project, error) {
	project, err := scanProject(store.database.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("project %s: %w", id, ErrNotFound)
	}
	return project, err
}

// ProjectFilter narrows Projects.
type ProjectFilter struct {
	Query           string
	Kind            string
	ParentID        string
	IncludeArchived bool
}

// Projects lists projects by name.
func (store *Store) Projects(filter ProjectFilter) ([]Project, error) {
	var conditions []string
	var arguments []any
	if !filter.IncludeArchived {
		conditions = append(conditions, "archived_at = 0")
	}
	if filter.Kind != "" {
		conditions = append(conditions, "kind = ?")
		arguments = append(arguments, filter.Kind)
	}
	if filter.ParentID != "" {
		conditions = append(conditions, "parent_id = ?")
		arguments = append(arguments, filter.ParentID)
	}
	if filter.Query != "" {
		conditions = append(conditions, "(name LIKE ? OR goal LIKE ? OR id = ?)")
		pattern := "%" + filter.Query + "%"
		arguments = append(arguments, pattern, pattern, filter.Query)
	}
	query := `SELECT ` + projectColumns + ` FROM projects`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := store.database.Query(query+" ORDER BY name COLLATE NOCASE, id", arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := []Project{}
	for rows.Next() {
		project, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	return projects, rows.Err()
}

func formatShare(share *float64) string {
	if share == nil {
		return ""
	}
	return strconv.FormatFloat(*share, 'f', -1, 64)
}

// UpdateProject changes the given fields and records each change.
// archived true archives, false restores; nil leaves it.
func (store *Store) UpdateProject(id string, fields ProjectFields, archived *bool, changedBy string) (Project, error) {
	transaction, err := store.database.Begin()
	if err != nil {
		return Project{}, err
	}
	defer transaction.Rollback()
	current, err := scanProject(transaction.QueryRow(`SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, fmt.Errorf("project %s: %w", id, ErrNotFound)
	} else if err != nil {
		return Project{}, err
	}
	if err := store.validate(transaction, id, fields); err != nil {
		return Project{}, err
	}
	now := store.now().Unix()
	changed := false
	record := func(field, column, oldValue, newValue string, value any) error {
		if oldValue == newValue {
			return nil
		}
		changed = true
		if _, err := transaction.Exec(`UPDATE projects SET `+column+` = ? WHERE id = ?`, value, id); err != nil {
			return err
		}
		_, err := transaction.Exec(`INSERT INTO project_changes (project_id, field, old_value, new_value, changed_by, changed_at) VALUES (?, ?, ?, ?, ?, ?)`,
			id, field, oldValue, newValue, changedBy, now)
		return err
	}
	type textField struct {
		name    string
		current string
		next    *string
	}
	for _, field := range []textField{{"name", current.Name, fields.Name}, {"kind", current.Kind, fields.Kind}, {"goal", current.Goal, fields.Goal},
		{"done_when", current.DoneWhen, fields.DoneWhen}, {"stage", current.Stage, fields.Stage}} {
		if field.next == nil {
			continue
		}
		next := *field.next
		if field.name == "name" {
			next = strings.TrimSpace(next)
		}
		if err := record(field.name, field.name, field.current, next, next); err != nil {
			return Project{}, err
		}
	}
	if fields.ParentID != nil {
		var value any
		if *fields.ParentID != "" {
			value = *fields.ParentID
		}
		if err := record("parent_id", "parent_id", current.ParentID, *fields.ParentID, value); err != nil {
			return Project{}, err
		}
	}
	if fields.SpendTargetShare != nil {
		var value any
		if *fields.SpendTargetShare != nil {
			value = **fields.SpendTargetShare
		}
		if err := record("spend_target_share", "spend_target_share", formatShare(current.SpendTargetShare), formatShare(*fields.SpendTargetShare), value); err != nil {
			return Project{}, err
		}
	}
	if archived != nil {
		oldValue, newValue, archivedAt := "false", "false", int64(0)
		if current.ArchivedAt != 0 {
			oldValue = "true"
		}
		if *archived {
			newValue, archivedAt = "true", now
		}
		if err := record("archived", "archived_at", oldValue, newValue, archivedAt); err != nil {
			return Project{}, err
		}
	}
	if changed {
		if _, err := transaction.Exec(`UPDATE projects SET updated_at = ? WHERE id = ?`, now, id); err != nil {
			return Project{}, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return Project{}, err
	}
	return store.Project(id)
}

// Changes lists a project's recorded changes, oldest first.
func (store *Store) Changes(projectID string) ([]Change, error) {
	if _, err := store.Project(projectID); err != nil {
		return nil, err
	}
	rows, err := store.database.Query(`SELECT id, project_id, field, old_value, new_value, changed_by, changed_at FROM project_changes WHERE project_id = ? ORDER BY id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []Change{}
	for rows.Next() {
		var change Change
		if err := rows.Scan(&change.ID, &change.ProjectID, &change.Field, &change.OldValue, &change.NewValue, &change.ChangedBy, &change.ChangedAt); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

// AddLink records that a project owns an entity. The caller has checked the
// entity with its owner and passes the owner's name for it as label.
func (store *Store) AddLink(link Link) (Link, error) {
	if _, err := store.Project(link.ProjectID); err != nil {
		return Link{}, err
	}
	link.CreatedAt = store.now().Unix()
	result, err := store.database.Exec(`INSERT INTO project_links (project_id, entity_type, entity_ref, label, note, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		link.ProjectID, link.EntityType, link.EntityRef, link.Label, link.Note, link.CreatedBy, link.CreatedAt)
	var sqliteError sqlite3.Error
	if errors.As(err, &sqliteError) && sqliteError.ExtendedCode == sqlite3.ErrConstraintUnique {
		return Link{}, fmt.Errorf("%w: %s already owns %s %s", ErrConflict, link.ProjectID, link.EntityType, link.EntityRef)
	}
	if err != nil {
		return Link{}, err
	}
	link.ID, err = result.LastInsertId()
	return link, err
}

// DeleteLink removes one link of a project.
func (store *Store) DeleteLink(projectID string, linkID int64) error {
	result, err := store.database.Exec(`DELETE FROM project_links WHERE id = ? AND project_id = ?`, linkID, projectID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return fmt.Errorf("link %d of %s: %w", linkID, projectID, ErrNotFound)
	}
	return nil
}

// LinkFilter narrows Links. Zero fields do not filter.
type LinkFilter struct {
	ProjectID  string
	EntityType string
	EntityRef  string
}

// Links lists links, oldest first.
func (store *Store) Links(filter LinkFilter) ([]Link, error) {
	var conditions []string
	var arguments []any
	if filter.ProjectID != "" {
		conditions = append(conditions, "project_id = ?")
		arguments = append(arguments, filter.ProjectID)
	}
	if filter.EntityType != "" {
		conditions = append(conditions, "entity_type = ?")
		arguments = append(arguments, filter.EntityType)
	}
	if filter.EntityRef != "" {
		conditions = append(conditions, "entity_ref = ?")
		arguments = append(arguments, filter.EntityRef)
	}
	query := `SELECT id, project_id, entity_type, entity_ref, label, note, created_by, created_at FROM project_links`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := store.database.Query(query+" ORDER BY id", arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []Link{}
	for rows.Next() {
		var link Link
		if err := rows.Scan(&link.ID, &link.ProjectID, &link.EntityType, &link.EntityRef, &link.Label, &link.Note, &link.CreatedBy, &link.CreatedAt); err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}
