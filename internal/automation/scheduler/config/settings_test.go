package config

import (
	"slices"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// The registry gives the service what envOr and os.Getenv gave it before
// 2026-09-21, with nothing set and with every variable set.
func TestLoadReadsTheSameValuesItAlwaysDid(t *testing.T) {
	t.Setenv("HOME", "/home/someone")
	unset, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		ListenHost:    "127.0.0.1",
		Port:          8092,
		DatabasePath:  "/home/someone/.scheduler/scheduler.db",
		Token:         "",
		NATSURL:       "nats://localhost:4222",
		BridgeURL:     "http://localhost:8160",
		NoteboardURL:  "http://localhost:8191",
		ModelStoreURL: "http://localhost:8155",
	}
	if got := Load(unset); got != want {
		t.Errorf("Load with nothing set = %+v, want %+v", got, want)
	}
	if err := unset.CheckRequired(); err != nil {
		t.Errorf("CheckRequired with a known home directory = %v", err)
	}

	set, err := NewSettingsRegistry(servicesettings.MapEnvironment(everyVariableSetToItsOwnValue))
	if err != nil {
		t.Fatal(err)
	}
	want = Config{
		ListenHost:         "192.0.2.1",
		Port:               19104,
		DatabasePath:       "/srv/scheduler.db",
		Token:              "a-token",
		NATSURL:            "nats://127.0.0.1:14222",
		BridgeURL:          "http://bridge.test:1",
		BridgeServiceToken: "a-bridge-token",
		NoteboardURL:       "http://noteboard.test:2",
		ModelStoreURL:      "http://model-store.test:3",
	}
	if got := Load(set); got != want {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
	for key, want := range map[string]string{
		SettingEmailClassifierModelRole:      "best",
		SettingEmailClassifierMergeModelRole: "balanced",
		SettingKanbanClassifierModelRole:     "efficient",
		SettingKanbanScoperModelRole:         "default",
		SettingDemoMailGeneratorModelRole:    "best",
		SettingAutoworkerAnthropicModelRole:  "best",
		SettingAutoworkerCodexModelRole:      "efficient",
		SettingAgentJobModelRole:             "balanced",
		SettingHeraldModelRole:               "efficient",
		SettingKanbanDispatcherModelRole:     "best",
		SettingDemoWorkerModelRole:           "balanced",
	} {
		if got := set.ModelRole(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// A variable set to the empty string is the same as unset, as it was when
	// envOr compared os.Getenv to "".
	empty, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SCHEDULER_PORT": "", "NATS_URL": "", "LLMBRIDGE_URL": "", "NOTEBOARD_URL": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if got := Load(empty); got.Port != 8092 || got.NATSURL != DefaultNATSURL || got.BridgeURL != DefaultBridgeURL || got.NoteboardURL != DefaultNoteboardURL {
		t.Errorf("empty variables: %+v", got)
	}
}

var everyVariableSetToItsOwnValue = map[string]string{
	"SCHEDULER_LISTEN_HOST":   "192.0.2.1",
	"SCHEDULER_PORT":          "19104",
	"SCHEDULER_DB":            "/srv/scheduler.db",
	"SCHEDULER_TOKEN":         "a-token",
	"NATS_URL":                "nats://127.0.0.1:14222",
	"LLMBRIDGE_URL":           "http://bridge.test:1",
	"LLMBRIDGE_SERVICE_TOKEN": "a-bridge-token",
	"NOTEBOARD_URL":           "http://noteboard.test:2",
	"MODEL_STORE_URL":         "http://model-store.test:3",
	// The model roles are seeded from these once, then stored by the service.
	"SCHEDULER_EMAIL_CLASSIFIER_MODEL_ROLE":       "best",
	"SCHEDULER_EMAIL_CLASSIFIER_MERGE_MODEL_ROLE": "balanced",
	"SCHEDULER_KANBAN_CLASSIFIER_MODEL_ROLE":      "efficient",
	"SCHEDULER_KANBAN_SCOPER_MODEL_ROLE":          "default",
	"SCHEDULER_DEMO_MAIL_GENERATOR_MODEL_ROLE":    "best",
	"SCHEDULER_AUTOWORKER_ANTHROPIC_MODEL_ROLE":   "best",
	"SCHEDULER_AUTOWORKER_CODEX_MODEL_ROLE":       "efficient",
	"SCHEDULER_AGENT_JOB_MODEL_ROLE":              "balanced",
	"SCHEDULER_HERALD_MODEL_ROLE":                 "efficient",
	"SCHEDULER_KANBAN_DISPATCHER_MODEL_ROLE":      "best",
	"SCHEDULER_DEMO_WORKER_MODEL_ROLE":            "balanced",
}

// Every model-role setting is a stored, editable, required model_role, and
// each model-calling job binary's role is one of them. A role setting that was
// not Editable could only be changed where the service is started.
func TestEveryModelRoleSettingIsAStoredModelRole(t *testing.T) {
	declared := map[string]servicesettings.Definition{}
	for _, definition := range SettingDefinitions() {
		declared[definition.Key] = definition
	}
	for _, key := range ModelRoleSettingKeys {
		definition, ok := declared[key]
		if !ok {
			t.Errorf("%s is in ModelRoleSettingKeys and not declared", key)
			continue
		}
		if !definition.Editable || !definition.Required || definition.ValueType != msg.ServiceSettingValueTypeModelRole || definition.Kind != msg.ServiceSettingKindBehavior {
			t.Errorf("%s = %+v, want an editable, required behavior setting of type model_role", key, definition)
		}
		if !slices.Contains(msg.CanonicalModelRoles, msg.ModelRole(definition.Default)) {
			t.Errorf("%s defaults to %q, which is no canonical role", key, definition.Default)
		}
	}
	for _, definition := range SettingDefinitions() {
		if definition.ValueType == msg.ServiceSettingValueTypeModelRole && !slices.Contains(ModelRoleSettingKeys, definition.Key) {
			t.Errorf("%s is a model role missing from ModelRoleSettingKeys, so model-store never checks it", definition.Key)
		}
	}
}

// The same-values test sets every declared variable. A definition added
// without a row there is a setting nothing reads back.
func TestTheSameValuesTestCoversEveryDeclaredSetting(t *testing.T) {
	for _, definition := range SettingDefinitions() {
		if _, covered := everyVariableSetToItsOwnValue[definition.EnvironmentVariable]; !covered {
			t.Errorf("%s is declared and TestLoadReadsTheSameValuesItAlwaysDid does not set it", definition.EnvironmentVariable)
		}
	}
}

// Before 2026-09-21 an empty HOME opened .scheduler/scheduler.db in the working
// directory, silently: a scheduler with no jobs. Now the setting is unset and
// the service refuses to start.
func TestAnUnknownHomeDirectoryLeavesTheDatabaseUnsetAndCheckRequiredSaysSo(t *testing.T) {
	t.Setenv("HOME", "")
	registry, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.CheckRequired(); err == nil || !strings.Contains(err.Error(), "SCHEDULER_DB is unset") {
		t.Fatalf("CheckRequired = %v, want it to name SCHEDULER_DB", err)
	}
}

// SCHEDULER_URL is how jobs find the scheduler, and jobs are its children; a
// misspelling of an owned name is refused.
func TestTheRegistryRefusesAMisspellingAndNotTheVariableJobsUseToFindIt(t *testing.T) {
	for _, misspelled := range []string{"SCHEDULER_DB_PATH", "SCHEDULER_PORTS", "SCHEDULER_TOKEN_FILE"} {
		_, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "x"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and scheduler declares no such setting") {
			t.Errorf("NewSettingsRegistry with %s = %v, want a refusal naming it", misspelled, err)
		}
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SCHEDULER_URL": "http://localhost:8092", "SCHEDULER_SOURCE": "x", "LLM_BRIDGE_URL": "http://localhost:8160"})); err != nil {
		t.Errorf("a variable the service does not own was refused: %v", err)
	}
	// A port that is not a whole number stops the start at the registry rather
	// than at ListenAndServe.
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"SCHEDULER_PORT": ":8092"})); err == nil {
		t.Error("SCHEDULER_PORT=:8092 was accepted")
	}
}
