package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	hookstore "github.com/kayushkin/hook-store"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// startOnInstance is the single chokepoint for spawning a Claude Code
// harness process — it synthesizes the registered hooks into the session's
// settings, populates the instance's Machine for transport routing, and
// delegates to the harness manager. Every spawn path (create, resume,
// auto-resume, fork) calls through here so hook wiring and machine
// resolution stay consistent.
func (s *Server) startOnInstance(ctx context.Context, sess *store.Session, inst *msg.Instance, credID string) (harness.HarnessProcess, error) {
	if inst.Machine == nil {
		if inst.MachineID == "" {
			return nil, fmt.Errorf("instance %s has no machine_id", inst.ID)
		}
		m, err := s.harnessStore.GetMachine(inst.MachineID)
		if err != nil {
			return nil, fmt.Errorf("load machine %s for instance %s: %w", inst.MachineID, inst.ID, err)
		}
		inst.Machine = m
	}
	s.injectHookSettings(sess)
	s.injectAgentsContext(sess)
	if err := s.injectMCPConfig(sess); err != nil {
		return nil, fmt.Errorf("inject mcp config: %w", err)
	}
	return s.harness.StartOnInstance(ctx, sess, inst, credID)
}

// injectHookSettings merges a synthesized Claude Code settings JSON into
// the session's HarnessConfig under the "settings" key, so buildStartParams
// passes it to the harness via --settings. The mutation is in-memory only
// (not persisted) so hook-store changes take effect on the next spawn.
// If the caller has already set a custom settings value, it wins.
func (s *Server) injectHookSettings(sess *store.Session) {
	if sess == nil || s.hookStore == nil {
		return
	}
	if sess.Harness != msg.HarnessClaudeCode {
		return
	}

	var cfg map[string]json.RawMessage
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			log.Printf("[hooks] HarnessConfig unparseable for %s: %v", sess.BridgeID, err)
			return
		}
	}
	if cfg == nil {
		cfg = make(map[string]json.RawMessage)
	}
	if _, ok := cfg["settings"]; ok {
		// Explicit user override — don't clobber.
		return
	}

	settings, err := s.buildClaudeCodeSettings(sess)
	if err != nil {
		log.Printf("[hooks] synthesize settings for %s: %v", sess.BridgeID, err)
		return
	}
	if settings == "" {
		return
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		log.Printf("[hooks] encode settings for %s: %v", sess.BridgeID, err)
		return
	}
	cfg["settings"] = encoded

	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[hooks] re-marshal HarnessConfig for %s: %v", sess.BridgeID, err)
		return
	}
	sess.HarnessConfig = merged
}

// buildClaudeCodeSettings reads the hook-store, selects every enabled
// claude-code hook whose scope applies to this session (global + this
// instance + this session), and returns a JSON string in Claude Code's
// settings.json format:
//
//	{
//	  "hooks": {
//	    "PreToolUse": [
//	      { "matcher": "...", "hooks": [{"type":"command","command":"curl …/hooks/exec/<id>"}] }
//	    ]
//	  }
//	}
//
// Each entry's command POSTs the stdin payload Claude Code hands to the
// hook straight to /hooks/exec/<id>, which runs the hook-store's configured
// shell command and returns its stdout. From CC's perspective the exec
// response is indistinguishable from a local shell hook.
func (s *Server) buildClaudeCodeSettings(sess *store.Session) (string, error) {
	base := publicBaseURL(s.cfg.ListenAddr)
	byEvent := make(map[string][]any, 4)

	// Permission gate: a PreToolUse HTTP hook routed to bridge-server's
	// /permission/cc-prehook endpoint. Replaces the embedded bridge_perm
	// MCP path (see permission-store/docs/cc-mcp-retrospective.md). CC
	// supports type:"http" natively and honors the per-hook timeout
	// field, so no curl wrapper or env-var clamp logic is needed.
	//
	// matcher "*" → fires on every tool call. Prepended to PreToolUse
	// so the permission decision lands before any snapshot hooks run;
	// CC's hook executor processes entries in the order we declare.
	//
	// Timeout: 86400s = 1 day. Long enough that no human-driven approval
	// flow will hit it; far below the 24-day Node-side TIMEOUT_MAX
	// trap that bit the MCP path.
	if sess.BridgeID != "" {
		permEntry := map[string]any{
			"matcher": "*",
			"hooks": []map[string]any{
				{
					"type":    "http",
					"url":     fmt.Sprintf("%s/permission/cc-prehook/%s", base, sess.BridgeID),
					"timeout": 86400,
				},
			},
		}
		byEvent["PreToolUse"] = append(byEvent["PreToolUse"], permEntry)
	}

	// User-registered hooks from hook-store. These sit AFTER the
	// permission hook in PreToolUse so they only run when the call
	// was allowed.
	if s.hookStore != nil {
		applicable, err := s.collectApplicableHooks(sess)
		if err != nil {
			return "", err
		}
		for _, h := range applicable {
			cmd := fmt.Sprintf(
				"curl -sfS -X POST -H 'Content-Type: application/json' --data-binary @- %s/hooks/exec/%s",
				base, h.ID,
			)
			entry := map[string]any{
				"matcher": h.Matcher,
				"hooks": []map[string]any{
					{"type": "command", "command": cmd},
				},
			}
			byEvent[h.Event] = append(byEvent[h.Event], entry)
		}
	}

	if len(byEvent) == 0 {
		return "", nil
	}

	out, err := json.Marshal(map[string]any{"hooks": byEvent})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// collectApplicableHooks runs three scoped queries and concatenates them.
// Instance and session filters are skipped when the session has no id for
// that scope, which avoids matching hooks registered with scope_id="".
func (s *Server) collectApplicableHooks(sess *store.Session) ([]msg.Hook, error) {
	filters := []hookstore.ListFilter{
		{Harness: msg.HarnessClaudeCode, ScopeKind: msg.HookScopeGlobal, EnabledSet: true, Enabled: true},
	}
	if sess.InstanceID != "" {
		filters = append(filters, hookstore.ListFilter{
			Harness: msg.HarnessClaudeCode, ScopeKind: msg.HookScopeInstance,
			ScopeID: sess.InstanceID, EnabledSet: true, Enabled: true,
		})
	}
	if sess.BridgeID != "" {
		filters = append(filters, hookstore.ListFilter{
			Harness: msg.HarnessClaudeCode, ScopeKind: msg.HookScopeSession,
			ScopeID: sess.BridgeID, EnabledSet: true, Enabled: true,
		})
	}

	var all []msg.Hook
	for _, f := range filters {
		hooks, err := s.hookStore.ListHooks(f)
		if err != nil {
			return nil, err
		}
		all = append(all, hooks...)
	}
	return all, nil
}

// publicBaseURL converts a listen address (":8160", "0.0.0.0:8160", or a
// full URL) into an origin the harness can reach. For bare ports we resolve
// to localhost since the harness subprocess runs on the same host.
func publicBaseURL(listenAddr string) string {
	if strings.HasPrefix(listenAddr, "http://") || strings.HasPrefix(listenAddr, "https://") {
		return strings.TrimRight(listenAddr, "/")
	}
	host := listenAddr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	return "http://" + host
}
