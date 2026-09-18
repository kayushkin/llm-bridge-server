package server

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// This server's own settings: what GET /settings describes and PUT
// /settings/{key} changes. The declarations are internal/config's
// (SettingDefinitions); llm-bridge's servicesettings package does the reading,
// storing and describing. What is here is the part only the server can do:
// attach its database as the record, check a named instance with harness-store
// before a write, and push a changed value into the components that hold one.

// attachServiceSettings returns the settings registry this server runs on, with
// the stored behaviour settings attached. A Config read by config.Load carries
// its registry; a Config built as a literal — every test — does not, so one is
// built with no environment and the stored settings are seeded from the
// literal's own fields. Either way the record in st is the source from here on.
func attachServiceSettings(cfg *config.Config, st *store.Store) *servicesettings.Registry {
	settings := cfg.Settings
	if settings == nil {
		built, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(nil))
		if err != nil {
			panic(fmt.Sprintf("service settings: %v", err))
		}
		settings = built
	}
	if err := settings.AttachStoredValues(st.ServiceSettingValues(), cfg.StoredSettingSeeds()); err != nil {
		panic(fmt.Sprintf("service settings: %v", err))
	}
	return settings
}

// wireServiceSettings connects the registry to the server: instance checks on
// write, and the classifier and triage retuned on change. Called once from New,
// after the components exist.
func (s *Server) wireServiceSettings() {
	if s.harnessStore != nil {
		for _, key := range []string{config.SettingSignalClassifierInstance, config.SettingPromptDriftTaggerInstance} {
			s.settings.SetValidator(key, s.checkSettingNamesAnEnabledInstance)
		}
	}
	s.settings.OnChange(func(string) { s.retuneFromServiceSettings() })
	s.retuneFromServiceSettings()
}

// checkSettingNamesAnEnabledInstance refuses an instance id harness-store does
// not know or has disabled: a one-shot call on it would fail every time, and
// the setting would look fine until someone read the log.
func (s *Server) checkSettingNamesAnEnabledInstance(instanceID string) error {
	instance, err := s.harnessStore.GetInstance(instanceID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("harness-store knows no instance %q", instanceID)
	}
	if err != nil {
		return fmt.Errorf("%w: harness-store: %v", servicesettings.ErrSettingOwnerUnavailable, err)
	}
	if !instance.Enabled {
		return fmt.Errorf("instance %q is disabled", instanceID)
	}
	return nil
}

// retuneFromServiceSettings puts the stored signal_classifier.* values in force
// in the classifier and in question triage, which shares its model and timeout.
func (s *Server) retuneFromServiceSettings() {
	optOut := make(map[msg.Harness]bool)
	for _, harnessName := range s.settings.StringList(config.SettingSignalClassifierOptOutHarnesses) {
		optOut[msg.Harness(harnessName)] = true
	}
	tuning := signalClassifierTuning{
		model:    s.settings.String(config.SettingSignalClassifierModel),
		timeout:  s.settings.Duration(config.SettingSignalClassifierTimeout),
		maxChars: s.settings.Integer(config.SettingSignalClassifierMaximumCharacters),
		optOut:   optOut,
	}
	if s.signalClassifier != nil {
		s.signalClassifier.retune(tuning)
	}
	if s.questionTriage != nil {
		s.questionTriage.retune(tuning.model, tuning.timeout)
	}
}
