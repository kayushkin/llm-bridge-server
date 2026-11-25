// Package calendarstore keeps calendar events as their calendars wrote them:
// every VEVENT of an imported iCalendar file, whole, with who it was with as
// people-store ids.
//
// Events are cited elsewhere by their ids (calendar_event_000001), which
// reference-store lists, so a journal timeline or a note can link one as a
// chip.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every record has an owner (owner_principal_id), and
// every read is narrowed to an ownership.Scope.
package calendarstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// The type parts of the public ids: calendar_000001, calendar_event_000001,
// calendar_import_000001.
const (
	CalendarIDPrefix = "calendar"
	EventIDPrefix    = "calendar_event"
	ImportIDPrefix   = "calendar_import"
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
	return postgresstorage.Module{Schema: "calendar", Role: "calendar_module", Migrations: migrations}, nil
}

// Link sources: how a link from an event to a person was made.
const (
	LinkedByAttendeeEmail = "attendee-email"
	LinkedByAgent         = "agent"
	LinkedByOperator      = "operator"
)

// LinkSources is GET /vocabularies' link_sources. Only agent and operator may
// be sent to POST /events/{id}/people; attendee-email links are the import's.
var LinkSources = []string{LinkedByAttendeeEmail, LinkedByAgent, LinkedByOperator}

// Calendar is one calendar a source keeps, in one owner's records.
type Calendar struct {
	ID               string `json:"id"`
	OwnerPrincipalID string `json:"owner_principal_id"`
	Name             string `json:"name"`
	Source           string `json:"source"`
	SourceRef        string `json:"source_ref"`
	CreatedAt        int64  `json:"created_at"`
}

// PersonLink is one person an event was with.
type PersonLink struct {
	PersonID string `json:"person_id"`
	LinkedBy string `json:"linked_by"`
	Reason   string `json:"reason"`
	LinkedAt int64  `json:"linked_at"`
}

// Event is one stored calendar event. It has its calendar's owner.
type Event struct {
	ID               string       `json:"id"`
	OwnerPrincipalID string       `json:"owner_principal_id"`
	CalendarID       string       `json:"calendar_id"`
	UID              string       `json:"uid"`
	RecurrenceID     string       `json:"recurrence_id"`
	SourceEventID    string       `json:"source_event_id"`
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

// Import is one file read into a calendar. It has its calendar's owner.
type Import struct {
	ID               string `json:"id"`
	OwnerPrincipalID string `json:"owner_principal_id"`
	CalendarID       string `json:"calendar_id"`
	FileName         string `json:"file_name"`
	Format           string `json:"format"`
	FileSHA256       string `json:"file_sha256"`
	Created          int    `json:"created"`
	Updated          int    `json:"updated"`
	Unchanged        int    `json:"unchanged"`
	PeopleLinked     int    `json:"people_linked"`
	Skipped          int    `json:"skipped"`
	ImportedAt       int64  `json:"imported_at"`
}

// Revision is an event as it was before a change. It has the event's owner.
type Revision struct {
	RevisionID       int64           `json:"revision_id"`
	OwnerPrincipalID string          `json:"owner_principal_id"`
	Reason           string          `json:"reason"`
	ReplacedAt       int64           `json:"replaced_at"`
	Event            json.RawMessage `json:"event"`
}

// People is people-store, asked about one owner's address book.
type People interface {
	// PersonByEmail is the person in ownerPrincipalID's records holding the
	// address, or "" when none does.
	PersonByEmail(ctx context.Context, ownerPrincipalID, email string) (string, error)
	// PersonExists says whether ownerPrincipalID's records hold the person,
	// not deleted.
	PersonExists(ctx context.Context, ownerPrincipalID, personID string) (bool, error)
}

// Store keeps calendars, events and imports in the module's tables.
type Store struct {
	db     *sql.DB
	people People
	now    func() time.Time
}

// New is the store over db, the module's pool (Module), asking people about
// attendees' addresses and linked persons.
func New(db *sql.DB, people People) *Store {
	return &Store{db: db, people: people, now: time.Now}
}

// queryer is a pool or a transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// scopeCondition is the scope as a condition on column, numbered after the
// arguments already in args, which it appends to. A scope that was never set
// is an error, not "everyone".
func scopeCondition(scope ownership.Scope, column string, args []any) (string, []any, error) {
	if err := scope.Check(); err != nil {
		return "", nil, err
	}
	condition, scopeArgs := scope.Condition(column, len(args)+1)
	return condition, append(args, scopeArgs...), nil
}

func hashOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// EnsureCalendar returns the calendar ownerPrincipalID keeps with this source
// and source_ref, making it first if there is none. created says which.
func (s *Store) EnsureCalendar(ctx context.Context, ownerPrincipalID, name, source, sourceRef string) (calendar Calendar, created bool, err error) {
	if err := ownership.CheckPrincipalID("owner_principal_id", ownerPrincipalID); err != nil {
		return calendar, false, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	name, source, sourceRef = strings.TrimSpace(name), strings.TrimSpace(source), strings.TrimSpace(sourceRef)
	if name == "" || source == "" || sourceRef == "" {
		return calendar, false, fmt.Errorf("%w: name, source and source_ref are required", ErrInvalid)
	}
	var seq int64
	err = s.db.QueryRowContext(ctx, `INSERT INTO calendars (owner_principal_id, name, source, source_ref, created_at)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (owner_principal_id, source, source_ref) DO NOTHING RETURNING seq`,
		ownerPrincipalID, name, source, sourceRef, s.now().Unix()).Scan(&seq)
	switch {
	case err == nil:
		created = true
	case errors.Is(err, sql.ErrNoRows):
	default:
		return calendar, false, err
	}
	calendar, err = scanCalendar(s.db.QueryRowContext(ctx, `SELECT `+calendarColumns+` FROM calendars
		WHERE owner_principal_id = $1 AND source = $2 AND source_ref = $3`, ownerPrincipalID, source, sourceRef))
	return calendar, created, err
}

const calendarColumns = `seq, owner_principal_id, name, source, source_ref, created_at`

func scanCalendar(scanner interface{ Scan(...any) error }) (Calendar, error) {
	var calendar Calendar
	var seq int64
	err := scanner.Scan(&seq, &calendar.OwnerPrincipalID, &calendar.Name, &calendar.Source, &calendar.SourceRef, &calendar.CreatedAt)
	calendar.ID = postgresstorage.FormatRecordID(CalendarIDPrefix, seq)
	return calendar, err
}

// Calendars lists the calendars in scope in the order made.
func (s *Store) Calendars(ctx context.Context, scope ownership.Scope) ([]Calendar, error) {
	condition, args, err := scopeCondition(scope, "owner_principal_id", nil)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+calendarColumns+` FROM calendars WHERE `+condition+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	calendars := []Calendar{}
	for rows.Next() {
		calendar, err := scanCalendar(rows)
		if err != nil {
			return nil, err
		}
		calendars = append(calendars, calendar)
	}
	return calendars, rows.Err()
}

// calendarInScope reads one calendar. A malformed id, a missing calendar and
// one outside the scope are all not found.
func (s *Store) calendarInScope(ctx context.Context, scope ownership.Scope, id string) (Calendar, int64, error) {
	seq, err := postgresstorage.ParseRecordID(CalendarIDPrefix, id)
	if err != nil {
		return Calendar{}, 0, fmt.Errorf("%w: calendar %s", ErrNotFound, id)
	}
	condition, args, err := scopeCondition(scope, "owner_principal_id", []any{seq})
	if err != nil {
		return Calendar{}, 0, err
	}
	calendar, err := scanCalendar(s.db.QueryRowContext(ctx, `SELECT `+calendarColumns+` FROM calendars WHERE seq = $1 AND `+condition, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return calendar, 0, fmt.Errorf("%w: calendar %s", ErrNotFound, id)
	}
	return calendar, seq, err
}

// ImportICS reads an iCalendar file into a calendar in scope. An event is
// found again by (uid, recurrence_id): new ones are created, changed ones are
// updated after their old row is kept as a revision, and the same text again
// changes nothing. An event the file no longer holds is kept. Every attendee
// address is then asked of the calendar owner's people records, and the
// attendee-email links of each event the import touched are made again from
// the answers. The import and its events take the calendar's owner.
func (s *Store) ImportICS(ctx context.Context, scope ownership.Scope, calendarID, fileName string, content []byte) (Import, error) {
	calendar, calendarSeq, err := s.calendarInScope(ctx, scope, calendarID)
	if err != nil {
		return Import{}, err
	}
	parsed, err := ParseICS(content)
	if err != nil {
		return Import{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return s.importParsed(ctx, calendar, calendarSeq, fileName, ImportFormatICS, content, parsed)
}

// ImportGoogleEvents reads a JSON array of Google Calendar event resources
// into a calendar in scope whose source is google, by the same rules as
// ImportICS. An event is found first by Google's id, then by (uid,
// recurrence_id), which is how an event an .ics export brought is found and
// given its Google id rather than stored twice. An event deleted from Google
// and sent with only its id marks the row it finds cancelled, keeping the
// rest; one the store never held is counted in skipped.
func (s *Store) ImportGoogleEvents(ctx context.Context, scope ownership.Scope, calendarID, fileName string, content []byte) (Import, error) {
	calendar, calendarSeq, err := s.calendarInScope(ctx, scope, calendarID)
	if err != nil {
		return Import{}, err
	}
	if calendar.Source != "google" {
		return Import{}, fmt.Errorf("%w: calendar %s is kept by %s, not google", ErrInvalid, calendarID, calendar.Source)
	}
	parsed, err := ParseGoogleEvents(content)
	if err != nil {
		return Import{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return s.importParsed(ctx, calendar, calendarSeq, fileName, ImportFormatGoogleEventJSON, content, parsed)
}

// importParsed writes parsed events into a calendar in one transaction.
func (s *Store) importParsed(ctx context.Context, calendar Calendar, calendarSeq int64, fileName, format string, content []byte, parsed []ParsedEvent) (Import, error) {
	owner := calendar.OwnerPrincipalID
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
			personID, err := s.people.PersonByEmail(ctx, owner, attendee.Email)
			if err != nil {
				return Import{}, fmt.Errorf("ask people-store about an attendee: %w", err)
			}
			personOf[attendee.Email] = personID
		}
	}

	record := Import{OwnerPrincipalID: owner, CalendarID: calendar.ID, FileName: fileName, Format: format,
		FileSHA256: hashOf(string(content)), ImportedAt: s.now().Unix()}
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		// Imports into one calendar wait for each other, as they did when
		// one process wrote them one at a time.
		if _, err := tx.ExecContext(ctx, `SELECT seq FROM calendars WHERE seq = $1 FOR UPDATE`, calendarSeq); err != nil {
			return err
		}
		var importSeq int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO calendar_imports (owner_principal_id, calendar_seq, file_name, format, file_sha256, imported_at)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING seq`, owner, calendarSeq, fileName, format, record.FileSHA256, record.ImportedAt).Scan(&importSeq); err != nil {
			return fmt.Errorf("insert import: %w", err)
		}
		record.ID = postgresstorage.FormatRecordID(ImportIDPrefix, importSeq)
		seen := map[string]bool{}
		for _, event := range parsed {
			keys := []string{"uid\x00" + event.UID + "\x00" + event.RecurrenceID}
			if event.OnlyStatusKnown {
				keys = nil
			}
			if event.SourceEventID != "" {
				keys = append(keys, "id\x00"+event.SourceEventID)
			}
			for _, key := range keys {
				if seen[key] {
					return fmt.Errorf("%w: the import holds event %s (%s %s) twice", ErrInvalid, event.SourceEventID, event.UID, event.RecurrenceID)
				}
				seen[key] = true
			}
			outcome, eventSeq, err := s.upsertEvent(ctx, tx, owner, calendarSeq, importSeq, event)
			if err != nil {
				return err
			}
			switch outcome {
			case "created":
				record.Created++
			case "updated":
				record.Updated++
			case "status-updated":
				record.Updated++
				continue
			case "skipped":
				record.Skipped++
				continue
			default:
				record.Unchanged++
				continue
			}
			linked, err := relinkAttendees(ctx, tx, owner, eventSeq, event.Attendees, personOf, record.ImportedAt)
			if err != nil {
				return err
			}
			record.PeopleLinked += linked
		}
		if _, err := tx.ExecContext(ctx, `UPDATE calendar_imports SET created = $1, updated = $2, unchanged = $3, people_linked = $4, skipped = $5 WHERE seq = $6`,
			record.Created, record.Updated, record.Unchanged, record.PeopleLinked, record.Skipped, importSeq); err != nil {
			return fmt.Errorf("record the import's counts: %w", err)
		}
		return nil
	})
	if err != nil {
		return Import{}, err
	}
	return record, nil
}

func (s *Store) upsertEvent(ctx context.Context, tx *sql.Tx, owner string, calendarSeq, importSeq int64, event ParsedEvent) (outcome string, eventSeq int64, err error) {
	rawHash := hashOf(event.Raw)
	eventSeq, existingHash, err := findEvent(ctx, tx, calendarSeq, event)
	now := s.now().Unix()
	if event.OnlyStatusKnown {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return "skipped", 0, nil
		case err != nil:
			return "", 0, err
		case existingHash == rawHash:
			return "unchanged", eventSeq, nil
		}
		if err := s.keepRevision(ctx, tx, eventSeq, "import", now); err != nil {
			return "", 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE calendar_events SET status = $1, raw = $2, raw_hash = $3, import_seq = $4, updated_at = $5 WHERE seq = $6`,
			event.Status, event.Raw, rawHash, importSeq, now, eventSeq); err != nil {
			return "", 0, fmt.Errorf("update event: %w", err)
		}
		return "status-updated", eventSeq, nil
	}
	columns := []any{event.Summary, event.Description, event.Location, event.StartsAt, event.EndsAt, event.AllDay,
		event.StartDate, event.EndDate, event.TimeZone, event.RecurrenceRule, event.Status, event.Transparency,
		event.OrganizerEmail, event.OrganizerName, event.SourceCreatedAt, event.SourceModifiedAt, event.Sequence,
		event.Raw, rawHash, importSeq, event.SourceEventID}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		args := append([]any{owner, calendarSeq, event.UID, event.RecurrenceID}, columns...)
		args = append(args, now, now)
		if err := tx.QueryRowContext(ctx, `INSERT INTO calendar_events (owner_principal_id, calendar_seq, uid, recurrence_id,
			summary, description, location, starts_at, ends_at, all_day, start_date, end_date, time_zone,
			recurrence_rule, status, transparency, organizer_email, organizer_name, source_created_at,
			source_modified_at, sequence, raw, raw_hash, import_seq, source_event_id, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27)
			RETURNING seq`, args...).Scan(&eventSeq); err != nil {
			return "", 0, fmt.Errorf("insert event: %w", err)
		}
		return "created", eventSeq, writeAttendees(ctx, tx, owner, eventSeq, event.Attendees)
	case err != nil:
		return "", 0, err
	case existingHash == rawHash:
		return "unchanged", eventSeq, nil
	}
	if err := s.keepRevision(ctx, tx, eventSeq, "import", now); err != nil {
		return "", 0, err
	}
	args := append(columns, now, eventSeq)
	if _, err := tx.ExecContext(ctx, `UPDATE calendar_events SET summary = $1, description = $2, location = $3, starts_at = $4,
		ends_at = $5, all_day = $6, start_date = $7, end_date = $8, time_zone = $9, recurrence_rule = $10, status = $11,
		transparency = $12, organizer_email = $13, organizer_name = $14, source_created_at = $15, source_modified_at = $16,
		sequence = $17, raw = $18, raw_hash = $19, import_seq = $20, source_event_id = COALESCE(NULLIF($21, ''), source_event_id),
		updated_at = $22 WHERE seq = $23`, args...); err != nil {
		return "", 0, fmt.Errorf("update event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_event_attendees WHERE event_seq = $1`, eventSeq); err != nil {
		return "", 0, err
	}
	return "updated", eventSeq, writeAttendees(ctx, tx, owner, eventSeq, event.Attendees)
}

// findEvent finds the row an imported event is: by Google's id when it has
// one, then by (uid, recurrence_id). A row found by the key that already holds
// a different Google id, or found by id under a different key, is refused: the
// two sources disagree about which event this is.
func findEvent(ctx context.Context, tx *sql.Tx, calendarSeq int64, event ParsedEvent) (eventSeq int64, rawHash string, err error) {
	if event.SourceEventID != "" {
		var uid, recurrenceID string
		err = tx.QueryRowContext(ctx, `SELECT seq, raw_hash, uid, recurrence_id FROM calendar_events WHERE calendar_seq = $1 AND source_event_id = $2`,
			calendarSeq, event.SourceEventID).Scan(&eventSeq, &rawHash, &uid, &recurrenceID)
		if err == nil && !event.OnlyStatusKnown && (uid != event.UID || recurrenceID != event.RecurrenceID) {
			return 0, "", fmt.Errorf("%w: Google event %s is stored as %s (%s %s) but now names %s %s",
				ErrConflict, event.SourceEventID, postgresstorage.FormatRecordID(EventIDPrefix, eventSeq), uid, recurrenceID, event.UID, event.RecurrenceID)
		}
		if err == nil || !errors.Is(err, sql.ErrNoRows) || event.OnlyStatusKnown {
			return eventSeq, rawHash, err
		}
	}
	var sourceEventID string
	err = tx.QueryRowContext(ctx, `SELECT seq, raw_hash, source_event_id FROM calendar_events WHERE calendar_seq = $1 AND uid = $2 AND recurrence_id = $3`,
		calendarSeq, event.UID, event.RecurrenceID).Scan(&eventSeq, &rawHash, &sourceEventID)
	if err == nil && event.SourceEventID != "" && sourceEventID != "" && sourceEventID != event.SourceEventID {
		return 0, "", fmt.Errorf("%w: %s (%s %s) is Google event %s, not %s",
			ErrConflict, postgresstorage.FormatRecordID(EventIDPrefix, eventSeq), event.UID, event.RecurrenceID, sourceEventID, event.SourceEventID)
	}
	return eventSeq, rawHash, err
}

func writeAttendees(ctx context.Context, tx *sql.Tx, owner string, eventSeq int64, attendees []Attendee) error {
	for position, attendee := range attendees {
		if _, err := tx.ExecContext(ctx, `INSERT INTO calendar_event_attendees (owner_principal_id, event_seq, position, email, name, role, participation_status)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, owner, eventSeq, position, attendee.Email, attendee.Name, attendee.Role, attendee.ParticipationStatus); err != nil {
			return fmt.Errorf("insert attendee: %w", err)
		}
	}
	return nil
}

// relinkAttendees makes an event's attendee-email links again. A person an
// agent or the operator linked keeps that link.
func relinkAttendees(ctx context.Context, tx *sql.Tx, owner string, eventSeq int64, attendees []Attendee, personOf map[string]string, now int64) (int, error) {
	if _, err := tx.ExecContext(ctx, `DELETE FROM calendar_event_people WHERE event_seq = $1 AND linked_by = $2`, eventSeq, LinkedByAttendeeEmail); err != nil {
		return 0, err
	}
	linked := 0
	for _, attendee := range attendees {
		personID := personOf[attendee.Email]
		if personID == "" {
			continue
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO calendar_event_people (owner_principal_id, event_seq, person_id, linked_by, reason, linked_at)
			VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (event_seq, person_id) DO NOTHING`,
			owner, eventSeq, personID, LinkedByAttendeeEmail, "people-store holds the attendee's address", now)
		if err != nil {
			return 0, err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if count > 0 {
			linked++
		}
	}
	return linked, nil
}

func (s *Store) keepRevision(ctx context.Context, tx *sql.Tx, eventSeq int64, reason string, now int64) error {
	event, err := readEvent(ctx, tx, ownership.EveryOwner(), eventSeq, true)
	if err != nil {
		return err
	}
	snapshot, err := json.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO calendar_event_revisions (owner_principal_id, event_seq, reason, replaced_at, event_json) VALUES ($1, $2, $3, $4, $5)`,
		event.OwnerPrincipalID, eventSeq, reason, now, string(snapshot))
	return err
}

const eventColumns = `seq, owner_principal_id, calendar_seq, uid, recurrence_id, source_event_id, summary, description, location, starts_at, ends_at,
	all_day, start_date, end_date, time_zone, recurrence_rule, status, transparency, organizer_email,
	organizer_name, source_created_at, source_modified_at, sequence, raw, import_seq, created_at, updated_at`

func scanEvent(scanner interface{ Scan(...any) error }) (Event, int64, error) {
	var event Event
	var seq, calendarSeq, importSeq int64
	err := scanner.Scan(&seq, &event.OwnerPrincipalID, &calendarSeq, &event.UID, &event.RecurrenceID, &event.SourceEventID, &event.Summary,
		&event.Description, &event.Location, &event.StartsAt, &event.EndsAt, &event.AllDay, &event.StartDate,
		&event.EndDate, &event.TimeZone, &event.RecurrenceRule, &event.Status, &event.Transparency,
		&event.OrganizerEmail, &event.OrganizerName, &event.SourceCreatedAt, &event.SourceModifiedAt,
		&event.Sequence, &event.Raw, &importSeq, &event.CreatedAt, &event.UpdatedAt)
	event.ID = postgresstorage.FormatRecordID(EventIDPrefix, seq)
	event.CalendarID = postgresstorage.FormatRecordID(CalendarIDPrefix, calendarSeq)
	event.ImportID = postgresstorage.FormatRecordID(ImportIDPrefix, importSeq)
	return event, seq, err
}

// readEvent reads one event in scope with its attendees and people. One
// missing or outside the scope is not found.
func readEvent(ctx context.Context, q queryer, scope ownership.Scope, seq int64, withRaw bool) (Event, error) {
	condition, args, err := scopeCondition(scope, "owner_principal_id", []any{seq})
	if err != nil {
		return Event{}, err
	}
	event, _, err := scanEvent(q.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM calendar_events WHERE seq = $1 AND `+condition, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return event, fmt.Errorf("%w: %s", ErrNotFound, postgresstorage.FormatRecordID(EventIDPrefix, seq))
	}
	if err != nil {
		return event, err
	}
	if !withRaw {
		event.Raw = ""
	}
	return event, fillRelations(ctx, q, seq, &event)
}

// eventInScope reads an event by its public id. A malformed id is not found.
func (s *Store) eventInScope(ctx context.Context, scope ownership.Scope, id string, withRaw bool) (Event, int64, error) {
	seq, err := postgresstorage.ParseRecordID(EventIDPrefix, id)
	if err != nil {
		return Event{}, 0, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	event, err := readEvent(ctx, s.db, scope, seq, withRaw)
	return event, seq, err
}

func fillRelations(ctx context.Context, q queryer, seq int64, event *Event) error {
	event.Attendees = []Attendee{}
	rows, err := q.QueryContext(ctx, `SELECT email, name, role, participation_status FROM calendar_event_attendees WHERE event_seq = $1 ORDER BY position`, seq)
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
	if err := rows.Err(); err != nil {
		return err
	}
	event.People = []PersonLink{}
	// COLLATE "C" orders person ids byte by byte, as SQLite did.
	rows, err = q.QueryContext(ctx, `SELECT person_id, linked_by, reason, linked_at FROM calendar_event_people WHERE event_seq = $1 ORDER BY linked_at, person_id COLLATE "C"`, seq)
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

// GetEvent reads one event in scope with its raw text, attendees and people.
func (s *Store) GetEvent(ctx context.Context, scope ownership.Scope, id string) (Event, error) {
	event, _, err := s.eventInScope(ctx, scope, id, true)
	return event, err
}

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

// likePattern is text as an ILIKE pattern matching it anywhere, with the
// pattern's own marks escaped so the text is matched as written.
func likePattern(text string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
	return "%" + escaped + "%"
}

// Events lists the events in scope oldest first, without their raw text.
func (s *Store) Events(ctx context.Context, scope ownership.Scope, query EventQuery) ([]Event, error) {
	if query.Limit <= 0 {
		query.Limit = 100
	}
	if query.Limit > MaxEventLimit {
		return nil, fmt.Errorf("%w: limit is at most %d", ErrInvalid, MaxEventLimit)
	}
	if query.Offset < 0 {
		return nil, fmt.Errorf("%w: offset is at least 0", ErrInvalid)
	}
	condition, args, err := scopeCondition(scope, "owner_principal_id", nil)
	if err != nil {
		return nil, err
	}
	sqlText := `SELECT ` + eventColumns + ` FROM calendar_events WHERE ` + condition
	next := func(value any) string {
		args = append(args, value)
		return "$" + strconv.Itoa(len(args))
	}
	if query.CalendarID != "" {
		calendarSeq, err := postgresstorage.ParseRecordID(CalendarIDPrefix, query.CalendarID)
		if err != nil {
			// No calendar has a malformed id, so none of its events match.
			return []Event{}, nil
		}
		sqlText += ` AND calendar_seq = ` + next(calendarSeq)
	}
	if query.From != 0 {
		sqlText += ` AND starts_at >= ` + next(query.From)
	}
	if query.To != 0 {
		sqlText += ` AND starts_at < ` + next(query.To)
	}
	if query.Text != "" {
		like := next(likePattern(query.Text))
		sqlText += ` AND (summary ILIKE ` + like + ` OR location ILIKE ` + like + ` OR description ILIKE ` + like + `)`
	}
	if query.PersonID != "" {
		sqlText += ` AND seq IN (SELECT event_seq FROM calendar_event_people WHERE person_id = ` + next(query.PersonID) + `)`
	}
	sqlText += ` ORDER BY starts_at, seq LIMIT ` + next(query.Limit) + ` OFFSET ` + next(query.Offset)
	rows, err := s.db.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, err
	}
	events := []Event{}
	var seqs []int64
	for rows.Next() {
		event, seq, err := scanEvent(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		event.Raw = ""
		events = append(events, event)
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for index := range events {
		if err := fillRelations(ctx, s.db, seqs[index], &events[index]); err != nil {
			return nil, err
		}
	}
	return events, nil
}

// LinkPerson records that an event in scope was with a person, as an agent or
// the operator judged. The person must be live in the event owner's people
// records: one only another owner keeps is refused as an unknown one is.
func (s *Store) LinkPerson(ctx context.Context, scope ownership.Scope, eventID string, link PersonLink) (Event, error) {
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
	event, seq, err := s.eventInScope(ctx, scope, eventID, false)
	if err != nil {
		return Event{}, err
	}
	exists, err := s.people.PersonExists(ctx, event.OwnerPrincipalID, link.PersonID)
	if err != nil {
		return Event{}, fmt.Errorf("ask people-store about %s: %w", link.PersonID, err)
	}
	if !exists {
		return Event{}, fmt.Errorf("%w: people-store has no live %s", ErrInvalid, link.PersonID)
	}
	result, err := s.db.ExecContext(ctx, `INSERT INTO calendar_event_people (owner_principal_id, event_seq, person_id, linked_by, reason, linked_at)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT (event_seq, person_id) DO NOTHING`,
		event.OwnerPrincipalID, seq, link.PersonID, link.LinkedBy, link.Reason, s.now().Unix())
	if err != nil {
		return Event{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return Event{}, err
	}
	if count == 0 {
		return Event{}, fmt.Errorf("%w: %s is already linked to %s", ErrConflict, link.PersonID, eventID)
	}
	return readEvent(ctx, s.db, scope, seq, false)
}

// UnlinkPerson removes a person from an event in scope. An attendee-email
// link comes back with the next import that changes the event.
func (s *Store) UnlinkPerson(ctx context.Context, scope ownership.Scope, eventID, personID string) error {
	_, seq, err := s.eventInScope(ctx, scope, eventID, false)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM calendar_event_people WHERE event_seq = $1 AND person_id = $2`, seq, personID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("%w: %s is not linked to %s", ErrNotFound, personID, eventID)
	}
	return nil
}

// Revisions lists the earlier rows of an event in scope, newest first.
func (s *Store) Revisions(ctx context.Context, scope ownership.Scope, eventID string) ([]Revision, error) {
	_, seq, err := s.eventInScope(ctx, scope, eventID, false)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, owner_principal_id, reason, replaced_at, event_json FROM calendar_event_revisions WHERE event_seq = $1 ORDER BY seq DESC`, seq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	revisions := []Revision{}
	for rows.Next() {
		var revision Revision
		var snapshot string
		if err := rows.Scan(&revision.RevisionID, &revision.OwnerPrincipalID, &revision.Reason, &revision.ReplacedAt, &snapshot); err != nil {
			return nil, err
		}
		revision.Event = json.RawMessage(snapshot)
		revisions = append(revisions, revision)
	}
	return revisions, rows.Err()
}

// Imports lists the imports in scope, newest first.
func (s *Store) Imports(ctx context.Context, scope ownership.Scope) ([]Import, error) {
	condition, args, err := scopeCondition(scope, "owner_principal_id", nil)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT seq, owner_principal_id, calendar_seq, file_name, format, file_sha256, created, updated, unchanged,
		people_linked, skipped, imported_at FROM calendar_imports WHERE `+condition+` ORDER BY seq DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	imports := []Import{}
	for rows.Next() {
		var record Import
		var seq, calendarSeq int64
		if err := rows.Scan(&seq, &record.OwnerPrincipalID, &calendarSeq, &record.FileName, &record.Format, &record.FileSHA256, &record.Created,
			&record.Updated, &record.Unchanged, &record.PeopleLinked, &record.Skipped, &record.ImportedAt); err != nil {
			return nil, err
		}
		record.ID = postgresstorage.FormatRecordID(ImportIDPrefix, seq)
		record.CalendarID = postgresstorage.FormatRecordID(CalendarIDPrefix, calendarSeq)
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

// Counts counts the rows in scope.
func (s *Store) Counts(ctx context.Context, scope ownership.Scope) (Counts, error) {
	condition, args, err := scopeCondition(scope, "owner_principal_id", nil)
	if err != nil {
		return Counts{}, err
	}
	var counts Counts
	err = s.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM calendars WHERE `+condition+`),
		(SELECT COUNT(*) FROM calendar_events WHERE `+condition+`),
		(SELECT COUNT(*) FROM calendar_event_people WHERE `+condition+`),
		(SELECT COUNT(*) FROM calendar_imports WHERE `+condition+`)`, args...).
		Scan(&counts.Calendars, &counts.Events, &counts.People, &counts.Imports)
	return counts, err
}
