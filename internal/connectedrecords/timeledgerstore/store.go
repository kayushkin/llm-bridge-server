// Package timeledgerstore keeps an account of what the operator did: time
// entries over spans of time, nested and overlapping, each linked by id to the
// records that brought it in and the records that show whether it happened,
// and reviewed by a model and by the operator.
//
// Entries, kinds and reviews are cited elsewhere by their ids
// (time_entry_000001, time_entry_kind_000001, time_entry_review_000001),
// which kanban-store's registry claims, so a note or a timeline can link one
// as a chip.
package timeledgerstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// Actor is who does something, and what kind of actor they are.
type Actor struct {
	By   string `json:"by"`
	Kind string `json:"by_kind"`
}

func (actor Actor) validate() error {
	if strings.TrimSpace(actor.By) == "" {
		return fmt.Errorf("%w: by is required: who is doing this", ErrInvalid)
	}
	if _, known := actorRank[actor.Kind]; !known {
		return fmt.Errorf("%w: by_kind %q is not one of %v", ErrInvalid, actor.Kind, ActorKinds)
	}
	return nil
}

// mayReplace reports whether actor may change a value last set by a kind.
func (actor Actor) mayReplace(setByKind string) bool {
	return actorRank[actor.Kind] >= actorRank[setByKind]
}

// Kind is a label an entry can carry; an entry carries any number.
type Kind struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	CreatedBy     string `json:"created_by"`
	CreatedByKind string `json:"created_by_kind"`
	CreatedAt     int64  `json:"created_at"`
}

// KindAssignment is one kind on one entry.
type KindAssignment struct {
	KindID         string `json:"kind_id"`
	Name           string `json:"name"`
	AssignedBy     string `json:"assigned_by"`
	AssignedByKind string `json:"assigned_by_kind"`
	Reason         string `json:"reason"`
	AssignedAt     int64  `json:"assigned_at"`
}

// Link is one record linked to an entry.
type Link struct {
	LinkType           string `json:"link_type"`
	TimeEntryID        string `json:"time_entry_id"`
	Ref                string `json:"ref"`
	OccurrenceStartsAt *int64 `json:"occurrence_starts_at,omitempty"`
	OverlapSeconds     *int64 `json:"overlap_seconds,omitempty"`
	AmountCents        *int64 `json:"amount_cents,omitempty"`
	BearsOn            string `json:"bears_on"`
	LinkedBy           string `json:"linked_by"`
	LinkedByKind       string `json:"linked_by_kind"`
	Reason             string `json:"reason"`
	LinkedAt           int64  `json:"linked_at"`
	RemovedAt          *int64 `json:"removed_at,omitempty"`
	RemovedBy          string `json:"removed_by,omitempty"`
	RemovedByKind      string `json:"removed_by_kind,omitempty"`
}

// Review is one judgment of whether an entry happened.
type Review struct {
	ID                         string          `json:"id"`
	TimeEntryID                string          `json:"time_entry_id"`
	Outcome                    string          `json:"outcome"`
	Reason                     string          `json:"reason"`
	ReviewedBy                 string          `json:"reviewed_by"`
	ReviewedByKind             string          `json:"reviewed_by_kind"`
	Model                      string          `json:"model"`
	InputTokens                int64           `json:"input_tokens"`
	OutputTokens               int64           `json:"output_tokens"`
	CostUSD                    float64         `json:"cost_usd"`
	LinksSeen                  json.RawMessage `json:"links_seen"`
	SuggestedParentTimeEntryID string          `json:"suggested_parent_time_entry_id"`
	SuggestedKindNames         []string        `json:"suggested_kind_names"`
	CreatedAt                  int64           `json:"created_at"`
}

// TimeEntry is one thing done, or planned and skipped, over a span of time.
type TimeEntry struct {
	ID                string           `json:"id"`
	Title             string           `json:"title"`
	TitleBy           string           `json:"title_by"`
	TitleByKind       string           `json:"title_by_kind"`
	StartsAt          int64            `json:"starts_at"`
	EndsAt            int64            `json:"ends_at"`
	AllDay            bool             `json:"all_day"`
	StartDate         string           `json:"start_date"`
	EndDate           string           `json:"end_date"`
	TimeZone          string           `json:"time_zone"`
	SpanBy            string           `json:"span_by"`
	SpanByKind        string           `json:"span_by_kind"`
	Place             string           `json:"place"`
	PlaceBy           string           `json:"place_by"`
	PlaceByKind       string           `json:"place_by_kind"`
	ParentTimeEntryID string           `json:"parent_time_entry_id"`
	ParentSetBy       string           `json:"parent_set_by"`
	ParentSetByKind   string           `json:"parent_set_by_kind"`
	CreatedBy         string           `json:"created_by"`
	CreatedByKind     string           `json:"created_by_kind"`
	CreatedReason     string           `json:"created_reason"`
	CreatedAt         int64            `json:"created_at"`
	UpdatedAt         int64            `json:"updated_at"`
	EvidenceChangedAt int64            `json:"evidence_changed_at"`
	DeletedAt         *int64           `json:"deleted_at,omitempty"`
	DeletedBy         string           `json:"deleted_by,omitempty"`
	Outcome           string           `json:"outcome"`
	OutcomeReviewID   string           `json:"outcome_review_id"`
	Kinds             []KindAssignment `json:"kinds"`
	Links             []Link           `json:"links,omitempty"`
	ChildIDs          []string         `json:"child_ids,omitempty"`
	Reviews           []Review         `json:"reviews,omitempty"`
}

// Store is the SQLite database behind the service.
type Store struct {
	db      *sql.DB
	dataDir string
	mu      sync.Mutex
	now     func() time.Time
}

// Open opens or creates time-ledger.db in dataDir.
func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", filepath.Join(dataDir, "time-ledger.db")+"?_foreign_keys=on&_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := seedKinds(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is where the database lives.
func (s *Store) DataDir() string { return s.dataDir }

// SeedKinds are the kinds a new database starts with, from the operator's own
// calendar: anyone may add more with POST /kinds.
var SeedKinds = []Kind{
	{Name: "journey", Description: "time away from home: more than 80 km away, or a flight"},
	{Name: "routine", Description: "something done again and again on a schedule"},
	{Name: "appointment", Description: "a booked slot with a provider: haircut, dentist, doctor, telehealth"},
	{Name: "class", Description: "a lesson or course with a teacher"},
	{Name: "dance", Description: "dancing: a class, a social, a festival"},
	{Name: "social", Description: "time with friends or family: a birthday, a dinner, a game night"},
	{Name: "show", Description: "a concert, a play, a performance watched"},
	{Name: "sport", Description: "exercise or a sport: workout, skating, pickleball"},
	{Name: "work", Description: "paid work or work on a project"},
	{Name: "conference", Description: "a conference, a meetup, a talk, a tech week"},
	{Name: "errand", Description: "a task out of the house: a pickup, a drop-off, a repair"},
	{Name: "health", Description: "care for body or mind: a checkup, a vaccine, therapy"},
}

func seedKinds(db *sql.DB) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM time_entry_kinds`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	now := time.Now().Unix()
	for _, kind := range SeedKinds {
		if _, err := insertKind(db, kind.Name, kind.Description, Actor{By: "time-ledger-store seed", Kind: ActorProgram}, now); err != nil {
			return err
		}
	}
	return nil
}

type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// assignID gives the row just inserted its public id, from its seq.
func assignID(tx execer, table, prefix string, result sql.Result) (string, error) {
	seq, err := result.LastInsertId()
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("%s_%06d", prefix, seq)
	if _, err := tx.Exec(`UPDATE `+table+` SET id = ? WHERE seq = ?`, id, seq); err != nil {
		return "", err
	}
	return id, nil
}

var kindNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*( [a-z0-9-]+)*$`)

func insertKind(tx execer, name, description string, actor Actor, now int64) (string, error) {
	result, err := tx.Exec(`INSERT INTO time_entry_kinds (id, name, description, created_by, created_by_kind, created_at)
		VALUES ('pending-' || hex(randomblob(8)), ?, ?, ?, ?, ?)`, name, description, actor.By, actor.Kind, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", fmt.Errorf("%w: a kind named %q exists", ErrConflict, name)
		}
		return "", err
	}
	return assignID(tx, "time_entry_kinds", "time_entry_kind", result)
}

// CreateKind adds a kind.
func (s *Store) CreateKind(name, description string, actor Actor) (Kind, error) {
	if err := actor.validate(); err != nil {
		return Kind{}, err
	}
	name = strings.TrimSpace(name)
	if !kindNamePattern.MatchString(name) {
		return Kind{}, fmt.Errorf("%w: a kind name is lower-case words, like %q", ErrInvalid, "tech meetup")
	}
	if strings.TrimSpace(description) == "" {
		return Kind{}, fmt.Errorf("%w: description is required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := insertKind(s.db, name, description, actor, s.now().Unix())
	if err != nil {
		return Kind{}, err
	}
	return s.Kind(id)
}

const kindColumns = `id, name, description, created_by, created_by_kind, created_at`

func scanKind(row interface{ Scan(...any) error }) (Kind, error) {
	var kind Kind
	err := row.Scan(&kind.ID, &kind.Name, &kind.Description, &kind.CreatedBy, &kind.CreatedByKind, &kind.CreatedAt)
	return kind, err
}

// Kind reads one kind.
func (s *Store) Kind(id string) (Kind, error) {
	kind, err := scanKind(s.db.QueryRow(`SELECT `+kindColumns+` FROM time_entry_kinds WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Kind{}, fmt.Errorf("%w: kind %s", ErrNotFound, id)
	}
	return kind, err
}

// Kinds lists every kind by name.
func (s *Store) Kinds() ([]Kind, error) {
	rows, err := s.db.Query(`SELECT ` + kindColumns + ` FROM time_entry_kinds ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	kinds := []Kind{}
	for rows.Next() {
		kind, err := scanKind(rows)
		if err != nil {
			return nil, err
		}
		kinds = append(kinds, kind)
	}
	return kinds, rows.Err()
}

// EntryInput is what POST /time-entries takes.
type EntryInput struct {
	Title             string      `json:"title"`
	StartsAt          int64       `json:"starts_at"`
	EndsAt            int64       `json:"ends_at"`
	AllDay            bool        `json:"all_day"`
	StartDate         string      `json:"start_date"`
	EndDate           string      `json:"end_date"`
	TimeZone          string      `json:"time_zone"`
	Place             string      `json:"place"`
	ParentTimeEntryID string      `json:"parent_time_entry_id"`
	KindIDs           []string    `json:"kind_ids"`
	Links             []LinkInput `json:"links"`
	By                string      `json:"by"`
	ByKind            string      `json:"by_kind"`
	Reason            string      `json:"reason"`
}

// LinkInput is one link to make: in POST /time-entries' links, or alone.
type LinkInput struct {
	LinkType           string `json:"link_type"`
	Ref                string `json:"ref"`
	OccurrenceStartsAt *int64 `json:"occurrence_starts_at"`
	OverlapSeconds     *int64 `json:"overlap_seconds"`
	AmountCents        *int64 `json:"amount_cents"`
	BearsOn            string `json:"bears_on"`
	Reason             string `json:"reason"`
}

var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func validateSpan(startsAt, endsAt int64, allDay bool, startDate, endDate, timeZone string) error {
	if _, err := time.LoadLocation(timeZone); err != nil || timeZone == "" {
		return fmt.Errorf("%w: time_zone %q is not an IANA zone", ErrInvalid, timeZone)
	}
	if startsAt <= 0 || endsAt < startsAt {
		return fmt.Errorf("%w: starts_at must be set and ends_at may not be before it", ErrInvalid)
	}
	if allDay {
		if !datePattern.MatchString(startDate) || !datePattern.MatchString(endDate) || endDate <= startDate {
			return fmt.Errorf("%w: an all-day entry needs start_date and end_date (exclusive), YYYY-MM-DD", ErrInvalid)
		}
	} else if startDate != "" || endDate != "" {
		return fmt.Errorf("%w: start_date and end_date are for all-day entries", ErrInvalid)
	}
	return nil
}

// CreateEntry makes an entry with its kinds and links in one transaction. A
// source link that already belongs to another entry is a conflict naming it.
func (s *Store) CreateEntry(input EntryInput) (TimeEntry, error) {
	actor := Actor{By: input.By, Kind: input.ByKind}
	if err := actor.validate(); err != nil {
		return TimeEntry{}, err
	}
	input.Title = strings.TrimSpace(input.Title)
	if input.Title == "" {
		return TimeEntry{}, fmt.Errorf("%w: title is required", ErrInvalid)
	}
	if strings.TrimSpace(input.Reason) == "" {
		return TimeEntry{}, fmt.Errorf("%w: reason is required: why this entry exists", ErrInvalid)
	}
	if err := validateSpan(input.StartsAt, input.EndsAt, input.AllDay, input.StartDate, input.EndDate, input.TimeZone); err != nil {
		return TimeEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return TimeEntry{}, err
	}
	defer tx.Rollback()
	now := s.now().Unix()
	parentSetBy, parentSetByKind := "", ""
	if input.ParentTimeEntryID != "" {
		if err := checkParent(tx, "", input.ParentTimeEntryID, input.StartsAt, input.EndsAt); err != nil {
			return TimeEntry{}, err
		}
		parentSetBy, parentSetByKind = actor.By, actor.Kind
	}
	placeBy, placeByKind := "", ""
	if input.Place != "" {
		placeBy, placeByKind = actor.By, actor.Kind
	}
	result, err := tx.Exec(`INSERT INTO time_entries (id, title, title_by, title_by_kind, span_by, span_by_kind, starts_at, ends_at, all_day, start_date, end_date,
		time_zone, place, place_by, place_by_kind, parent_time_entry_id, parent_set_by, parent_set_by_kind, created_by, created_by_kind, created_reason,
		created_at, updated_at, evidence_changed_at)
		VALUES ('pending-' || hex(randomblob(8)), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.Title, actor.By, actor.Kind, actor.By, actor.Kind, input.StartsAt, input.EndsAt, input.AllDay, input.StartDate, input.EndDate,
		input.TimeZone, input.Place, placeBy, placeByKind, input.ParentTimeEntryID, parentSetBy, parentSetByKind, actor.By, actor.Kind, input.Reason,
		now, now, now)
	if err != nil {
		return TimeEntry{}, err
	}
	id, err := assignID(tx, "time_entries", "time_entry", result)
	if err != nil {
		return TimeEntry{}, err
	}
	for _, kindID := range input.KindIDs {
		if err := assignKind(tx, id, kindID, actor, input.Reason, now); err != nil {
			return TimeEntry{}, err
		}
	}
	for _, link := range input.Links {
		if link.Reason == "" {
			link.Reason = input.Reason
		}
		if _, err := addLink(tx, id, link, actor, now); err != nil {
			return TimeEntry{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return TimeEntry{}, err
	}
	return s.Entry(id, false)
}

// checkParent refuses a parent that is missing, deleted, does not contain the
// span, or is the entry itself or one of its descendants.
func checkParent(tx *sql.Tx, entryID, parentID string, startsAt, endsAt int64) error {
	var parentStarts, parentEnds int64
	var deletedAt sql.NullInt64
	err := tx.QueryRow(`SELECT starts_at, ends_at, deleted_at FROM time_entries WHERE id = ?`, parentID).Scan(&parentStarts, &parentEnds, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) || deletedAt.Valid {
		return fmt.Errorf("%w: parent %s does not exist", ErrInvalid, parentID)
	}
	if err != nil {
		return err
	}
	if startsAt < parentStarts || endsAt > parentEnds {
		return fmt.Errorf("%w: the entry (%d–%d) does not lie inside parent %s (%d–%d)", ErrConflict, startsAt, endsAt, parentID, parentStarts, parentEnds)
	}
	for ancestor := parentID; ancestor != ""; {
		if ancestor == entryID {
			return fmt.Errorf("%w: %s cannot sit inside itself", ErrConflict, entryID)
		}
		var next sql.NullString
		if err := tx.QueryRow(`SELECT parent_time_entry_id FROM time_entries WHERE id = ?`, ancestor).Scan(&next); err != nil {
			return err
		}
		ancestor = next.String
	}
	return nil
}

func assignKind(tx *sql.Tx, entryID, kindID string, actor Actor, reason string, now int64) error {
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM time_entry_kinds WHERE id = ?`, kindID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("%w: kind %s does not exist", ErrInvalid, kindID)
	}
	var live int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM time_entry_kind_assignments WHERE time_entry_id = ? AND kind_id = ? AND removed_at IS NULL`, entryID, kindID).Scan(&live); err != nil {
		return err
	}
	if live > 0 {
		return nil
	}
	if actor.Kind != ActorOperator {
		var removedByOperator int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM time_entry_kind_assignments WHERE time_entry_id = ? AND kind_id = ? AND removed_by_kind = 'operator'`, entryID, kindID).Scan(&removedByOperator); err != nil {
			return err
		}
		if removedByOperator > 0 {
			return fmt.Errorf("%w: the operator took kind %s off %s", ErrConflict, kindID, entryID)
		}
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	_, err := tx.Exec(`INSERT INTO time_entry_kind_assignments (time_entry_id, kind_id, assigned_by, assigned_by_kind, reason, assigned_at) VALUES (?, ?, ?, ?, ?, ?)`,
		entryID, kindID, actor.By, actor.Kind, reason, now)
	return err
}

// AssignKind puts a kind on an entry. Putting on one it carries changes nothing.
func (s *Store) AssignKind(entryID, kindID string, actor Actor, reason string) (TimeEntry, error) {
	if err := actor.validate(); err != nil {
		return TimeEntry{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return TimeEntry{}, err
	}
	defer tx.Rollback()
	if err := liveEntry(tx, entryID); err != nil {
		return TimeEntry{}, err
	}
	if err := assignKind(tx, entryID, kindID, actor, reason, s.now().Unix()); err != nil {
		return TimeEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return TimeEntry{}, err
	}
	return s.Entry(entryID, false)
}

// RemoveKind takes a kind off an entry.
func (s *Store) RemoveKind(entryID, kindID string, actor Actor) error {
	if err := actor.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var assignedByKind string
	err := s.db.QueryRow(`SELECT assigned_by_kind FROM time_entry_kind_assignments WHERE time_entry_id = ? AND kind_id = ? AND removed_at IS NULL`, entryID, kindID).Scan(&assignedByKind)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s does not carry kind %s", ErrNotFound, entryID, kindID)
	}
	if err != nil {
		return err
	}
	if !actor.mayReplace(assignedByKind) {
		return fmt.Errorf("%w: kind %s was put on by a %s; a %s may not take it off", ErrConflict, kindID, assignedByKind, actor.Kind)
	}
	_, err = s.db.Exec(`UPDATE time_entry_kind_assignments SET removed_at = ?, removed_by = ?, removed_by_kind = ? WHERE time_entry_id = ? AND kind_id = ? AND removed_at IS NULL`,
		s.now().Unix(), actor.By, actor.Kind, entryID, kindID)
	return err
}

func liveEntry(tx *sql.Tx, entryID string) error {
	var deletedAt sql.NullInt64
	err := tx.QueryRow(`SELECT deleted_at FROM time_entries WHERE id = ?`, entryID).Scan(&deletedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: time entry %s", ErrNotFound, entryID)
	}
	if err != nil {
		return err
	}
	if deletedAt.Valid {
		return fmt.Errorf("%w: time entry %s was deleted", ErrConflict, entryID)
	}
	return nil
}

func linkTypeOf(name string) (LinkType, error) {
	linkType, known := LinkTypes[name]
	if !known {
		return LinkType{}, fmt.Errorf("%w: link type %q is not one of %v", ErrInvalid, name, LinkTypeNames())
	}
	return linkType, nil
}

// addLink makes one link, or answers the identical live one. It refuses a
// link the operator removed unless the operator makes it again.
func addLink(tx *sql.Tx, entryID string, input LinkInput, actor Actor, now int64) (Link, error) {
	linkType, err := linkTypeOf(input.LinkType)
	if err != nil {
		return Link{}, err
	}
	if !linkType.refPattern.MatchString(input.Ref) {
		return Link{}, fmt.Errorf("%w: %q is not a %s id (%s)", ErrInvalid, input.Ref, linkType.Name, linkType.refPattern)
	}
	if !contains(BearsOn, input.BearsOn) {
		return Link{}, fmt.Errorf("%w: bears_on %q is not one of %v", ErrInvalid, input.BearsOn, BearsOn)
	}
	if input.BearsOn == BearsOnSource && !linkType.CanBeSource {
		return Link{}, fmt.Errorf("%w: a %s link cannot be an entry's source", ErrInvalid, linkType.Name)
	}
	if linkType.HasOccurrence != (input.OccurrenceStartsAt != nil) {
		return Link{}, fmt.Errorf("%w: occurrence_starts_at is required for %s links and only for them", ErrInvalid, linkType.Name)
	}
	if !linkType.HasOverlap && input.OverlapSeconds != nil {
		return Link{}, fmt.Errorf("%w: overlap_seconds is only for trips and visits", ErrInvalid)
	}
	if !linkType.HasAmount && input.AmountCents != nil {
		return Link{}, fmt.Errorf("%w: amount_cents is only for transactions", ErrInvalid)
	}
	if strings.TrimSpace(input.Reason) == "" {
		return Link{}, fmt.Errorf("%w: reason is required: why this record bears on the entry", ErrInvalid)
	}
	where, args := linkKey(linkType, input.Ref, input.OccurrenceStartsAt)
	existing, err := queryLinks(tx, linkType, `time_entry_id = ? AND `+where+` AND removed_at IS NULL`, append([]any{entryID}, args...)...)
	if err != nil {
		return Link{}, err
	}
	if len(existing) > 0 {
		if existing[0].BearsOn == input.BearsOn {
			return existing[0], nil
		}
		return Link{}, fmt.Errorf("%w: %s %s is already linked to %s as %s; remove it first", ErrConflict, linkType.Name, input.Ref, entryID, existing[0].BearsOn)
	}
	if actor.Kind != ActorOperator {
		removed, err := queryLinks(tx, linkType, `time_entry_id = ? AND `+where+` AND removed_by_kind = 'operator'`, append([]any{entryID}, args...)...)
		if err != nil {
			return Link{}, err
		}
		if len(removed) > 0 {
			return Link{}, fmt.Errorf("%w: the operator took %s %s off %s", ErrConflict, linkType.Name, input.Ref, entryID)
		}
	}
	if input.BearsOn == BearsOnSource {
		owners, err := queryLinks(tx, linkType, where+` AND removed_at IS NULL AND bears_on = 'source'`, args...)
		if err != nil {
			return Link{}, err
		}
		if len(owners) > 0 {
			return Link{}, fmt.Errorf("%w: %s %s is already the source of %s", ErrConflict, linkType.Name, input.Ref, owners[0].TimeEntryID)
		}
	}
	columns := []string{"time_entry_id", linkType.RefColumn, "bears_on", "linked_by", "linked_by_kind", "reason", "linked_at"}
	values := []any{entryID, input.Ref, input.BearsOn, actor.By, actor.Kind, input.Reason, now}
	if linkType.HasOccurrence {
		columns, values = append(columns, "occurrence_starts_at"), append(values, *input.OccurrenceStartsAt)
	}
	if linkType.HasOverlap && input.OverlapSeconds != nil {
		columns, values = append(columns, "overlap_seconds"), append(values, *input.OverlapSeconds)
	}
	if linkType.HasAmount && input.AmountCents != nil {
		columns, values = append(columns, "amount_cents"), append(values, *input.AmountCents)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(columns)), ", ")
	if _, err := tx.Exec(`INSERT INTO `+linkType.Table+` (`+strings.Join(columns, ", ")+`) VALUES (`+placeholders+`)`, values...); err != nil {
		return Link{}, err
	}
	if _, err := tx.Exec(`UPDATE time_entries SET evidence_changed_at = ? WHERE id = ?`, now, entryID); err != nil {
		return Link{}, err
	}
	made, err := queryLinks(tx, linkType, `time_entry_id = ? AND `+where+` AND removed_at IS NULL`, append([]any{entryID}, args...)...)
	if err != nil {
		return Link{}, err
	}
	return made[0], nil
}

func linkKey(linkType LinkType, ref string, occurrence *int64) (string, []any) {
	if linkType.HasOccurrence {
		if occurrence == nil {
			return linkType.RefColumn + ` = ?`, []any{ref}
		}
		return linkType.RefColumn + ` = ? AND occurrence_starts_at = ?`, []any{ref, *occurrence}
	}
	return linkType.RefColumn + ` = ?`, []any{ref}
}

type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
}

func queryLinks(db querier, linkType LinkType, where string, args ...any) ([]Link, error) {
	occurrence, overlap, amount := "NULL", "NULL", "NULL"
	if linkType.HasOccurrence {
		occurrence = "occurrence_starts_at"
	}
	if linkType.HasOverlap {
		overlap = "overlap_seconds"
	}
	if linkType.HasAmount {
		amount = "amount_cents"
	}
	rows, err := db.Query(`SELECT time_entry_id, `+linkType.RefColumn+`, `+occurrence+`, `+overlap+`, `+amount+`, bears_on, linked_by, linked_by_kind, reason, linked_at,
		removed_at, removed_by, removed_by_kind FROM `+linkType.Table+` WHERE `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []Link{}
	for rows.Next() {
		link := Link{LinkType: linkType.Name}
		var occurrenceValue, overlapValue, amountValue, removedAt sql.NullInt64
		if err := rows.Scan(&link.TimeEntryID, &link.Ref, &occurrenceValue, &overlapValue, &amountValue, &link.BearsOn, &link.LinkedBy, &link.LinkedByKind,
			&link.Reason, &link.LinkedAt, &removedAt, &link.RemovedBy, &link.RemovedByKind); err != nil {
			return nil, err
		}
		link.OccurrenceStartsAt = nullableInt(occurrenceValue)
		link.OverlapSeconds = nullableInt(overlapValue)
		link.AmountCents = nullableInt(amountValue)
		link.RemovedAt = nullableInt(removedAt)
		links = append(links, link)
	}
	return links, rows.Err()
}

func nullableInt(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	number := value.Int64
	return &number
}

// AddLink links a record to an entry.
func (s *Store) AddLink(entryID string, input LinkInput, actor Actor) (Link, error) {
	if err := actor.validate(); err != nil {
		return Link{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	if err := liveEntry(tx, entryID); err != nil {
		return Link{}, err
	}
	link, err := addLink(tx, entryID, input, actor, s.now().Unix())
	if err != nil {
		return Link{}, err
	}
	return link, tx.Commit()
}

// RemoveLink marks a live link removed. It is kept, and a removal by the
// operator keeps models and programs from making it again.
func (s *Store) RemoveLink(entryID, linkTypeName, ref string, occurrence *int64, actor Actor) error {
	if err := actor.validate(); err != nil {
		return err
	}
	linkType, err := linkTypeOf(linkTypeName)
	if err != nil {
		return err
	}
	if linkType.HasOccurrence && occurrence == nil {
		return fmt.Errorf("%w: occurrence_starts_at is required to remove a %s link", ErrInvalid, linkType.Name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	where, args := linkKey(linkType, ref, occurrence)
	live, err := queryLinks(tx, linkType, `time_entry_id = ? AND `+where+` AND removed_at IS NULL`, append([]any{entryID}, args...)...)
	if err != nil {
		return err
	}
	if len(live) == 0 {
		return fmt.Errorf("%w: %s is not linked to %s %s", ErrNotFound, entryID, linkType.Name, ref)
	}
	if !actor.mayReplace(live[0].LinkedByKind) {
		return fmt.Errorf("%w: a %s made this link; a %s may not remove it", ErrConflict, live[0].LinkedByKind, actor.Kind)
	}
	now := s.now().Unix()
	if _, err := tx.Exec(`UPDATE `+linkType.Table+` SET removed_at = ?, removed_by = ?, removed_by_kind = ? WHERE time_entry_id = ? AND `+where+` AND removed_at IS NULL`,
		append([]any{now, actor.By, actor.Kind, entryID}, args...)...); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE time_entries SET evidence_changed_at = ? WHERE id = ?`, now, entryID); err != nil {
		return err
	}
	return tx.Commit()
}

// LinksTo lists the links to one record, across every entry: the way an
// importer finds the entry a source already belongs to.
func (s *Store) LinksTo(linkTypeName, ref string, occurrence *int64, includeRemoved bool) ([]Link, error) {
	linkType, err := linkTypeOf(linkTypeName)
	if err != nil {
		return nil, err
	}
	where, args := linkKey(linkType, ref, occurrence)
	if !includeRemoved {
		where += ` AND removed_at IS NULL`
	}
	return queryLinks(s.db, linkType, where, args...)
}

// EntryChange is what PATCH /time-entries/{id} takes. A field left out stays.
type EntryChange struct {
	Title             *string `json:"title"`
	StartsAt          *int64  `json:"starts_at"`
	EndsAt            *int64  `json:"ends_at"`
	AllDay            *bool   `json:"all_day"`
	StartDate         *string `json:"start_date"`
	EndDate           *string `json:"end_date"`
	TimeZone          *string `json:"time_zone"`
	Place             *string `json:"place"`
	ParentTimeEntryID *string `json:"parent_time_entry_id"`
	By                string  `json:"by"`
	ByKind            string  `json:"by_kind"`
	Reason            string  `json:"reason"`
}

// UpdateEntry changes an entry, copying it to revisions first. A field set by
// a higher kind of actor than the caller is a conflict, and so is a span that
// would leave a child outside it.
func (s *Store) UpdateEntry(entryID string, change EntryChange) (TimeEntry, error) {
	actor := Actor{By: change.By, Kind: change.ByKind}
	if err := actor.validate(); err != nil {
		return TimeEntry{}, err
	}
	if strings.TrimSpace(change.Reason) == "" {
		return TimeEntry{}, fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return TimeEntry{}, err
	}
	defer tx.Rollback()
	if err := liveEntry(tx, entryID); err != nil {
		return TimeEntry{}, err
	}
	before, err := readEntry(tx, entryID)
	if err != nil {
		return TimeEntry{}, err
	}
	after := before
	now := s.now().Unix()
	if change.Title != nil && *change.Title != before.Title {
		if !actor.mayReplace(before.TitleByKind) {
			return TimeEntry{}, fmt.Errorf("%w: a %s set the title; a %s may not change it", ErrConflict, before.TitleByKind, actor.Kind)
		}
		after.Title, after.TitleBy, after.TitleByKind = strings.TrimSpace(*change.Title), actor.By, actor.Kind
		if after.Title == "" {
			return TimeEntry{}, fmt.Errorf("%w: title may not be empty", ErrInvalid)
		}
	}
	spanChanged := false
	setInt := func(field *int64, value *int64) {
		if value != nil && *value != *field {
			*field, spanChanged = *value, true
		}
	}
	setString := func(field *string, value *string) {
		if value != nil && *value != *field {
			*field, spanChanged = *value, true
		}
	}
	setInt(&after.StartsAt, change.StartsAt)
	setInt(&after.EndsAt, change.EndsAt)
	setString(&after.StartDate, change.StartDate)
	setString(&after.EndDate, change.EndDate)
	setString(&after.TimeZone, change.TimeZone)
	if change.AllDay != nil && *change.AllDay != after.AllDay {
		after.AllDay, spanChanged = *change.AllDay, true
	}
	if spanChanged {
		if !actor.mayReplace(before.SpanByKind) {
			return TimeEntry{}, fmt.Errorf("%w: a %s set the times; a %s may not change them", ErrConflict, before.SpanByKind, actor.Kind)
		}
		if err := validateSpan(after.StartsAt, after.EndsAt, after.AllDay, after.StartDate, after.EndDate, after.TimeZone); err != nil {
			return TimeEntry{}, err
		}
		after.SpanBy, after.SpanByKind = actor.By, actor.Kind
		var outside int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM time_entries WHERE parent_time_entry_id = ? AND deleted_at IS NULL AND (starts_at < ? OR ends_at > ?)`,
			entryID, after.StartsAt, after.EndsAt).Scan(&outside); err != nil {
			return TimeEntry{}, err
		}
		if outside > 0 {
			return TimeEntry{}, fmt.Errorf("%w: %d of %s's children would lie outside the new times", ErrConflict, outside, entryID)
		}
	}
	if change.Place != nil && *change.Place != before.Place {
		if !actor.mayReplace(before.PlaceByKind) {
			return TimeEntry{}, fmt.Errorf("%w: a %s set the place; a %s may not change it", ErrConflict, before.PlaceByKind, actor.Kind)
		}
		after.Place, after.PlaceBy, after.PlaceByKind = *change.Place, actor.By, actor.Kind
	}
	if change.ParentTimeEntryID != nil && *change.ParentTimeEntryID != before.ParentTimeEntryID {
		if !actor.mayReplace(before.ParentSetByKind) {
			return TimeEntry{}, fmt.Errorf("%w: a %s set the parent; a %s may not change it", ErrConflict, before.ParentSetByKind, actor.Kind)
		}
		after.ParentTimeEntryID, after.ParentSetBy, after.ParentSetByKind = *change.ParentTimeEntryID, actor.By, actor.Kind
	}
	if after.ParentTimeEntryID != "" && (spanChanged || after.ParentTimeEntryID != before.ParentTimeEntryID) {
		if err := checkParent(tx, entryID, after.ParentTimeEntryID, after.StartsAt, after.EndsAt); err != nil {
			return TimeEntry{}, err
		}
	}
	if after.Title == before.Title && !spanChanged && after.Place == before.Place && after.ParentTimeEntryID == before.ParentTimeEntryID {
		return s.entryIn(tx, entryID)
	}
	if err := keepRevision(tx, before, change.Reason, now); err != nil {
		return TimeEntry{}, err
	}
	evidenceChangedAt := before.EvidenceChangedAt
	if spanChanged {
		evidenceChangedAt = now
	}
	if _, err := tx.Exec(`UPDATE time_entries SET title = ?, title_by = ?, title_by_kind = ?, starts_at = ?, ends_at = ?, all_day = ?, start_date = ?, end_date = ?,
		time_zone = ?, span_by = ?, span_by_kind = ?, place = ?, place_by = ?, place_by_kind = ?, parent_time_entry_id = NULLIF(?, ''), parent_set_by = ?,
		parent_set_by_kind = ?, updated_at = ?, evidence_changed_at = ? WHERE id = ?`,
		after.Title, after.TitleBy, after.TitleByKind, after.StartsAt, after.EndsAt, after.AllDay, after.StartDate, after.EndDate,
		after.TimeZone, after.SpanBy, after.SpanByKind, after.Place, after.PlaceBy, after.PlaceByKind, after.ParentTimeEntryID, after.ParentSetBy,
		after.ParentSetByKind, now, evidenceChangedAt, entryID); err != nil {
		return TimeEntry{}, err
	}
	if err := tx.Commit(); err != nil {
		return TimeEntry{}, err
	}
	return s.Entry(entryID, false)
}

func keepRevision(tx *sql.Tx, entry TimeEntry, reason string, now int64) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO time_entry_revisions (time_entry_id, reason, replaced_at, entry) VALUES (?, ?, ?, ?)`, entry.ID, reason, now, string(body))
	return err
}

// DeleteEntry soft-deletes an entry with no live children. Its links stay, so
// an importer finds its source taken and does not make it again.
func (s *Store) DeleteEntry(entryID string, actor Actor) error {
	if err := actor.validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := liveEntry(tx, entryID); err != nil {
		return err
	}
	entry, err := readEntry(tx, entryID)
	if err != nil {
		return err
	}
	if !actor.mayReplace(entry.CreatedByKind) {
		return fmt.Errorf("%w: a %s made %s; a %s may not delete it", ErrConflict, entry.CreatedByKind, entryID, actor.Kind)
	}
	var children int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM time_entries WHERE parent_time_entry_id = ? AND deleted_at IS NULL`, entryID).Scan(&children); err != nil {
		return err
	}
	if children > 0 {
		return fmt.Errorf("%w: %s has %d live children; move or delete them first", ErrConflict, entryID, children)
	}
	now := s.now().Unix()
	if err := keepRevision(tx, entry, "deleted by "+actor.By, now); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE time_entries SET deleted_at = ?, deleted_by = ?, updated_at = ? WHERE id = ?`, now, actor.By, now, entryID); err != nil {
		return err
	}
	return tx.Commit()
}

const entryColumns = `e.id, e.title, e.title_by, e.title_by_kind, e.starts_at, e.ends_at, e.all_day, e.start_date, e.end_date, e.time_zone, e.span_by, e.span_by_kind,
	e.place, e.place_by, e.place_by_kind, COALESCE(e.parent_time_entry_id, ''), e.parent_set_by, e.parent_set_by_kind, e.created_by, e.created_by_kind,
	e.created_reason, e.created_at, e.updated_at, e.evidence_changed_at, e.deleted_at, e.deleted_by,
	COALESCE((SELECT r.id FROM time_entry_reviews r WHERE r.time_entry_id = e.id AND r.reviewed_by_kind = 'operator' ORDER BY r.seq DESC LIMIT 1),
	         (SELECT r.id FROM time_entry_reviews r WHERE r.time_entry_id = e.id AND r.reviewed_by_kind = 'model' ORDER BY r.seq DESC LIMIT 1), '')`

func scanEntry(row interface{ Scan(...any) error }) (TimeEntry, error) {
	var entry TimeEntry
	var deletedAt sql.NullInt64
	err := row.Scan(&entry.ID, &entry.Title, &entry.TitleBy, &entry.TitleByKind, &entry.StartsAt, &entry.EndsAt, &entry.AllDay, &entry.StartDate, &entry.EndDate,
		&entry.TimeZone, &entry.SpanBy, &entry.SpanByKind, &entry.Place, &entry.PlaceBy, &entry.PlaceByKind, &entry.ParentTimeEntryID, &entry.ParentSetBy,
		&entry.ParentSetByKind, &entry.CreatedBy, &entry.CreatedByKind, &entry.CreatedReason, &entry.CreatedAt, &entry.UpdatedAt, &entry.EvidenceChangedAt,
		&deletedAt, &entry.DeletedBy, &entry.OutcomeReviewID)
	entry.DeletedAt = nullableInt(deletedAt)
	return entry, err
}

type rowQuerier interface {
	querier
	QueryRow(query string, args ...any) *sql.Row
}

func readEntry(db rowQuerier, entryID string) (TimeEntry, error) {
	entry, err := scanEntry(db.QueryRow(`SELECT `+entryColumns+` FROM time_entries e WHERE e.id = ?`, entryID))
	if errors.Is(err, sql.ErrNoRows) {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, entryID)
	}
	if err != nil {
		return TimeEntry{}, err
	}
	return entry, fillEntry(db, &entry, false)
}

func fillEntry(db rowQuerier, entry *TimeEntry, full bool) error {
	entry.Outcome = OutcomeUnknown
	if entry.OutcomeReviewID != "" {
		if err := db.QueryRow(`SELECT outcome FROM time_entry_reviews WHERE id = ?`, entry.OutcomeReviewID).Scan(&entry.Outcome); err != nil {
			return err
		}
	}
	rows, err := db.Query(`SELECT a.kind_id, k.name, a.assigned_by, a.assigned_by_kind, a.reason, a.assigned_at FROM time_entry_kind_assignments a
		JOIN time_entry_kinds k ON k.id = a.kind_id WHERE a.time_entry_id = ? AND a.removed_at IS NULL ORDER BY k.name`, entry.ID)
	if err != nil {
		return err
	}
	entry.Kinds = []KindAssignment{}
	for rows.Next() {
		var assignment KindAssignment
		if err := rows.Scan(&assignment.KindID, &assignment.Name, &assignment.AssignedBy, &assignment.AssignedByKind, &assignment.Reason, &assignment.AssignedAt); err != nil {
			rows.Close()
			return err
		}
		entry.Kinds = append(entry.Kinds, assignment)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if !full {
		return nil
	}
	entry.Links = []Link{}
	for _, name := range LinkTypeNames() {
		links, err := queryLinks(db, LinkTypes[name], `time_entry_id = ?`, entry.ID)
		if err != nil {
			return err
		}
		entry.Links = append(entry.Links, links...)
	}
	childRows, err := db.Query(`SELECT id FROM time_entries WHERE parent_time_entry_id = ? AND deleted_at IS NULL ORDER BY starts_at, seq`, entry.ID)
	if err != nil {
		return err
	}
	entry.ChildIDs = []string{}
	for childRows.Next() {
		var childID string
		if err := childRows.Scan(&childID); err != nil {
			childRows.Close()
			return err
		}
		entry.ChildIDs = append(entry.ChildIDs, childID)
	}
	childRows.Close()
	if err := childRows.Err(); err != nil {
		return err
	}
	entry.Reviews, err = queryReviews(db, `time_entry_id = ?`, entry.ID)
	return err
}

func (s *Store) entryIn(tx *sql.Tx, entryID string) (TimeEntry, error) {
	if err := tx.Commit(); err != nil {
		return TimeEntry{}, err
	}
	return s.Entry(entryID, false)
}

// Entry reads one entry; full adds its links, live children and reviews.
func (s *Store) Entry(entryID string, full bool) (TimeEntry, error) {
	entry, err := scanEntry(s.db.QueryRow(`SELECT `+entryColumns+` FROM time_entries e WHERE e.id = ?`, entryID))
	if errors.Is(err, sql.ErrNoRows) {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, entryID)
	}
	if err != nil {
		return TimeEntry{}, err
	}
	return entry, fillEntry(s.db, &entry, full)
}

// EntryFilter is GET /time-entries' query.
type EntryFilter struct {
	From, To       int64 // entries overlapping [From, To)
	EndedBefore    int64
	Parent         string // an id, or "none" for entries inside no other
	KindID         string
	Outcome        string
	Query          string
	NeedsReview    bool // no review, or the newest is older than the evidence
	IncludeDeleted bool
	Limit, Offset  int
}

// MaxListLimit is the most entries one list answers.
const MaxListLimit = 1000

// Entries lists entries, earliest first.
func (s *Store) Entries(filter EntryFilter) ([]TimeEntry, error) {
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 0 || filter.Limit > MaxListLimit {
		return nil, fmt.Errorf("%w: limit is at most %d", ErrInvalid, MaxListLimit)
	}
	conditions, args := []string{}, []any{}
	if !filter.IncludeDeleted {
		conditions = append(conditions, `e.deleted_at IS NULL`)
	}
	if filter.From != 0 {
		conditions, args = append(conditions, `e.ends_at > ?`), append(args, filter.From)
	}
	if filter.To != 0 {
		conditions, args = append(conditions, `e.starts_at < ?`), append(args, filter.To)
	}
	if filter.EndedBefore != 0 {
		conditions, args = append(conditions, `e.ends_at <= ?`), append(args, filter.EndedBefore)
	}
	switch filter.Parent {
	case "":
	case "none":
		conditions = append(conditions, `e.parent_time_entry_id IS NULL`)
	default:
		conditions, args = append(conditions, `e.parent_time_entry_id = ?`), append(args, filter.Parent)
	}
	if filter.KindID != "" {
		conditions, args = append(conditions, `EXISTS (SELECT 1 FROM time_entry_kind_assignments a WHERE a.time_entry_id = e.id AND a.kind_id = ? AND a.removed_at IS NULL)`), append(args, filter.KindID)
	}
	if filter.Query != "" {
		conditions, args = append(conditions, `(e.title LIKE ? OR e.place LIKE ?)`), append(args, "%"+filter.Query+"%", "%"+filter.Query+"%")
	}
	if filter.NeedsReview {
		conditions = append(conditions, `NOT EXISTS (SELECT 1 FROM time_entry_reviews r WHERE r.time_entry_id = e.id AND r.created_at >= e.evidence_changed_at)`)
	}
	if filter.Outcome != "" && !contains(Outcomes, filter.Outcome) {
		return nil, fmt.Errorf("%w: outcome %q is not one of %v", ErrInvalid, filter.Outcome, Outcomes)
	}
	where := ""
	if len(conditions) > 0 {
		where = ` WHERE ` + strings.Join(conditions, ` AND `)
	}
	query := `SELECT ` + entryColumns + ` FROM time_entries e` + where + ` ORDER BY e.starts_at, e.seq`
	if filter.Outcome == "" {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, filter.Limit, filter.Offset)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	entries := []TimeEntry{}
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, entry)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kept := []TimeEntry{}
	for i := range entries {
		if err := fillEntry(s.db, &entries[i], false); err != nil {
			return nil, err
		}
		if filter.Outcome == "" || entries[i].Outcome == filter.Outcome {
			kept = append(kept, entries[i])
		}
	}
	if filter.Outcome != "" {
		if filter.Offset >= len(kept) {
			return []TimeEntry{}, nil
		}
		kept = kept[filter.Offset:]
		if len(kept) > filter.Limit {
			kept = kept[:filter.Limit]
		}
	}
	return kept, nil
}

// Revisions lists an entry's earlier versions, newest first.
func (s *Store) Revisions(entryID string) ([]map[string]any, error) {
	rows, err := s.db.Query(`SELECT revision_id, reason, replaced_at, entry FROM time_entry_revisions WHERE time_entry_id = ? ORDER BY revision_id DESC`, entryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []map[string]any{}
	for rows.Next() {
		var revisionID, replacedAt int64
		var reason, body string
		if err := rows.Scan(&revisionID, &reason, &replacedAt, &body); err != nil {
			return nil, err
		}
		revisions = append(revisions, map[string]any{"revision_id": revisionID, "reason": reason, "replaced_at": replacedAt, "entry": json.RawMessage(body)})
	}
	return revisions, rows.Err()
}

// ReviewInput is what POST /time-entries/{id}/reviews takes.
type ReviewInput struct {
	Outcome                    string          `json:"outcome"`
	Reason                     string          `json:"reason"`
	By                         string          `json:"by"`
	ByKind                     string          `json:"by_kind"`
	Model                      string          `json:"model"`
	InputTokens                int64           `json:"input_tokens"`
	OutputTokens               int64           `json:"output_tokens"`
	CostUSD                    float64         `json:"cost_usd"`
	LinksSeen                  json.RawMessage `json:"links_seen"`
	SuggestedParentTimeEntryID string          `json:"suggested_parent_time_entry_id"`
	SuggestedKindNames         []string        `json:"suggested_kind_names"`
}

// AddReview records a judgment of whether an entry happened. Only operators
// and models review; a program's rules make links, not judgments.
func (s *Store) AddReview(entryID string, input ReviewInput) (Review, error) {
	actor := Actor{By: input.By, Kind: input.ByKind}
	if err := actor.validate(); err != nil {
		return Review{}, err
	}
	if actor.Kind == ActorProgram {
		return Review{}, fmt.Errorf("%w: a review is the operator's or a model's", ErrInvalid)
	}
	if !contains(Outcomes, input.Outcome) {
		return Review{}, fmt.Errorf("%w: outcome %q is not one of %v", ErrInvalid, input.Outcome, Outcomes)
	}
	if strings.TrimSpace(input.Reason) == "" {
		return Review{}, fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	if actor.Kind == ActorModel && input.Model == "" {
		return Review{}, fmt.Errorf("%w: a model's review names the model", ErrInvalid)
	}
	if len(input.LinksSeen) == 0 {
		input.LinksSeen = json.RawMessage(`[]`)
	}
	if !json.Valid(input.LinksSeen) {
		return Review{}, fmt.Errorf("%w: links_seen is not JSON", ErrInvalid)
	}
	if input.SuggestedKindNames == nil {
		input.SuggestedKindNames = []string{}
	}
	kindNames, err := json.Marshal(input.SuggestedKindNames)
	if err != nil {
		return Review{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Review{}, err
	}
	defer tx.Rollback()
	if err := liveEntry(tx, entryID); err != nil {
		return Review{}, err
	}
	result, err := tx.Exec(`INSERT INTO time_entry_reviews (id, time_entry_id, outcome, reason, reviewed_by, reviewed_by_kind, model, input_tokens, output_tokens,
		cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names, created_at)
		VALUES ('pending-' || hex(randomblob(8)), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		entryID, input.Outcome, input.Reason, actor.By, actor.Kind, input.Model, input.InputTokens, input.OutputTokens,
		input.CostUSD, string(input.LinksSeen), input.SuggestedParentTimeEntryID, string(kindNames), s.now().Unix())
	if err != nil {
		return Review{}, err
	}
	id, err := assignID(tx, "time_entry_reviews", "time_entry_review", result)
	if err != nil {
		return Review{}, err
	}
	if err := tx.Commit(); err != nil {
		return Review{}, err
	}
	return s.Review(id)
}

func queryReviews(db querier, where string, args ...any) ([]Review, error) {
	rows, err := db.Query(`SELECT id, time_entry_id, outcome, reason, reviewed_by, reviewed_by_kind, model, input_tokens, output_tokens, cost_usd, links_seen,
		suggested_parent_time_entry_id, suggested_kind_names, created_at FROM time_entry_reviews WHERE `+where+` ORDER BY seq DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reviews := []Review{}
	for rows.Next() {
		var review Review
		var linksSeen, kindNames string
		if err := rows.Scan(&review.ID, &review.TimeEntryID, &review.Outcome, &review.Reason, &review.ReviewedBy, &review.ReviewedByKind, &review.Model,
			&review.InputTokens, &review.OutputTokens, &review.CostUSD, &linksSeen, &review.SuggestedParentTimeEntryID, &kindNames, &review.CreatedAt); err != nil {
			return nil, err
		}
		review.LinksSeen = json.RawMessage(linksSeen)
		if err := json.Unmarshal([]byte(kindNames), &review.SuggestedKindNames); err != nil {
			return nil, err
		}
		reviews = append(reviews, review)
	}
	return reviews, rows.Err()
}

// Review reads one review.
func (s *Store) Review(id string) (Review, error) {
	reviews, err := queryReviews(s.db, `id = ?`, id)
	if err != nil {
		return Review{}, err
	}
	if len(reviews) == 0 {
		return Review{}, fmt.Errorf("%w: review %s", ErrNotFound, id)
	}
	return reviews[0], nil
}

// Reviews lists an entry's reviews, newest first.
func (s *Store) Reviews(entryID string) ([]Review, error) {
	return queryReviews(s.db, `time_entry_id = ?`, entryID)
}

// Counts is GET /health's counts.
func (s *Store) Counts() (map[string]int64, error) {
	counts := map[string]int64{}
	for name, query := range map[string]string{
		"time_entries": `SELECT COUNT(*) FROM time_entries WHERE deleted_at IS NULL`,
		"kinds":        `SELECT COUNT(*) FROM time_entry_kinds`,
		"reviews":      `SELECT COUNT(*) FROM time_entry_reviews`,
	} {
		var count int64
		if err := s.db.QueryRow(query).Scan(&count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	var links int64
	for _, linkType := range LinkTypes {
		var count int64
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + linkType.Table + ` WHERE removed_at IS NULL`).Scan(&count); err != nil {
			return nil, err
		}
		links += count
	}
	counts["links"] = links
	return counts, nil
}
