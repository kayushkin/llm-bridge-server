package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// SessionFile is the canonical type from llm-bridge/msg/event.go.
type SessionFile = msg.SessionFile

// ErrSessionFileNotFound is a file id this session does not hold.
var ErrSessionFileNotFound = errors.New("session file not found")

// migrateSessionFiles creates the table of which file-store file belongs to
// which session. Called from migrate().
//
// file-store owns the bytes and their record; this server owns only which
// session a file was shared into, by whom, and where the agent reads it. So
// file_id is file-store's id and the primary key: one file is shared into one
// session.
func (s *Store) migrateSessionFiles() error {
	_, err := s.db.Exec(`
		CREATE TABLE IF NOT EXISTS session_files (
			file_id    TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			filename   TEXT NOT NULL,
			media_type TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			shared_by  TEXT NOT NULL,
			path       TEXT NOT NULL,
			created_at DATETIME NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_session_files_session ON session_files(session_id, created_at);
	`)
	return err
}

// InsertSessionFile records a file shared into a session.
func (s *Store) InsertSessionFile(file SessionFile) error {
	_, err := s.db.Exec(
		`INSERT INTO session_files (file_id, session_id, filename, media_type, size_bytes, shared_by, path, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		file.FileID, file.SessionID, file.Filename, file.MediaType, file.SizeBytes, string(file.SharedBy), file.Path, file.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert session file %s: %w", file.FileID, err)
	}
	return nil
}

// ListSessionFiles returns every file shared into a session, oldest first.
func (s *Store) ListSessionFiles(sessionID string) ([]SessionFile, error) {
	rows, err := s.db.Query(
		`SELECT file_id, session_id, filename, media_type, size_bytes, shared_by, path, created_at
		 FROM session_files WHERE session_id = ? ORDER BY created_at, file_id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	files := []SessionFile{}
	for rows.Next() {
		file, err := scanSessionFile(rows)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
	}
	return files, rows.Err()
}

// GetSessionFile returns one file of one session. A file shared into another
// session is ErrSessionFileNotFound here, the same answer as a missing one:
// the session in the path is what access was checked against.
func (s *Store) GetSessionFile(sessionID, fileID string) (SessionFile, error) {
	row := s.db.QueryRow(
		`SELECT file_id, session_id, filename, media_type, size_bytes, shared_by, path, created_at
		 FROM session_files WHERE session_id = ? AND file_id = ?`, sessionID, fileID)
	file, err := scanSessionFile(row)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionFile{}, ErrSessionFileNotFound
	}
	return file, err
}

type sessionFileScanner interface {
	Scan(dest ...any) error
}

func scanSessionFile(scanner sessionFileScanner) (SessionFile, error) {
	var file SessionFile
	var sharedBy string
	var createdAt time.Time
	if err := scanner.Scan(&file.FileID, &file.SessionID, &file.Filename, &file.MediaType, &file.SizeBytes, &sharedBy, &file.Path, &createdAt); err != nil {
		return SessionFile{}, err
	}
	file.SharedBy = msg.SessionFileSharer(sharedBy)
	file.CreatedAt = createdAt.UTC()
	return file, nil
}
