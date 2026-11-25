package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/config"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/db"
	"github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/modelstoreroles"
)

// The environment seeds a role once; after that the database is the record,
// and a new role is checked with model-store before it is stored.
func TestAttachStoredSettingsSeedsOnceAndChecksNewRolesWithModelStore(t *testing.T) {
	modelStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"canonical":["best","default","balanced","efficient"],"models":{"best":["m1"],"default":["m1"],"balanced":["m2","m3"],"efficient":["m4"]}}`))
	}))
	defer modelStore.Close()
	databasePath := filepath.Join(t.TempDir(), "scheduler.db")

	start := func(environment map[string]string) *servicesettings.Registry {
		t.Helper()
		environment["SCHEDULER_DB"] = databasePath
		settings, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(environment))
		if err != nil {
			t.Fatal(err)
		}
		store, err := db.New(databasePath)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		if err := attachStoredSettings(settings, store, modelstoreroles.New(modelStore.URL)); err != nil {
			t.Fatal(err)
		}
		return settings
	}

	first := start(map[string]string{"SCHEDULER_KANBAN_SCOPER_MODEL_ROLE": "balanced"})
	if got := first.ModelRole(config.SettingKanbanScoperModelRole); got != "balanced" {
		t.Fatalf("first start = %q, want the environment's seed", got)
	}
	if got := first.ModelRole(config.SettingEmailClassifierModelRole); got != config.DefaultEmailClassifierModelRole {
		t.Fatalf("an unset role = %q, want the default seed", got)
	}
	if err := first.Set(config.SettingKanbanScoperModelRole, "cheapest"); err == nil {
		t.Fatal("a role model-store does not list was stored")
	}
	if err := first.Set(config.SettingKanbanScoperModelRole, "best"); err != nil {
		t.Fatal(err)
	}

	// A restart with the old variable still set keeps the stored value.
	second := start(map[string]string{"SCHEDULER_KANBAN_SCOPER_MODEL_ROLE": "balanced"})
	if got := second.ModelRole(config.SettingKanbanScoperModelRole); got != "best" {
		t.Fatalf("after restart = %q, want the stored best, not the environment", got)
	}
}
