package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// SessionFile is the canonical type from llm-bridge/msg/event.go.
type SessionFile = msg.SessionFile

// ErrSessionFileNotFound is a file id this session does not hold.
var ErrSessionFileNotFound = errors.New("session file not found")

// InsertSessionFile records a file shared into a session.
func (s *Store) InsertSessionFile(file SessionFile) error {
	generation := ""
	if file.Generation != nil {
		encoded, err := json.Marshal(file.Generation)
		if err != nil {
			return fmt.Errorf("encode generation of session file %s: %w", file.FileID, err)
		}
		generation = string(encoded)
	}
	_, err := s.db.Exec(
		`INSERT INTO session_files (file_id, session_id, filename, media_type, size_bytes, shared_by, path, source_path, generation, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		file.FileID, file.SessionID, file.Filename, file.MediaType, file.SizeBytes, string(file.SharedBy), file.Path, file.SourcePath, generation, file.CreatedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert session file %s: %w", file.FileID, err)
	}
	return nil
}

// ListSessionFiles returns every file shared into a session, oldest first.
func (s *Store) ListSessionFiles(sessionID string) ([]SessionFile, error) {
	rows, err := s.db.Query(
		`SELECT file_id, session_id, filename, media_type, size_bytes, shared_by, path, source_path, generation, created_at
		 FROM session_files WHERE session_id = $1 ORDER BY created_at, file_id`, sessionID)
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
		`SELECT file_id, session_id, filename, media_type, size_bytes, shared_by, path, source_path, generation, created_at
		 FROM session_files WHERE session_id = $1 AND file_id = $2`, sessionID, fileID)
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
	var sharedBy, generation string
	var createdAt time.Time
	if err := scanner.Scan(&file.FileID, &file.SessionID, &file.Filename, &file.MediaType, &file.SizeBytes, &sharedBy, &file.Path, &file.SourcePath, &generation, &createdAt); err != nil {
		return SessionFile{}, err
	}
	file.SharedBy = msg.SessionFileSharer(sharedBy)
	if generation != "" {
		file.Generation = &msg.SessionFileGeneration{}
		if err := json.Unmarshal([]byte(generation), file.Generation); err != nil {
			return SessionFile{}, fmt.Errorf("session file %s: stored generation is not valid JSON: %w", file.FileID, err)
		}
	}
	file.CreatedAt = createdAt.UTC()
	return file, nil
}
