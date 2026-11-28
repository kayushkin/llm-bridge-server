package server

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/signals"

	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationroutes"

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
// the stored behavior settings attached. A Config read by config.Load carries
// its registry; a Config built as a literal — every test — does not, so one is
// built with no environment and the stored settings are seeded from the
// literal's own fields. Either way the record in st is the source from here on.
func attachServiceSettings(cfg *config.Config, st *store.Store) *servicesettings.Registry {
	settings := cfg.Settings
	// A loaded Config seeds from what the registry already read, as written:
	// "15m", not the "15m0s" a time.Duration prints. Only a literal has
	// values the registry never saw.
	var seeds map[string]string
	if settings == nil {
		built, err := config.NewSettingsRegistry(servicesettings.MapEnvironment(nil))
		if err != nil {
			panic(fmt.Sprintf("service settings: %v", err))
		}
		settings = built
		seeds = cfg.StoredSettingSeeds()
	}
	if err := st.MoveServiceSettingKeys(config.RenamedStoredSettingKeys, config.RetiredStoredSettingKeys); err != nil {
		panic(fmt.Sprintf("service settings: %v", err))
	}
	if err := settings.AttachStoredValues(st.ServiceSettingValues(), seeds); err != nil {
		panic(fmt.Sprintf("service settings: %v", err))
	}
	return settings
}

// wireServiceSettings connects the registry to the server: instance checks on
// write, and the classifier and triage retuned on change. Called once from New,
// after the components exist.
func (s *Server) wireServiceSettings() {
	if s.harnessStore != nil {
		s.settings.SetValidator(config.SettingOneShotInstanceByProvider, s.oneShot.CheckOneShotInstanceByProvider)
		s.settings.SetValidator(config.SettingImageGenerationInstanceID, func(value string) error {
			if value == "" {
				return nil
			}
			return s.CheckSettingNamesAnEnabledInstance(value)
		})
	}
	for _, key := range config.ModelRoleSettings {
		s.settings.SetValidator(key, s.oneShot.CheckSettingNamesAnAssignedModelRole)
	}
	s.settings.SetValidator(config.SettingOperationsGrantEnforcement, operationroutes.CheckOperationGrantEnforcementValue)
	s.settings.SetValidator(config.SettingServerName, checkServerNameValue)
	s.settings.SetValidator(config.SettingKeepAwakeGraceMinutes, checkKeepAwakeGraceMinutesValue)
	s.settings.SetValidator(config.SettingSessionHostHoldMinutes, checkSessionHostHoldMinutesValue)
	// Read when a runner connects, so a change reaches each runner at its
	// next connection.
	s.harness.SetRunnerHoldPolicy(func() (bool, time.Duration) {
		return s.settings.Boolean(config.SettingSessionHostHoldSessions),
			time.Duration(s.settings.Integer(config.SettingSessionHostHoldMinutes)) * time.Minute
	})
	s.settings.OnChange(func(key string) {
		switch key {
		case config.SettingServerName:
			go s.tellConnectedServersThisServersName()
		case config.SettingKeepAwakeEnabled, config.SettingKeepAwakeGraceMinutes:
			if s.keepAwake != nil {
				go s.keepAwake.Check()
			}
		}
	})
	s.settings.OnChange(func(string) { s.retuneFromServiceSettings() })
	s.retuneFromServiceSettings()
}

// CheckSettingNamesAnEnabledInstance refuses an instance id harness-store does
// not know or has disabled: a one-shot call on it would fail every time, and
// the setting would look fine until someone read the log.
func (s *Server) CheckSettingNamesAnEnabledInstance(instanceID string) error {
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
// in the classifier, and question_triage.model_role with the classifier's
// timeout in question triage.
func (s *Server) retuneFromServiceSettings() {
	optOut := make(map[msg.Harness]bool)
	for _, harnessName := range s.settings.StringList(config.SettingSignalClassifierOptOutHarnesses) {
		optOut[msg.Harness(harnessName)] = true
	}
	tuning := signals.SignalClassifierTuning{
		ModelRole: s.settings.ModelRole(config.SettingSignalClassifierModelRole),
		Timeout:   s.settings.Duration(config.SettingSignalClassifierTimeout),
		MaxChars:  s.settings.Integer(config.SettingSignalClassifierMaximumCharacters),
		OptOut:    optOut,
	}
	if s.signalClassifier != nil {
		s.signalClassifier.Retune(tuning)
	}
	if s.questionTriage != nil {
		s.questionTriage.Retune(s.settings.ModelRole(config.SettingQuestionTriageModelRole), tuning.Timeout)
	}
}

// checkSessionHostHoldMinutesValue refuses a hold limit under a minute: zero
// would have a runner interrupt every held process the moment it lost this
// server, which is not holding at all.
func checkSessionHostHoldMinutesValue(value string) error {
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "-") || trimmed == "0" || trimmed == "" {
		return errors.New("session_host.hold_without_server_minutes is 1 or more")
	}
	return nil
}
