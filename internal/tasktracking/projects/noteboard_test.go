package projects

import (
	"context"
	"errors"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// The store reaches noteboard in the same process: a next step is a todo in
// the noteboard module, and a note link names a live item there.
func TestNextStepsAreTodosInTheNoteboardModule(t *testing.T) {
	projectsModule, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	noteboardModule, err := items.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, projectsModule, noteboardModule)
	store := New(database.Open(projectsModule))
	noteboard := NoteboardModule{Store: items.New(database.Open(noteboardModule))}
	owners := &Owners{Noteboard: noteboard}

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
	todo, err := noteboard.Store.GetItem(step.NoteID)
	if err != nil || todo.Type != "todo" || todo.Priority != 3 || len(todo.Tags) != 1 || todo.Tags[0] != NextStepTag {
		t.Fatalf("the next step is %+v in noteboard (%v)", todo, err)
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
	links = append(links, Link{ID: 99, EntityType: EntityNote, EntityRef: "9d3a2f6e-0000-4000-8000-000000000000"})
	notes, missing, err := owners.LinkedNotes(context.Background(), links)
	if err != nil || len(notes) != 1 || notes[0].NoteID != step.NoteID || notes[0].Priority != 3 || len(missing) != 1 {
		t.Errorf("linked notes %+v, missing %v (%v)", notes, missing, err)
	}
	if notes[0].CreatedAt != step.CreatedAt || notes[0].CreatedAt == "" {
		t.Errorf("created_at %q, the step answered %q", notes[0].CreatedAt, step.CreatedAt)
	}
}
