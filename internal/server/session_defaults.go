package server

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/sessions/harnessconfig"

	"github.com/kayushkin/llm-bridge-server/internal/agents/bundles"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// What a new session runs with when its creator did not say: model, effort,
// spend ceiling and disabled tools. Decided here, once, at create, by one
// precedence:
//
//  1. the session's own value — what POST /sessions carried;
//  2. the session's bundle, which names a model and an effort and nothing else.
//     A bundle is chosen for the work, by a board, a tag rule or the caller;
//  3. the per-harness default in bridge-prefs, chosen for the machine;
//  4. for the model only, the registry's default role (model_selection.go).
//
// The winner is pinned into the row, with the layer that decided it under
// harness_config.setting_layers, so a later change to a bundle or to the
// harness defaults does not move a session that already exists and the session
// can always say why it runs as it does. The effective-config view reads that
// record.
//
// Until 2026-09-18 only the chat pane applied the harness defaults, by copying
// them into the request it sent, and nothing applied a bundle's model or effort
// at all — so a session a dispatcher created ran at the harness's own effort,
// with no spend ceiling, on whatever the harness default model was, whatever
// its board's bundle said.

// pinnedSettingLayers is harness_config.setting_layers, decoded.
type pinnedSettingLayers map[msg.EffectiveSettingKey]msg.EffectiveConfigLayer

// sessionBundleResolution resolves sess's bundle, or returns nil when it names
// none. A bundle that cannot be resolved is an error: the spawn would refuse
// the session anyway, and deciding its model without the bundle would pin the
// wrong one.
func (s *Server) sessionBundleResolution(ctx context.Context, sess *store.Session) (*bundles.ResolveResponse, error) {
	if sess.BundleID == "" {
		return nil, nil
	}
	if s.bundleStore == nil {
		return nil, fmt.Errorf("session %s names bundle %s and %s", sess.SessionID, sess.BundleID, bundleStoreOffMessage)
	}
	resolution, err := s.bundleStore.ResolveSessionBundle(ctx, sess.BundleID)
	if err != nil {
		return nil, fmt.Errorf("resolve bundle %s: %w", sess.BundleID, err)
	}
	return resolution, nil
}

// harnessDefaultsFor is the per-harness record of bridge-prefs, empty when
// there is none.
func (s *Server) harnessDefaultsFor(harness msg.Harness) msg.HarnessDefaults {
	if s.bridgePrefs == nil {
		return msg.HarnessDefaults{}
	}
	return s.bridgePrefs.get().Defaults[string(harness)]
}

// snapshotSessionDefaultsIntoSession decides model, effort, spend ceiling and
// disabled tools for a session about to be created and pins each into sess (in
// memory; the caller inserts the row). maxBudgetWasSent says whether the
// request carried max_budget at all: a sent 0 is the creator saying "no
// ceiling", and outranks a default, while an absent one takes the default.
func (s *Server) snapshotSessionDefaultsIntoSession(ctx context.Context, sess *store.Session, maxBudgetWasSent bool) error {
	bundle, err := s.sessionBundleResolution(ctx, sess)
	if err != nil {
		return err
	}
	selection, err := s.resolveModelSelectionWithBundle(sess, bundle)
	if err != nil {
		return err
	}
	if err := writeModelSelectionIntoSession(sess, selection); err != nil {
		return err
	}

	cfg, err := harnessconfig.Parse(sess)
	if err != nil {
		return err
	}
	defaults := s.harnessDefaultsFor(sess.Harness)
	layers := pinnedSettingLayers{}
	pin := func(key string, value any) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", key, err)
		}
		cfg[key] = raw
		return nil
	}

	// effort
	ownEffort := ""
	if raw, ok := cfg[harnessconfig.KeyEffort]; ok {
		if err := json.Unmarshal(raw, &ownEffort); err != nil {
			return fmt.Errorf("harness_config.effort is not a string: %w", err)
		}
	}
	switch {
	case ownEffort != "":
		layers[msg.EffectiveSettingEffort] = msg.EffectiveLayerSession
	case bundle != nil && bundle.Effort != "":
		if err := pin(harnessconfig.KeyEffort, bundle.Effort); err != nil {
			return err
		}
		layers[msg.EffectiveSettingEffort] = msg.EffectiveLayerBundle
	case defaults.Effort != "":
		if err := pin(harnessconfig.KeyEffort, defaults.Effort); err != nil {
			return err
		}
		layers[msg.EffectiveSettingEffort] = msg.EffectiveLayerHarnessDefault
	}

	// spend ceiling: a column, not a harness_config key
	switch {
	case maxBudgetWasSent:
		layers[msg.EffectiveSettingMaxBudget] = msg.EffectiveLayerSession
	case defaults.MaxBudget != nil:
		if *defaults.MaxBudget < 0 {
			return fmt.Errorf("bridge-prefs defaults for %s hold a negative max_budget (%v); fix it on the Settings page", sess.Harness, *defaults.MaxBudget)
		}
		sess.MaxBudgetUSD = *defaults.MaxBudget
		layers[msg.EffectiveSettingMaxBudget] = msg.EffectiveLayerHarnessDefault
	}

	// disabled tools: a sent list, even an empty one, is the creator's answer
	// and outranks the harness default. What the bundle denies and what
	// tool-store has switched off are unioned in whatever the creator sent: a
	// deny the caller could undo would not be a deny.
	byLayer := map[msg.EffectiveConfigLayer][]string{}
	var chosen []string
	raw, sent := cfg[harnessconfig.KeyDisabledTools]
	if sent {
		if err := json.Unmarshal(raw, &chosen); err != nil {
			return fmt.Errorf("harness_config.disabled_tools is not a string array: %w", err)
		}
		byLayer[msg.EffectiveLayerSession] = chosen
	} else if len(defaults.DisabledTools) > 0 {
		chosen = defaults.DisabledTools
		byLayer[msg.EffectiveLayerHarnessDefault] = chosen
	}
	denials, err := s.decideHarnessToolDenials(ctx, sess.Harness, bundle)
	if err != nil {
		return err
	}
	if len(denials.Bundle) > 0 {
		byLayer[msg.EffectiveLayerBundle] = denials.Bundle
	}
	if len(denials.ToolStore) > 0 {
		byLayer[msg.EffectiveLayerToolStore] = denials.ToolStore
	}
	disabled := unionInOrder(chosen, denials.Bundle, denials.ToolStore)
	var deniedReadPaths []string
	if bundle != nil {
		deniedReadPaths = bundle.DeniedReadPaths
	}
	_, callerSetSettings := cfg[harnessconfig.KeySettings]
	if err := checkDeniedReadPathsAreEnforceable(sess.Harness, deniedReadPaths, disabled, denials.CommandRunningTools, callerSetSettings); err != nil {
		return err
	}
	for _, layer := range []msg.EffectiveConfigLayer{msg.EffectiveLayerSession, msg.EffectiveLayerBundle, msg.EffectiveLayerHarnessDefault, msg.EffectiveLayerToolStore} {
		if _, contributed := byLayer[layer]; contributed {
			layers[msg.EffectiveSettingDisabledTools] = layer
			break
		}
	}
	if sent || len(disabled) > 0 {
		if err := pin(harnessconfig.KeyDisabledTools, disabled); err != nil {
			return err
		}
		if err := pin(harnessconfig.KeyDisabledToolsByLayer, byLayer); err != nil {
			return err
		}
	}
	if bundleRestrictsSession(bundle) {
		_, callerSetTools := cfg[harnessconfig.KeyBuiltInToolAllowList]
		allowed, err := restrictedSessionBuiltInToolAllowList(sess.Harness, denials.EnabledInToolStore, disabled, callerSetTools)
		if err != nil {
			return err
		}
		if err := pin(harnessconfig.KeyBuiltInToolAllowList, allowed); err != nil {
			return err
		}
		if err := pin(harnessconfig.KeyBuiltInToolAllowListPinnedFor, sess.BundleID); err != nil {
			return err
		}
	}
	if len(deniedReadPaths) > 0 {
		if err := pin(harnessconfig.KeyDeniedReadPaths, deniedReadPaths); err != nil {
			return err
		}
		layers[msg.EffectiveSettingDeniedReadPaths] = msg.EffectiveLayerBundle
	}

	if err := pin(harnessconfig.KeySettingLayers, layers); err != nil {
		return err
	}
	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal harness_config for %s: %w", sess.SessionID, err)
	}
	sess.HarnessConfig = merged
	return nil
}

// settingLayersPinnedOn reads harness_config.setting_layers. A row created
// before the record existed has none, which is not an error.
func settingLayersPinnedOn(cfg map[string]json.RawMessage) pinnedSettingLayers {
	layers := pinnedSettingLayers{}
	if raw, ok := cfg[harnessconfig.KeySettingLayers]; ok {
		_ = json.Unmarshal(raw, &layers)
	}
	return layers
}

// markSettingChosenOnSession records that a setting now holds the session's own
// value: POST /sessions/{id}/config changed it after create.
func markSettingChosenOnSession(cfg map[string]json.RawMessage, key msg.EffectiveSettingKey) error {
	layers := settingLayersPinnedOn(cfg)
	layers[key] = msg.EffectiveLayerSession
	raw, err := json.Marshal(layers)
	if err != nil {
		return err
	}
	cfg[harnessconfig.KeySettingLayers] = raw
	return nil
}

// harnessConfigWithSettingsChosenOnSession returns current with each key marked
// as the session's own value in harness_config.setting_layers.
func harnessConfigWithSettingsChosenOnSession(current json.RawMessage, keys ...msg.EffectiveSettingKey) (json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	if len(current) > 0 {
		if err := json.Unmarshal(current, &cfg); err != nil {
			return nil, fmt.Errorf("stored harness_config is not an object: %w", err)
		}
	}
	for _, key := range keys {
		if err := markSettingChosenOnSession(cfg, key); err != nil {
			return nil, err
		}
	}
	return json.Marshal(cfg)
}
