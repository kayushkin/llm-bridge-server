package server

import (
	"encoding/json"
	"log"

	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// injectAgentsContext merges AGENTS.md content (resolved by agent-store
// from global + project scopes) into the session's HarnessConfig as
// system_prompt, so that non-native harnesses receive the same context
// that Claude Code reads natively from CLAUDE.md.
//
// Native harnesses (claude-code) are skipped — they discover their own
// CLAUDE.md on session start and a server-side injection would just
// duplicate. The resolver enforces this via SkipReason; we honor it.
//
// If the caller already set a system_prompt on HarnessConfig, that wins —
// explicit overrides are respected. Mutation is in-memory only (not
// persisted) so future edits to AGENTS.md take effect on the next spawn.
func (s *Server) injectAgentsContext(sess *store.Session) {
	if sess == nil || s.agentStore == nil {
		return
	}

	res, err := s.agentStore.ResolveContext(string(sess.Harness), workDirForSession(sess))
	if err != nil {
		log.Printf("[context] resolve for %s: %v", sess.SessionID, err)
		return
	}
	if res.SkipReason != "" || res.Content == "" {
		return
	}

	var cfg map[string]json.RawMessage
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[context] HarnessConfig unparseable for %s: %v", sess.SessionID, err)
			return
		}
	}
	if cfg == nil {
		cfg = make(map[string]json.RawMessage)
	}
	if _, ok := cfg["system_prompt"]; ok {
		return
	}

	encoded, err := json.Marshal(res.Content)
	if err != nil {
		log.Printf("[context] encode system_prompt for %s: %v", sess.SessionID, err)
		return
	}
	cfg["system_prompt"] = encoded

	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[context] re-marshal HarnessConfig for %s: %v", sess.SessionID, err)
		return
	}
	sess.HarnessConfig = merged
}

// workDirForSession returns the project directory the session is anchored
// to, or "" if unknown. Today the only place a workdir survives onto the
// session is buildStartParams' DisplayName-as-path overload (set by the
// claudecode harness on session resume); that's the same signal we read
// here so resume picks up the same project AGENTS.md.
func workDirForSession(sess *store.Session) string {
	if sess == nil {
		return ""
	}
	if sess.DisplayName != "" && sess.DisplayName[0] == '/' {
		return sess.DisplayName
	}
	return ""
}
