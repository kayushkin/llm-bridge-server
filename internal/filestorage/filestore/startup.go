package filestore

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Service is everything a running file-store holds.
type Service struct {
	Settings *servicesettings.Registry
	Store    *Store
	Server   *Server
}

// Start builds the service from an environment: settings declared and checked,
// the store opened, the stored settings attached, the API wired. main calls it
// with the process environment and the tests with a map, so the path a deploy
// takes is the path the tests take.
func Start(environment servicesettings.Environment) (*Service, error) {
	definitions, err := SettingDefinitions()
	if err != nil {
		return nil, err
	}
	settings, err := servicesettings.New(ServiceName, OwnedEnvironmentPrefixes, definitions, environment)
	if err != nil {
		return nil, err
	}
	if err := settings.CheckRequired(); err != nil {
		return nil, err
	}
	settings.SetValidator(SettingMaximumFileBytes, ValidateMaximumFileBytes)
	settings.SetValidator(SettingInlineContentTypes, ValidateInlineContentTypes)

	store, err := Open(settings.String(SettingDataDirectory))
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := settings.AttachStoredValues(store.StoredSettings(), nil); err != nil {
		store.Close()
		return nil, err
	}
	// A value seeded from the environment, or stored by an older build, never
	// passed through Set, so the validators have not judged it. A size limit of
	// zero or a page type on the inline list must stop the start, not serve.
	inForce := map[string]string{
		SettingMaximumFileBytes:   strconv.Itoa(settings.Integer(SettingMaximumFileBytes)),
		SettingInlineContentTypes: strings.Join(settings.StringList(SettingInlineContentTypes), ","),
	}
	for key, validate := range map[string]func(string) error{
		SettingMaximumFileBytes:   ValidateMaximumFileBytes,
		SettingInlineContentTypes: ValidateInlineContentTypes,
	} {
		if err := validate(inForce[key]); err != nil {
			store.Close()
			return nil, err
		}
	}
	server, err := NewServer(store, settings)
	if err != nil {
		store.Close()
		return nil, err
	}
	return &Service{Settings: settings, Store: store, Server: server}, nil
}
