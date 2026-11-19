package snapshotstore

import (
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// Phase identifies when a snapshot was captured relative to a tool call.
type Phase string

const (
	PhaseBefore Phase = "before"
	PhaseAfter  Phase = "after"
)

// Snapshot is one captured file state for a single tool call phase.
type Snapshot struct {
	SessionID string    `json:"session_id"`
	ToolUseID string    `json:"tool_use_id"`
	Phase     Phase     `json:"phase"`
	FilePath  string    `json:"file_path"`
	BlobSHA   string    `json:"blob_sha,omitempty"`
	Size      int64     `json:"size"`
	IsBinary  bool      `json:"is_binary,omitempty"`
	TooLarge  bool      `json:"too_large,omitempty"`
	Missing   bool      `json:"missing,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Capture reads filePath and records a snapshot row. If the file is missing,
// binary, or over MaxSize, a metadata-only row is written and no blob is
// stored. Returns the written Snapshot.
func (s *Store) Capture(sessionID, toolUseID string, phase Phase, filePath string) (*Snapshot, error) {
	if sessionID == "" || toolUseID == "" {
		return nil, fmt.Errorf("snapshotstore: sessionID and toolUseID required")
	}
	if phase != PhaseBefore && phase != PhaseAfter {
		return nil, fmt.Errorf("snapshotstore: invalid phase %q", phase)
	}

	snap := &Snapshot{
		SessionID: sessionID,
		ToolUseID: toolUseID,
		Phase:     phase,
		FilePath:  filePath,
	}

	info, err := os.Stat(filePath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		snap.Missing = true
		return snap, s.insert(snap)
	case err != nil:
		return nil, fmt.Errorf("stat %s: %w", filePath, err)
	case info.IsDir():
		return nil, fmt.Errorf("snapshotstore: %s is a directory", filePath)
	}

	snap.Size = info.Size()
	if snap.Size > s.maxSize {
		snap.TooLarge = true
		return snap, s.insert(snap)
	}

	content, err := readCapped(filePath, s.maxSize)
	if err != nil {
		return nil, err
	}
	if IsBinary(content) {
		snap.IsBinary = true
		return snap, s.insert(snap)
	}

	sha, err := s.WriteBlob(content)
	if err != nil {
		return nil, err
	}
	snap.BlobSHA = sha
	return snap, s.insert(snap)
}

// Get returns every snapshot recorded for the (session, tool_use_id) pair —
// across all files and both phases. Callers that want one before/after pair
// per file should group by FilePath. Order is unspecified.
func (s *Store) Get(sessionID, toolUseID string) ([]Snapshot, error) {
	rows, err := s.db.Query(`
		SELECT session_id, tool_use_id, phase, file_path, blob_sha, size,
		       is_binary, too_large, missing, created_at
		FROM snapshots WHERE session_id = ? AND tool_use_id = ?`,
		sessionID, toolUseID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Snapshot
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *snap)
	}
	return out, rows.Err()
}

// PurgeOlderThan deletes snapshot rows created before cutoff. Returns the
// number of rows removed. Callers should run GC() afterwards to reclaim
// disk from blobs left unreferenced.
func (s *Store) PurgeOlderThan(cutoff time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM snapshots WHERE created_at < ?`, cutoff.UTC())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) insert(snap *Snapshot) error {
	now := time.Now().UTC()
	snap.CreatedAt = now
	_, err := s.db.Exec(`
		INSERT OR REPLACE INTO snapshots
		(session_id, tool_use_id, phase, file_path, blob_sha, size,
		 is_binary, too_large, missing, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.SessionID, snap.ToolUseID, string(snap.Phase), snap.FilePath,
		snap.BlobSHA, snap.Size, snap.IsBinary, snap.TooLarge, snap.Missing, now,
	)
	return err
}

func scanSnapshot(rows *sql.Rows) (*Snapshot, error) {
	var snap Snapshot
	var phase string
	if err := rows.Scan(
		&snap.SessionID, &snap.ToolUseID, &phase, &snap.FilePath,
		&snap.BlobSHA, &snap.Size, &snap.IsBinary, &snap.TooLarge,
		&snap.Missing, &snap.CreatedAt,
	); err != nil {
		return nil, err
	}
	snap.Phase = Phase(phase)
	return &snap, nil
}

func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit+1))
}
