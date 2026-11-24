package projectstore

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

// GoalIDPrefix is the type part of a goal's public id: goal_000001.
const GoalIDPrefix = "goal"

func formatGoalID(number int64) string {
	return postgresstorage.FormatRecordID(GoalIDPrefix, number)
}

// goalIDOf is the public id of a stored reference to a goal; "" for NULL.
func goalIDOf(number sql.NullInt64) string {
	if !number.Valid {
		return ""
	}
	return formatGoalID(number.Int64)
}

// A goal is something a project wants that finishes: it is reached or
// dropped. A sub-project is a lasting area with its own repos; a goal has no
// repos, only the work that serves it (goal_links).

// States of a goal.
const (
	// GoalProposed: a model or an agent suggested it and nobody accepted it yet.
	GoalProposed = "proposed"
	GoalOpen     = "open"
	GoalReached  = "reached"
	GoalDropped  = "dropped"
	// GoalDismissed: a proposal a person turned down.
	GoalDismissed = "dismissed"
)

// GoalStates lists every state of a goal.
var GoalStates = []string{GoalProposed, GoalOpen, GoalReached, GoalDropped, GoalDismissed}

// DefaultGoalState is a new goal's state when none is given.
const DefaultGoalState = GoalOpen

// goalStatesANewGoalMayHave are the states a caller may create a goal in.
var goalStatesANewGoalMayHave = []string{GoalOpen, GoalProposed}

// goalStateMoves lists, for each state, the states a goal may move to from it.
var goalStateMoves = map[string][]string{
	GoalProposed:  {GoalOpen, GoalDismissed},
	GoalOpen:      {GoalReached, GoalDropped},
	GoalReached:   {GoalOpen},
	GoalDropped:   {GoalOpen},
	GoalDismissed: {},
}

// goalStateIsClosed says whether a goal in the state carries closed_by,
// closed_at and closed_because.
func goalStateIsClosed(state string) bool {
	return state == GoalReached || state == GoalDropped || state == GoalDismissed
}

// GoalLinkEntityTypes lists what can serve a goal.
var GoalLinkEntityTypes = []EntityTypeInfo{
	{EntityNote, "noteboard", "A noteboard item that serves the goal, usually a todo: a step toward it; the item's uuid."},
	{EntitySession, "llm-bridge-server", "A session that works toward the goal; llm-bridge-server's id."},
}

func goalLinkEntityTypeKnown(entityType string) bool {
	for _, info := range GoalLinkEntityTypes {
		if info.Type == entityType {
			return true
		}
	}
	return false
}

// Goal is a goal's own fields.
type Goal struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	// ParentGoalID is "" for a goal at the top of its project.
	ParentGoalID string `json:"parent_goal_id"`
	Statement    string `json:"statement"`
	DoneWhen     string `json:"done_when"`
	State        string `json:"state"`
	// Priority is as noteboard's: a higher number is more urgent.
	Priority int `json:"priority"`
	// TargetAt is unix seconds; 0 when no date is set.
	TargetAt  int64  `json:"target_at"`
	CreatedBy string `json:"created_by"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// ClosedBy, ClosedAt and ClosedBecause are set while the goal is reached,
	// dropped or dismissed.
	ClosedBy      string `json:"closed_by"`
	ClosedAt      int64  `json:"closed_at"`
	ClosedBecause string `json:"closed_because"`
}

// GoalLink is one thing that serves a goal.
type GoalLink struct {
	ID         int64  `json:"id"`
	GoalID     string `json:"goal_id"`
	EntityType string `json:"entity_type"`
	EntityRef  string `json:"entity_ref"`
	Label      string `json:"label"`
	Note       string `json:"note"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  int64  `json:"created_at"`
}

// GoalChange is one recorded change to a goal's field.
type GoalChange struct {
	ID        int64  `json:"id"`
	GoalID    string `json:"goal_id"`
	Field     string `json:"field"`
	OldValue  string `json:"old_value"`
	NewValue  string `json:"new_value"`
	ChangedBy string `json:"changed_by"`
	ChangedAt int64  `json:"changed_at"`
}

const goalColumns = `number, project_number, parent_goal_number, statement, done_when, state, priority, target_at, created_by, created_at, updated_at, closed_by, closed_at, closed_because`

func scanGoal(row scanner) (Goal, error) {
	var goal Goal
	var number, projectNumber int64
	var parent sql.NullInt64
	err := row.Scan(&number, &projectNumber, &parent, &goal.Statement, &goal.DoneWhen, &goal.State, &goal.Priority, &goal.TargetAt,
		&goal.CreatedBy, &goal.CreatedAt, &goal.UpdatedAt, &goal.ClosedBy, &goal.ClosedAt, &goal.ClosedBecause)
	if err != nil {
		return Goal{}, err
	}
	goal.ID = formatGoalID(number)
	goal.ProjectID = formatProjectID(projectNumber)
	goal.ParentGoalID = goalIDOf(parent)
	return goal, nil
}

// goalOn reads one goal; forUpdate locks its row until the transaction ends.
func goalOn(handle queryer, id string, forUpdate bool) (Goal, error) {
	number, ok := recordNumberOf(GoalIDPrefix, id)
	if !ok {
		return Goal{}, fmt.Errorf("goal %s: %w", id, ErrNotFound)
	}
	query := `SELECT ` + goalColumns + ` FROM goals WHERE number = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	goal, err := scanGoal(handle.QueryRow(query, number))
	if errors.Is(err, sql.ErrNoRows) {
		return Goal{}, fmt.Errorf("goal %s: %w", id, ErrNotFound)
	}
	return goal, err
}

// nullableGoalNumber is a stored reference to a goal: NULL for "".
func nullableGoalNumber(id string) (any, error) {
	if id == "" {
		return nil, nil
	}
	number, ok := recordNumberOf(GoalIDPrefix, id)
	if !ok {
		return nil, fmt.Errorf("%w: parent_goal_id %s names no goal", ErrInvalid, id)
	}
	return number, nil
}

// GoalFields are the fields a caller sets on create or patch. A nil field is
// left as it is (on create: its default).
type GoalFields struct {
	ParentGoalID  *string
	Statement     *string
	DoneWhen      *string
	State         *string
	Priority      *int
	TargetAt      *int64
	ClosedBecause *string
}

// validateGoalFields checks the fields that do not depend on the goal's
// current state. goalID is "" on create.
func validateGoalFields(transaction *sql.Tx, goalID, projectID string, fields GoalFields) error {
	if fields.Statement != nil && strings.TrimSpace(*fields.Statement) == "" {
		return fmt.Errorf("%w: statement must not be empty", ErrInvalid)
	}
	if fields.State != nil && !contains(GoalStates, *fields.State) {
		return fmt.Errorf("%w: state %q is not one of %s", ErrInvalid, *fields.State, strings.Join(GoalStates, ", "))
	}
	if fields.TargetAt != nil && *fields.TargetAt < 0 {
		return fmt.Errorf("%w: target_at must be unix seconds, or 0 for no date", ErrInvalid)
	}
	if fields.ParentGoalID != nil && *fields.ParentGoalID != "" {
		// The parent must be a goal of the same project, and must not be the
		// goal or below it.
		for ancestor := *fields.ParentGoalID; ancestor != ""; {
			if ancestor == goalID {
				return fmt.Errorf("%w: parent_goal_id %s would make the goal its own ancestor", ErrInvalid, *fields.ParentGoalID)
			}
			ancestorNumber, ok := recordNumberOf(GoalIDPrefix, ancestor)
			if !ok {
				return fmt.Errorf("%w: parent_goal_id %s names no goal", ErrInvalid, ancestor)
			}
			var next sql.NullInt64
			var ancestorProjectNumber int64
			if err := transaction.QueryRow(`SELECT parent_goal_number, project_number FROM goals WHERE number = $1`, ancestorNumber).Scan(&next, &ancestorProjectNumber); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("%w: parent_goal_id %s names no goal", ErrInvalid, ancestor)
				}
				return err
			}
			if ancestorProjectID := formatProjectID(ancestorProjectNumber); ancestorProjectID != projectID {
				return fmt.Errorf("%w: parent_goal_id %s is a goal of %s, not of %s", ErrInvalid, ancestor, ancestorProjectID, projectID)
			}
			ancestor = goalIDOf(next)
		}
	}
	return nil
}

// CreateGoal creates a goal of a project. Statement is required; state
// defaults to open and may be proposed.
func (store *Store) CreateGoal(projectID string, fields GoalFields, createdBy string) (Goal, error) {
	project, err := store.Project(projectID)
	if err != nil {
		return Goal{}, err
	}
	if project.ArchivedAt != 0 {
		return Goal{}, fmt.Errorf("%w: %s is archived, so it takes no new goal", ErrInvalid, projectID)
	}
	if fields.Statement == nil {
		return Goal{}, fmt.Errorf("%w: statement is required", ErrInvalid)
	}
	if fields.ClosedBecause != nil {
		return Goal{}, fmt.Errorf("%w: closed_because is taken only when a goal moves to reached, dropped or dismissed", ErrInvalid)
	}
	state := DefaultGoalState
	if fields.State != nil {
		state = *fields.State
	}
	projectNumber, err := projectNumberOf(projectID)
	if err != nil {
		return Goal{}, err
	}
	var number int64
	err = store.inTransaction(func(transaction *sql.Tx) error {
		if err := validateGoalFields(transaction, "", projectID, fields); err != nil {
			return err
		}
		if !contains(goalStatesANewGoalMayHave, state) {
			return fmt.Errorf("%w: a new goal is %s, not %s", ErrInvalid, strings.Join(goalStatesANewGoalMayHave, " or "), state)
		}
		now := store.now().Unix()
		var parent any
		if fields.ParentGoalID != nil {
			if parent, err = nullableGoalNumber(*fields.ParentGoalID); err != nil {
				return err
			}
		}
		doneWhen, priority, targetAt := "", 0, int64(0)
		if fields.DoneWhen != nil {
			doneWhen = *fields.DoneWhen
		}
		if fields.Priority != nil {
			priority = *fields.Priority
		}
		if fields.TargetAt != nil {
			targetAt = *fields.TargetAt
		}
		return transaction.QueryRow(`INSERT INTO goals (project_number, parent_goal_number, statement, done_when, state, priority, target_at, created_by, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10) RETURNING number`,
			projectNumber, parent, strings.TrimSpace(*fields.Statement), doneWhen, state, priority, targetAt, createdBy, now, now).Scan(&number)
	})
	if err != nil {
		return Goal{}, err
	}
	return store.Goal(formatGoalID(number))
}

// Goal returns one goal.
func (store *Store) Goal(id string) (Goal, error) {
	return goalOn(store.database, id, false)
}

// GoalFilter narrows Goals. An empty ProjectID lists every project's goals.
// States lists the states wanted; empty means every state.
type GoalFilter struct {
	ProjectID string
	States    []string
}

// goalStatesListedByDefault are the states a list shows when the caller names
// none: every state but dismissed.
var goalStatesListedByDefault = []string{GoalProposed, GoalOpen, GoalReached, GoalDropped}

// Goals lists goals, each project's together, parents before their children.
func (store *Store) Goals(filter GoalFilter) ([]Goal, error) {
	var conditions []string
	var arguments []any
	placeholder := func(value any) string {
		arguments = append(arguments, value)
		return "$" + strconv.Itoa(len(arguments))
	}
	if filter.ProjectID != "" {
		projectNumber, ok := recordNumberOf(ProjectIDPrefix, filter.ProjectID)
		if !ok {
			return []Goal{}, nil
		}
		conditions = append(conditions, "project_number = "+placeholder(projectNumber))
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, 0, len(filter.States))
		for _, state := range filter.States {
			placeholders = append(placeholders, placeholder(state))
		}
		conditions = append(conditions, "state IN ("+strings.Join(placeholders, ", ")+")")
	}
	query := `SELECT ` + goalColumns + ` FROM goals`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := store.database.Query(query+" ORDER BY project_number, number", arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	goals := []Goal{}
	for rows.Next() {
		goal, err := scanGoal(rows)
		if err != nil {
			return nil, err
		}
		goals = append(goals, goal)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return goalsParentsBeforeChildren(goals), nil
}

// goalsParentsBeforeChildren orders goals project by project, each parent
// directly before its children, the more urgent sibling first and then the
// older. A goal whose parent is not in the list stands at the top of its
// project.
func goalsParentsBeforeChildren(goals []Goal) []Goal {
	listed := map[string]bool{}
	for _, goal := range goals {
		listed[goal.ID] = true
	}
	const top = ""
	children := map[string][]Goal{}
	var projectIDs []string
	seenProject := map[string]bool{}
	for _, goal := range goals {
		parent := top
		if goal.ParentGoalID != "" && listed[goal.ParentGoalID] {
			parent = goal.ParentGoalID
		}
		key := goal.ProjectID + "\x00" + parent
		children[key] = append(children[key], goal)
		if !seenProject[goal.ProjectID] {
			seenProject[goal.ProjectID] = true
			projectIDs = append(projectIDs, goal.ProjectID)
		}
	}
	sort.Strings(projectIDs)
	ordered := make([]Goal, 0, len(goals))
	var write func(projectID, parent string)
	write = func(projectID, parent string) {
		siblings := children[projectID+"\x00"+parent]
		sort.SliceStable(siblings, func(left, right int) bool {
			if siblings[left].Priority != siblings[right].Priority {
				return siblings[left].Priority > siblings[right].Priority
			}
			return siblings[left].ID < siblings[right].ID
		})
		for _, goal := range siblings {
			ordered = append(ordered, goal)
			write(projectID, goal.ID)
		}
	}
	for _, projectID := range projectIDs {
		write(projectID, top)
	}
	return ordered
}

// UpdateGoal changes the given fields and records each change. A state move
// outside goalStateMoves is ErrConflict. A move to reached, dropped or
// dismissed needs closed_because and changedBy; a reopen clears the closed
// fields.
func (store *Store) UpdateGoal(id string, fields GoalFields, changedBy string) (Goal, error) {
	err := store.inTransaction(func(transaction *sql.Tx) error {
		return store.updateGoal(transaction, id, fields, changedBy)
	})
	if err != nil {
		return Goal{}, err
	}
	return store.Goal(id)
}

// updateGoal is UpdateGoal inside its transaction, with the goal's row locked.
func (store *Store) updateGoal(transaction *sql.Tx, id string, fields GoalFields, changedBy string) error {
	current, err := goalOn(transaction, id, true)
	if err != nil {
		return err
	}
	number, _ := recordNumberOf(GoalIDPrefix, id)
	if err := validateGoalFields(transaction, id, current.ProjectID, fields); err != nil {
		return err
	}
	stateMoves := fields.State != nil && *fields.State != current.State
	closes := stateMoves && goalStateIsClosed(*fields.State)
	if stateMoves && !contains(goalStateMoves[current.State], *fields.State) {
		return fmt.Errorf("%w: a %s goal cannot become %s; from %s it may become: %s", ErrConflict, current.State, *fields.State, current.State,
			strings.Join(goalStateMoves[current.State], ", "))
	}
	if closes {
		if fields.ClosedBecause == nil || strings.TrimSpace(*fields.ClosedBecause) == "" {
			return fmt.Errorf("%w: closed_because is required to move a goal to %s", ErrInvalid, *fields.State)
		}
		if strings.TrimSpace(changedBy) == "" {
			return fmt.Errorf("%w: changed_by is required to move a goal to %s", ErrInvalid, *fields.State)
		}
	} else if fields.ClosedBecause != nil {
		return fmt.Errorf("%w: closed_because is taken only when a goal moves to reached, dropped or dismissed", ErrInvalid)
	}

	now := store.now().Unix()
	changed := false
	record := func(field, oldValue, newValue string, value any) error {
		column := field
		if field == "parent_goal_id" {
			column = "parent_goal_number"
		}
		if oldValue == newValue {
			return nil
		}
		changed = true
		if _, err := transaction.Exec(`UPDATE goals SET `+column+` = $1 WHERE number = $2`, value, number); err != nil {
			return err
		}
		_, err := transaction.Exec(`INSERT INTO goal_changes (goal_number, field, old_value, new_value, changed_by, changed_at) VALUES ($1, $2, $3, $4, $5, $6)`,
			number, field, oldValue, newValue, changedBy, now)
		return err
	}
	if fields.Statement != nil {
		next := strings.TrimSpace(*fields.Statement)
		if err := record("statement", current.Statement, next, next); err != nil {
			return err
		}
	}
	if fields.DoneWhen != nil {
		if err := record("done_when", current.DoneWhen, *fields.DoneWhen, *fields.DoneWhen); err != nil {
			return err
		}
	}
	if fields.ParentGoalID != nil {
		value, err := nullableGoalNumber(*fields.ParentGoalID)
		if err != nil {
			return err
		}
		if err := record("parent_goal_id", current.ParentGoalID, *fields.ParentGoalID, value); err != nil {
			return err
		}
	}
	if fields.Priority != nil {
		if err := record("priority", strconv.Itoa(current.Priority), strconv.Itoa(*fields.Priority), *fields.Priority); err != nil {
			return err
		}
	}
	if fields.TargetAt != nil {
		if err := record("target_at", strconv.FormatInt(current.TargetAt, 10), strconv.FormatInt(*fields.TargetAt, 10), *fields.TargetAt); err != nil {
			return err
		}
	}
	if stateMoves {
		if err := record("state", current.State, *fields.State, *fields.State); err != nil {
			return err
		}
		// closed_by and closed_at are the state change's changed_by and
		// changed_at, so only the reason is recorded as a change of its own.
		closedBy, closedAt, closedBecause := "", int64(0), ""
		if closes {
			closedBy, closedAt, closedBecause = strings.TrimSpace(changedBy), now, strings.TrimSpace(*fields.ClosedBecause)
		}
		if err := record("closed_because", current.ClosedBecause, closedBecause, closedBecause); err != nil {
			return err
		}
		if _, err := transaction.Exec(`UPDATE goals SET closed_by = $1, closed_at = $2 WHERE number = $3`, closedBy, closedAt, number); err != nil {
			return err
		}
	}
	if changed {
		if _, err := transaction.Exec(`UPDATE goals SET updated_at = $1 WHERE number = $2`, now, number); err != nil {
			return err
		}
	}
	return nil
}

// GoalChanges lists a goal's recorded changes, oldest first.
func (store *Store) GoalChanges(goalID string) ([]GoalChange, error) {
	if _, err := store.Goal(goalID); err != nil {
		return nil, err
	}
	number, _ := recordNumberOf(GoalIDPrefix, goalID)
	return store.goalChangesWhere(`goal_number = $1`, number)
}

// goalChangesOfProject lists the recorded changes of every goal of a project,
// oldest first.
func (store *Store) goalChangesOfProject(projectID string) ([]GoalChange, error) {
	projectNumber, ok := recordNumberOf(ProjectIDPrefix, projectID)
	if !ok {
		return []GoalChange{}, nil
	}
	return store.goalChangesWhere(`goal_number IN (SELECT number FROM goals WHERE project_number = $1)`, projectNumber)
}

func (store *Store) goalChangesWhere(condition string, argument any) ([]GoalChange, error) {
	rows, err := store.database.Query(`SELECT id, goal_number, field, old_value, new_value, changed_by, changed_at FROM goal_changes WHERE `+condition+` ORDER BY id`, argument)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	changes := []GoalChange{}
	for rows.Next() {
		var change GoalChange
		var goalNumber int64
		if err := rows.Scan(&change.ID, &goalNumber, &change.Field, &change.OldValue, &change.NewValue, &change.ChangedBy, &change.ChangedAt); err != nil {
			return nil, err
		}
		change.GoalID = formatGoalID(goalNumber)
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

// AddGoalLink records that an entity serves a goal. The caller has checked
// the entity with its owner and passes the owner's name for it as label.
func (store *Store) AddGoalLink(link GoalLink) (GoalLink, error) {
	if _, err := store.Goal(link.GoalID); err != nil {
		return GoalLink{}, err
	}
	link.CreatedAt = store.now().Unix()
	number, _ := recordNumberOf(GoalIDPrefix, link.GoalID)
	err := store.database.QueryRow(`INSERT INTO goal_links (goal_number, entity_type, entity_ref, label, note, created_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (goal_number, entity_type, entity_ref) DO NOTHING RETURNING id`,
		number, link.EntityType, link.EntityRef, link.Label, link.Note, link.CreatedBy, link.CreatedAt).Scan(&link.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return GoalLink{}, fmt.Errorf("%w: %s is already served by %s %s", ErrConflict, link.GoalID, link.EntityType, link.EntityRef)
	}
	if err != nil {
		return GoalLink{}, err
	}
	return link, nil
}

// DeleteGoalLink removes one link of a goal.
func (store *Store) DeleteGoalLink(goalID string, linkID int64) error {
	number, ok := recordNumberOf(GoalIDPrefix, goalID)
	if !ok {
		return fmt.Errorf("link %d of %s: %w", linkID, goalID, ErrNotFound)
	}
	result, err := store.database.Exec(`DELETE FROM goal_links WHERE id = $1 AND goal_number = $2`, linkID, number)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return fmt.Errorf("link %d of %s: %w", linkID, goalID, ErrNotFound)
	}
	return nil
}

// GoalLinkFilter narrows GoalLinks. Zero fields do not filter.
type GoalLinkFilter struct {
	GoalID string
	// ProjectID keeps the links of that project's goals.
	ProjectID  string
	EntityType string
	EntityRef  string
}

// GoalLinks lists goal links, oldest first.
func (store *Store) GoalLinks(filter GoalLinkFilter) ([]GoalLink, error) {
	var conditions []string
	var arguments []any
	placeholder := func(value any) string {
		arguments = append(arguments, value)
		return "$" + strconv.Itoa(len(arguments))
	}
	links := []GoalLink{}
	if filter.GoalID != "" {
		number, ok := recordNumberOf(GoalIDPrefix, filter.GoalID)
		if !ok {
			return links, nil
		}
		conditions = append(conditions, "goal_number = "+placeholder(number))
	}
	if filter.ProjectID != "" {
		projectNumber, ok := recordNumberOf(ProjectIDPrefix, filter.ProjectID)
		if !ok {
			return links, nil
		}
		conditions = append(conditions, "goal_number IN (SELECT number FROM goals WHERE project_number = "+placeholder(projectNumber)+")")
	}
	if filter.EntityType != "" {
		conditions = append(conditions, "entity_type = "+placeholder(filter.EntityType))
	}
	if filter.EntityRef != "" {
		conditions = append(conditions, "entity_ref = "+placeholder(filter.EntityRef))
	}
	query := `SELECT id, goal_number, entity_type, entity_ref, label, note, created_by, created_at FROM goal_links`
	if len(conditions) > 0 {
		query += " WHERE " + strings.Join(conditions, " AND ")
	}
	rows, err := store.database.Query(query+" ORDER BY id", arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var link GoalLink
		var goalNumber int64
		if err := rows.Scan(&link.ID, &goalNumber, &link.EntityType, &link.EntityRef, &link.Label, &link.Note, &link.CreatedBy, &link.CreatedAt); err != nil {
			return nil, err
		}
		link.GoalID = formatGoalID(goalNumber)
		links = append(links, link)
	}
	return links, rows.Err()
}
