package server

import (
	"encoding/json"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Per-session permission-mode model: every session gets its own
// permission_mode string snapshotted into HarnessConfig at creation time
// (from the current global value). From then on the per-session value is
// durable — changing the global does NOT affect existing sessions; only
// an explicit PUT /sessions/{id}/permission-mode does.
//
// The mode is one of "ask" / "auto" / "bypass" (see msg.PermissionMode*).
// Empty string falls back to the global default, which itself defaults to
// "ask". The legacy bypass_permissions boolean still on disk is read
// back-compat (true → bypass, false → ask) and rewritten as a mode the
// next time the prefs are saved.
//
// Three helpers carve up the lifecycle:
//
//   • snapshotPermissionModeIntoSession — pin the global into HarnessConfig at create
//   • permissionModeForSession         — read the effective mode (per-session
//                                        wins; falls back to legacy bool,
//                                        then global, then "ask")
//   • injectPermissionModeFlag         — at start time, ensure HarnessConfig
//                                        carries a definite mode so it
//                                        flows to harness start params

// snapshotPermissionModeIntoSession copies the current global permission
// mode onto sess.HarnessConfig.permission_mode if the caller hasn't
// already set one. Called once in handleCreateSession. Records the value
// either way (including "ask") so the session is fully independent from
// the global thereafter.
func (s *Server) snapshotPermissionModeIntoSession(sess *store.Session) {
	if sess == nil || s.bridgePrefs == nil {
		return
	}

	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[permission_mode] HarnessConfig unparseable for %s: %v", sess.SessionID, err)
			return
		}
	}
	// Honor caller-supplied value (either field): take permission_mode if
	// present, fall back to legacy bypass_permissions bool so existing
	// callers keep working until they migrate.
	if _, ok := cfg["permission_mode"]; ok {
		return
	}
	if raw, ok := cfg["bypass_permissions"]; ok {
		var b bool
		if err := json.Unmarshal(raw, &b); err == nil {
			cfg["permission_mode"] = json.RawMessage(`"` + bypassBoolToMode(b) + `"`)
			delete(cfg, "bypass_permissions")
			s.writeBackHarnessConfig(sess, cfg)
		}
		return
	}

	mode := s.globalPermissionMode()
	cfg["permission_mode"] = json.RawMessage(`"` + mode + `"`)
	s.writeBackHarnessConfig(sess, cfg)
}

// permissionModeForSession returns the effective mode for a session. Order
// of precedence: per-session permission_mode → per-session legacy
// bypass_permissions bool → global permission_mode → global legacy
// BypassPermissions bool → "ask".
func (s *Server) permissionModeForSession(sess *store.Session) string {
	if sess != nil && len(sess.HarnessConfig) > 0 {
		var cfg map[string]json.RawMessage
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err == nil {
			if raw, ok := cfg["permission_mode"]; ok {
				var v string
				if err := json.Unmarshal(raw, &v); err == nil && v != "" {
					return v
				}
			}
			if raw, ok := cfg["bypass_permissions"]; ok {
				var b bool
				if err := json.Unmarshal(raw, &b); err == nil {
					return bypassBoolToMode(b)
				}
			}
		}
	}
	return s.globalPermissionMode()
}

// globalPermissionMode reads the global default, preferring the new
// PermissionMode field but honoring the legacy BypassPermissions bool
// when the new field hasn't been written yet.
func (s *Server) globalPermissionMode() string {
	if s.bridgePrefs == nil {
		return msg.PermissionModeAsk
	}
	prefs := s.bridgePrefs.get()
	if prefs.PermissionMode != "" {
		return prefs.PermissionMode
	}
	return bypassBoolToMode(prefs.BypassPermissions)
}

// bypassBoolToMode translates the legacy boolean form to the new enum.
// true → bypass, false → ask. Used only for migration; new code should
// write the mode directly.
func bypassBoolToMode(b bool) string {
	if b {
		return msg.PermissionModeBypass
	}
	return msg.PermissionModeAsk
}

// injectPermissionModeFlag ensures sess.HarnessConfig.permission_mode
// carries the effective mode before start params are built. New sessions
// already have the snapshot from create time; legacy sessions (created
// before the snapshot existed, or before this refactor) get the current
// effective value injected so the harness still receives a definite
// signal. Mutation is in-memory only; persistence happens via PUT
// .../permission-mode.
//
// Each harness bridge owns the translation from "auto"/"bypass" to its
// own gating mechanism (codex: ApprovalMode + SandboxPolicy; claudecode:
// --permission-mode flag; etc.). The prehook is the universal gate.
func (s *Server) injectPermissionModeFlag(sess *store.Session) {
	if sess == nil {
		return
	}
	cfg := make(map[string]json.RawMessage)
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[permission_mode] HarnessConfig unparseable for %s: %v", sess.SessionID, err)
			return
		}
	}
	if _, ok := cfg["permission_mode"]; ok {
		// Drop the legacy field if both are present so harnesses don't see
		// stale data. permission_mode is authoritative.
		if _, hasLegacy := cfg["bypass_permissions"]; hasLegacy {
			delete(cfg, "bypass_permissions")
			s.writeBackHarnessConfig(sess, cfg)
		}
		return
	}
	mode := s.permissionModeForSession(sess)
	cfg["permission_mode"] = json.RawMessage(`"` + mode + `"`)
	delete(cfg, "bypass_permissions")
	s.writeBackHarnessConfig(sess, cfg)
}

func (s *Server) writeBackHarnessConfig(sess *store.Session, cfg map[string]json.RawMessage) {
	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[permission_mode] re-marshal HarnessConfig for %s: %v", sess.SessionID, err)
		return
	}
	sess.HarnessConfig = merged
}
