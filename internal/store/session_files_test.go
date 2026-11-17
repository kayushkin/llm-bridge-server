package store

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

// A session_files table from before files said where they came from: its
// rows keep reading, with no origin, once the store has opened it, and
// opening it again adds nothing twice.
func TestASessionFileRecordedBeforeOriginsStillLists(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "bridge.db")
	old, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`
		CREATE TABLE session_files (
			file_id    TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			filename   TEXT NOT NULL,
			media_type TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			shared_by  TEXT NOT NULL,
			path       TEXT NOT NULL,
			created_at DATETIME NOT NULL
		);
		INSERT INTO session_files VALUES ('file_000001', 'br_old', 'hood.png', 'image/png', 4, 'user', '/work/session-files/br_old/file_000001/hood.png', '2026-11-01 10:00:00');
	`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	for opening := 1; opening <= 2; opening++ {
		s, err := New(databasePath)
		if err != nil {
			t.Fatalf("opening %d: %v", opening, err)
		}
		files, err := s.ListSessionFiles("br_old")
		if err != nil || len(files) != 1 {
			t.Fatalf("opening %d: listed %+v, %v", opening, files, err)
		}
		if files[0].FileID != "file_000001" || files[0].SourcePath != "" || files[0].Generation != nil {
			t.Errorf("opening %d: old row = %+v", opening, files[0])
		}
		s.Close()
	}
}

func TestASessionFileKeepsItsOrigin(t *testing.T) {
	s := testStore(t)
	generation := &msg.SessionFileGeneration{Prompt: "a hood", ReferenceImagePaths: []string{"/work/renders/sketch.png"}, InstanceID: "inst-images"}
	created := time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC)
	for _, file := range []SessionFile{
		{FileID: "file_000001", SessionID: "br_1", Filename: "hood.png", MediaType: "image/png", SizeBytes: 4, SharedBy: msg.SessionFileSharedByAgent,
			Path: "/work/session-files/br_1/file_000001/hood.png", SourcePath: "/work/renders/hood.png", Generation: generation, CreatedAt: created},
		{FileID: "file_000002", SessionID: "br_1", Filename: "notes.txt", MediaType: "text/plain", SizeBytes: 2, SharedBy: msg.SessionFileSharedByUser,
			Path: "/work/session-files/br_1/file_000002/notes.txt", CreatedAt: created.Add(time.Minute)},
	} {
		if err := s.InsertSessionFile(file); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.GetSessionFile("br_1", "file_000001")
	if err != nil || got.SourcePath != "/work/renders/hood.png" || got.Generation == nil || got.Generation.Prompt != "a hood" ||
		got.Generation.InstanceID != "inst-images" || len(got.Generation.ReferenceImagePaths) != 1 {
		t.Errorf("get = %+v (generation %+v), %v", got, got.Generation, err)
	}
	listed, err := s.ListSessionFiles("br_1")
	if err != nil || len(listed) != 2 || listed[0].Generation == nil || listed[1].SourcePath != "" || listed[1].Generation != nil {
		t.Errorf("list = %+v, %v", listed, err)
	}

	// A stored generation that does not parse is the caller's error, not an
	// empty origin.
	if _, err := s.db.Exec(`UPDATE session_files SET generation = '{not json' WHERE file_id = 'file_000001'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListSessionFiles("br_1"); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Errorf("list with a broken generation: %v, want an error naming it", err)
	}
	if _, err := s.GetSessionFile("br_1", "file_000001"); err == nil {
		t.Error("get with a broken generation answered no error")
	}
}
