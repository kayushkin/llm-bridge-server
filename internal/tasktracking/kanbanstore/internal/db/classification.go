package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/kayushkin/kanban-store/internal/model"
	"github.com/kayushkin/llm-bridge/msg"
)

// Board classification: taxonomy and policy revisions, the decisions a
// classifier publishes, the reviews people add to them, and the labels a card
// carries as a result. Every write here runs in one BEGIN IMMEDIATE
// transaction, so two writers are served one after the other and each sees
// what the other committed; a check and the write it guards never straddle
// someone else's write.
//
// Nothing here is ever rewritten. Taxonomy and policy revisions are added,
// decisions are inserted once, reviews are appended; the only rows that
// change are a card's labels, and each change names the review behind it.

// ClassificationRefusalKind is why a classification write was refused, which
// the API turns into a status.
type ClassificationRefusalKind int

const (
	// ClassificationRefusalInvalid: the request is wrong whatever the state (400).
	ClassificationRefusalInvalid ClassificationRefusalKind = iota
	// ClassificationRefusalStale: If-Match named a revision that is not current (412).
	ClassificationRefusalStale
	// ClassificationRefusalConflict: the request is well formed but the state
	// refuses it — a labels revision moved, an idempotency key was reused with
	// a different body, a policy does not allow it (409).
	ClassificationRefusalConflict
	// ClassificationRefusalNotFound: the named record is not on this board (404).
	ClassificationRefusalNotFound
)

// ClassificationRefusal is a refused classification write. Code is a stable
// word a client can branch on; Current, when set, is the state the caller
// should have sent against.
type ClassificationRefusal struct {
	Kind    ClassificationRefusalKind
	Code    string
	Message string
	Current any
}

func (refusal *ClassificationRefusal) Error() string { return refusal.Code + ": " + refusal.Message }

func refuse(kind ClassificationRefusalKind, code, format string, arguments ...any) *ClassificationRefusal {
	return &ClassificationRefusal{Kind: kind, Code: code, Message: fmt.Sprintf(format, arguments...)}
}

// migrationPrincipal is written as created_by on what the one-time move of
// name-only taxonomies created.
const migrationPrincipal = "migration:classification-stable-ids"

func migrateBoardClassification(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS classification_identifiers (
			sequence   INTEGER PRIMARY KEY AUTOINCREMENT,
			id         TEXT NOT NULL UNIQUE,
			kind       TEXT NOT NULL CHECK (kind IN ('axis', 'value')),
			board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			axis_id    TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_classification_identifiers_board ON classification_identifiers(board_id);

		CREATE TABLE IF NOT EXISTS classification_taxonomy_revisions (
			board_id        TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			revision        INTEGER NOT NULL,
			taxonomy        TEXT,
			semantic_digest TEXT NOT NULL DEFAULT '',
			created_at      DATETIME NOT NULL,
			created_by      TEXT NOT NULL,
			PRIMARY KEY (board_id, revision)
		);

		CREATE TABLE IF NOT EXISTS classification_taxonomy_migrations (
			board_id    TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			axis_name   TEXT NOT NULL,
			value_name  TEXT NOT NULL,
			assigned_id TEXT NOT NULL,
			migrated_at DATETIME NOT NULL,
			PRIMARY KEY (board_id, axis_name, value_name)
		);

		CREATE TABLE IF NOT EXISTS classification_policy_revisions (
			board_id          TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			revision          INTEGER NOT NULL,
			axis_policies     TEXT NOT NULL,
			taxonomy_revision INTEGER NOT NULL,
			created_at        DATETIME NOT NULL,
			created_by        TEXT NOT NULL,
			PRIMARY KEY (board_id, revision)
		);

		CREATE TABLE IF NOT EXISTS classification_decisions (
			id                        TEXT PRIMARY KEY,
			sequence                  INTEGER NOT NULL UNIQUE,
			board_id                  TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			organization_id           TEXT NOT NULL,
			card_id                   TEXT NOT NULL,
			publication_key           TEXT NOT NULL UNIQUE,
			publication_digest        TEXT NOT NULL,
			operation_id              TEXT NOT NULL,
			checkpoint_id             TEXT NOT NULL,
			attempt_number            INTEGER NOT NULL,
			initiating_principal_id   TEXT NOT NULL,
			source_digest             TEXT NOT NULL,
			source_revision           TEXT NOT NULL DEFAULT '',
			source_snapshot           TEXT,
			source_removed_at         DATETIME,
			taxonomy_revision         INTEGER NOT NULL,
			taxonomy_digest           TEXT NOT NULL,
			policy_revision           INTEGER NOT NULL,
			prompt_revision           TEXT NOT NULL,
			model                     TEXT NOT NULL,
			origin                    TEXT NOT NULL,
			selections                TEXT NOT NULL,
			rationale                 TEXT NOT NULL DEFAULT '',
			model_reported_confidence REAL,
			needs_review              INTEGER NOT NULL,
			review_reasons            TEXT NOT NULL,
			completed_at              DATETIME NOT NULL,
			published_at              DATETIME NOT NULL,
			FOREIGN KEY (board_id, taxonomy_revision) REFERENCES classification_taxonomy_revisions(board_id, revision),
			FOREIGN KEY (board_id, policy_revision) REFERENCES classification_policy_revisions(board_id, revision)
		);
		CREATE INDEX IF NOT EXISTS idx_classification_decisions_board     ON classification_decisions(board_id, sequence);
		CREATE INDEX IF NOT EXISTS idx_classification_decisions_card      ON classification_decisions(board_id, card_id, sequence);
		CREATE INDEX IF NOT EXISTS idx_classification_decisions_operation ON classification_decisions(board_id, operation_id);
		CREATE INDEX IF NOT EXISTS idx_classification_decisions_source    ON classification_decisions(card_id);

		CREATE TABLE IF NOT EXISTS classification_reviews (
			id                        TEXT PRIMARY KEY,
			decision_id               TEXT NOT NULL REFERENCES classification_decisions(id) ON DELETE CASCADE,
			board_id                  TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			card_id                   TEXT NOT NULL,
			reviewer_principal_id     TEXT NOT NULL,
			action                    TEXT NOT NULL,
			axis_ids                  TEXT NOT NULL,
			corrections               TEXT,
			explanation               TEXT NOT NULL DEFAULT '',
			supersedes_review_ids     TEXT NOT NULL,
			expected_labels_revision  INTEGER NOT NULL,
			resulting_labels_revision INTEGER NOT NULL,
			taxonomy_revision         INTEGER NOT NULL,
			policy_revision           INTEGER NOT NULL,
			idempotency_key           TEXT NOT NULL,
			request_digest            TEXT NOT NULL,
			created_at                DATETIME NOT NULL,
			UNIQUE (board_id, reviewer_principal_id, idempotency_key)
		);
		CREATE INDEX IF NOT EXISTS idx_classification_reviews_decision ON classification_reviews(decision_id, created_at);

		CREATE TABLE IF NOT EXISTS classification_labels (
			board_id           TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			card_id            TEXT NOT NULL,
			axis_id            TEXT NOT NULL,
			value_ids          TEXT NOT NULL,
			origin_decision_id TEXT NOT NULL,
			origin_review_id   TEXT NOT NULL,
			updated_at         DATETIME NOT NULL,
			PRIMARY KEY (board_id, card_id, axis_id)
		);

		CREATE TABLE IF NOT EXISTS classification_label_revisions (
			board_id   TEXT NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
			card_id    TEXT NOT NULL,
			revision   INTEGER NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (board_id, card_id)
		);
	`); err != nil {
		return err
	}
	return migrateNameOnlyTaxonomies(db)
}

// migrateNameOnlyTaxonomies moves each board's taxonomy out of the boards row,
// where it had names and no ids, into revision 1 with an id minted for every
// axis and value, and records which name got which id. It runs once, in one
// transaction, and drops the column at the end, so a second start finds
// nothing to do. A stored taxonomy that does not parse or validate stops the
// start: guessing at it would give history ids that mean the wrong thing.
func migrateNameOnlyTaxonomies(db *sql.DB) error {
	present, err := columnExists(db, "boards", "taxonomy")
	if err != nil || !present {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, taxonomy FROM boards WHERE taxonomy IS NOT NULL AND TRIM(taxonomy) != ''`)
	if err != nil {
		return err
	}
	stored := map[string]string{}
	var boardIDs []string
	for rows.Next() {
		var boardID, taxonomy string
		if err := rows.Scan(&boardID, &taxonomy); err != nil {
			rows.Close()
			return err
		}
		stored[boardID] = taxonomy
		boardIDs = append(boardIDs, boardID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	sort.Strings(boardIDs)
	stamp := now()
	for _, boardID := range boardIDs {
		var taxonomy msg.ClassificationTaxonomy
		if err := json.Unmarshal([]byte(stored[boardID]), &taxonomy); err != nil {
			return fmt.Errorf("board %s: its stored taxonomy does not parse, so it cannot be given ids: %w", boardID, err)
		}
		if err := taxonomy.Validate(); err != nil {
			return fmt.Errorf("board %s: its stored taxonomy does not validate, so it cannot be given ids: %w", boardID, err)
		}
		for axisIndex := range taxonomy.Axes {
			axis := &taxonomy.Axes[axisIndex]
			if axis.ID, err = mintClassificationIdentifier(tx, "axis", boardID, ""); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO classification_taxonomy_migrations (board_id, axis_name, value_name, assigned_id, migrated_at) VALUES (?, ?, '', ?, ?)`,
				boardID, axis.Name, axis.ID, stamp); err != nil {
				return err
			}
			for valueIndex := range axis.Values {
				value := &axis.Values[valueIndex]
				if value.ID, err = mintClassificationIdentifier(tx, "value", boardID, axis.ID); err != nil {
					return err
				}
				if _, err := tx.Exec(`INSERT INTO classification_taxonomy_migrations (board_id, axis_name, value_name, assigned_id, migrated_at) VALUES (?, ?, ?, ?, ?)`,
					boardID, axis.Name, value.Name, value.ID, stamp); err != nil {
					return err
				}
			}
		}
		if err := insertTaxonomyRevision(tx, boardID, 1, &taxonomy, migrationPrincipal, stamp); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`ALTER TABLE boards DROP COLUMN taxonomy`); err != nil {
		return fmt.Errorf("drop boards.taxonomy after moving it to revisions: %w", err)
	}
	return tx.Commit()
}

func columnExists(db *sql.DB, table, column string) (bool, error) {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// executor is what both *sql.Tx and *sql.Conn offer.
type executor interface {
	Exec(query string, arguments ...any) (sql.Result, error)
	Query(query string, arguments ...any) (*sql.Rows, error)
	QueryRow(query string, arguments ...any) *sql.Row
}

// connectionExecutor runs statements on one connection, which is how a
// BEGIN IMMEDIATE transaction is held: database/sql's Begin starts a deferred one.
type connectionExecutor struct{ connection *sql.Conn }

func (c connectionExecutor) Exec(query string, arguments ...any) (sql.Result, error) {
	return c.connection.ExecContext(context.Background(), query, arguments...)
}

func (c connectionExecutor) Query(query string, arguments ...any) (*sql.Rows, error) {
	return c.connection.QueryContext(context.Background(), query, arguments...)
}

func (c connectionExecutor) QueryRow(query string, arguments ...any) *sql.Row {
	return c.connection.QueryRowContext(context.Background(), query, arguments...)
}

// writeImmediately runs work in a BEGIN IMMEDIATE transaction: it takes the
// write lock before reading, so the reads it checks against are still true
// when it writes. work's error rolls everything back.
func (s *Store) writeImmediately(work func(executor) error) (err error) {
	connection, err := s.db.Conn(context.Background())
	if err != nil {
		return err
	}
	defer connection.Close()
	run := connectionExecutor{connection}
	if _, err := run.Exec(`BEGIN IMMEDIATE`); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			if _, rollbackErr := run.Exec(`ROLLBACK`); rollbackErr != nil && err == nil {
				err = rollbackErr
			}
		}
	}()
	if err := work(run); err != nil {
		return err
	}
	if _, err := run.Exec(`COMMIT`); err != nil {
		return err
	}
	committed = true
	return nil
}

// mintClassificationIdentifier hands out the next axis or value id,
// classification_axis_000001 or classification_value_000001, one sequence for
// both kinds across every board. Ids are never reused.
func mintClassificationIdentifier(run executor, kind, boardID, axisID string) (string, error) {
	placeholder := "pending-" + uuid.NewString()
	result, err := run.Exec(`INSERT INTO classification_identifiers (id, kind, board_id, axis_id, created_at) VALUES (?, ?, ?, ?, ?)`,
		placeholder, kind, boardID, axisID, now())
	if err != nil {
		return "", err
	}
	sequence, err := result.LastInsertId()
	if err != nil {
		return "", err
	}
	id := fmt.Sprintf("classification_%s_%06d", kind, sequence)
	if _, err := run.Exec(`UPDATE classification_identifiers SET id = ? WHERE sequence = ?`, id, sequence); err != nil {
		return "", err
	}
	return id, nil
}

func insertTaxonomyRevision(run executor, boardID string, revision int64, taxonomy *msg.ClassificationTaxonomy, principalID string, at time.Time) error {
	var encoded any
	digest := ""
	if taxonomy != nil {
		raw, err := json.Marshal(taxonomy)
		if err != nil {
			return err
		}
		encoded = string(raw)
		if digest, err = taxonomy.SemanticDigest(); err != nil {
			return err
		}
	}
	_, err := run.Exec(`INSERT INTO classification_taxonomy_revisions (board_id, revision, taxonomy, semantic_digest, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
		boardID, revision, encoded, digest, at, principalID)
	return err
}

// taxonomyRevisionRecord is one row of classification_taxonomy_revisions.
type taxonomyRevisionRecord struct {
	Revision  int64
	Taxonomy  *msg.ClassificationTaxonomy
	Digest    string
	CreatedAt time.Time
	CreatedBy string
}

// currentTaxonomyRevision is the board's newest revision, or a zero record
// when it never had one.
func currentTaxonomyRevision(run executor, boardID string) (taxonomyRevisionRecord, error) {
	return readTaxonomyRevision(run, boardID, `SELECT revision, taxonomy, semantic_digest, created_at, created_by FROM classification_taxonomy_revisions WHERE board_id = ? ORDER BY revision DESC LIMIT 1`, boardID)
}

func taxonomyRevisionNumbered(run executor, boardID string, revision int64) (taxonomyRevisionRecord, error) {
	return readTaxonomyRevision(run, boardID, `SELECT revision, taxonomy, semantic_digest, created_at, created_by FROM classification_taxonomy_revisions WHERE board_id = ? AND revision = ?`, boardID, revision)
}

func readTaxonomyRevision(run executor, boardID, query string, arguments ...any) (taxonomyRevisionRecord, error) {
	var record taxonomyRevisionRecord
	var taxonomy sql.NullString
	err := run.QueryRow(query, arguments...).Scan(&record.Revision, &taxonomy, &record.Digest, &record.CreatedAt, &record.CreatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return taxonomyRevisionRecord{}, nil
	}
	if err != nil {
		return record, err
	}
	if taxonomy.Valid {
		var decoded msg.ClassificationTaxonomy
		if err := json.Unmarshal([]byte(taxonomy.String), &decoded); err != nil {
			return record, fmt.Errorf("board %s taxonomy revision %d does not parse: %w", boardID, record.Revision, err)
		}
		record.Taxonomy = &decoded
	}
	return record, nil
}

func currentPolicy(run executor, boardID string) (*msg.BoardClassificationPolicy, error) {
	return readPolicy(run, boardID, `SELECT revision, axis_policies, created_at, created_by FROM classification_policy_revisions WHERE board_id = ? ORDER BY revision DESC LIMIT 1`, boardID)
}

func policyRevisionNumbered(run executor, boardID string, revision int64) (*msg.BoardClassificationPolicy, error) {
	return readPolicy(run, boardID, `SELECT revision, axis_policies, created_at, created_by FROM classification_policy_revisions WHERE board_id = ? AND revision = ?`, boardID, revision)
}

// readPolicy returns nil when there is no such revision.
func readPolicy(run executor, boardID, query string, arguments ...any) (*msg.BoardClassificationPolicy, error) {
	policy := &msg.BoardClassificationPolicy{BoardID: boardID}
	var axisPolicies string
	err := run.QueryRow(query, arguments...).Scan(&policy.Revision, &axisPolicies, &policy.UpdatedAt, &policy.UpdatedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(axisPolicies), &policy.AxisPolicies); err != nil {
		return nil, fmt.Errorf("board %s policy revision %d does not parse: %w", boardID, policy.Revision, err)
	}
	return policy, nil
}

func boardOrganization(run executor, boardID string) (string, error) {
	var organization sql.NullString
	err := run.QueryRow(`SELECT organization_id FROM boards WHERE id = ?`, boardID).Scan(&organization)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return organization.String, err
}

// BoardClassification is the board's classification settings, without the
// caller's actions, which only the API knows.
func (s *Store) BoardClassification(boardID string) (*msg.BoardClassification, error) {
	return boardClassification(s.db, boardID)
}

func boardClassification(run executor, boardID string) (*msg.BoardClassification, error) {
	organization, err := boardOrganization(run, boardID)
	if err != nil {
		return nil, err
	}
	taxonomy, err := currentTaxonomyRevision(run, boardID)
	if err != nil {
		return nil, err
	}
	policy, err := currentPolicy(run, boardID)
	if err != nil {
		return nil, err
	}
	out := &msg.BoardClassification{
		BoardID: boardID, OrganizationID: organization,
		Taxonomy: taxonomy.Taxonomy, TaxonomyRevision: taxonomy.Revision, TaxonomyDigest: taxonomy.Digest,
		Policy: policy, SupportedModes: msg.ClassificationAxisModes, CallerActions: []msg.BoardClassificationAction{},
	}
	if taxonomy.Revision > 0 {
		updatedAt := taxonomy.CreatedAt
		out.TaxonomyUpdatedAt, out.TaxonomyUpdatedBy = &updatedAt, taxonomy.CreatedBy
	}
	return out, nil
}

// SetBoardTaxonomy writes a new taxonomy revision when expectedRevision is
// the board's current one. A taxonomy identical to the current one writes
// nothing. Nil clears the taxonomy, as a revision of its own.
func (s *Store) SetBoardTaxonomy(boardID string, expectedRevision int64, taxonomy *msg.ClassificationTaxonomy, principalID string) (*model.TaxonomyUpdateResult, error) {
	var result *model.TaxonomyUpdateResult
	err := s.writeImmediately(func(run executor) error {
		if _, err := boardOrganization(run, boardID); err != nil {
			return err
		}
		current, err := currentTaxonomyRevision(run, boardID)
		if err != nil {
			return err
		}
		if expectedRevision != current.Revision {
			refusal := refuse(ClassificationRefusalStale, "taxonomy_revision_moved", "If-Match names taxonomy revision %d, and the board's is %d", expectedRevision, current.Revision)
			refusal.Current, _ = boardClassification(run, boardID)
			return refusal
		}
		policy, err := currentPolicy(run, boardID)
		if err != nil {
			return err
		}
		if taxonomy == nil {
			if current.Taxonomy == nil {
				result, err = taxonomyUpdateResult(run, boardID, current.Digest, "")
				return err
			}
			if policy != nil {
				for axisID, axisPolicy := range policy.AxisPolicies {
					if axisPolicy.Mode != msg.ClassificationAxisModeOff {
						return refuse(ClassificationRefusalConflict, "policy_uses_taxonomy", "policy revision %d has axis %s %s; turn every axis off before clearing the taxonomy", policy.Revision, axisID, axisPolicy.Mode)
					}
				}
			}
			if err := insertTaxonomyRevision(run, boardID, current.Revision+1, nil, principalID, now()); err != nil {
				return err
			}
			result, err = taxonomyUpdateResult(run, boardID, current.Digest, "")
			return err
		}
		if err := taxonomy.Validate(); err != nil {
			return refuse(ClassificationRefusalInvalid, "taxonomy_invalid", "%v", err)
		}
		needsMinting, err := checkTaxonomyIdentifiers(run, boardID, taxonomy, current.Taxonomy)
		if err != nil {
			return err
		}
		if !needsMinting && current.Taxonomy != nil {
			sent, _ := json.Marshal(taxonomy)
			stored, _ := json.Marshal(current.Taxonomy)
			if string(sent) == string(stored) {
				result, err = taxonomyUpdateResult(run, boardID, current.Digest, current.Digest)
				return err
			}
		}
		for axisIndex := range taxonomy.Axes {
			axis := &taxonomy.Axes[axisIndex]
			if axis.ID == "" {
				if axis.ID, err = mintClassificationIdentifier(run, "axis", boardID, ""); err != nil {
					return err
				}
			}
			for valueIndex := range axis.Values {
				if axis.Values[valueIndex].ID == "" {
					if axis.Values[valueIndex].ID, err = mintClassificationIdentifier(run, "value", boardID, axis.ID); err != nil {
						return err
					}
				}
			}
		}
		if err := taxonomy.ValidateStableIdentity(); err != nil {
			return refuse(ClassificationRefusalInvalid, "taxonomy_invalid", "%v", err)
		}
		if policy != nil {
			if err := msg.ValidateAxisPolicies(policy.AxisPolicies, *taxonomy); err != nil {
				return refuse(ClassificationRefusalConflict, "policy_uses_taxonomy", "policy revision %d would no longer hold: %v; change the policy first", policy.Revision, err)
			}
		}
		if err := insertTaxonomyRevision(run, boardID, current.Revision+1, taxonomy, principalID, now()); err != nil {
			return err
		}
		newDigest, err := taxonomy.SemanticDigest()
		if err != nil {
			return err
		}
		result, err = taxonomyUpdateResult(run, boardID, current.Digest, newDigest)
		if err != nil {
			return err
		}
		result.Preview.LabelsOnRetiredEntries, err = countLabelsOnRetiredEntries(run, boardID, *taxonomy)
		return err
	})
	return result, err
}

// checkTaxonomyIdentifiers holds every id the request carries to this
// board's own ids — an axis id must be one of its axes, a value id one of that
// axis's values — and refuses a request that leaves out an id the current
// taxonomy has. It reports whether any entry has no id yet.
func checkTaxonomyIdentifiers(run executor, boardID string, taxonomy *msg.ClassificationTaxonomy, current *msg.ClassificationTaxonomy) (bool, error) {
	type identifier struct{ kind, axisID string }
	known := map[string]identifier{}
	rows, err := run.Query(`SELECT id, kind, axis_id FROM classification_identifiers WHERE board_id = ?`, boardID)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id string
		var entry identifier
		if err := rows.Scan(&id, &entry.kind, &entry.axisID); err != nil {
			rows.Close()
			return false, err
		}
		known[id] = entry
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	needsMinting := false
	sent := map[string]bool{}
	for _, axis := range taxonomy.Axes {
		if axis.ID == "" {
			needsMinting = true
		} else if entry, ok := known[axis.ID]; !ok || entry.kind != "axis" {
			return false, refuse(ClassificationRefusalInvalid, "unknown_identifier", "axis %q has id %s, which is not an axis of this board; leave id out to add a new axis", axis.Name, axis.ID)
		}
		sent[axis.ID] = true
		for _, value := range axis.Values {
			if value.ID == "" {
				needsMinting = true
				continue
			}
			entry, ok := known[value.ID]
			if !ok || entry.kind != "value" {
				return false, refuse(ClassificationRefusalInvalid, "unknown_identifier", "axis %q value %q has id %s, which is not a value of this board; leave id out to add a new value", axis.Name, value.Name, value.ID)
			}
			if axis.ID == "" || entry.axisID != axis.ID {
				return false, refuse(ClassificationRefusalInvalid, "value_moved_axis", "value %s belongs to axis %s and cannot move to axis %q", value.ID, entry.axisID, axis.Name)
			}
			sent[value.ID] = true
		}
	}
	if current != nil {
		for _, axis := range current.Axes {
			if !sent[axis.ID] {
				return false, refuse(ClassificationRefusalInvalid, "identifier_left_out", "axis %s (%q) is missing; send it with archived set to retire it", axis.ID, axis.Name)
			}
			for _, value := range axis.Values {
				if !sent[value.ID] {
					return false, refuse(ClassificationRefusalInvalid, "identifier_left_out", "axis %s value %s (%q) is missing; send it with archived set to retire it", axis.ID, value.ID, value.Name)
				}
			}
		}
	}
	return needsMinting, nil
}

func taxonomyUpdateResult(run executor, boardID, previousDigest, newDigest string) (*model.TaxonomyUpdateResult, error) {
	classification, err := boardClassification(run, boardID)
	if err != nil {
		return nil, err
	}
	result := &model.TaxonomyUpdateResult{Classification: *classification,
		Preview: model.TaxonomyChangePreview{SemanticChange: previousDigest != newDigest}}
	err = run.QueryRow(`SELECT COUNT(*) FROM classification_decisions WHERE board_id = ? AND taxonomy_digest != ?`, boardID, newDigest).
		Scan(&result.Preview.DecisionsUnderOtherTaxonomies)
	return result, err
}

// countLabelsOnRetiredEntries counts this board's labels on an archived axis,
// or holding an archived value.
func countLabelsOnRetiredEntries(run executor, boardID string, taxonomy msg.ClassificationTaxonomy) (int, error) {
	var retired []any
	for _, axis := range taxonomy.Axes {
		if axis.Archived {
			retired = append(retired, axis.ID)
		}
		for _, value := range axis.Values {
			if value.Archived {
				retired = append(retired, value.ID)
			}
		}
	}
	if len(retired) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(retired)), ",")
	arguments := append([]any{boardID}, retired...)
	arguments = append(arguments, retired...)
	var count int
	err := run.QueryRow(`SELECT COUNT(*) FROM classification_labels label WHERE board_id = ? AND (axis_id IN (`+placeholders+`)
		OR EXISTS (SELECT 1 FROM json_each(label.value_ids) WHERE json_each.value IN (`+placeholders+`)))`, arguments...).Scan(&count)
	return count, err
}

// SetBoardClassificationPolicy writes a new policy revision when
// expectedRevision is the board's current one (0 for a board with none). The
// policy is held to the board's current taxonomy. An identical policy writes
// nothing.
func (s *Store) SetBoardClassificationPolicy(boardID string, expectedRevision int64, axisPolicies map[string]msg.ClassificationAxisPolicy, principalID string) (*msg.BoardClassification, error) {
	var result *msg.BoardClassification
	err := s.writeImmediately(func(run executor) error {
		if _, err := boardOrganization(run, boardID); err != nil {
			return err
		}
		current, err := currentPolicy(run, boardID)
		if err != nil {
			return err
		}
		currentRevision := int64(0)
		if current != nil {
			currentRevision = current.Revision
		}
		if expectedRevision != currentRevision {
			refusal := refuse(ClassificationRefusalStale, "policy_revision_moved", "If-Match names policy revision %d, and the board's is %d", expectedRevision, currentRevision)
			refusal.Current, _ = boardClassification(run, boardID)
			return refusal
		}
		taxonomy, err := currentTaxonomyRevision(run, boardID)
		if err != nil {
			return err
		}
		if taxonomy.Taxonomy == nil {
			return refuse(ClassificationRefusalConflict, "board_has_no_taxonomy", "board %s has no taxonomy; set one before its policy", boardID)
		}
		if axisPolicies == nil {
			return refuse(ClassificationRefusalInvalid, "policy_invalid", "axis_policies is required; send {} for a board with every axis off")
		}
		if err := msg.ValidateAxisPolicies(axisPolicies, *taxonomy.Taxonomy); err != nil {
			return refuse(ClassificationRefusalInvalid, "policy_invalid", "%v", err)
		}
		encoded, err := json.Marshal(axisPolicies)
		if err != nil {
			return err
		}
		if current != nil {
			stored, _ := json.Marshal(current.AxisPolicies)
			if string(stored) == string(encoded) {
				result, err = boardClassification(run, boardID)
				return err
			}
		}
		if _, err := run.Exec(`INSERT INTO classification_policy_revisions (board_id, revision, axis_policies, taxonomy_revision, created_at, created_by) VALUES (?, ?, ?, ?, ?, ?)`,
			boardID, currentRevision+1, string(encoded), taxonomy.Revision, now(), principalID); err != nil {
			return err
		}
		result, err = boardClassification(run, boardID)
		return err
	})
	return result, err
}

// PublishClassificationDecision records one classifier answer. The caller
// has checked the initiating principal's authority; this checks everything
// the store's own records decide, in the same transaction as the write. It
// reports whether the decision is new; a second publication of the same
// answer returns the first.
func (s *Store) PublishClassificationDecision(publication msg.BoardClassificationDecisionPublication) (*model.ClassificationDecision, bool, error) {
	var decision *model.ClassificationDecision
	created := false
	err := s.writeImmediately(func(run executor) error {
		organization, err := boardOrganization(run, publication.BoardID)
		if err != nil {
			return err
		}
		key, err := publication.PublicationKey()
		if err != nil {
			return err
		}
		encoded, err := json.Marshal(publication)
		if err != nil {
			return err
		}
		digest := sha256Hex(encoded)
		var existingID, existingDigest string
		err = run.QueryRow(`SELECT id, publication_digest FROM classification_decisions WHERE publication_key = ?`, key).Scan(&existingID, &existingDigest)
		if err == nil {
			if existingDigest != digest {
				refusal := refuse(ClassificationRefusalConflict, "publication_key_reused", "decision %s already answers publication key %s with a different payload", existingID, key)
				refusal.Current, _ = readDecision(run, publication.BoardID, existingID, false)
				return refusal
			}
			decision, err = readDecision(run, publication.BoardID, existingID, false)
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if organization == "" {
			return refuse(ClassificationRefusalConflict, "board_has_no_organization", "board %s has no organization_id, so nothing on it is classified", publication.BoardID)
		}
		var placed int
		if err := run.QueryRow(`SELECT COUNT(*) FROM placements WHERE board_id = ? AND card_id = ?`, publication.BoardID, publication.CardID).Scan(&placed); err != nil {
			return err
		}
		if placed == 0 {
			return refuse(ClassificationRefusalConflict, "card_not_on_board", "card %s is not on board %s", publication.CardID, publication.BoardID)
		}
		taxonomy, err := taxonomyRevisionNumbered(run, publication.BoardID, publication.TaxonomyRevision)
		if err != nil {
			return err
		}
		if taxonomy.Taxonomy == nil {
			return refuse(ClassificationRefusalConflict, "taxonomy_revision_unknown", "board %s has no taxonomy at revision %d", publication.BoardID, publication.TaxonomyRevision)
		}
		if taxonomy.Digest != publication.TaxonomyDigest {
			return refuse(ClassificationRefusalConflict, "taxonomy_digest_mismatch", "taxonomy revision %d has digest %s, and the publication says %s", taxonomy.Revision, taxonomy.Digest, publication.TaxonomyDigest)
		}
		policy, err := policyRevisionNumbered(run, publication.BoardID, publication.PolicyRevision)
		if err != nil {
			return err
		}
		if policy == nil {
			return refuse(ClassificationRefusalConflict, "policy_revision_unknown", "board %s has no policy at revision %d", publication.BoardID, publication.PolicyRevision)
		}
		if err := publication.Validate(*taxonomy.Taxonomy); err != nil {
			return refuse(ClassificationRefusalInvalid, "publication_invalid", "%v", err)
		}
		for axisID := range publication.Selections {
			if policy.ModeOf(axisID) == msg.ClassificationAxisModeOff {
				return refuse(ClassificationRefusalInvalid, "axis_off", "axis %s is off under policy revision %d, so nothing may be published on it", axisID, policy.Revision)
			}
		}
		source, err := json.Marshal(publication.Source)
		if err != nil {
			return err
		}
		modelEvidence, _ := json.Marshal(publication.Model)
		selections, _ := json.Marshal(publication.Selections)
		reasons := publication.ReviewReasons
		if reasons == nil {
			reasons = []msg.OperationEvidence{}
		}
		reviewReasons, _ := json.Marshal(reasons)
		var sequence int64
		if err := run.QueryRow(`SELECT COALESCE(MAX(sequence), 0) + 1 FROM classification_decisions`).Scan(&sequence); err != nil {
			return err
		}
		id := uuid.NewString()
		if _, err := run.Exec(`INSERT INTO classification_decisions (id, sequence, board_id, organization_id, card_id, publication_key, publication_digest,
			operation_id, checkpoint_id, attempt_number, initiating_principal_id, source_digest, source_revision, source_snapshot,
			taxonomy_revision, taxonomy_digest, policy_revision, prompt_revision, model, origin, selections, rationale,
			model_reported_confidence, needs_review, review_reasons, completed_at, published_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, sequence, publication.BoardID, organization, publication.CardID, key, digest,
			publication.OperationID, publication.CheckpointID, publication.AttemptNumber, publication.InitiatingPrincipalID,
			publication.SourceDigest, publication.Source.SourceRevision, string(source),
			publication.TaxonomyRevision, publication.TaxonomyDigest, publication.PolicyRevision, publication.PromptRevision,
			string(modelEvidence), string(publication.Origin), string(selections), publication.Rationale,
			publication.ModelReportedConfidence, publication.NeedsReview, string(reviewReasons), publication.CompletedAt.UTC(), now()); err != nil {
			return err
		}
		created = true
		decision, err = readDecision(run, publication.BoardID, id, false)
		return err
	})
	return decision, created, err
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

const decisionColumns = `d.id, d.board_id, d.organization_id, d.card_id, d.publication_key, d.operation_id, d.checkpoint_id, d.attempt_number,
	d.initiating_principal_id, d.source_digest, d.source_revision, d.source_snapshot, d.source_removed_at, d.taxonomy_revision, d.taxonomy_digest,
	d.policy_revision, d.prompt_revision, d.model, d.origin, d.selections, d.rationale, d.model_reported_confidence, d.needs_review,
	d.review_reasons, d.completed_at, d.published_at,
	(SELECT COUNT(*) FROM classification_reviews r WHERE r.decision_id = d.id)`

// scanDecision reads one decision row. withSource keeps the source text,
// which only the single decision read returns.
func scanDecision(row scanner, withSource bool) (*model.ClassificationDecision, error) {
	decision := &model.ClassificationDecision{}
	var source sql.NullString
	var removedAt sql.NullTime
	var modelEvidence, selections, reviewReasons string
	var confidence sql.NullFloat64
	if err := row.Scan(&decision.ID, &decision.BoardID, &decision.OrganizationID, &decision.CardID, &decision.PublicationKey,
		&decision.OperationID, &decision.CheckpointID, &decision.AttemptNumber, &decision.InitiatingPrincipalID,
		&decision.SourceDigest, &decision.SourceRevision, &source, &removedAt, &decision.TaxonomyRevision, &decision.TaxonomyDigest,
		&decision.PolicyRevision, &decision.PromptRevision, &modelEvidence, &decision.Origin, &selections, &decision.Rationale,
		&confidence, &decision.NeedsReview, &reviewReasons, &decision.CompletedAt, &decision.PublishedAt, &decision.ReviewCount); err != nil {
		return nil, err
	}
	if withSource && source.Valid {
		decision.Source = &msg.ClassificationSourceSnapshot{}
		if err := json.Unmarshal([]byte(source.String), decision.Source); err != nil {
			return nil, fmt.Errorf("decision %s source does not parse: %w", decision.ID, err)
		}
	}
	if removedAt.Valid {
		decision.SourceRemovedAt = &removedAt.Time
	}
	if confidence.Valid {
		decision.ModelReportedConfidence = &confidence.Float64
	}
	for what, pair := range map[string]struct {
		raw    string
		target any
	}{"model": {modelEvidence, &decision.Model}, "selections": {selections, &decision.Selections}, "review_reasons": {reviewReasons, &decision.ReviewReasons}} {
		if err := json.Unmarshal([]byte(pair.raw), pair.target); err != nil {
			return nil, fmt.Errorf("decision %s %s does not parse: %w", decision.ID, what, err)
		}
	}
	return decision, nil
}

func readDecision(run executor, boardID, decisionID string, withSource bool) (*model.ClassificationDecision, error) {
	decision, err := scanDecision(run.QueryRow(`SELECT `+decisionColumns+` FROM classification_decisions d WHERE d.board_id = ? AND d.id = ?`, boardID, decisionID), withSource)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return decision, err
}

// ClassificationDecisionFilter narrows a board's decisions. Empty fields do
// not filter.
type ClassificationDecisionFilter struct {
	CardID       string
	ReviewState  model.ClassificationReviewState
	AxisID       string
	ValueID      string
	SourceDigest string
	OperationID  string
}

// MaximumClassificationDecisionPage is the most decisions one page returns.
const MaximumClassificationDecisionPage = 100

// ErrBadCursor is a cursor that is malformed or belongs to another board.
var ErrBadCursor = errors.New("cursor is not one this board's decision list issued")

func encodeDecisionCursor(boardID string, sequence int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(boardID + "|" + strconv.FormatInt(sequence, 10)))
}

func decodeDecisionCursor(boardID, cursor string) (int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, ErrBadCursor
	}
	cursorBoard, sequenceText, found := strings.Cut(string(raw), "|")
	if !found || cursorBoard != boardID {
		return 0, ErrBadCursor
	}
	sequence, err := strconv.ParseInt(sequenceText, 10, 64)
	if err != nil {
		return 0, ErrBadCursor
	}
	return sequence, nil
}

// ListClassificationDecisions is one page of a board's decisions, newest
// first, without their source text. Total counts every match.
func (s *Store) ListClassificationDecisions(boardID string, filter ClassificationDecisionFilter, cursor string, limit int) (*model.ClassificationDecisionPage, error) {
	if _, err := boardOrganization(s.db, boardID); err != nil {
		return nil, err
	}
	where := []string{"d.board_id = ?"}
	arguments := []any{boardID}
	for column, value := range map[string]string{"d.card_id": filter.CardID, "d.source_digest": filter.SourceDigest, "d.operation_id": filter.OperationID} {
		if value != "" {
			where = append(where, column+" = ?")
			arguments = append(arguments, value)
		}
	}
	switch filter.ReviewState {
	case "":
	case model.ClassificationReviewStateUnreviewed:
		where = append(where, "NOT EXISTS (SELECT 1 FROM classification_reviews r WHERE r.decision_id = d.id)")
	case model.ClassificationReviewStateReviewed:
		where = append(where, "EXISTS (SELECT 1 FROM classification_reviews r WHERE r.decision_id = d.id)")
	case model.ClassificationReviewStateNeedsReview:
		where = append(where, "d.needs_review = 1 AND NOT EXISTS (SELECT 1 FROM classification_reviews r WHERE r.decision_id = d.id)")
	default:
		return nil, fmt.Errorf("review_state %q is not one of %v", filter.ReviewState, model.ClassificationReviewStates)
	}
	switch {
	case filter.AxisID != "" && filter.ValueID != "":
		where = append(where, "EXISTS (SELECT 1 FROM json_each(d.selections) axis, json_each(axis.value) chosen WHERE axis.key = ? AND chosen.value = ?)")
		arguments = append(arguments, filter.AxisID, filter.ValueID)
	case filter.AxisID != "":
		where = append(where, "EXISTS (SELECT 1 FROM json_each(d.selections) axis WHERE axis.key = ?)")
		arguments = append(arguments, filter.AxisID)
	case filter.ValueID != "":
		return nil, errors.New("value_id needs axis_id")
	}
	page := &model.ClassificationDecisionPage{Decisions: []model.ClassificationDecision{}}
	conditions := strings.Join(where, " AND ")
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM classification_decisions d WHERE `+conditions, arguments...).Scan(&page.Total); err != nil {
		return nil, err
	}
	if cursor != "" {
		before, err := decodeDecisionCursor(boardID, cursor)
		if err != nil {
			return nil, err
		}
		conditions += " AND d.sequence < ?"
		arguments = append(arguments, before)
	}
	rows, err := s.db.Query(`SELECT `+decisionColumns+`, d.sequence FROM classification_decisions d WHERE `+conditions+` ORDER BY d.sequence DESC LIMIT ?`,
		append(arguments, limit+1)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sequences []int64
	for rows.Next() {
		var sequence int64
		decision, err := scanDecision(sequencedRow{rows, &sequence}, false)
		if err != nil {
			return nil, err
		}
		page.Decisions = append(page.Decisions, *decision)
		sequences = append(sequences, sequence)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(page.Decisions) > limit {
		page.Decisions = page.Decisions[:limit]
		page.NextCursor = encodeDecisionCursor(boardID, sequences[limit-1])
	}
	return page, nil
}

// sequencedRow scans a decision row followed by its sequence.
type sequencedRow struct {
	rows     *sql.Rows
	sequence *int64
}

func (row sequencedRow) Scan(destinations ...any) error {
	return row.rows.Scan(append(destinations, row.sequence)...)
}

// ClassificationDecisionDetail is one decision with its source and taxonomy,
// its reviews, and the card's labels now.
func (s *Store) ClassificationDecisionDetail(boardID, decisionID string) (*model.ClassificationDecisionDetail, error) {
	decision, err := readDecision(s.db, boardID, decisionID, true)
	if err != nil {
		return nil, err
	}
	taxonomy, err := taxonomyRevisionNumbered(s.db, boardID, decision.TaxonomyRevision)
	if err != nil {
		return nil, err
	}
	decision.Taxonomy = taxonomy.Taxonomy
	reviews, err := reviewsOfDecision(s.db, decisionID)
	if err != nil {
		return nil, err
	}
	labels, err := cardLabels(s.db, boardID, decision.CardID)
	if err != nil {
		return nil, err
	}
	return &model.ClassificationDecisionDetail{Decision: *decision, Reviews: reviews, Labels: *labels}, nil
}

const reviewColumns = `id, decision_id, board_id, card_id, reviewer_principal_id, action, axis_ids, corrections, explanation,
	supersedes_review_ids, expected_labels_revision, resulting_labels_revision, taxonomy_revision, policy_revision, idempotency_key, created_at`

func scanReview(row scanner) (*model.ClassificationReview, error) {
	review := &model.ClassificationReview{}
	var axisIDs, supersedes string
	var corrections sql.NullString
	if err := row.Scan(&review.ID, &review.DecisionID, &review.BoardID, &review.CardID, &review.ReviewerPrincipalID, &review.Action,
		&axisIDs, &corrections, &review.Explanation, &supersedes, &review.ExpectedLabelsRevision, &review.ResultingLabelsRevision,
		&review.TaxonomyRevision, &review.PolicyRevision, &review.IdempotencyKey, &review.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(axisIDs), &review.AxisIDs); err != nil {
		return nil, fmt.Errorf("review %s axis_ids does not parse: %w", review.ID, err)
	}
	if err := json.Unmarshal([]byte(supersedes), &review.SupersedesReviewIDs); err != nil {
		return nil, fmt.Errorf("review %s supersedes_review_ids does not parse: %w", review.ID, err)
	}
	if corrections.Valid {
		if err := json.Unmarshal([]byte(corrections.String), &review.Corrections); err != nil {
			return nil, fmt.Errorf("review %s corrections does not parse: %w", review.ID, err)
		}
	}
	return review, nil
}

func reviewsOfDecision(run executor, decisionID string) ([]model.ClassificationReview, error) {
	rows, err := run.Query(`SELECT `+reviewColumns+` FROM classification_reviews WHERE decision_id = ? ORDER BY created_at, id`, decisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	reviews := []model.ClassificationReview{}
	for rows.Next() {
		review, err := scanReview(rows)
		if err != nil {
			return nil, err
		}
		reviews = append(reviews, *review)
	}
	return reviews, rows.Err()
}

// CardClassificationLabels is every label a card carries on a board, with
// the labels revision; a card never labeled has revision 0 and no labels.
func (s *Store) CardClassificationLabels(boardID, cardID string) (*model.CardClassificationLabels, error) {
	if _, err := s.GetPlacement(cardID, boardID); err != nil {
		return nil, err
	}
	return cardLabels(s.db, boardID, cardID)
}

func cardLabels(run executor, boardID, cardID string) (*model.CardClassificationLabels, error) {
	out := &model.CardClassificationLabels{BoardID: boardID, CardID: cardID, Labels: []model.ClassificationLabel{}}
	err := run.QueryRow(`SELECT revision FROM classification_label_revisions WHERE board_id = ? AND card_id = ?`, boardID, cardID).Scan(&out.Revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rows, err := run.Query(`SELECT axis_id, value_ids, origin_decision_id, origin_review_id, updated_at FROM classification_labels
		WHERE board_id = ? AND card_id = ? ORDER BY axis_id`, boardID, cardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var label model.ClassificationLabel
		var valueIDs string
		if err := rows.Scan(&label.AxisID, &valueIDs, &label.OriginDecisionID, &label.OriginReviewID, &label.UpdatedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(valueIDs), &label.ValueIDs); err != nil {
			return nil, fmt.Errorf("card %s label on axis %s does not parse: %w", cardID, label.AxisID, err)
		}
		out.Labels = append(out.Labels, label)
	}
	return out, rows.Err()
}

// ReviewClassificationDecision records one person's review and, for accept
// and correct, sets the card's labels on the reviewed axes — both or neither.
func (s *Store) ReviewClassificationDecision(boardID, decisionID, reviewerPrincipalID string, request model.ClassificationReviewRequest) (*model.ClassificationReviewResult, bool, error) {
	var result *model.ClassificationReviewResult
	created := false
	err := s.writeImmediately(func(run executor) error {
		encodedRequest, err := json.Marshal(request)
		if err != nil {
			return err
		}
		requestDigest := sha256Hex(encodedRequest)
		existing, err := scanReview(run.QueryRow(`SELECT `+reviewColumns+` FROM classification_reviews WHERE board_id = ? AND reviewer_principal_id = ? AND idempotency_key = ?`,
			boardID, reviewerPrincipalID, request.IdempotencyKey))
		if err == nil {
			var existingDigest string
			if err := run.QueryRow(`SELECT request_digest FROM classification_reviews WHERE id = ?`, existing.ID).Scan(&existingDigest); err != nil {
				return err
			}
			if existingDigest != requestDigest || existing.DecisionID != decisionID {
				refusal := refuse(ClassificationRefusalConflict, "idempotency_key_reused", "review %s already used idempotency key %q with a different request", existing.ID, request.IdempotencyKey)
				refusal.Current = existing
				return refusal
			}
			labels, err := cardLabels(run, boardID, existing.CardID)
			if err != nil {
				return err
			}
			result = &model.ClassificationReviewResult{Review: *existing, Labels: *labels}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		decision, err := readDecision(run, boardID, decisionID, false)
		if errors.Is(err, ErrNotFound) {
			return refuse(ClassificationRefusalNotFound, "decision_not_found", "board %s has no decision %s", boardID, decisionID)
		}
		if err != nil {
			return err
		}
		taxonomy, err := currentTaxonomyRevision(run, boardID)
		if err != nil {
			return err
		}
		if taxonomy.Taxonomy == nil {
			return refuse(ClassificationRefusalConflict, "board_has_no_taxonomy", "board %s has no taxonomy now, so nothing on it can be reviewed", boardID)
		}
		policy, err := currentPolicy(run, boardID)
		if err != nil {
			return err
		}
		newValues, err := reviewedValues(request, decision, *taxonomy.Taxonomy, policy)
		if err != nil {
			return err
		}
		labels, err := cardLabels(run, boardID, decision.CardID)
		if err != nil {
			return err
		}
		if labels.Revision != request.ExpectedLabelsRevision {
			refusal := refuse(ClassificationRefusalConflict, "labels_revision_moved", "the card's labels are at revision %d, and the review expected %d", labels.Revision, request.ExpectedLabelsRevision)
			refusal.Current = labels
			return refusal
		}
		if err := checkSupersedes(request, labels, newValues); err != nil {
			return err
		}
		stamp := now()
		resulting := labels.Revision
		if len(newValues) > 0 {
			resulting++
		}
		review := model.ClassificationReview{
			ID: uuid.NewString(), DecisionID: decisionID, BoardID: boardID, CardID: decision.CardID, ReviewerPrincipalID: reviewerPrincipalID,
			Action: request.Action, AxisIDs: request.AxisIDs, Corrections: request.Corrections, Explanation: request.Explanation,
			SupersedesReviewIDs: request.SupersedesReviewIDs, ExpectedLabelsRevision: request.ExpectedLabelsRevision,
			ResultingLabelsRevision: resulting, TaxonomyRevision: taxonomy.Revision, IdempotencyKey: request.IdempotencyKey, CreatedAt: stamp,
		}
		if review.SupersedesReviewIDs == nil {
			review.SupersedesReviewIDs = []string{}
		}
		if policy != nil {
			review.PolicyRevision = policy.Revision
		}
		axisIDs, _ := json.Marshal(review.AxisIDs)
		supersedes, _ := json.Marshal(review.SupersedesReviewIDs)
		var corrections any
		if review.Corrections != nil {
			encoded, _ := json.Marshal(review.Corrections)
			corrections = string(encoded)
		}
		if _, err := run.Exec(`INSERT INTO classification_reviews (id, decision_id, board_id, card_id, reviewer_principal_id, action, axis_ids, corrections,
			explanation, supersedes_review_ids, expected_labels_revision, resulting_labels_revision, taxonomy_revision, policy_revision,
			idempotency_key, request_digest, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			review.ID, review.DecisionID, boardID, review.CardID, reviewerPrincipalID, string(review.Action), string(axisIDs), corrections,
			review.Explanation, string(supersedes), review.ExpectedLabelsRevision, review.ResultingLabelsRevision, review.TaxonomyRevision,
			review.PolicyRevision, review.IdempotencyKey, requestDigest, stamp); err != nil {
			return err
		}
		if len(newValues) > 0 {
			for axisID, valueIDs := range newValues {
				encoded, _ := json.Marshal(valueIDs)
				if _, err := run.Exec(`INSERT INTO classification_labels (board_id, card_id, axis_id, value_ids, origin_decision_id, origin_review_id, updated_at)
					VALUES (?, ?, ?, ?, ?, ?, ?)
					ON CONFLICT (board_id, card_id, axis_id) DO UPDATE SET value_ids = excluded.value_ids,
						origin_decision_id = excluded.origin_decision_id, origin_review_id = excluded.origin_review_id, updated_at = excluded.updated_at`,
					boardID, review.CardID, axisID, string(encoded), decisionID, review.ID, stamp); err != nil {
					return err
				}
			}
			if _, err := run.Exec(`INSERT INTO classification_label_revisions (board_id, card_id, revision, updated_at) VALUES (?, ?, ?, ?)
				ON CONFLICT (board_id, card_id) DO UPDATE SET revision = excluded.revision, updated_at = excluded.updated_at`,
				boardID, review.CardID, resulting, stamp); err != nil {
				return err
			}
		}
		after, err := cardLabels(run, boardID, review.CardID)
		if err != nil {
			return err
		}
		created = true
		result = &model.ClassificationReviewResult{Review: review, Labels: *after}
		return nil
	})
	return result, created, err
}

// reviewedValues checks a review against the decision, the current taxonomy
// and the current policy, and returns the labels it sets, by axis. Reject
// sets none.
func reviewedValues(request model.ClassificationReviewRequest, decision *model.ClassificationDecision, taxonomy msg.ClassificationTaxonomy, policy *msg.BoardClassificationPolicy) (map[string][]string, error) {
	if strings.TrimSpace(request.IdempotencyKey) == "" {
		return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "idempotency_key is required")
	}
	known := false
	for _, action := range model.ClassificationReviewActions {
		if request.Action == action {
			known = true
		}
	}
	if !known {
		return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "action %q is not one of %v", request.Action, model.ClassificationReviewActions)
	}
	if len(request.AxisIDs) == 0 {
		return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "axis_ids is required: name the axes you reviewed")
	}
	if request.Action == model.ClassificationReviewActionCorrect {
		if len(request.Corrections) != len(request.AxisIDs) {
			return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "corrections must name exactly the axes in axis_ids")
		}
	} else if len(request.Corrections) > 0 {
		return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "corrections go with correct only")
	}
	out := map[string][]string{}
	for _, axisID := range request.AxisIDs {
		if _, twice := out[axisID]; twice {
			return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "axis %s is named twice", axisID)
		}
		axis, found := taxonomy.AxisByID(axisID)
		if !found || axis.Archived {
			return nil, refuse(ClassificationRefusalConflict, "taxonomy_incompatible", "axis %s is not an axis of the board's current taxonomy", axisID)
		}
		if mode := policy.ModeOf(axisID); mode != msg.ClassificationAxisModeAssisted {
			return nil, refuse(ClassificationRefusalConflict, "axis_not_assisted", "axis %s is %s under the board's current policy; only an assisted axis is reviewed", axisID, mode)
		}
		var valueIDs []string
		switch request.Action {
		case model.ClassificationReviewActionAccept:
			selected, answered := decision.Selections[axisID]
			if !answered {
				return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "the decision has no answer on axis %s to accept", axisID)
			}
			valueIDs = selected
		case model.ClassificationReviewActionCorrect:
			corrected, given := request.Corrections[axisID]
			if !given {
				return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "corrections has no values for axis %s", axisID)
			}
			valueIDs = corrected
		case model.ClassificationReviewActionReject:
			out[axisID] = nil
			continue
		}
		if valueIDs == nil {
			valueIDs = []string{}
		}
		if err := msg.ValidateAxisSelection(axis, valueIDs); err != nil {
			return nil, refuse(ClassificationRefusalConflict, "taxonomy_incompatible", "%v", err)
		}
		if axis.Required && len(valueIDs) == 0 {
			return nil, refuse(ClassificationRefusalInvalid, "review_invalid", "axis %s is required and would be left empty; correct it with a value", axisID)
		}
		out[axisID] = valueIDs
	}
	if request.Action == model.ClassificationReviewActionReject {
		return map[string][]string{}, nil
	}
	return out, nil
}

// checkSupersedes makes a review name every review whose label it replaces,
// and nothing else.
func checkSupersedes(request model.ClassificationReviewRequest, labels *model.CardClassificationLabels, newValues map[string][]string) error {
	replaced := map[string]bool{}
	for _, label := range labels.Labels {
		if _, changing := newValues[label.AxisID]; changing && label.OriginReviewID != "" {
			replaced[label.OriginReviewID] = true
		}
	}
	named := map[string]bool{}
	for _, reviewID := range request.SupersedesReviewIDs {
		if !replaced[reviewID] {
			return refuse(ClassificationRefusalInvalid, "review_invalid", "supersedes_review_ids names %s, which set no label this review changes", reviewID)
		}
		named[reviewID] = true
	}
	for reviewID := range replaced {
		if !named[reviewID] {
			refusal := refuse(ClassificationRefusalConflict, "supersedes_required", "review %s set a label this review would replace; name it in supersedes_review_ids", reviewID)
			refusal.Current = labels
			return refusal
		}
	}
	return nil
}

// RemoveClassificationSourcesOfCard deletes the text every decision about a
// card was given, keeping its digest, when the card itself is purged: a
// decision must not keep what its card no longer has.
func (s *Store) RemoveClassificationSourcesOfCard(cardID string) error {
	_, err := s.db.Exec(`UPDATE classification_decisions SET source_snapshot = NULL, source_removed_at = ? WHERE card_id = ? AND source_snapshot IS NOT NULL`, now(), cardID)
	return err
}
