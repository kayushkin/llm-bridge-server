// Package timeledgerstore keeps an account of what someone did: time entries
// over spans of time, nested and overlapping, each linked by id to the records
// that brought it in and the records that show whether it happened, and
// reviewed by a model and by the person whose account it is.
//
// Entries, kinds and reviews are cited elsewhere by their ids
// (time_entry_000001, time_entry_kind_000001, time_entry_review_000001),
// which reference-store lists, so a note or a timeline can link one as a chip.
//
// The records live in Postgres, in the schema of Module, through
// postgresstorage. The server provisions, migrates and opens the module, then
// hands the pool to New. Every record has an owner (owner_principal_id), and
// every read and write is narrowed to an ownership.Scope.
package timeledgerstore

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/postgresstorage"
)

var (
	ErrNotFound = errors.New("not found")
	ErrInvalid  = errors.New("invalid")
	ErrConflict = errors.New("conflict")
)

// The type parts of the public ids: time_entry_000001,
// time_entry_kind_000001, time_entry_review_000001.
const (
	EntryIDPrefix  = "time_entry"
	KindIDPrefix   = "time_entry_kind"
	ReviewIDPrefix = "time_entry_review"
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
	return postgresstorage.Module{Schema: "time_ledger", Role: "time_ledger_module", Migrations: migrations}, nil
}

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

// Kind is a label an owner's entries can carry; an entry carries any number.
type Kind struct {
	ID               string `json:"id"`
	OwnerPrincipalID string `json:"owner_principal_id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	CreatedBy        string `json:"created_by"`
	CreatedByKind    string `json:"created_by_kind"`
	CreatedAt        int64  `json:"created_at"`
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

// Link is one record linked to an entry. It has the entry's owner.
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

// Review is one judgment of whether an entry happened. It has the entry's
// owner.
type Review struct {
	ID                         string          `json:"id"`
	OwnerPrincipalID           string          `json:"owner_principal_id"`
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
	OwnerPrincipalID  string           `json:"owner_principal_id"`
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

// Store keeps entries, kinds, reviews and links in the module's tables.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// New is the store over db, the module's pool (Module).
func New(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// SeedKinds are the kinds an owner's account starts with: the first time an
// owner lists kinds or makes one, and holds none, these are made for them.
// Anyone may add more with POST /kinds.
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

// seedActor is who makes an owner's first kinds.
var seedActor = Actor{By: "time-ledger-store seed", Kind: ActorProgram}

// queryer is a pool or a transaction.
type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// arguments numbers a query's arguments as they are added.
type arguments []any

func (args *arguments) add(value any) string {
	*args = append(*args, value)
	return "$" + strconv.Itoa(len(*args))
}

// scopeCondition is the scope as a condition on column, its argument added
// to args. A scope that was never set is an error, not "everyone".
func scopeCondition(scope ownership.Scope, column string, args *arguments) (string, error) {
	if err := scope.Check(); err != nil {
		return "", err
	}
	if scope.IsEveryOwner() {
		return "TRUE", nil
	}
	return column + " = " + args.add(scope.OwnerPrincipalID()), nil
}

// lockOwner serializes writes to one owner's account for the rest of the
// transaction, so the checks a write makes (one source per entry, children
// inside parents) hold when it commits.
func lockOwner(ctx context.Context, tx *sql.Tx, ownerPrincipalID string) error {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "timeledgerstore.owner."+ownerPrincipalID); err != nil {
		return fmt.Errorf("locking the account of %s: %w", ownerPrincipalID, err)
	}
	return nil
}

// isUniqueViolation reports a write a uniqueness rule refused.
func isUniqueViolation(err error) bool {
	var postgresError *pgconn.PgError
	return errors.As(err, &postgresError) && postgresError.Code == "23505"
}

func checkOwner(ownerPrincipalID string) error {
	if err := ownership.CheckPrincipalID("owner_principal_id", ownerPrincipalID); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return nil
}

var kindNamePattern = regexp.MustCompile(`^[a-z][a-z0-9-]*( [a-z0-9-]+)*$`)

func insertKind(ctx context.Context, tx *sql.Tx, ownerPrincipalID, name, description string, actor Actor, now int64) (int64, error) {
	var seq int64
	err := tx.QueryRowContext(ctx, `INSERT INTO time_entry_kinds (owner_principal_id, name, description, created_by, created_by_kind, created_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING seq`, ownerPrincipalID, name, description, actor.By, actor.Kind, now).Scan(&seq)
	if isUniqueViolation(err) {
		return 0, fmt.Errorf("%w: a kind named %q exists", ErrConflict, name)
	}
	return seq, err
}

// seedKindsOf makes SeedKinds for an owner who holds no kind. The caller
// holds the owner's lock.
func (s *Store) seedKindsOf(ctx context.Context, tx *sql.Tx, ownerPrincipalID string) error {
	var held int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM time_entry_kinds WHERE owner_principal_id = $1`, ownerPrincipalID).Scan(&held); err != nil {
		return err
	}
	if held > 0 {
		return nil
	}
	now := s.now().Unix()
	for _, kind := range SeedKinds {
		if _, err := insertKind(ctx, tx, ownerPrincipalID, kind.Name, kind.Description, seedActor, now); err != nil {
			return fmt.Errorf("seeding kind %s for %s: %w", kind.Name, ownerPrincipalID, err)
		}
	}
	return nil
}

// ensureSeedKinds seeds an owner's kinds when the owner holds none.
func (s *Store) ensureSeedKinds(ctx context.Context, ownerPrincipalID string) error {
	var held bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM time_entry_kinds WHERE owner_principal_id = $1)`, ownerPrincipalID).Scan(&held); err != nil {
		return err
	}
	if held {
		return nil
	}
	return postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockOwner(ctx, tx, ownerPrincipalID); err != nil {
			return err
		}
		return s.seedKindsOf(ctx, tx, ownerPrincipalID)
	})
}

// CreateKind adds a kind to an owner's account.
func (s *Store) CreateKind(ctx context.Context, ownerPrincipalID, name, description string, actor Actor) (Kind, error) {
	if err := checkOwner(ownerPrincipalID); err != nil {
		return Kind{}, err
	}
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
	var seq int64
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockOwner(ctx, tx, ownerPrincipalID); err != nil {
			return err
		}
		if err := s.seedKindsOf(ctx, tx, ownerPrincipalID); err != nil {
			return err
		}
		var err error
		seq, err = insertKind(ctx, tx, ownerPrincipalID, name, description, actor, s.now().Unix())
		return err
	})
	if err != nil {
		return Kind{}, err
	}
	return s.Kind(ctx, ownership.OwnedBy(ownerPrincipalID), postgresstorage.FormatRecordID(KindIDPrefix, seq))
}

const kindColumns = `seq, owner_principal_id, name, description, created_by, created_by_kind, created_at`

func scanKind(row interface{ Scan(...any) error }) (Kind, error) {
	var kind Kind
	var seq int64
	err := row.Scan(&seq, &kind.OwnerPrincipalID, &kind.Name, &kind.Description, &kind.CreatedBy, &kind.CreatedByKind, &kind.CreatedAt)
	kind.ID = postgresstorage.FormatRecordID(KindIDPrefix, seq)
	return kind, err
}

// Kind reads one kind. A malformed id, a missing kind and one outside the
// scope are all not found.
func (s *Store) Kind(ctx context.Context, scope ownership.Scope, id string) (Kind, error) {
	seq, err := postgresstorage.ParseRecordID(KindIDPrefix, id)
	if err != nil {
		return Kind{}, fmt.Errorf("%w: kind %s", ErrNotFound, id)
	}
	args := arguments{seq}
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return Kind{}, err
	}
	kind, err := scanKind(s.db.QueryRowContext(ctx, `SELECT `+kindColumns+` FROM time_entry_kinds WHERE seq = $1 AND `+condition, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return Kind{}, fmt.Errorf("%w: kind %s", ErrNotFound, id)
	}
	return kind, err
}

// Kinds lists the kinds in scope by name. A scope of one owner who holds no
// kind yet gets SeedKinds first.
func (s *Store) Kinds(ctx context.Context, scope ownership.Scope) ([]Kind, error) {
	if err := scope.Check(); err != nil {
		return nil, err
	}
	if !scope.IsEveryOwner() {
		if err := s.ensureSeedKinds(ctx, scope.OwnerPrincipalID()); err != nil {
			return nil, err
		}
	}
	var args arguments
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+kindColumns+` FROM time_entry_kinds WHERE `+condition+` ORDER BY name, owner_principal_id`, args...)
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

// CreateEntry makes an entry in an owner's account, with its kinds and links,
// in one transaction. A source link that already belongs to another of the
// owner's entries is a conflict naming it.
func (s *Store) CreateEntry(ctx context.Context, ownerPrincipalID string, input EntryInput) (TimeEntry, error) {
	if err := checkOwner(ownerPrincipalID); err != nil {
		return TimeEntry{}, err
	}
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
	var seq int64
	err := postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockOwner(ctx, tx, ownerPrincipalID); err != nil {
			return err
		}
		now := s.now().Unix()
		var parentSeq sql.NullInt64
		parentSetBy, parentSetByKind := "", ""
		if input.ParentTimeEntryID != "" {
			number, err := checkParent(ctx, tx, ownerPrincipalID, 0, input.ParentTimeEntryID, input.StartsAt, input.EndsAt)
			if err != nil {
				return err
			}
			parentSeq = sql.NullInt64{Int64: number, Valid: true}
			parentSetBy, parentSetByKind = actor.By, actor.Kind
		}
		placeBy, placeByKind := "", ""
		if input.Place != "" {
			placeBy, placeByKind = actor.By, actor.Kind
		}
		err := tx.QueryRowContext(ctx, `INSERT INTO time_entries (owner_principal_id, title, title_by, title_by_kind, span_by, span_by_kind, starts_at, ends_at,
			all_day, start_date, end_date, time_zone, place, place_by, place_by_kind, parent_seq, parent_set_by, parent_set_by_kind, created_by,
			created_by_kind, created_reason, created_at, updated_at, evidence_changed_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24) RETURNING seq`,
			ownerPrincipalID, input.Title, actor.By, actor.Kind, actor.By, actor.Kind, input.StartsAt, input.EndsAt,
			input.AllDay, input.StartDate, input.EndDate, input.TimeZone, input.Place, placeBy, placeByKind, parentSeq, parentSetBy, parentSetByKind, actor.By,
			actor.Kind, input.Reason, now, now, now).Scan(&seq)
		if err != nil {
			return err
		}
		for _, kindID := range input.KindIDs {
			if err := assignKind(ctx, tx, ownerPrincipalID, seq, kindID, actor, input.Reason, now); err != nil {
				return err
			}
		}
		for _, link := range input.Links {
			if link.Reason == "" {
				link.Reason = input.Reason
			}
			if _, err := addLink(ctx, tx, ownerPrincipalID, seq, link, actor, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return s.Entry(ctx, ownership.OwnedBy(ownerPrincipalID), postgresstorage.FormatRecordID(EntryIDPrefix, seq), false)
}

// checkParent refuses a parent that is missing, deleted, another owner's,
// does not contain the span, or is the entry itself or one of its
// descendants. entrySeq is 0 for an entry not made yet. It answers the
// parent's number.
func checkParent(ctx context.Context, tx *sql.Tx, ownerPrincipalID string, entrySeq int64, parentID string, startsAt, endsAt int64) (int64, error) {
	parentSeq, err := postgresstorage.ParseRecordID(EntryIDPrefix, parentID)
	if err != nil {
		return 0, fmt.Errorf("%w: parent %s does not exist", ErrInvalid, parentID)
	}
	var parentStarts, parentEnds int64
	var deletedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT starts_at, ends_at, deleted_at FROM time_entries WHERE seq = $1 AND owner_principal_id = $2`,
		parentSeq, ownerPrincipalID).Scan(&parentStarts, &parentEnds, &deletedAt)
	if errors.Is(err, sql.ErrNoRows) || deletedAt.Valid {
		return 0, fmt.Errorf("%w: parent %s does not exist", ErrInvalid, parentID)
	}
	if err != nil {
		return 0, err
	}
	if startsAt < parentStarts || endsAt > parentEnds {
		return 0, fmt.Errorf("%w: the entry (%d–%d) does not lie inside parent %s (%d–%d)", ErrConflict, startsAt, endsAt, parentID, parentStarts, parentEnds)
	}
	for ancestor := parentSeq; ancestor != 0; {
		if ancestor == entrySeq {
			return 0, fmt.Errorf("%w: %s cannot sit inside itself", ErrConflict, postgresstorage.FormatRecordID(EntryIDPrefix, entrySeq))
		}
		var next sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT parent_seq FROM time_entries WHERE seq = $1`, ancestor).Scan(&next); err != nil {
			return 0, err
		}
		ancestor = next.Int64
	}
	return parentSeq, nil
}

func assignKind(ctx context.Context, tx *sql.Tx, ownerPrincipalID string, entrySeq int64, kindID string, actor Actor, reason string, now int64) error {
	kindSeq, err := postgresstorage.ParseRecordID(KindIDPrefix, kindID)
	if err != nil {
		return fmt.Errorf("%w: kind %s does not exist", ErrInvalid, kindID)
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM time_entry_kinds WHERE seq = $1 AND owner_principal_id = $2)`, kindSeq, ownerPrincipalID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("%w: kind %s does not exist", ErrInvalid, kindID)
	}
	var live bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM time_entry_kind_assignments WHERE time_entry_seq = $1 AND kind_seq = $2 AND removed_at IS NULL)`,
		entrySeq, kindSeq).Scan(&live); err != nil {
		return err
	}
	if live {
		return nil
	}
	if actor.Kind != ActorOperator {
		var removedByOperator bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM time_entry_kind_assignments WHERE time_entry_seq = $1 AND kind_seq = $2 AND removed_by_kind = 'operator')`,
			entrySeq, kindSeq).Scan(&removedByOperator); err != nil {
			return err
		}
		if removedByOperator {
			return fmt.Errorf("%w: the operator took kind %s off %s", ErrConflict, kindID, postgresstorage.FormatRecordID(EntryIDPrefix, entrySeq))
		}
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO time_entry_kind_assignments (owner_principal_id, time_entry_seq, kind_seq, assigned_by, assigned_by_kind, reason, assigned_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`, ownerPrincipalID, entrySeq, kindSeq, actor.By, actor.Kind, reason, now)
	return err
}

// entryInScope finds an entry's number and owner within scope. A malformed
// id, a missing entry and one outside the scope are all not found.
func entryInScope(ctx context.Context, db queryer, scope ownership.Scope, entryID string) (int64, string, error) {
	seq, err := postgresstorage.ParseRecordID(EntryIDPrefix, entryID)
	if err != nil {
		return 0, "", fmt.Errorf("%w: time entry %s", ErrNotFound, entryID)
	}
	args := arguments{seq}
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return 0, "", err
	}
	var owner string
	err = db.QueryRowContext(ctx, `SELECT owner_principal_id FROM time_entries WHERE seq = $1 AND `+condition, args...).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("%w: time entry %s", ErrNotFound, entryID)
	}
	return seq, owner, err
}

// writeOnEntry runs work in a transaction holding the lock of the owner of an
// entry in scope. live refuses a deleted entry.
func (s *Store) writeOnEntry(ctx context.Context, scope ownership.Scope, entryID string, live bool, work func(tx *sql.Tx, seq int64, owner string) error) error {
	seq, owner, err := entryInScope(ctx, s.db, scope, entryID)
	if err != nil {
		return err
	}
	return postgresstorage.InTransaction(ctx, s.db, func(tx *sql.Tx) error {
		if err := lockOwner(ctx, tx, owner); err != nil {
			return err
		}
		if live {
			var deletedAt sql.NullInt64
			if err := tx.QueryRowContext(ctx, `SELECT deleted_at FROM time_entries WHERE seq = $1`, seq).Scan(&deletedAt); err != nil {
				return err
			}
			if deletedAt.Valid {
				return fmt.Errorf("%w: time entry %s was deleted", ErrConflict, entryID)
			}
		}
		return work(tx, seq, owner)
	})
}

// AssignKind puts one of the entry owner's kinds on an entry. Putting on one
// it carries changes nothing.
func (s *Store) AssignKind(ctx context.Context, scope ownership.Scope, entryID, kindID string, actor Actor, reason string) (TimeEntry, error) {
	if err := actor.validate(); err != nil {
		return TimeEntry{}, err
	}
	var owner string
	err := s.writeOnEntry(ctx, scope, entryID, true, func(tx *sql.Tx, seq int64, entryOwner string) error {
		owner = entryOwner
		return assignKind(ctx, tx, entryOwner, seq, kindID, actor, reason, s.now().Unix())
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return s.Entry(ctx, ownership.OwnedBy(owner), entryID, false)
}

// RemoveKind takes a kind off an entry.
func (s *Store) RemoveKind(ctx context.Context, scope ownership.Scope, entryID, kindID string, actor Actor) error {
	if err := actor.validate(); err != nil {
		return err
	}
	return s.writeOnEntry(ctx, scope, entryID, false, func(tx *sql.Tx, seq int64, _ string) error {
		kindSeq, err := postgresstorage.ParseRecordID(KindIDPrefix, kindID)
		if err != nil {
			return fmt.Errorf("%w: %s does not carry kind %s", ErrNotFound, entryID, kindID)
		}
		var assignedByKind string
		err = tx.QueryRowContext(ctx, `SELECT assigned_by_kind FROM time_entry_kind_assignments WHERE time_entry_seq = $1 AND kind_seq = $2 AND removed_at IS NULL`,
			seq, kindSeq).Scan(&assignedByKind)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s does not carry kind %s", ErrNotFound, entryID, kindID)
		}
		if err != nil {
			return err
		}
		if !actor.mayReplace(assignedByKind) {
			return fmt.Errorf("%w: kind %s was put on by a %s; a %s may not take it off", ErrConflict, kindID, assignedByKind, actor.Kind)
		}
		_, err = tx.ExecContext(ctx, `UPDATE time_entry_kind_assignments SET removed_at = $1, removed_by = $2, removed_by_kind = $3
			WHERE time_entry_seq = $4 AND kind_seq = $5 AND removed_at IS NULL`, s.now().Unix(), actor.By, actor.Kind, seq, kindSeq)
		return err
	})
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
func addLink(ctx context.Context, tx *sql.Tx, ownerPrincipalID string, entrySeq int64, input LinkInput, actor Actor, now int64) (Link, error) {
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
	entryID := postgresstorage.FormatRecordID(EntryIDPrefix, entrySeq)
	onThisEntry := func(extra string) (string, arguments) {
		args := arguments{entrySeq}
		return `time_entry_seq = $1 AND ` + linkKey(linkType, input.Ref, input.OccurrenceStartsAt, &args) + extra, args
	}
	where, args := onThisEntry(` AND removed_at IS NULL`)
	existing, err := queryLinks(ctx, tx, linkType, where, args...)
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
		where, args := onThisEntry(` AND removed_by_kind = 'operator'`)
		removed, err := queryLinks(ctx, tx, linkType, where, args...)
		if err != nil {
			return Link{}, err
		}
		if len(removed) > 0 {
			return Link{}, fmt.Errorf("%w: the operator took %s %s off %s", ErrConflict, linkType.Name, input.Ref, entryID)
		}
	}
	if input.BearsOn == BearsOnSource {
		args := arguments{ownerPrincipalID}
		where := `owner_principal_id = $1 AND ` + linkKey(linkType, input.Ref, input.OccurrenceStartsAt, &args) + ` AND removed_at IS NULL AND bears_on = 'source'`
		owners, err := queryLinks(ctx, tx, linkType, where, args...)
		if err != nil {
			return Link{}, err
		}
		if len(owners) > 0 {
			return Link{}, fmt.Errorf("%w: %s %s is already the source of %s", ErrConflict, linkType.Name, input.Ref, owners[0].TimeEntryID)
		}
	}
	columns := []string{"owner_principal_id", "time_entry_seq", linkType.RefColumn, "bears_on", "linked_by", "linked_by_kind", "reason", "linked_at"}
	values := arguments{}
	placeholders := []string{}
	for _, value := range []any{ownerPrincipalID, entrySeq, input.Ref, input.BearsOn, actor.By, actor.Kind, input.Reason, now} {
		placeholders = append(placeholders, values.add(value))
	}
	if linkType.HasOccurrence {
		columns, placeholders = append(columns, "occurrence_starts_at"), append(placeholders, values.add(*input.OccurrenceStartsAt))
	}
	if linkType.HasOverlap && input.OverlapSeconds != nil {
		columns, placeholders = append(columns, "overlap_seconds"), append(placeholders, values.add(*input.OverlapSeconds))
	}
	if linkType.HasAmount && input.AmountCents != nil {
		columns, placeholders = append(columns, "amount_cents"), append(placeholders, values.add(*input.AmountCents))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+linkType.Table+` (`+strings.Join(columns, ", ")+`) VALUES (`+strings.Join(placeholders, ", ")+`)`, values...); err != nil {
		if isUniqueViolation(err) {
			return Link{}, fmt.Errorf("%w: %s %s is already linked: %v", ErrConflict, linkType.Name, input.Ref, err)
		}
		return Link{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE time_entries SET evidence_changed_at = $1 WHERE seq = $2`, now, entrySeq); err != nil {
		return Link{}, err
	}
	where, args = onThisEntry(` AND removed_at IS NULL`)
	made, err := queryLinks(ctx, tx, linkType, where, args...)
	if err != nil {
		return Link{}, err
	}
	return made[0], nil
}

// linkKey is the condition naming one record of linkType, its arguments
// added to args.
func linkKey(linkType LinkType, ref string, occurrence *int64, args *arguments) string {
	condition := linkType.RefColumn + ` = ` + args.add(ref)
	if linkType.HasOccurrence && occurrence != nil {
		condition += ` AND occurrence_starts_at = ` + args.add(*occurrence)
	}
	return condition
}

func queryLinks(ctx context.Context, db queryer, linkType LinkType, where string, args ...any) ([]Link, error) {
	occurrence, overlap, amount := "NULL::bigint", "NULL::bigint", "NULL::bigint"
	if linkType.HasOccurrence {
		occurrence = "occurrence_starts_at"
	}
	if linkType.HasOverlap {
		overlap = "overlap_seconds"
	}
	if linkType.HasAmount {
		amount = "amount_cents"
	}
	rows, err := db.QueryContext(ctx, `SELECT time_entry_seq, `+linkType.RefColumn+`, `+occurrence+`, `+overlap+`, `+amount+`, bears_on, linked_by, linked_by_kind,
		reason, linked_at, removed_at, removed_by, removed_by_kind FROM `+linkType.Table+` WHERE `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := []Link{}
	for rows.Next() {
		link := Link{LinkType: linkType.Name}
		var entrySeq int64
		var occurrenceValue, overlapValue, amountValue, removedAt sql.NullInt64
		if err := rows.Scan(&entrySeq, &link.Ref, &occurrenceValue, &overlapValue, &amountValue, &link.BearsOn, &link.LinkedBy, &link.LinkedByKind,
			&link.Reason, &link.LinkedAt, &removedAt, &link.RemovedBy, &link.RemovedByKind); err != nil {
			return nil, err
		}
		link.TimeEntryID = postgresstorage.FormatRecordID(EntryIDPrefix, entrySeq)
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
func (s *Store) AddLink(ctx context.Context, scope ownership.Scope, entryID string, input LinkInput, actor Actor) (Link, error) {
	if err := actor.validate(); err != nil {
		return Link{}, err
	}
	var link Link
	err := s.writeOnEntry(ctx, scope, entryID, true, func(tx *sql.Tx, seq int64, owner string) error {
		var err error
		link, err = addLink(ctx, tx, owner, seq, input, actor, s.now().Unix())
		return err
	})
	return link, err
}

// RemoveLink marks a live link removed. It is kept, and a removal by the
// operator keeps models and programs from making it again.
func (s *Store) RemoveLink(ctx context.Context, scope ownership.Scope, entryID, linkTypeName, ref string, occurrence *int64, actor Actor) error {
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
	return s.writeOnEntry(ctx, scope, entryID, false, func(tx *sql.Tx, seq int64, _ string) error {
		args := arguments{seq}
		where := `time_entry_seq = $1 AND ` + linkKey(linkType, ref, occurrence, &args) + ` AND removed_at IS NULL`
		live, err := queryLinks(ctx, tx, linkType, where, args...)
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
		update := arguments{now, actor.By, actor.Kind, seq}
		if _, err := tx.ExecContext(ctx, `UPDATE `+linkType.Table+` SET removed_at = $1, removed_by = $2, removed_by_kind = $3
			WHERE time_entry_seq = $4 AND `+linkKey(linkType, ref, occurrence, &update)+` AND removed_at IS NULL`, update...); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE time_entries SET evidence_changed_at = $1 WHERE seq = $2`, now, seq)
		return err
	})
}

// LinksTo lists the links to one record from every entry in scope: the way
// an importer finds the entry a source already belongs to.
func (s *Store) LinksTo(ctx context.Context, scope ownership.Scope, linkTypeName, ref string, occurrence *int64, includeRemoved bool) ([]Link, error) {
	linkType, err := linkTypeOf(linkTypeName)
	if err != nil {
		return nil, err
	}
	var args arguments
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return nil, err
	}
	where := condition + ` AND ` + linkKey(linkType, ref, occurrence, &args)
	if !includeRemoved {
		where += ` AND removed_at IS NULL`
	}
	return queryLinks(ctx, s.db, linkType, where, args...)
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
func (s *Store) UpdateEntry(ctx context.Context, scope ownership.Scope, entryID string, change EntryChange) (TimeEntry, error) {
	actor := Actor{By: change.By, Kind: change.ByKind}
	if err := actor.validate(); err != nil {
		return TimeEntry{}, err
	}
	if strings.TrimSpace(change.Reason) == "" {
		return TimeEntry{}, fmt.Errorf("%w: reason is required", ErrInvalid)
	}
	var owner string
	err := s.writeOnEntry(ctx, scope, entryID, true, func(tx *sql.Tx, seq int64, entryOwner string) error {
		owner = entryOwner
		before, err := readEntry(ctx, tx, seq)
		if err != nil {
			return err
		}
		after := before
		now := s.now().Unix()
		if change.Title != nil && *change.Title != before.Title {
			if !actor.mayReplace(before.TitleByKind) {
				return fmt.Errorf("%w: a %s set the title; a %s may not change it", ErrConflict, before.TitleByKind, actor.Kind)
			}
			after.Title, after.TitleBy, after.TitleByKind = strings.TrimSpace(*change.Title), actor.By, actor.Kind
			if after.Title == "" {
				return fmt.Errorf("%w: title may not be empty", ErrInvalid)
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
				return fmt.Errorf("%w: a %s set the times; a %s may not change them", ErrConflict, before.SpanByKind, actor.Kind)
			}
			if err := validateSpan(after.StartsAt, after.EndsAt, after.AllDay, after.StartDate, after.EndDate, after.TimeZone); err != nil {
				return err
			}
			after.SpanBy, after.SpanByKind = actor.By, actor.Kind
			var outside int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM time_entries WHERE parent_seq = $1 AND deleted_at IS NULL AND (starts_at < $2 OR ends_at > $3)`,
				seq, after.StartsAt, after.EndsAt).Scan(&outside); err != nil {
				return err
			}
			if outside > 0 {
				return fmt.Errorf("%w: %d of %s's children would lie outside the new times", ErrConflict, outside, entryID)
			}
		}
		if change.Place != nil && *change.Place != before.Place {
			if !actor.mayReplace(before.PlaceByKind) {
				return fmt.Errorf("%w: a %s set the place; a %s may not change it", ErrConflict, before.PlaceByKind, actor.Kind)
			}
			after.Place, after.PlaceBy, after.PlaceByKind = *change.Place, actor.By, actor.Kind
		}
		if change.ParentTimeEntryID != nil && *change.ParentTimeEntryID != before.ParentTimeEntryID {
			if !actor.mayReplace(before.ParentSetByKind) {
				return fmt.Errorf("%w: a %s set the parent; a %s may not change it", ErrConflict, before.ParentSetByKind, actor.Kind)
			}
			after.ParentTimeEntryID, after.ParentSetBy, after.ParentSetByKind = *change.ParentTimeEntryID, actor.By, actor.Kind
		}
		var parentSeq sql.NullInt64
		if after.ParentTimeEntryID != "" {
			if spanChanged || after.ParentTimeEntryID != before.ParentTimeEntryID {
				number, err := checkParent(ctx, tx, entryOwner, seq, after.ParentTimeEntryID, after.StartsAt, after.EndsAt)
				if err != nil {
					return err
				}
				parentSeq = sql.NullInt64{Int64: number, Valid: true}
			} else {
				number, err := postgresstorage.ParseRecordID(EntryIDPrefix, after.ParentTimeEntryID)
				if err != nil {
					return err
				}
				parentSeq = sql.NullInt64{Int64: number, Valid: true}
			}
		}
		if after.Title == before.Title && !spanChanged && after.Place == before.Place && after.ParentTimeEntryID == before.ParentTimeEntryID {
			return nil
		}
		if err := keepRevision(ctx, tx, before, seq, change.Reason, now); err != nil {
			return err
		}
		evidenceChangedAt := before.EvidenceChangedAt
		if spanChanged {
			evidenceChangedAt = now
		}
		_, err = tx.ExecContext(ctx, `UPDATE time_entries SET title = $1, title_by = $2, title_by_kind = $3, starts_at = $4, ends_at = $5, all_day = $6,
			start_date = $7, end_date = $8, time_zone = $9, span_by = $10, span_by_kind = $11, place = $12, place_by = $13, place_by_kind = $14,
			parent_seq = $15, parent_set_by = $16, parent_set_by_kind = $17, updated_at = $18, evidence_changed_at = $19 WHERE seq = $20`,
			after.Title, after.TitleBy, after.TitleByKind, after.StartsAt, after.EndsAt, after.AllDay,
			after.StartDate, after.EndDate, after.TimeZone, after.SpanBy, after.SpanByKind, after.Place, after.PlaceBy, after.PlaceByKind,
			parentSeq, after.ParentSetBy, after.ParentSetByKind, now, evidenceChangedAt, seq)
		return err
	})
	if err != nil {
		return TimeEntry{}, err
	}
	return s.Entry(ctx, ownership.OwnedBy(owner), entryID, false)
}

func keepRevision(ctx context.Context, tx *sql.Tx, entry TimeEntry, seq int64, reason string, now int64) error {
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO time_entry_revisions (owner_principal_id, time_entry_seq, reason, replaced_at, entry) VALUES ($1, $2, $3, $4, $5)`,
		entry.OwnerPrincipalID, seq, reason, now, string(body))
	return err
}

// DeleteEntry soft-deletes an entry with no live children. Its links stay, so
// an importer finds its source taken and does not make it again.
func (s *Store) DeleteEntry(ctx context.Context, scope ownership.Scope, entryID string, actor Actor) error {
	if err := actor.validate(); err != nil {
		return err
	}
	return s.writeOnEntry(ctx, scope, entryID, true, func(tx *sql.Tx, seq int64, _ string) error {
		entry, err := readEntry(ctx, tx, seq)
		if err != nil {
			return err
		}
		if !actor.mayReplace(entry.CreatedByKind) {
			return fmt.Errorf("%w: a %s made %s; a %s may not delete it", ErrConflict, entry.CreatedByKind, entryID, actor.Kind)
		}
		var children int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM time_entries WHERE parent_seq = $1 AND deleted_at IS NULL`, seq).Scan(&children); err != nil {
			return err
		}
		if children > 0 {
			return fmt.Errorf("%w: %s has %d live children; move or delete them first", ErrConflict, entryID, children)
		}
		now := s.now().Unix()
		if err := keepRevision(ctx, tx, entry, seq, "deleted by "+actor.By, now); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE time_entries SET deleted_at = $1, deleted_by = $2, updated_at = $3 WHERE seq = $4`, now, actor.By, now, seq)
		return err
	})
}

// entryColumns read an entry with the review that gives its outcome: the
// newest operator review, else the newest model review (the only two kinds a
// review has).
const entryColumns = `e.seq, e.owner_principal_id, e.title, e.title_by, e.title_by_kind, e.starts_at, e.ends_at, e.all_day, e.start_date, e.end_date,
	e.time_zone, e.span_by, e.span_by_kind, e.place, e.place_by, e.place_by_kind, e.parent_seq, e.parent_set_by, e.parent_set_by_kind, e.created_by,
	e.created_by_kind, e.created_reason, e.created_at, e.updated_at, e.evidence_changed_at, e.deleted_at, e.deleted_by, o.seq, o.outcome`

const entryFrom = ` FROM time_entries e LEFT JOIN LATERAL (SELECT r.seq, r.outcome FROM time_entry_reviews r WHERE r.time_entry_seq = e.seq
	ORDER BY (r.reviewed_by_kind = 'operator') DESC, r.seq DESC LIMIT 1) o ON TRUE`

func scanEntry(row interface{ Scan(...any) error }) (TimeEntry, int64, error) {
	var entry TimeEntry
	var seq int64
	var parentSeq, deletedAt, outcomeReviewSeq sql.NullInt64
	var outcome sql.NullString
	err := row.Scan(&seq, &entry.OwnerPrincipalID, &entry.Title, &entry.TitleBy, &entry.TitleByKind, &entry.StartsAt, &entry.EndsAt, &entry.AllDay,
		&entry.StartDate, &entry.EndDate, &entry.TimeZone, &entry.SpanBy, &entry.SpanByKind, &entry.Place, &entry.PlaceBy, &entry.PlaceByKind,
		&parentSeq, &entry.ParentSetBy, &entry.ParentSetByKind, &entry.CreatedBy, &entry.CreatedByKind, &entry.CreatedReason, &entry.CreatedAt,
		&entry.UpdatedAt, &entry.EvidenceChangedAt, &deletedAt, &entry.DeletedBy, &outcomeReviewSeq, &outcome)
	if err != nil {
		return TimeEntry{}, 0, err
	}
	entry.ID = postgresstorage.FormatRecordID(EntryIDPrefix, seq)
	if parentSeq.Valid {
		entry.ParentTimeEntryID = postgresstorage.FormatRecordID(EntryIDPrefix, parentSeq.Int64)
	}
	entry.DeletedAt = nullableInt(deletedAt)
	entry.Outcome = OutcomeUnknown
	if outcomeReviewSeq.Valid {
		entry.OutcomeReviewID = postgresstorage.FormatRecordID(ReviewIDPrefix, outcomeReviewSeq.Int64)
		entry.Outcome = outcome.String
	}
	return entry, seq, nil
}

// readEntry reads one entry by number, with its kinds.
func readEntry(ctx context.Context, db queryer, seq int64) (TimeEntry, error) {
	entry, _, err := scanEntry(db.QueryRowContext(ctx, `SELECT `+entryColumns+entryFrom+` WHERE e.seq = $1`, seq))
	if errors.Is(err, sql.ErrNoRows) {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, postgresstorage.FormatRecordID(EntryIDPrefix, seq))
	}
	if err != nil {
		return TimeEntry{}, err
	}
	return entry, fillEntry(ctx, db, &entry, seq, false)
}

func fillEntry(ctx context.Context, db queryer, entry *TimeEntry, seq int64, full bool) error {
	rows, err := db.QueryContext(ctx, `SELECT a.kind_seq, k.name, a.assigned_by, a.assigned_by_kind, a.reason, a.assigned_at FROM time_entry_kind_assignments a
		JOIN time_entry_kinds k ON k.seq = a.kind_seq WHERE a.time_entry_seq = $1 AND a.removed_at IS NULL ORDER BY k.name`, seq)
	if err != nil {
		return err
	}
	entry.Kinds = []KindAssignment{}
	for rows.Next() {
		var assignment KindAssignment
		var kindSeq int64
		if err := rows.Scan(&kindSeq, &assignment.Name, &assignment.AssignedBy, &assignment.AssignedByKind, &assignment.Reason, &assignment.AssignedAt); err != nil {
			rows.Close()
			return err
		}
		assignment.KindID = postgresstorage.FormatRecordID(KindIDPrefix, kindSeq)
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
		links, err := queryLinks(ctx, db, LinkTypes[name], `time_entry_seq = $1`, seq)
		if err != nil {
			return err
		}
		entry.Links = append(entry.Links, links...)
	}
	childRows, err := db.QueryContext(ctx, `SELECT seq FROM time_entries WHERE parent_seq = $1 AND deleted_at IS NULL ORDER BY starts_at, seq`, seq)
	if err != nil {
		return err
	}
	entry.ChildIDs = []string{}
	for childRows.Next() {
		var childSeq int64
		if err := childRows.Scan(&childSeq); err != nil {
			childRows.Close()
			return err
		}
		entry.ChildIDs = append(entry.ChildIDs, postgresstorage.FormatRecordID(EntryIDPrefix, childSeq))
	}
	childRows.Close()
	if err := childRows.Err(); err != nil {
		return err
	}
	entry.Reviews, err = queryReviews(ctx, db, `time_entry_seq = $1`, seq)
	return err
}

// Entry reads one entry in scope; full adds its links, live children and
// reviews.
func (s *Store) Entry(ctx context.Context, scope ownership.Scope, entryID string, full bool) (TimeEntry, error) {
	seq, err := postgresstorage.ParseRecordID(EntryIDPrefix, entryID)
	if err != nil {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, entryID)
	}
	args := arguments{seq}
	condition, err := scopeCondition(scope, "e.owner_principal_id", &args)
	if err != nil {
		return TimeEntry{}, err
	}
	entry, _, err := scanEntry(s.db.QueryRowContext(ctx, `SELECT `+entryColumns+entryFrom+` WHERE e.seq = $1 AND `+condition, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return TimeEntry{}, fmt.Errorf("%w: time entry %s", ErrNotFound, entryID)
	}
	if err != nil {
		return TimeEntry{}, err
	}
	return entry, fillEntry(ctx, s.db, &entry, seq, full)
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

// likePattern is text as an ILIKE pattern matching it anywhere, with the
// pattern's own marks escaped so the text is matched as written.
func likePattern(text string) string {
	escaped := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(text)
	return "%" + escaped + "%"
}

// Entries lists the entries in scope, earliest first.
func (s *Store) Entries(ctx context.Context, scope ownership.Scope, filter EntryFilter) ([]TimeEntry, error) {
	if filter.Limit == 0 {
		filter.Limit = 100
	}
	if filter.Limit < 0 || filter.Limit > MaxListLimit {
		return nil, fmt.Errorf("%w: limit is at most %d", ErrInvalid, MaxListLimit)
	}
	if filter.Offset < 0 {
		return nil, fmt.Errorf("%w: offset may not be below 0", ErrInvalid)
	}
	if filter.Outcome != "" && !contains(Outcomes, filter.Outcome) {
		return nil, fmt.Errorf("%w: outcome %q is not one of %v", ErrInvalid, filter.Outcome, Outcomes)
	}
	var args arguments
	condition, err := scopeCondition(scope, "e.owner_principal_id", &args)
	if err != nil {
		return nil, err
	}
	conditions := []string{condition}
	if !filter.IncludeDeleted {
		conditions = append(conditions, `e.deleted_at IS NULL`)
	}
	if filter.From != 0 {
		conditions = append(conditions, `e.ends_at > `+args.add(filter.From))
	}
	if filter.To != 0 {
		conditions = append(conditions, `e.starts_at < `+args.add(filter.To))
	}
	if filter.EndedBefore != 0 {
		conditions = append(conditions, `e.ends_at <= `+args.add(filter.EndedBefore))
	}
	switch filter.Parent {
	case "":
	case "none":
		conditions = append(conditions, `e.parent_seq IS NULL`)
	default:
		parentSeq, err := postgresstorage.ParseRecordID(EntryIDPrefix, filter.Parent)
		if err != nil {
			// No entry sits inside an entry that cannot exist.
			return []TimeEntry{}, nil
		}
		conditions = append(conditions, `e.parent_seq = `+args.add(parentSeq))
	}
	if filter.KindID != "" {
		kindSeq, err := postgresstorage.ParseRecordID(KindIDPrefix, filter.KindID)
		if err != nil {
			return []TimeEntry{}, nil
		}
		conditions = append(conditions, `EXISTS (SELECT 1 FROM time_entry_kind_assignments a WHERE a.time_entry_seq = e.seq AND a.kind_seq = `+args.add(kindSeq)+` AND a.removed_at IS NULL)`)
	}
	if filter.Query != "" {
		like := args.add(likePattern(filter.Query))
		conditions = append(conditions, `(e.title ILIKE `+like+` OR e.place ILIKE `+like+`)`)
	}
	if filter.NeedsReview {
		conditions = append(conditions, `NOT EXISTS (SELECT 1 FROM time_entry_reviews r WHERE r.time_entry_seq = e.seq AND r.created_at >= e.evidence_changed_at)`)
	}
	if filter.Outcome != "" {
		conditions = append(conditions, `COALESCE(o.outcome, 'unknown') = `+args.add(filter.Outcome))
	}
	query := `SELECT ` + entryColumns + entryFrom + ` WHERE ` + strings.Join(conditions, ` AND `) + ` ORDER BY e.starts_at, e.seq LIMIT ` +
		args.add(filter.Limit) + ` OFFSET ` + args.add(filter.Offset)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	entries := []TimeEntry{}
	seqs := []int64{}
	for rows.Next() {
		entry, seq, err := scanEntry(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		entries = append(entries, entry)
		seqs = append(seqs, seq)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range entries {
		if err := fillEntry(ctx, s.db, &entries[i], seqs[i], false); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// Revisions lists an entry's earlier versions, newest first.
func (s *Store) Revisions(ctx context.Context, scope ownership.Scope, entryID string) ([]map[string]any, error) {
	seq, _, err := entryInScope(ctx, s.db, scope, entryID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT revision_id, reason, replaced_at, entry FROM time_entry_revisions WHERE time_entry_seq = $1 ORDER BY revision_id DESC`, seq)
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
func (s *Store) AddReview(ctx context.Context, scope ownership.Scope, entryID string, input ReviewInput) (Review, error) {
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
	var reviewSeq int64
	var owner string
	err = s.writeOnEntry(ctx, scope, entryID, true, func(tx *sql.Tx, seq int64, entryOwner string) error {
		owner = entryOwner
		return tx.QueryRowContext(ctx, `INSERT INTO time_entry_reviews (owner_principal_id, time_entry_seq, outcome, reason, reviewed_by, reviewed_by_kind, model,
			input_tokens, output_tokens, cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING seq`,
			entryOwner, seq, input.Outcome, input.Reason, actor.By, actor.Kind, input.Model,
			input.InputTokens, input.OutputTokens, input.CostUSD, string(input.LinksSeen), input.SuggestedParentTimeEntryID, string(kindNames), s.now().Unix()).Scan(&reviewSeq)
	})
	if err != nil {
		return Review{}, err
	}
	return s.Review(ctx, ownership.OwnedBy(owner), postgresstorage.FormatRecordID(ReviewIDPrefix, reviewSeq))
}

func queryReviews(ctx context.Context, db queryer, where string, args ...any) ([]Review, error) {
	rows, err := db.QueryContext(ctx, `SELECT seq, owner_principal_id, time_entry_seq, outcome, reason, reviewed_by, reviewed_by_kind, model, input_tokens,
		output_tokens, cost_usd, links_seen, suggested_parent_time_entry_id, suggested_kind_names, created_at FROM time_entry_reviews WHERE `+where+` ORDER BY seq DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reviews := []Review{}
	for rows.Next() {
		var review Review
		var seq, entrySeq int64
		var linksSeen, kindNames string
		if err := rows.Scan(&seq, &review.OwnerPrincipalID, &entrySeq, &review.Outcome, &review.Reason, &review.ReviewedBy, &review.ReviewedByKind, &review.Model,
			&review.InputTokens, &review.OutputTokens, &review.CostUSD, &linksSeen, &review.SuggestedParentTimeEntryID, &kindNames, &review.CreatedAt); err != nil {
			return nil, err
		}
		review.ID = postgresstorage.FormatRecordID(ReviewIDPrefix, seq)
		review.TimeEntryID = postgresstorage.FormatRecordID(EntryIDPrefix, entrySeq)
		review.LinksSeen = json.RawMessage(linksSeen)
		if err := json.Unmarshal([]byte(kindNames), &review.SuggestedKindNames); err != nil {
			return nil, fmt.Errorf("review %s: suggested_kind_names: %w", review.ID, err)
		}
		reviews = append(reviews, review)
	}
	return reviews, rows.Err()
}

// Review reads one review in scope.
func (s *Store) Review(ctx context.Context, scope ownership.Scope, id string) (Review, error) {
	seq, err := postgresstorage.ParseRecordID(ReviewIDPrefix, id)
	if err != nil {
		return Review{}, fmt.Errorf("%w: review %s", ErrNotFound, id)
	}
	args := arguments{seq}
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return Review{}, err
	}
	reviews, err := queryReviews(ctx, s.db, `seq = $1 AND `+condition, args...)
	if err != nil {
		return Review{}, err
	}
	if len(reviews) == 0 {
		return Review{}, fmt.Errorf("%w: review %s", ErrNotFound, id)
	}
	return reviews[0], nil
}

// Reviews lists an entry's reviews, newest first.
func (s *Store) Reviews(ctx context.Context, scope ownership.Scope, entryID string) ([]Review, error) {
	seq, _, err := entryInScope(ctx, s.db, scope, entryID)
	if err != nil {
		return nil, err
	}
	return queryReviews(ctx, s.db, `time_entry_seq = $1`, seq)
}

// Counts is GET /health's counts, over the records in scope.
func (s *Store) Counts(ctx context.Context, scope ownership.Scope) (map[string]int64, error) {
	var args arguments
	condition, err := scopeCondition(scope, "owner_principal_id", &args)
	if err != nil {
		return nil, err
	}
	counts := map[string]int64{}
	for name, query := range map[string]string{
		"time_entries": `SELECT COUNT(*) FROM time_entries WHERE deleted_at IS NULL AND ` + condition,
		"kinds":        `SELECT COUNT(*) FROM time_entry_kinds WHERE ` + condition,
		"reviews":      `SELECT COUNT(*) FROM time_entry_reviews WHERE ` + condition,
	} {
		var count int64
		if err := s.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
			return nil, err
		}
		counts[name] = count
	}
	var links int64
	for _, linkType := range LinkTypes {
		var count int64
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+linkType.Table+` WHERE removed_at IS NULL AND `+condition, args...).Scan(&count); err != nil {
			return nil, err
		}
		links += count
	}
	counts["links"] = links
	return counts, nil
}
