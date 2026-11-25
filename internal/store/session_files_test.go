package store

import (
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/llm-bridge/msg"
)

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
