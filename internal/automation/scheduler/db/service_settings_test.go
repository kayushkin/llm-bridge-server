package db

import (
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

func TestServiceSettingValuesSaveReplacesAndLoadReturnsEveryKey(t *testing.T) {
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	store := New(database.Open(module))
	values := store.ServiceSettingValues()
	if err := values.Save("kanban_scoper_model_role", "efficient"); err != nil {
		t.Fatal(err)
	}
	if err := values.Save("kanban_scoper_model_role", "balanced"); err != nil {
		t.Fatal(err)
	}
	if err := values.Save("email_classifier_model_role", "default"); err != nil {
		t.Fatal(err)
	}
	store.Close()

	// A second open reads what the first wrote: the record outlives a restart.
	reopened := New(database.Open(module))
	defer reopened.Close()
	held, err := reopened.ServiceSettingValues().Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 2 || held["kanban_scoper_model_role"] != "balanced" || held["email_classifier_model_role"] != "default" {
		t.Errorf("held = %v, want the two keys with the last value written", held)
	}
}
