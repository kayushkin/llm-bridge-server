package server

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
	modelstore "github.com/kayushkin/model-store"
)

// The ONE place a session's model is decided.
//
// model-store is the only source of a model. Every layer below it merely
// selects or overrides, and every winner is resolved THROUGH the registry —
// an id the registry does not know is refused, never passed along. The layers,
// in precedence order:
//
//  1. the session's own override — harness_config.model, set at create or by a
//     later POST /config. A canonical role name ("efficient") is accepted here
//     and recorded as the Role that was asked for.
//  2. the per-harness default in bridge-prefs (what dash /settings edits).
//  3. the registry's own `default` role.
//
// There is deliberately no fourth tier. The old behaviour — pass nothing and
// let the harness fall back to whatever its account default happened to be —
// is exactly how sessions ran on a model nobody chose while every picker showed
// the one they did. If none of the three layers can answer, the session does
// not start, and the error says which layers were empty.
//
// The lifecycle mirrors permission_mode.go, which is why that field never had
// this bug: snapshot the decision into the row at create, resolve it centrally,
// and inject it at the spawn chokepoint as a backstop for rows that predate
// the snapshot.
//
// Note what this does NOT claim: a session runs many models. Claude Code puts
// its title generation on a cheaper model regardless of this choice, and a
// subagent resolves its own. The value decided here is the DEFAULT for the
// session's next turn; the authoritative record of what actually ran is per
// call, and reconciling the two is a separate concern.

const (
	// harnessConfigKeyModel is the wire key every harness bridge already reads
	// for the model it should spawn on (llm-bridge-claudecode turns it into
	// --model). The resolved registry id is written here.
	harnessConfigKeyModel = "model"
	// harnessConfigKeyModelSelection carries the typed msg.ModelSelection —
	// the id plus which layer chose it — so a session can always say WHY it is
	// on the model it is on. Harness bridges ignore the key.
	harnessConfigKeyModelSelection = "model_selection"
)

// resolveModelSelection decides the model for sess by the precedence above and
// returns the typed selection. It never mutates sess.
func (s *Server) resolveModelSelection(sess *store.Session) (msg.ModelSelection, error) {
	if sess == nil {
		return msg.ModelSelection{}, fmt.Errorf("resolve model: nil session")
	}
	if s.modelStore == nil {
		return msg.ModelSelection{}, fmt.Errorf("resolve model for %s: model-store is not configured, and the registry is the only source of a model", sess.SessionID)
	}

	cfg, err := parseHarnessConfig(sess)
	if err != nil {
		// Refused rather than guessed: an unreadable blob may hold the
		// override the caller asked for, and choosing a different model than
		// the one they named is the silent wrong answer this exists to end.
		return msg.ModelSelection{}, fmt.Errorf("resolve model for %s: %w", sess.SessionID, err)
	}

	// 1. The session's own override.
	if raw, ok := cfg[harnessConfigKeyModel]; ok {
		var requested string
		if err := json.Unmarshal(raw, &requested); err != nil {
			return msg.ModelSelection{}, fmt.Errorf("resolve model for %s: harness_config.model is not a string: %w", sess.SessionID, err)
		}
		if requested != "" {
			return s.selectionFromRequested(requested, msg.ModelSelectedBySession)
		}
	}

	// 2. The per-harness default in bridge-prefs.
	if s.bridgePrefs != nil {
		if defaults, ok := s.bridgePrefs.get().Defaults[string(sess.Harness)]; ok && defaults.Model != "" {
			return s.selectionFromRequested(defaults.Model, msg.ModelSelectedByPrefs)
		}
	}

	// 3. The registry's own default role — the floor.
	m, err := s.modelStore.ResolveRole(modelstore.RoleDefault)
	if err != nil {
		return msg.ModelSelection{}, fmt.Errorf("resolve model for %s: no session override, no bridge-prefs default for harness %q, and the registry's default role cannot answer: %w", sess.SessionID, sess.Harness, err)
	}
	return msg.ModelSelection{
		Model:      msg.ModelID(m.ID),
		Role:       msg.ModelRoleDefault,
		SelectedBy: msg.ModelSelectedByRole,
	}, nil
}

// selectionFromRequested resolves what a layer asked for — a registry id, an
// alias, or a canonical role name — through model-store, attributing the
// result to that layer. Anything the registry cannot resolve is an error.
func (s *Server) selectionFromRequested(requested string, selectedBy msg.ModelSelectedBy) (msg.ModelSelection, error) {
	if s.modelStore == nil {
		return msg.ModelSelection{}, fmt.Errorf("resolve %q: model-store is not configured, and the registry is the only source of a model", requested)
	}
	if modelstore.IsCanonicalRole(requested) {
		m, err := s.modelStore.ResolveRole(requested)
		if err != nil {
			return msg.ModelSelection{}, fmt.Errorf("%s names role %q: %w", selectedBy, requested, err)
		}
		return msg.ModelSelection{
			Model:      msg.ModelID(m.ID),
			Role:       msg.ModelRole(requested),
			SelectedBy: selectedBy,
		}, nil
	}
	m, err := s.modelStore.ResolveModel(requested)
	if err != nil {
		return msg.ModelSelection{}, fmt.Errorf("%s names %q, which model-store does not know: %w", selectedBy, requested, err)
	}
	return msg.ModelSelection{
		Model:      msg.ModelID(m.ID),
		SelectedBy: selectedBy,
	}, nil
}

// snapshotModelSelectionIntoSession pins the resolved model and its selection
// into sess.HarnessConfig. Called once in handleCreateSession before the row is
// inserted, so the decision is durable from the session's first millisecond and
// a later change to bridge-prefs does not move a session that already exists.
// A session whose model cannot be resolved is not created.
func (s *Server) snapshotModelSelectionIntoSession(sess *store.Session) error {
	selection, err := s.resolveModelSelection(sess)
	if err != nil {
		return err
	}
	return writeModelSelectionIntoSession(sess, selection)
}

// injectModelSelection ensures sess.HarnessConfig carries a resolved model and
// its selection before start params are built. Rows created through
// handleCreateSession already have the snapshot; rows that predate it, or that
// carry a bare override with no selection (a legacy row, or one written by
// POST /config before that path recorded a selection), get resolved here so
// the harness never spawns without a definite model. Mutation is in-memory
// only, like the other injectors; persistence belongs to the create and config
// paths. A failure here fails the spawn, loudly.
func (s *Server) injectModelSelection(sess *store.Session) error {
	if sess == nil {
		return fmt.Errorf("inject model selection: nil session")
	}
	cfg, err := parseHarnessConfig(sess)
	if err != nil {
		return fmt.Errorf("inject model selection for %s: %w", sess.SessionID, err)
	}
	_, hasModel := cfg[harnessConfigKeyModel]
	_, hasSelection := cfg[harnessConfigKeyModelSelection]
	if hasModel && hasSelection {
		return nil // already pinned by create or config
	}
	selection, err := s.resolveModelSelection(sess)
	if err != nil {
		return err
	}
	log.Printf("[model_selection] %s: no pinned model on the row; resolved %s (selected_by=%s) at spawn", sess.SessionID, selection.Model, selection.SelectedBy)
	return writeModelSelectionIntoSession(sess, selection)
}

// writeModelSelectionIntoSession writes both keys into sess.HarnessConfig
// (in memory). The resolved id goes under "model" so every harness bridge
// reads it unchanged; the typed selection goes under "model_selection".
func writeModelSelectionIntoSession(sess *store.Session, selection msg.ModelSelection) error {
	cfg, err := parseHarnessConfig(sess)
	if err != nil {
		return err
	}
	idRaw, err := json.Marshal(string(selection.Model))
	if err != nil {
		return fmt.Errorf("marshal model id: %w", err)
	}
	selectionRaw, err := json.Marshal(selection)
	if err != nil {
		return fmt.Errorf("marshal model selection: %w", err)
	}
	cfg[harnessConfigKeyModel] = idRaw
	cfg[harnessConfigKeyModelSelection] = selectionRaw
	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal harness_config for %s: %w", sess.SessionID, err)
	}
	sess.HarnessConfig = merged
	return nil
}

// parseHarnessConfig decodes sess.HarnessConfig as an object, treating an
// empty blob as an empty object. An unparseable blob is an error, not an
// empty map — see resolveModelSelection for why.
func parseHarnessConfig(sess *store.Session) (map[string]json.RawMessage, error) {
	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) == 0 {
		return cfg, nil
	}
	if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
		return nil, fmt.Errorf("harness_config is not a JSON object: %w", err)
	}
	return cfg, nil
}
