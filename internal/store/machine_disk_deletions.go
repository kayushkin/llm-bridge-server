package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// machine_disk_deletions records every deletion asked for on the Machines
// page's disk usage: who asked, for what, and how it ended. A row is written
// before the runner is asked, so a deletion that starts is never
// unrecorded, and rows are never deleted.

// MachineDiskDeletionIDPrefix starts every deletion id: machine_disk_deletion_000001.
const MachineDiskDeletionIDPrefix = "machine_disk_deletion_"

// MachineDiskDeletion is one recorded deletion.
type MachineDiskDeletion struct {
	ID                     string                     `json:"id"`
	MachineID              string                     `json:"machine_id"`
	Path                   string                     `json:"path"`
	Kind                   msg.DiskUsageEntryKind     `json:"kind"`
	ExpectedSizeBytes      uint64                     `json:"expected_size_bytes"`
	RequestedByPrincipalID string                     `json:"requested_by_principal_id,omitempty"`
	RequestedAt            time.Time                  `json:"requested_at"`
	State                  msg.DiskUsageDeletionState `json:"state"`
	FinishedAt             *time.Time                 `json:"finished_at,omitempty"`
	DeletedBytes           uint64                     `json:"deleted_bytes"`
	DeletedFileCount       int64                      `json:"deleted_file_count"`
	FailedEntryCount       int64                      `json:"failed_entry_count"`
	Errors                 []string                   `json:"errors,omitempty"`
	RefusalReason          string                     `json:"refusal_reason,omitempty"`
}

func (s *Store) migrateMachineDiskDeletions() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS machine_disk_deletions (
			number                    INTEGER PRIMARY KEY AUTOINCREMENT,
			machine_id                TEXT NOT NULL,
			path                      TEXT NOT NULL,
			kind                      TEXT NOT NULL,
			expected_size_bytes       INTEGER NOT NULL,
			requested_by_principal_id TEXT NOT NULL DEFAULT '',
			requested_at              TEXT NOT NULL,
			state                     TEXT NOT NULL,
			finished_at               TEXT,
			deleted_bytes             INTEGER NOT NULL DEFAULT 0,
			deleted_file_count        INTEGER NOT NULL DEFAULT 0,
			failed_entry_count        INTEGER NOT NULL DEFAULT 0,
			errors_json               TEXT NOT NULL DEFAULT '[]',
			refusal_reason            TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_machine_disk_deletions_machine ON machine_disk_deletions(machine_id, number);
	`)
	return err
}

func machineDiskDeletionID(number int64) string {
	return fmt.Sprintf("%s%06d", MachineDiskDeletionIDPrefix, number)
}

func machineDiskDeletionNumber(id string) (int64, error) {
	number, err := strconv.ParseInt(strings.TrimPrefix(id, MachineDiskDeletionIDPrefix), 10, 64)
	if err != nil || !strings.HasPrefix(id, MachineDiskDeletionIDPrefix) {
		return 0, fmt.Errorf("%q is not a disk deletion id", id)
	}
	return number, nil
}

// RecordRequestedMachineDiskDeletion writes a deletion about to be asked
// for, in state requested, and returns it with its id.
func (s *Store) RecordRequestedMachineDiskDeletion(deletion MachineDiskDeletion) (MachineDiskDeletion, error) {
	if deletion.MachineID == "" || deletion.Path == "" || deletion.Kind == "" {
		return deletion, fmt.Errorf("record disk deletion: machine, path and kind are required")
	}
	deletion.RequestedAt = time.Now().UTC()
	deletion.State = msg.DiskUsageDeletionRequested
	result, err := s.db.Exec(`
		INSERT INTO machine_disk_deletions (machine_id, path, kind, expected_size_bytes, requested_by_principal_id, requested_at, state)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		deletion.MachineID, deletion.Path, string(deletion.Kind), int64(deletion.ExpectedSizeBytes),
		deletion.RequestedByPrincipalID, deletion.RequestedAt.Format(machineStatsTimeFormat), string(deletion.State))
	if err != nil {
		return deletion, err
	}
	number, err := result.LastInsertId()
	if err != nil {
		return deletion, err
	}
	deletion.ID = machineDiskDeletionID(number)
	return deletion, nil
}

// ErrMachineDiskDeletionNotFound is returned for an id no row has.
var ErrMachineDiskDeletionNotFound = errors.New("no such disk deletion")

// UpdateMachineDiskDeletion writes where a deletion is: refused, running, or
// how it ended. It is refused for a row of another machine, so a runner can
// settle only its own machine's deletions.
func (s *Store) UpdateMachineDiskDeletion(machineID, id string, update msg.DiskUsageDeletion, refusalReason string) error {
	number, err := machineDiskDeletionNumber(id)
	if err != nil {
		return err
	}
	errorsJSON, err := json.Marshal(append([]string{}, update.Errors...))
	if err != nil {
		return err
	}
	var finishedAt any
	if update.FinishedAt != nil {
		finishedAt = update.FinishedAt.UTC().Format(machineStatsTimeFormat)
	}
	result, err := s.db.Exec(`
		UPDATE machine_disk_deletions
		SET state = ?, finished_at = ?, deleted_bytes = ?, deleted_file_count = ?, failed_entry_count = ?, errors_json = ?, refusal_reason = ?
		WHERE number = ? AND machine_id = ?`,
		string(update.State), finishedAt, int64(update.DeletedBytes), update.DeletedFileCount, update.FailedEntryCount,
		string(errorsJSON), refusalReason, number, machineID)
	if err != nil {
		return err
	}
	if changed, _ := result.RowsAffected(); changed == 0 {
		return fmt.Errorf("%w: %s on machine %s", ErrMachineDiskDeletionNotFound, id, machineID)
	}
	return nil
}

// MarkUnfinishedMachineDiskDeletionsLost marks a machine's deletions still
// requested or running as lost, when its runner went away before saying
// how they ended. A report that arrives later still settles them.
func (s *Store) MarkUnfinishedMachineDiskDeletionsLost(machineID string) (int64, error) {
	result, err := s.db.Exec(`UPDATE machine_disk_deletions SET state = ? WHERE machine_id = ? AND state IN (?, ?)`,
		string(msg.DiskUsageDeletionLost), machineID, string(msg.DiskUsageDeletionRequested), string(msg.DiskUsageDeletionRunning))
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// MachineDiskDeletions returns a machine's newest deletions first.
func (s *Store) MachineDiskDeletions(machineID string, limit int) ([]MachineDiskDeletion, error) {
	rows, err := s.db.Query(`
		SELECT number, machine_id, path, kind, expected_size_bytes, requested_by_principal_id, requested_at, state,
		       finished_at, deleted_bytes, deleted_file_count, failed_entry_count, errors_json, refusal_reason
		FROM machine_disk_deletions WHERE machine_id = ? ORDER BY number DESC LIMIT ?`, machineID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deletions := []MachineDiskDeletion{}
	for rows.Next() {
		var deletion MachineDiskDeletion
		var number, expected, deletedBytes int64
		var kind, requestedAt, state, errorsJSON string
		var finishedAt sql.NullString
		if err := rows.Scan(&number, &deletion.MachineID, &deletion.Path, &kind, &expected, &deletion.RequestedByPrincipalID,
			&requestedAt, &state, &finishedAt, &deletedBytes, &deletion.DeletedFileCount, &deletion.FailedEntryCount,
			&errorsJSON, &deletion.RefusalReason); err != nil {
			return nil, err
		}
		deletion.ID = machineDiskDeletionID(number)
		deletion.Kind = msg.DiskUsageEntryKind(kind)
		deletion.State = msg.DiskUsageDeletionState(state)
		deletion.ExpectedSizeBytes = uint64(expected)
		deletion.DeletedBytes = uint64(deletedBytes)
		if deletion.RequestedAt, err = time.Parse(machineStatsTimeFormat, requestedAt); err != nil {
			return nil, fmt.Errorf("disk deletion %s: requested_at %q: %w", deletion.ID, requestedAt, err)
		}
		if finishedAt.Valid {
			finished, err := time.Parse(machineStatsTimeFormat, finishedAt.String)
			if err != nil {
				return nil, fmt.Errorf("disk deletion %s: finished_at %q: %w", deletion.ID, finishedAt.String, err)
			}
			deletion.FinishedAt = &finished
		}
		if err := json.Unmarshal([]byte(errorsJSON), &deletion.Errors); err != nil {
			return nil, fmt.Errorf("disk deletion %s: errors: %w", deletion.ID, err)
		}
		deletions = append(deletions, deletion)
	}
	return deletions, rows.Err()
}

// MarkMachineDiskDeletionRunning records that the runner started a
// deletion, unless its report of the end has already arrived.
func (s *Store) MarkMachineDiskDeletionRunning(machineID, id string) error {
	number, err := machineDiskDeletionNumber(id)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE machine_disk_deletions SET state = ? WHERE number = ? AND machine_id = ? AND state = ?`,
		string(msg.DiskUsageDeletionRunning), number, machineID, string(msg.DiskUsageDeletionRequested))
	return err
}
