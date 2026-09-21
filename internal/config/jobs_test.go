package config

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Every directory under cmd/ is the service or a declared job binary, so a
// binary added later cannot escape the scan by being on neither list.
func TestEveryBinaryInCmdIsTheServiceOrADeclaredJob(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join("..", "..", "cmd"))
	if err != nil {
		t.Fatal(err)
	}
	var directories []string
	for _, entry := range entries {
		if entry.IsDir() {
			directories = append(directories, entry.Name())
		}
	}
	declared := []string{"scheduler"}
	for _, binary := range JobBinaries {
		declared = append(declared, binary.Name)
	}
	sort.Strings(directories)
	sort.Strings(declared)
	if strings.Join(directories, " ") != strings.Join(declared, " ") {
		t.Errorf("cmd/ holds %v; the service and JobBinaries name %v", directories, declared)
	}
}

// Each binary's declarations build, and with nothing set every binary starts:
// none of them has a setting it cannot run without.
func TestEveryJobBinaryBuildsARegistryFromAnEmptyEnvironment(t *testing.T) {
	for _, binary := range JobBinaries {
		registry, err := binary.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{}))
		if err != nil {
			t.Errorf("%s: %v", binary.Name, err)
			continue
		}
		if err := registry.CheckRequired(); err != nil {
			t.Errorf("%s: %v", binary.Name, err)
		}
	}
}

// A job's environment is the scheduler's, which carries the service's own
// variables and every other job's. No binary may refuse any of them.
func TestNoJobBinaryRefusesAVariableAnotherBinaryOrTheServiceDeclares(t *testing.T) {
	everyone := map[string]string{}
	for _, definition := range SettingDefinitions() {
		everyone[definition.EnvironmentVariable] = valueOfItsType(definition)
	}
	for _, binary := range JobBinaries {
		for _, definition := range binary.SettingDefinitions() {
			everyone[definition.EnvironmentVariable] = valueOfItsType(definition)
		}
	}
	for _, binary := range JobBinaries {
		if _, err := binary.NewSettingsRegistry(servicesettings.MapEnvironment(everyone)); err != nil {
			t.Errorf("%s refuses the fleet's environment: %v", binary.Name, err)
		}
	}
	if _, err := NewSettingsRegistry(servicesettings.MapEnvironment(everyone)); err != nil {
		t.Errorf("the service refuses its jobs' variables: %v", err)
	}
}

func valueOfItsType(definition servicesettings.Definition) string {
	switch definition.ValueType {
	case msg.ServiceSettingValueTypeBoolean:
		return "true"
	case msg.ServiceSettingValueTypeInteger:
		return "1"
	case msg.ServiceSettingValueTypeDecimal:
		return "1.5"
	}
	return "x"
}

// A misspelling of a name a binary owns stops that binary.
func TestAJobBinaryRefusesAMisspellingOfANameItOwns(t *testing.T) {
	for binary, misspelled := range map[*JobBinary]string{
		&Autoworker:          "AUTOWORKER_DEFAULT_MAX_BUDGET",
		&DemoMailGenerator:   "DEMO_MAIL_MODLE",
		&EmailClassifier:     "EMAIL_CLASSIFIER_MODLE",
		&KanbanClassifier:    "KANBAN_CLASSIFIER_MODELS",
		&KanbanScoper:        "KANBAN_SCOPER_ROLETAGS",
		&ReminderCoordinator: "REMINDER_TIMEZONE",
	} {
		_, err := binary.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{misspelled: "x"}))
		if err == nil || !strings.Contains(err.Error(), misspelled+" is set and "+binary.Name+" declares no such setting") {
			t.Errorf("%s with %s set = %v, want a refusal naming it", binary.Name, misspelled, err)
		}
	}
	// EMAIL_BOARD_ID is owned as a whole name, not as EMAIL_.
	if _, err := EmailClassifier.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"EMAIL_BOARD_IDS": "x"})); err == nil {
		t.Error("email-classifier accepted EMAIL_BOARD_IDS")
	}
}

// KANBAN_SCOPER_ROLE_TAGS used to read yes, on and anything unparseable as a
// quiet default. Now a value that is not a boolean stops the binary.
func TestKanbanScoperRefusesARoleTagsValueThatIsNotABoolean(t *testing.T) {
	if _, err := KanbanScoper.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"KANBAN_SCOPER_ROLE_TAGS": "yes"})); err == nil {
		t.Error("KANBAN_SCOPER_ROLE_TAGS=yes was accepted")
	}
	registry, err := KanbanScoper.NewSettingsRegistry(servicesettings.MapEnvironment(map[string]string{"KANBAN_SCOPER_ROLE_TAGS": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	if !registry.Boolean(SettingRoleTags) {
		t.Error("KANBAN_SCOPER_ROLE_TAGS=true read as false")
	}
}
