package server

import (
	"encoding/json"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// injectCodexBypass forces a codex session into full-access mode when the
// global bypass-permissions toggle is on. The codex bridge defaults to
// sandbox=workspace-write, which on Ubuntu 24.04 hosts with
// apparmor_restrict_unprivileged_userns=1 fails at the bwrap loopback step
// (RTM_NEWADDR EPERM) and silently kills every shell tool call. With
// BypassPermissions on, the user has explicitly opted out of every permission
// gate, so we forward the equivalent codex-side bypass: approval=never and
// sandbox=danger-full-access. Mirrors the claude_code path where bypass is
// honored by short-circuiting the PreToolUse prehook.
//
// Mutation is in-memory only (consistent with injectAgentsContext /
// injectHookSettings); persistence happens only when the user edits
// HarnessConfig directly.
func (s *Server) injectCodexBypass(sess *store.Session) {
	if sess == nil || s.bridgePrefs == nil {
		return
	}
	if sess.Harness != msg.HarnessCodex {
		return
	}
	if !s.bridgePrefs.get().BypassPermissions {
		return
	}

	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[codex-bypass] HarnessConfig unparseable for %s: %v", sess.BridgeID, err)
			return
		}
	}

	cfg["permission_mode"] = json.RawMessage(`"never"`)
	cfg["sandbox"] = json.RawMessage(`"danger-full-access"`)
	// auto_approve, when true, would clobber sandbox back to workspace-write
	// in llm-bridge-codex/handler.go HandleStart — strip it so our bypass
	// override holds.
	delete(cfg, "auto_approve")

	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[codex-bypass] re-marshal HarnessConfig for %s: %v", sess.BridgeID, err)
		return
	}
	sess.HarnessConfig = merged
}
