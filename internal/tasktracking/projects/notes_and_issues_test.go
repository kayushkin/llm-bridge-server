package projects

import (
	"context"
	"errors"
	"testing"

	issuesdb "github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/db"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issueitems"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/notes"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// The store reaches notes and issues in the same process: a next step is an
// issue, and a note link names a live note or issue.
func TestNextStepsAreIssuesAndNoteLinksNameNotesOrIssues(t *testing.T) {
	projectsModule, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	notesModule, err := notes.Module()
	if err != nil {
		t.Fatal(err)
	}
	issuesModule, err := issuesdb.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, projectsModule, notesModule, issuesModule)
	store := New(database.Open(projectsModule))
	modules := NotesAndIssuesModules{Notes: notes.New(database.Open(notesModule)), Issues: issueitems.New(database.Open(issuesModule))}
	owners := &Owners{NotesAndIssues: modules}
	note, err := modules.Notes.CreateItem(&model.CreateItemRequest{Type: model.TypeNote, Title: "Design"})
	if err != nil {
		t.Fatal(err)
	}
	if label, err := owners.Label(context.Background(), EntityNote, note.ID); err != nil || label != "Design" {
		t.Errorf("a note's label is %q (%v)", label, err)
	}

	name := "Inbox"
	project, err := store.CreateProject(ProjectFields{Name: &name}, "")
	if err != nil {
		t.Fatal(err)
	}
	priority := 3
	step, err := AddNextStep(context.Background(), store, owners, project.ID, NextStep{Title: "Merge the branch", Body: "It waits.", Priority: &priority})
	if err != nil {
		t.Fatalf("AddNextStep: %v", err)
	}
	todo, err := modules.Issues.GetItem(step.NoteID)
	if err != nil || todo.Type != "todo" || todo.Priority != 3 || len(todo.Tags) != 1 || todo.Tags[0] != NextStepTag {
		t.Fatalf("the next step is %+v in issues (%v)", todo, err)
	}
	if label, err := owners.Label(context.Background(), EntityNote, step.NoteID); err != nil || label != "Merge the branch" {
		t.Errorf("the note's label is %q (%v)", label, err)
	}
	if _, err := owners.Label(context.Background(), EntityNote, "9d3a2f6e-0000-4000-8000-000000000000"); !errors.Is(err, ErrInvalid) {
		t.Errorf("a note noteboard does not have is %v, want ErrInvalid", err)
	}
	links, err := store.Links(LinkFilter{ProjectID: project.ID})
	if err != nil {
		t.Fatal(err)
	}
	links = append(links, Link{ID: 98, EntityType: EntityNote, EntityRef: note.ID}, Link{ID: 99, EntityType: EntityNote, EntityRef: "9d3a2f6e-0000-4000-8000-000000000000"})
	linked, missing, err := owners.LinkedNotes(context.Background(), links)
	if err != nil || len(linked) != 2 || linked[0].NoteID != step.NoteID || linked[0].Priority != 3 || linked[1].NoteID != note.ID || len(missing) != 1 {
		t.Fatalf("linked notes %+v, missing %v (%v)", linked, missing, err)
	}
	if linked[0].CreatedAt != step.CreatedAt || linked[0].CreatedAt == "" {
		t.Errorf("created_at %q, the step answered %q", linked[0].CreatedAt, step.CreatedAt)
	}
}
