package filestore

import (
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kayushkin/llm-bridge/msg"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Setting keys. Every piece of this service's configuration is declared in
// SettingDefinitions and read through the registry; nothing calls os.Getenv.
const (
	SettingListenAddress      = "listen_address"
	SettingDataDirectory      = "data_directory"
	SettingServiceToken       = "service_token"
	SettingMaximumFileBytes   = "maximum_file_bytes"
	SettingInlineContentTypes = "inline_content_types"
)

// ServiceName is this service's name in settings, logs and owner labels.
const ServiceName = "file-store"

// OwnedEnvironmentPrefixes are the variable prefixes that are this service's
// alone: a set variable under one that nothing declares is a startup error.
var OwnedEnvironmentPrefixes = []string{"FILE_STORE_"}

// MinimumServiceTokenLength is the shortest token accepted. A short or empty
// token would match requests that ought to be refused.
const MinimumServiceTokenLength = 32

// SettingDefinitions declares the configuration. The data directory's default
// is under the home of whoever runs the process, so it is computed here.
func SettingDefinitions() ([]servicesettings.Definition, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find the home directory for the default data directory: %w", err)
	}
	return []servicesettings.Definition{
		{
			Key: SettingListenAddress, EnvironmentVariable: "FILE_STORE_ADDR",
			Kind: msg.ServiceSettingKindWiring, ValueType: msg.ServiceSettingValueTypeString,
			Default:     "127.0.0.1:8317",
			Description: "Where the HTTP API listens. Loopback on purpose: this service decides nothing about who may read a file, so only the services that do may reach it.",
		},
		{
			Key: SettingDataDirectory, EnvironmentVariable: "FILE_STORE_DATA_DIR",
			Kind: msg.ServiceSettingKindPath, ValueType: msg.ServiceSettingValueTypeString,
			Default:     filepath.Join(home, ".config", "file-store"),
			Description: "Holds file-store.db and blobs/, the uploaded bytes named by their SHA-256. Moving it moves every file.",
		},
		{
			Key: SettingServiceToken, EnvironmentVariable: "FILE_STORE_SERVICE_TOKEN",
			Kind: msg.ServiceSettingKindSecret, ValueType: msg.ServiceSettingValueTypeString,
			Required:    true,
			Description: "What a calling service sends as X-File-Store-Service-Token. At least 32 characters. Whoever holds it reads every file, so it goes only to services that check access themselves, and never into an agent's environment.",
		},
		{
			Key: SettingMaximumFileBytes, EnvironmentVariable: "FILE_STORE_MAXIMUM_FILE_BYTES",
			Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeInteger,
			Default: strconv.Itoa(25 * 1024 * 1024), Editable: true,
			Description: "The largest upload accepted, in bytes. A larger one is refused with 413 and nothing is kept. Lowering it does not touch files already stored.",
		},
		{
			Key: SettingInlineContentTypes, EnvironmentVariable: "FILE_STORE_INLINE_CONTENT_TYPES",
			Kind: msg.ServiceSettingKindBehavior, ValueType: msg.ServiceSettingValueTypeStringList,
			Default: "image/png,image/jpeg,image/gif,image/webp,application/pdf,text/plain", Editable: true,
			Description: "Declared content types served for display in a browser when a download asks ?inline=true. Every other file is served as an attachment. A type a browser runs as a page or a script is refused here.",
		},
	}, nil
}

// contentTypesABrowserExecutes may never be served inline: an uploaded page
// shown from the dashboard's origin runs with the dashboard's cookies.
var contentTypesABrowserExecutes = map[string]bool{
	"text/html": true, "application/xhtml+xml": true, "image/svg+xml": true,
	"text/xml": true, "application/xml": true,
	"text/javascript": true, "application/javascript": true, "application/ecmascript": true,
}

// ValidateMaximumFileBytes is the validator of SettingMaximumFileBytes.
func ValidateMaximumFileBytes(value string) error {
	bytes, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || bytes <= 0 {
		return fmt.Errorf("%w: %s must be a positive number of bytes, got %q", servicesettings.ErrInvalidSettingValue, SettingMaximumFileBytes, value)
	}
	return nil
}

// ValidateInlineContentTypes is the validator of SettingInlineContentTypes.
func ValidateInlineContentTypes(value string) error {
	for _, entry := range strings.Split(value, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		mediaType, parameters, err := mime.ParseMediaType(entry)
		if err != nil || len(parameters) > 0 || mediaType != entry {
			return fmt.Errorf("%w: %q is not a bare lowercase media type such as image/png", servicesettings.ErrInvalidSettingValue, entry)
		}
		if contentTypesABrowserExecutes[mediaType] {
			return fmt.Errorf("%w: %s is a type a browser runs, and a file of it shown inline would run as the dashboard", servicesettings.ErrInvalidSettingValue, mediaType)
		}
	}
	return nil
}
