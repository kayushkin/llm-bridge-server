// Package harnessconfig is a session's harness_config: the keys the server
// and the harness bridges read in it, and how it is read and written back.
package harnessconfig

import (
	"encoding/json"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// The harness_config keys the server writes or reads.
const (
	KeyDeniedReadPaths = "denied_read_paths"
	// KeySettings is Claude Code's --settings, which carries the
	// Read deny rules and the hooks; a caller that sets it replaces both.
	KeySettings             = "settings"
	KeyDisabledToolsByLayer = "disabled_tools_by_layer"
	// KeyBuiltInToolAllowList is Claude Code's --tools: the only
	// built-in tools it may offer. The server pins it on a restricted session.
	KeyBuiltInToolAllowList = "tools"
	// KeyBuiltInToolAllowListPinnedFor names the bundle whose
	// restriction pinned the allow list. Harness bridges ignore it; the
	// server reads it to hold a started session to its list.
	KeyBuiltInToolAllowListPinnedFor = "built_in_tool_allow_list_pinned_for_bundle"
	// KeyModel is the wire key every harness bridge already reads
	// for the model it should spawn on (llm-bridge-claudecode turns it into
	// --model). The resolved registry id is written here.
	KeyModel = "model"
	// KeyModelSelection carries the typed msg.ModelSelection —
	// the id plus which layer chose it — so a session can always say WHY it is
	// on the model it is on. Harness bridges ignore the key.
	KeyModelSelection = "model_selection"
	KeyEffort         = "effort"
	KeyDisabledTools  = "disabled_tools"
	// KeySettingLayers maps a setting (msg.EffectiveSettingKey) to
	// the layer (msg.EffectiveConfigLayer) that decided the value pinned on the
	// row. Harness bridges ignore the key. The model's layer is not here: it
	// has its own typed record, harness_config.model_selection.
	KeySettingLayers = "setting_layers"
)

// Parse decodes sess.HarnessConfig as an object, treating an
// empty blob as an empty object. An unparseable blob is an error, not an
// empty map — see resolveModelSelection for why.
func Parse(sess *store.Session) (map[string]json.RawMessage, error) {
	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		return nil, fmt.Errorf("harness_config is not a JSON object: %w", err)
	}
	return cfg, nil
}

// Replace writes cfg back into the session row it was parsed
// from, encoded as JSON.
func Replace(sess *store.Session, cfg map[string]json.RawMessage) error {
	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal HarnessConfig: %w", err)
	}
	sess.HarnessConfig = merged
	return nil
}
