// Package calendarstore keeps calendar events as their calendars wrote them:
// every VEVENT of an imported iCalendar file, whole, with who it was with as
// people-store ids.
//
// Events are cited elsewhere by their ids (calendar_event_000001), which
// kanban-store's registry claims, so a journal timeline or a note can link one
// as a chip.
package calendarstore

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// Link sources: how a link from an event to a person was made.
const (
	LinkedByAttendeeEmail = "attendee-email"
	LinkedByAgent         = "agent"
	LinkedByOperator      = "operator"
)

// LinkSources is GET /vocabularies' link_sources. Only agent and operator may
// be sent to POST /events/{id}/people; attendee-email links are the import's.
var LinkSources = []string{LinkedByAttendeeEmail, LinkedByAgent, LinkedByOperator}

// Calendar is one calendar a source keeps.
type Calendar struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	SourceRef string `json:"source_ref"`
	CreatedAt int64  `json:"created_at"`
}

// PersonLink is one person an event was with.
type PersonLink struct {
	PersonID string `json:"person_id"`
	LinkedBy string `json:"linked_by"`
	Reason   string `json:"reason"`
	LinkedAt int64  `json:"linked_at"`
}

// Event is one stored calendar event.
type Event struct {
	ID               string       `json:"id"`
	CalendarID       string       `json:"calendar_id"`
	UID              string       `json:"uid"`
	RecurrenceID     string       `json:"recurrence_id"`
	Summary          string       `json:"summary"`
	Description      string       `json:"description"`
	Location         string       `json:"location"`
	StartsAt         int64        `json:"starts_at"`
	EndsAt           int64        `json:"ends_at"`
	AllDay           bool         `json:"all_day"`
	StartDate        string       `json:"start_date"`
	EndDate          string       `json:"end_date"`
	TimeZone         string       `json:"time_zone"`
	RecurrenceRule   string       `json:"recurrence_rule"`
	Status           string       `json:"status"`
	Transparency     string       `json:"transparency"`
	OrganizerEmail   string       `json:"organizer_email"`
	OrganizerName    string       `json:"organizer_name"`
	SourceCreatedAt  int64        `json:"source_created_at"`
	SourceModifiedAt int64        `json:"source_modified_at"`
	Sequence         int64        `json:"sequence"`
	Raw              string       `json:"raw,omitempty"`
	ImportID         string       `json:"import_id"`
	CreatedAt        int64        `json:"created_at"`
	UpdatedAt        int64        `json:"updated_at"`
	Attendees        []Attendee   `json:"attendees"`
	People           []PersonLink `json:"people"`
}

// Import is one iCalendar file read into a calendar.
type Import struct {
	ID           string `json:"id"`
	CalendarID   string `json:"calendar_id"`
	FileName     string `json:"file_name"`
	FileSHA256   string `json:"file_sha256"`
	Created      int    `json:"created"`
	Updated      int    `json:"updated"`
	Unchanged    int    `json:"unchanged"`
	PeopleLinked int    `json:"people_linked"`
	ImportedAt   int64  `json:"imported_at"`
}

// Revision is an event as it was before a change.
type Revision struct {
	RevisionID int64           `json:"revision_id"`
	Reason     string          `json:"reason"`
	ReplacedAt int64           `json:"replaced_at"`
	Event      json.RawMessage `json:"event"`
}

// PeopleDirectory answers who an email address belongs to and whether a
// person exists. people-store is the real one.
type PeopleDirectory interface {
	PersonByEmail(email string) (personID string, err error)
	PersonExists(personID string) (bool, error)
}

// Store owns the database.
type Store struct {
	db         *sql.DB
	dataDir    string
	people     PeopleDirectory
	writeMutex sync.Mutex
	now        func() time.Time
}

// Open creates the data directory (0700) if needed, opens the database and
// applies the schema.
func Open(dataDir string, people PeopleDirectory) (*Store, error) {
	if dataDir == "" {
		return nil, errors.New("open store: no data directory given")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	if err := os.Chmod(dataDir, 0o700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dataDir, "calendar-store.db")
	db, err := sql.Open("sqlite3", dbPath+"?_foreign_keys=on&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("set WAL: %w", err)
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if err := os.Chmod(dbPath, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, dataDir: dataDir, people: people, now: time.Now}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// DataDir is where the database lives.
func (s *Store) DataDir() string { return s.dataDir }

func nextID(tx *sql.Tx, table, prefix string) (int64, string, error) {
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq), 0) + 1 FROM ` + table).Scan(&seq); err != nil {
		return 0, "", err
	}
	return seq, fmt.Sprintf("%s%06d", prefix, seq), nil
}

// EnsureCalendar returns the calendar with this source and source_ref, making
// it first if there is none. created says which.
func (s *Store) EnsureCalendar(name, source, sourceRef string) (calendar Calendar, created bool, err error) {
	name, source, sourceRef = strings.TrimSpace(name), strings.TrimSpace(source), strings.TrimSpace(sourceRef)
	if name == "" || source == "" || sourceRef == "" {
		return calendar, false, fmt.Errorf("%w: name, source and source_ref are required", ErrInvalid)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	row := s.db.QueryRow(`SELECT id, name, source, source_ref, created_at FROM calendars WHERE source = ? AND source_ref = ?`, source, sourceRef)
	if err := row.Scan(&calendar.ID, &calendar.Name, &calendar.Source, &calendar.SourceRef, &calendar.CreatedAt); err == nil {
		return calendar, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return calendar, false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return calendar, false, err
	}
	defer tx.Rollback()
	seq, id, err := nextID(tx, "calendars", "calendar_")
	if err != nil {
		return calendar, false, err
	}
	calendar = Calendar{ID: id, Name: name, Source: source, SourceRef: sourceRef, CreatedAt: s.now().Unix()}
	if _, err := tx.Exec(`INSERT INTO calendars (seq, id, name, source, source_ref, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		seq, id, name, source, sourceRef, calendar.CreatedAt); err != nil {
		return calendar, false, err
	}
	return calendar, true, tx.Commit()
}

// Calendars lists every calendar in the order made.
func (s *Store) Calendars() ([]Calendar, error) {
	rows, err := s.db.Query(`SELECT id, name, source, source_ref, created_at FROM calendars ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calendars := []Calendar{}
	for rows.Next() {
		var calendar Calendar
		if err := rows.Scan(&calendar.ID, &calendar.Name, &calendar.Source, &calendar.SourceRef, &calendar.CreatedAt); err != nil {
			return nil, err
		}
		calendars = append(calendars, calendar)
	}
	return calendars, rows.Err()
}

func (s *Store) calendarExists(id string) (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM calendars WHERE id = ?`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ImportICS reads an iCalendar file into a calendar. An event is found again
// by (uid, recurrence_id): new ones are created, changed ones are updated
// after their old row is kept as a revision, and the same text again changes
// nothing. An event the file no longer holds is kept. Every attendee address
// is then asked of people-store, and the attendee-email links of each event
// the import touched are made again from the answers.
func (s *Store) ImportICS(calendarID, fileName string, content []byte) (Import, error) {
	exists, err := s.calendarExists(calendarID)
	if err != nil {
		return Import{}, err
	}
	if !exists {
		return Import{}, fmt.Errorf("%w: calendar %s", ErrNotFound, calendarID)
	}
	parsed, err := ParseICS(content)
	if err != nil {
		return Import{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	// Ask people-store before the transaction, so a slow answer holds no lock.
	personOf := map[string]string{}
	for _, event := range parsed {
		for _, attendee := range event.Attendees {
			if attendee.Email == "" {
				continue
			}
			if _, asked := personOf[attendee.Email]; asked {
				continue
			}
			personID, err := s.people.PersonByEmail(attendee.Email)
			if err != nil {
				return Import{}, fmt.Errorf("ask people-store about an attendee: %w", err)
			}
			personOf[attendee.Email] = personID
		}
	}

	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Import{}, err
	}
	defer tx.Rollback()
	importSeq, importID, err := nextID(tx, "calendar_imports", "calendar_import_")
	if err != nil {
		return Import{}, err
	}
	record := Import{ID: importID, CalendarID: calendarID, FileName: fileName, FileSHA256: hashOf(string(content)), ImportedAt: s.now().Unix()}
	seen := map[string]bool{}
	for _, event := range parsed {
		key := event.UID + "\x00" + event.RecurrenceID
		if seen[key] {
			return Import{}, fmt.Errorf("%w: the file holds event %s (%s) twice", ErrInvalid, event.UID, event.RecurrenceID)
		}
		seen[key] = true
		outcome, eventID, err := s.upsertEvent(tx, calendarID, importID, event)
		if err != nil {
			return Import{}, err
		}
		switch outcome {
		case "created":
			record.Created++
		case "updated":
			record.Updated++
		default:
			record.Unchanged++
			continue
		}
		linked, err := s.relinkAttendees(tx, eventID, event.Attendees, personOf, record.ImportedAt)
		if err != nil {
			return Import{}, err
		}
		record.PeopleLinked += linked
	}
	if _, err := tx.Exec(`INSERT INTO calendar_imports (seq, id, calendar_id, file_name, file_sha256, created, updated, unchanged, people_linked, imported_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, importSeq, importID, calendarID, fileName, record.FileSHA256,
		record.Created, record.Updated, record.Unchanged, record.PeopleLinked, record.ImportedAt); err != nil {
		return Import{}, err
	}
	return record, tx.Commit()
}

func (s *Store) upsertEvent(tx *sql.Tx, calendarID, importID string, event ParsedEvent) (outcome, eventID string, err error) {
	rawHash := hashOf(event.Raw)
	var existingHash string
	err = tx.QueryRow(`SELECT id, raw_hash FROM calendar_events WHERE calendar_id = ? AND uid = ? AND recurrence_id = ?`,
		calendarID, event.UID, event.RecurrenceID).Scan(&eventID, &existingHash)
	now := s.now().Unix()
	columns := []any{event.Summary, event.Description, event.Location, event.StartsAt, event.EndsAt, event.AllDay,
		event.StartDate, event.EndDate, event.TimeZone, event.RecurrenceRule, event.Status, event.Transparency,
		event.OrganizerEmail, event.OrganizerName, event.SourceCreatedAt, event.SourceModifiedAt, event.Sequence,
		event.Raw, rawHash, importID}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		seq, id, err := nextID(tx, "calendar_events", "calendar_event_")
		if err != nil {
			return "", "", err
		}
		args := append([]any{seq, id, calendarID, event.UID, event.RecurrenceID}, columns...)
		args = append(args, now, now)
		if _, err := tx.Exec(`INSERT INTO calendar_events (seq, id, calendar_id, uid, recurrence_id,
			summary, description, location, starts_at, ends_at, all_day, start_date, end_date, time_zone,
			recurrence_rule, status, transparency, organizer_email, organizer_name, source_created_at,
			source_modified_at, sequence, raw, raw_hash, import_id, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, args...); err != nil {
			return "", "", fmt.Errorf("insert event: %w", err)
		}
		return "created", id, writeAttendees(tx, id, event.Attendees)
	case err != nil:
		return "", "", err
	case existingHash == rawHash:
		return "unchanged", eventID, nil
	}
	if err := s.keepRevision(tx, eventID, "import", now); err != nil {
		return "", "", err
	}
	args := append(columns, now, eventID)
	if _, err := tx.Exec(`UPDATE calendar_events SET summary = ?, description = ?, location = ?, starts_at = ?,
		ends_at = ?, all_day = ?, start_date = ?, end_date = ?, time_zone = ?, recurrence_rule = ?, status = ?,
		transparency = ?, organizer_email = ?, organizer_name = ?, source_created_at = ?, source_modified_at = ?,
		sequence = ?, raw = ?, raw_hash = ?, import_id = ?, updated_at = ? WHERE id = ?`, args...); err != nil {
		return "", "", fmt.Errorf("update event: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM calendar_event_attendees WHERE event_id = ?`, eventID); err != nil {
		return "", "", err
	}
	return "updated", eventID, writeAttendees(tx, eventID, event.Attendees)
}

func writeAttendees(tx *sql.Tx, eventID string, attendees []Attendee) error {
	for position, attendee := range attendees {
		if _, err := tx.Exec(`INSERT INTO calendar_event_attendees (event_id, position, email, name, role, participation_status)
			VALUES (?, ?, ?, ?, ?, ?)`, eventID, position, attendee.Email, attendee.Name, attendee.Role, attendee.ParticipationStatus); err != nil {
			return fmt.Errorf("insert attendee: %w", err)
		}
	}
	return nil
}

// relinkAttendees makes an event's attendee-email links again. A person an
// agent or the operator linked keeps that link.
func (s *Store) relinkAttendees(tx *sql.Tx, eventID string, attendees []Attendee, personOf map[string]string, now int64) (int, error) {
	if _, err := tx.Exec(`DELETE FROM calendar_event_people WHERE event_id = ? AND linked_by = ?`, eventID, LinkedByAttendeeEmail); err != nil {
		return 0, err
	}
	linked := 0
	for _, attendee := range attendees {
		personID := personOf[attendee.Email]
		if personID == "" {
			continue
		}
		result, err := tx.Exec(`INSERT OR IGNORE INTO calendar_event_people (event_id, person_id, linked_by, reason, linked_at)
			VALUES (?, ?, ?, ?, ?)`, eventID, personID, LinkedByAttendeeEmail, "people-store holds the attendee's address", now)
		if err != nil {
			return 0, err
		}
		if count, _ := result.RowsAffected(); count > 0 {
			linked++
		}
	}
	return linked, nil
}

func (s *Store) keepRevision(tx *sql.Tx, eventID, reason string, now int64) error {
	event, err := s.readEvent(tx, eventID, true)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO calendar_event_revisions (event_id, reason, replaced_at, event_json) VALUES (?, ?, ?, ?)`,
		eventID, reason, now, string(snapshot))
	return err
}

type queryer interface {
	QueryRow(query string, args ...any) *sql.Row
	Query(query string, args ...any) (*sql.Rows, error)
}

const eventColumns = `id, calendar_id, uid, recurrence_id, summary, description, location, starts_at, ends_at,
	all_day, start_date, end_date, time_zone, recurrence_rule, status, transparency, organizer_email,
	organizer_name, source_created_at, source_modified_at, sequence, raw, import_id, created_at, updated_at`

func scanEvent(scanner interface{ Scan(...any) error }) (Event, error) {
	var event Event
	err := scanner.Scan(&event.ID, &event.CalendarID, &event.UID, &event.RecurrenceID, &event.Summary,
		&event.Description, &event.Location, &event.StartsAt, &event.EndsAt, &event.AllDay, &event.StartDate,
		&event.EndDate, &event.TimeZone, &event.RecurrenceRule, &event.Status, &event.Transparency,
		&event.OrganizerEmail, &event.OrganizerName, &event.SourceCreatedAt, &event.SourceModifiedAt,
		&event.Sequence, &event.Raw, &event.ImportID, &event.CreatedAt, &event.UpdatedAt)
	return event, err
}

func (s *Store) readEvent(q queryer, id string, withRaw bool) (Event, error) {
	event, err := scanEvent(q.QueryRow(`SELECT `+eventColumns+` FROM calendar_events WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return event, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return event, err
	}
	if !withRaw {
		event.Raw = ""
	}
	return event, s.fillRelations(q, &event)
}

func (s *Store) fillRelations(q queryer, event *Event) error {
	event.Attendees = []Attendee{}
	rows, err := q.Query(`SELECT email, name, role, participation_status FROM calendar_event_attendees WHERE event_id = ? ORDER BY position`, event.ID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var attendee Attendee
		if err := rows.Scan(&attendee.Email, &attendee.Name, &attendee.Role, &attendee.ParticipationStatus); err != nil {
			rows.Close()
			return err
		}
		event.Attendees = append(event.Attendees, attendee)
	}
	rows.Close()
	event.People = []PersonLink{}
	rows, err = q.Query(`SELECT person_id, linked_by, reason, linked_at FROM calendar_event_people WHERE event_id = ? ORDER BY linked_at, person_id`, event.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var link PersonLink
		if err := rows.Scan(&link.PersonID, &link.LinkedBy, &link.Reason, &link.LinkedAt); err != nil {
			return err
		}
		event.People = append(event.People, link)
	}
	return rows.Err()
}

// GetEvent reads one event with its raw text, attendees and people.
func (s *Store) GetEvent(id string) (Event, error) { return s.readEvent(s.db, id, true) }

// EventQuery filters GET /events.
type EventQuery struct {
	CalendarID string
	From, To   int64
	Text       string
	PersonID   string
	Limit      int
	Offset     int
}

// MaxEventLimit is the most GET /events answers at once.
const MaxEventLimit = 1000

// Events lists events oldest first, without their raw text.
func (s *Store) Events(query EventQuery) ([]Event, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > MaxEventLimit {
		return nil, fmt.Errorf("%w: limit is at most %d", ErrInvalid, MaxEventLimit)
	}
	sqlText := `SELECT ` + eventColumns + ` FROM calendar_events WHERE 1 = 1`
	var args []any
	if query.CalendarID != "" {
		sqlText += ` AND calendar_id = ?`
		args = append(args, query.CalendarID)
	}
	if query.From != 0 {
		sqlText += ` AND starts_at >= ?`
		args = append(args, query.From)
	}
	if query.To != 0 {
		sqlText += ` AND starts_at < ?`
		args = append(args, query.To)
	}
	if query.Text != "" {
		sqlText += ` AND (summary LIKE ? OR location LIKE ? OR description LIKE ?)`
		like := "%" + query.Text + "%"
		args = append(args, like, like, like)
	}
	if query.PersonID != "" {
		sqlText += ` AND id IN (SELECT event_id FROM calendar_event_people WHERE person_id = ?)`
		args = append(args, query.PersonID)
	}
	sqlText += ` ORDER BY starts_at, seq LIMIT ? OFFSET ?`
	args = append(args, query.Limit, query.Offset)
	rows, err := s.db.Query(sqlText, args...)
	if err != nil {
		return nil, err
	}
	events := []Event{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		event.Raw = ""
		events = append(events, event)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range events {
		if err := s.fillRelations(s.db, &events[index]); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// LinkPerson records that an event was with a person, as an agent or the
// operator judged. The person must exist in people-store.
func (s *Store) LinkPerson(eventID string, link PersonLink) (Event, error) {
	link.PersonID = strings.TrimSpace(link.PersonID)
	link.Reason = strings.TrimSpace(link.Reason)
	if link.LinkedBy != LinkedByAgent && link.LinkedBy != LinkedByOperator {
		return Event{}, fmt.Errorf("%w: linked_by is %q or %q; %q links are made by imports", ErrInvalid, LinkedByAgent, LinkedByOperator, LinkedByAttendeeEmail)
	}
	if link.LinkedBy == LinkedByAgent && link.Reason == "" {
		return Event{}, fmt.Errorf("%w: an agent's link needs a reason", ErrInvalid)
	}
	if !strings.HasPrefix(link.PersonID, "person_") {
		return Event{}, fmt.Errorf("%w: person_id is a people-store id (person_000001)", ErrInvalid)
	}
	if _, err := s.readEvent(s.db, eventID, false); err != nil {
		return Event{}, err
	}
	exists, err := s.people.PersonExists(link.PersonID)
	if err != nil {
		return Event{}, fmt.Errorf("ask people-store about %s: %w", link.PersonID, err)
	}
	if !exists {
		return Event{}, fmt.Errorf("%w: people-store has no live %s", ErrInvalid, link.PersonID)
	}
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	result, err := s.db.Exec(`INSERT OR IGNORE INTO calendar_event_people (event_id, person_id, linked_by, reason, linked_at) VALUES (?, ?, ?, ?, ?)`,
		eventID, link.PersonID, link.LinkedBy, link.Reason, s.now().Unix())
	if err != nil {
		return Event{}, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return Event{}, fmt.Errorf("%w: %s is already linked to %s", ErrConflict, link.PersonID, eventID)
	}
	return s.readEvent(s.db, eventID, false)
}

// UnlinkPerson removes a person from an event. An attendee-email link comes
// back with the next import that changes the event.
func (s *Store) UnlinkPerson(eventID, personID string) error {
	s.writeMutex.Lock()
	defer s.writeMutex.Unlock()
	result, err := s.db.Exec(`DELETE FROM calendar_event_people WHERE event_id = ? AND person_id = ?`, eventID, personID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return fmt.Errorf("%w: %s is not linked to %s", ErrNotFound, personID, eventID)
	}
	return nil
}

// Revisions lists an event's earlier rows, newest first.
func (s *Store) Revisions(eventID string) ([]Revision, error) {
	if _, err := s.readEvent(s.db, eventID, false); err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`SELECT seq, reason, replaced_at, event_json FROM calendar_event_revisions WHERE event_id = ? ORDER BY seq DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var snapshot string
		if err := rows.Scan(&revision.RevisionID, &revision.Reason, &revision.ReplacedAt, &snapshot); err != nil {
			return nil, err
		}
		revision.Event = json.RawMessage(snapshot)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// Imports lists every import, newest first.
func (s *Store) Imports() ([]Import, error) {
	rows, err := s.db.Query(`SELECT id, calendar_id, file_name, file_sha256, created, updated, unchanged, people_linked, imported_at FROM calendar_imports ORDER BY seq DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	imports := []Import{}
	for rows.Next() {
		var record Import
		if err := rows.Scan(&record.ID, &record.CalendarID, &record.FileName, &record.FileSHA256, &record.Created,
			&record.Updated, &record.Unchanged, &record.PeopleLinked, &record.ImportedAt); err != nil {
			return nil, err
		}
		imports = append(imports, record)
	}
	return imports, rows.Err()
}

// Counts is GET /health's counts.
type Counts struct {
	Calendars int `json:"calendars"`
	Events    int `json:"events"`
	People    int `json:"people_links"`
	Imports   int `json:"imports"`
}

// Counts counts the rows.
func (s *Store) Counts() (Counts, error) {
	var counts Counts
	err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM calendars), (SELECT COUNT(*) FROM calendar_events),
		(SELECT COUNT(*) FROM calendar_event_people), (SELECT COUNT(*) FROM calendar_imports)`).
		Scan(&counts.Calendars, &counts.Events, &counts.People, &counts.Imports)
	return counts, err
}
