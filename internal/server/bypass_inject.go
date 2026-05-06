package server

import (
	"encoding/json"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Per-session bypass model: every session gets its own bypass_permissions
// boolean snapshotted into HarnessConfig at creation time (from the current
// global toggle). From then on the per-session value is durable — flipping
// the global toggle does NOT change existing sessions; only an explicit
// PATCH /sessions/{id}/bypass-permissions does.
//
// Three helpers below carve up the lifecycle:
//
//   • snapshotBypassIntoSession — pin the global into HarnessConfig at create
//   • bypassEnabledForSession  — read the effective value (per-session wins;
//                                falls back to global for legacy sessions
//                                created before the snapshot existed)
//   • injectBypassFlag         — at start time, ensure HarnessConfig carries
//                                the effective value so it flows to harness
//                                start params; harnesses do their own
//                                interpretation of the flag.

// snapshotBypassIntoSession copies the current global Bypass Permissions
// value onto sess.HarnessConfig.bypass_permissions if the caller hasn't
// already set one. Called once in handleCreateSession. Records the value
// either way (true OR false) so the session is fully independent from the
// global toggle thereafter.
func (s *Server) snapshotBypassIntoSession(sess *store.Session) {
	if sess == nil || s.bridgePrefs == nil {
		return
	}

	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[bypass] HarnessConfig unparseable for %s: %v", sess.BridgeID, err)
			return
		}
	}
	if _, ok := cfg["bypass_permissions"]; ok {
		return // caller-supplied value wins
	}

	val := s.bridgePrefs.get().BypassPermissions
	if val {
		cfg["bypass_permissions"] = json.RawMessage(`true`)
	} else {
		cfg["bypass_permissions"] = json.RawMessage(`false`)
	}

	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[bypass] re-marshal HarnessConfig for %s: %v", sess.BridgeID, err)
		return
	}
	sess.HarnessConfig = merged
}

// bypassEnabledForSession returns the effective bypass value for a session.
// Per-session value (HarnessConfig.bypass_permissions) wins; if absent
// (legacy session), falls back to the live global toggle. Single source of
// truth for "is this session bypassed?" — used by the CC PreToolUse prehook
// and by the harness-side flag injection.
func (s *Server) bypassEnabledForSession(sess *store.Session) bool {
	if sess != nil && len(sess.HarnessConfig) > 0 {
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err == nil {
			if raw, ok := cfg["bypass_permissions"]; ok {
				var v bool
				if err := json.Unmarshal(raw, &v); err == nil {
					return v
				}
			}
		}
	}
	if s.bridgePrefs == nil {
		return false
	}
	return s.bridgePrefs.get().BypassPermissions
}

// injectBypassFlag ensures sess.HarnessConfig.bypass_permissions matches the
// effective value before start params are built. New sessions already carry
// the snapshot from create time — this is a no-op for them. Legacy sessions
// (created before the snapshot existed) get the current global value
// injected so the harness still receives a definite signal. Mutation is
// in-memory only; persistence happens via PATCH .../bypass-permissions.
//
// Each harness bridge owns the translation from "bypass=true" to its own
// gating mechanism (codex: sandbox=danger-full-access + approval=never;
// claudecode: server-side prehook short-circuit; etc.).
func (s *Server) injectBypassFlag(sess *store.Session) {
	if sess == nil {
		return
	}
	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[bypass] HarnessConfig unparseable for %s: %v", sess.BridgeID, err)
			return
		}
	}
	if _, ok := cfg["bypass_permissions"]; ok {
		return // already set (snapshot at create or PATCH override)
	}
	if !s.bypassEnabledForSession(sess) {
		return // legacy session, global=off — nothing to inject
	}
	cfg["bypass_permissions"] = json.RawMessage(`true`)
	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[bypass] re-marshal HarnessConfig for %s: %v", sess.BridgeID, err)
		return
	}
	sess.HarnessConfig = merged
}
