package server

import (
	"encoding/json"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// injectBypassFlag forwards the global Bypass Permissions toggle (managed via
// /bridge/bypass-permissions, persisted in bridge-prefs) to the harness as a
// single canonical start-param flag — `bypass_permissions: true`. The
// translation from "this session is bypassed" to harness-specific behavior
// (codex sets sandbox=danger-full-access; claudecode short-circuits the
// PreToolUse prehook server-side, etc.) belongs in each harness bridge, not
// here. This keeps bridge-server harness-agnostic: it only owns the toggle's
// value and the channel for delivering it.
//
// Mutation is in-memory only (consistent with injectAgentsContext /
// injectHookSettings); persistence happens only when the user edits
// HarnessConfig directly.
func (s *Server) injectBypassFlag(sess *store.Session) {
	if sess == nil || s.bridgePrefs == nil {
		return
	}
	if !s.bridgePrefs.get().BypassPermissions {
		return
	}

	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[bypass] HarnessConfig unparseable for %s: %v", sess.BridgeID, err)
			return
		}
	}

	cfg["bypass_permissions"] = json.RawMessage(`true`)

	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[bypass] re-marshal HarnessConfig for %s: %v", sess.BridgeID, err)
		return
	}
	sess.HarnessConfig = merged
}
