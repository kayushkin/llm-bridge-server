package projectstore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Module is the Postgres schema and role the store keeps its records in, with
// the migrations that build them.
func Module() (postgresstorage.Module, error) {
	migrations, err := postgresstorage.MigrationsFromFiles(migrationFiles, "migrations")
	if err != nil {
		return postgresstorage.Module{}, err
	}
	return postgresstorage.Module{Schema: "projects", Role: "projects_module", Migrations: migrations}, nil
}

// The type parts of the public ids: project_000001, suggestion_000001 and
// session_filing_000001.
const (
	ProjectIDPrefix       = "project"
	SuggestionIDPrefix    = "suggestion"
	SessionFilingIDPrefix = "session_filing"
)

func formatProjectID(number int64) string {
	return postgresstorage.FormatRecordID(ProjectIDPrefix, number)
}

// recordNumberOf reads the number out of a public id of the type prefix
// names. An id that is not exactly the form the store writes names no record:
// project_0000001 is not project_000001, as it was not when the id was a
// stored string.
func recordNumberOf(prefix, id string) (int64, bool) {
	number, err := postgresstorage.ParseRecordID(prefix, id)
	if err != nil || postgresstorage.FormatRecordID(prefix, number) != id {
		return 0, false
	}
	return number, true
}

// projectNumberOf is a project id's number, or ErrNotFound for an id that names
// no project by its form alone.
func projectNumberOf(id string) (int64, error) {
	number, ok := recordNumberOf(ProjectIDPrefix, id)
	if !ok {
		return 0, fmt.Errorf("project %s: %w", id, ErrNotFound)
	}
	return number, nil
}

// nullableProjectNumber is a stored reference to a project: NULL for "".
func nullableProjectNumber(id string) (any, error) {
	if id == "" {
		return nil, nil
	}
	number, ok := recordNumberOf(ProjectIDPrefix, id)
	if !ok {
		return nil, fmt.Errorf("%w: %q is not a project id", ErrInvalid, id)
	}
	return number, nil
}

// projectIDOf is the public id of a stored reference to a project; "" for NULL.
func projectIDOf(number sql.NullInt64) string {
	if !number.Valid {
		return ""
	}
	return formatProjectID(number.Int64)
}

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

// Store keeps the records in the module's Postgres schema.
type Store struct {
	database *sql.DB
	now      func() time.Time
}

// New returns the store over database, the pool postgresstorage.Open gave
// Module. The schema must already be migrated.
func New(database *sql.DB) *Store {
	return &Store{database: database, now: time.Now}
}

// inTransaction runs work in one transaction.
func (store *Store) inTransaction(work func(transaction *sql.Tx) error) error {
	return postgresstorage.InTransaction(context.Background(), store.database, work)
}

const projectColumns = `number, name, kind, parent_number, goal, done_when, stage, spend_target_share, created_by, created_at, updated_at, archived_at`

type scanner interface{ Scan(...any) error }

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func scanProject(row scanner) (Project, error) {
	var project Project
	var number int64
	var parent sql.NullInt64
	var share sql.NullFloat64
	err := row.Scan(&number, &project.Name, &project.Kind, &parent, &project.Goal, &project.DoneWhen, &project.Stage,
		&share, &project.CreatedBy, &project.CreatedAt, &project.UpdatedAt, &project.ArchivedAt)
	if err != nil {
		return Project{}, err
	}
	project.ID = formatProjectID(number)
	project.ParentID = projectIDOf(parent)
	if share.Valid {
		project.SpendTargetShare = &share.Float64
	}
	return project, nil
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
			number, ok := recordNumberOf(ProjectIDPrefix, ancestor)
			if !ok {
				return fmt.Errorf("%w: parent_id %s names no project", ErrInvalid, ancestor)
			}
			var next sql.NullInt64
			if err := transaction.QueryRow(`SELECT parent_number FROM projects WHERE number = $1`, number).Scan(&next); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: parent_id %s names no project", ErrInvalid, ancestor)
				}
				return err
			}
			ancestor = projectIDOf(next)
		}
	}
	return nil
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
	var number int64
	err := store.inTransaction(func(transaction *sql.Tx) error {
		if err := store.validate(transaction, "", fields); err != nil {
			return err
		}
		now := store.now().Unix()
		var parent any
		if fields.ParentID != nil && *fields.ParentID != "" {
			parentNumber, err := nullableProjectNumber(*fields.ParentID)
			if err != nil {
				return err
			}
			parent = parentNumber
		}
		var goal, doneWhen string
		if fields.Goal != nil {
			goal = *fields.Goal
		}
		if fields.DoneWhen != nil {
			doneWhen = *fields.DoneWhen
		}
		var share sql.NullFloat64
		if fields.SpendTargetShare != nil && *fields.SpendTargetShare != nil {
			share = sql.NullFloat64{Float64: **fields.SpendTargetShare, Valid: true}
		}
		return transaction.QueryRow(`INSERT INTO projects (name, kind, parent_number, goal, done_when, stage, spend_target_share, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING number`,
			strings.TrimSpace(*fields.Name), *fields.Kind, parent, goal, doneWhen, *fields.Stage, share, createdBy, now, now).Scan(&number)
	})
	if err != nil {
		return Project{}, err
	}
	return store.Project(formatProjectID(number))
}

// Project returns one project.
func (store *Store) Project(id string) (Project, error) {
	return projectOn(store.database, id, false)
}

// projectOn reads one project; forUpdate locks its row until the
// transaction ends.
func projectOn(handle queryer, id string, forUpdate bool) (Project, error) {
	number, err := projectNumberOf(id)
	if err != nil {
		return Project{}, err
	}
	query := `SELECT ` + projectColumns + ` FROM projects WHERE number = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	project, err := scanProject(handle.QueryRow(query, number))
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

// Projects lists projects by name, ignoring case in ASCII letters, as
// SQLite's NOCASE did.
func (store *Store) Projects(filter ProjectFilter) ([]Project, error) {
	var conditions []string
	var arguments []any
	placeholder := func(value any) string {
		arguments = append(arguments, value)
		return "$" + strconv.Itoa(len(arguments))
	}
	if !filter.IncludeArchived {
		conditions = append(conditions, "archived_at = 0")
	}
	if filter.Kind != "" {
		conditions = append(conditions, "kind = "+placeholder(filter.Kind))
	}
	if filter.ParentID != "" {
		number, ok := recordNumberOf(ProjectIDPrefix, filter.ParentID)
		if !ok {
			return []Project{}, nil
		}
		conditions = append(conditions, "parent_number = "+placeholder(number))
	}
	if filter.Query != "" {
		// A pattern with no escape character, as SQLite's LIKE took it:
		// % and _ in the query still match anything, as they did.
		pattern := placeholder("%" + filter.Query + "%")
		condition := "(name ILIKE " + pattern + " ESCAPE '' OR goal ILIKE " + pattern + " ESCAPE ''"
		if number, ok := recordNumberOf(ProjectIDPrefix, filter.Query); ok {
			condition += " OR number = " + placeholder(number)
		}
		conditions = append(conditions, condition+")")
	}
	query := `SELECT ` + projectColumns + ` FROM projects`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := store.database.Query(query+` ORDER BY lower(name COLLATE "C"), number`, arguments...)
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
	err := store.inTransaction(func(transaction *sql.Tx) error {
		current, err := projectOn(transaction, id, true)
		if err != nil {
			return err
		}
		number, err := projectNumberOf(id)
		if err != nil {
			return err
		}
		if err := store.validate(transaction, id, fields); err != nil {
			return err
		}
		now := store.now().Unix()
		changed := false
		record := func(field, column, oldValue, newValue string, value any) error {
			if oldValue == newValue {
				return nil
			}
			changed = true
			if _, err := transaction.Exec(`UPDATE projects SET `+column+` = $1 WHERE number = $2`, value, number); err != nil {
				return err
			}
			_, err := transaction.Exec(`INSERT INTO project_changes (project_number, field, old_value, new_value, changed_by, changed_at) VALUES ($1, $2, $3, $4, $5, $6)`,
				number, field, oldValue, newValue, changedBy, now)
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
				return err
			}
		}
		if fields.ParentID != nil {
			value, err := nullableProjectNumber(*fields.ParentID)
			if err != nil {
				return err
			}
			if err := record("parent_id", "parent_number", current.ParentID, *fields.ParentID, value); err != nil {
				return err
			}
		}
		if fields.SpendTargetShare != nil {
			var value any
			if *fields.SpendTargetShare != nil {
				value = **fields.SpendTargetShare
			}
			if err := record("spend_target_share", "spend_target_share", formatShare(current.SpendTargetShare), formatShare(*fields.SpendTargetShare), value); err != nil {
				return err
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
				return err
			}
		}
		if changed {
			if _, err := transaction.Exec(`UPDATE projects SET updated_at = $1 WHERE number = $2`, now, number); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	return store.Project(id)
}

// Changes lists a project's recorded changes, oldest first.
func (store *Store) Changes(projectID string) ([]Change, error) {
	if _, err := store.Project(projectID); err != nil {
		return nil, err
	}
	number, err := projectNumberOf(projectID)
	if err != nil {
		return nil, err
	}
	rows, err := store.database.Query(`SELECT id, project_number, field, old_value, new_value, changed_by, changed_at FROM project_changes WHERE project_number = $1 ORDER BY id`, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []Change{}
	for rows.Next() {
		var change Change
		var projectNumber int64
		if err := rows.Scan(&change.ID, &projectNumber, &change.Field, &change.OldValue, &change.NewValue, &change.ChangedBy, &change.ChangedAt); err != nil {
			return nil, err
		}
		change.ProjectID = formatProjectID(projectNumber)
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

const linkColumns = `id, project_number, entity_type, entity_ref, label, note, created_by, created_at`

func scanLink(row scanner) (Link, error) {
	var link Link
	var projectNumber int64
	if err := row.Scan(&link.ID, &projectNumber, &link.EntityType, &link.EntityRef, &link.Label, &link.Note, &link.CreatedBy, &link.CreatedAt); err != nil {
		return Link{}, err
	}
	link.ProjectID = formatProjectID(projectNumber)
	return link, nil
}

// insertLink writes a link unless the project owns the entity already, and
// says whether it wrote one. It never fails on the duplicate, so it leaves a
// transaction it runs in usable.
func insertLink(handle queryer, link Link) (Link, bool, error) {
	number, err := projectNumberOf(link.ProjectID)
	if err != nil {
		return Link{}, false, err
	}
	err = handle.QueryRow(`INSERT INTO project_links (project_number, entity_type, entity_ref, label, note, created_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (project_number, entity_type, entity_ref) DO NOTHING RETURNING id`,
		number, link.EntityType, link.EntityRef, link.Label, link.Note, link.CreatedBy, link.CreatedAt).Scan(&link.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return Link{}, false, nil
	}
	if err != nil {
		return Link{}, false, err
	}
	return link, true, nil
}

// AddLink records that a project owns an entity. The caller has checked the
// entity with its owner and passes the owner's name for it as label.
func (store *Store) AddLink(link Link) (Link, error) {
	if _, err := store.Project(link.ProjectID); err != nil {
		return Link{}, err
	}
	link.CreatedAt = store.now().Unix()
	created, inserted, err := insertLink(store.database, link)
	if err != nil {
		return Link{}, err
	}
	if !inserted {
		return Link{}, fmt.Errorf("%w: %s already owns %s %s", ErrConflict, link.ProjectID, link.EntityType, link.EntityRef)
	}
	return created, nil
}

// DeleteLink removes one link of a project.
func (store *Store) DeleteLink(projectID string, linkID int64) error {
	number, ok := recordNumberOf(ProjectIDPrefix, projectID)
	if !ok {
		return fmt.Errorf("link %d of %s: %w", linkID, projectID, ErrNotFound)
	}
	result, err := store.database.Exec(`DELETE FROM project_links WHERE id = $1 AND project_number = $2`, linkID, number)
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
	placeholder := func(value any) string {
		arguments = append(arguments, value)
		return "$" + strconv.Itoa(len(arguments))
	}
	if filter.ProjectID != "" {
		number, ok := recordNumberOf(ProjectIDPrefix, filter.ProjectID)
		if !ok {
			return []Link{}, nil
		}
		conditions = append(conditions, "project_number = "+placeholder(number))
	}
	if filter.EntityType != "" {
		conditions = append(conditions, "entity_type = "+placeholder(filter.EntityType))
	}
	if filter.EntityRef != "" {
		conditions = append(conditions, "entity_ref = "+placeholder(filter.EntityRef))
	}
	query := `SELECT ` + linkColumns + ` FROM project_links`
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
		link, err := scanLink(rows)
		if err != nil {
			return nil, err
		}
		links = append(links, link)
	}
	return links, rows.Err()
}
