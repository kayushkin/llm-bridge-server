package harnessconfig

import (
	"encoding/json"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// DeniedReadPathsPinnedOn reads harness_config.denied_read_paths.
func DeniedReadPathsPinnedOn(cfg map[string]json.RawMessage) ([]string, error) {
	raw, ok := cfg[KeyDeniedReadPaths]
	if !ok {
		return nil, nil
	}
	var paths []string
	if err := json.Unmarshal(raw, &paths); err != nil {
		return nil, fmt.Errorf("harness_config.denied_read_paths is not a string array: %w", err)
	}
	return paths, nil
}

// SessionDeniedReadPaths is DeniedReadPathsPinnedOn for a stored session.
func SessionDeniedReadPaths(sess *store.Session) ([]string, error) {
	cfg, err := Parse(sess)
	if err != nil {
		return nil, err
	}
	return DeniedReadPathsPinnedOn(cfg)
}
