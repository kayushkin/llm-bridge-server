package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"

	hookstore "github.com/kayushkin/llm-bridge-server/internal/agents/hookstore"
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
//
// Two callers may reach here for one session at once, and only one may
// spawn. At startup auto-resume starts every session a restart interrupted
// in a goroutine, while the server is already answering; a message sent to
// the same session in that gap found no process yet and started a second
// one, and the manager's map kept whichever registered last while the other
// ran on unsupervised. A deploy that reports back to the session that
// started it sends exactly such a message, seconds after the restart. So the
// spawn holds a lock per session, and a caller that finds a process already
// running is handed that process instead of a second one.
func (s *Server) startOnInstance(ctx context.Context, sess *store.Session, inst *msg.Instance, credID string) (harness.HarnessProcess, error) {
	harnessStartLock, _ := s.harnessStartLockBySessionID.LoadOrStore(sess.SessionID, &sync.Mutex{})
	harnessStartLock.(*sync.Mutex).Lock()
	defer harnessStartLock.(*sync.Mutex).Unlock()
	if running := s.harness.Get(sess.SessionID); running != nil {
		log.Printf("[spawn] %s: a harness process is already running; using it instead of starting a second", sess.SessionID)
		return running, nil
	}
	// A session that has spent its ceiling does not get a process, whichever
	// path asked for one. The individual handlers check this too, because
	// only they can return a decent HTTP status for it — but the check
	// belongs here as well, because two of the spawn paths have no handler
	// to check anything. Auto-resume is the one that matters: it restarts a
	// session and REPLAYS its interrupted turn, which is precisely the turn
	// the gate just interrupted for costing too much. Without this the halt
	// would undo itself a minute later.
	if over, spendUSD, maxBudgetUSD := s.harness.SessionOverBudget(sess.SessionID); over {
		return nil, fmt.Errorf("session %s has spent $%.2f of its $%.2f ceiling; raise max_budget to continue", sess.SessionID, spendUSD, maxBudgetUSD)
	}
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
	subject, err := s.grantSubjectOfSession(sess)
	if err != nil {
		return nil, err
	}
	if err := s.checkPrincipalMayRunHere(ctx, subject, sess, inst); err != nil {
		return nil, err
	}
	if err := s.injectHookSettings(sess); err != nil {
		return nil, fmt.Errorf("inject hook settings: %w", err)
	}
	s.injectPromptContext(ctx, sess, inst)
	s.injectPermissionModeFlag(sess)
	if err := s.injectModelSelection(ctx, sess); err != nil {
		return nil, fmt.Errorf("inject model selection: %w", err)
	}
	if err := s.injectMCPConfig(sess); err != nil {
		return nil, fmt.Errorf("inject mcp config: %w", err)
	}
	if err := s.inlineMCPConfigForRunner(sess, inst); err != nil {
		return nil, fmt.Errorf("inline mcp config for runner: %w", err)
	}
	if err := s.injectComputerUse(sess, inst, s.harness.RunnerComputerUse); err != nil {
		return nil, fmt.Errorf("computer use: %w", err)
	}
	sessionEnvironment, err := s.sessionAgentEnvironment(sess)
	if err != nil {
		return nil, err
	}
	sessionEnvironment = append(sessionEnvironment, s.sessionPostingEnvironmentForInstance(sess, inst)...)
	return s.harness.StartOnInstance(ctx, sess, inst, credID, sessionEnvironment)
}

// injectHookSettings synthesizes a per-harness hook config blob and merges
// it into the session's HarnessConfig. The mutation is in-memory only
// (not persisted) so hook-store changes take effect on the next spawn.
//
// Per-harness storage key:
//
//   - claude_code → "settings" (consumed by --settings flag; the synthesized
//     JSON is the full CC settings.json shape with `hooks` block)
//   - codex       → "codex_hooks" (codex bridge translates the JSON tree
//     into `-c hooks.<EventName>=<inline-toml>` args at app-server spawn)
//
// If the caller has already set the harness-specific key, it wins.
//
// A nil hookStore is NOT a reason to skip this. The permission gate is
// synthesized from the session id alone and does not read the hook registry;
// only the user-hook section does, and both builders guard hookStore
// separately where they need it (buildClaudeCodeSettings, buildCodexHookConfig).
// Returning early here would turn a hook-registry outage into a silent
// permission-gate outage — main() sets the store to nil and continues when
// hookstore.Open fails, so that path is reachable in production.
// hookConfigKeyByHarness names the harnesses this server wires hooks into and
// the harness_config key each one's synthesized block is written under. It is
// what GET /hook-options serves, and TestEveryHarnessInTheHookOptionsIsWired
// holds it to the switch below: a harness listed here that the switch does not
// handle would be offered by the editor and never run.
var hookConfigKeyByHarness = map[msg.Harness]string{
	msg.HarnessClaudeCode: "settings",
	msg.HarnessCodex:      "codex_hooks",
	msg.HarnessDevin:      "devin_hooks",
}

func (s *Server) injectHookSettings(sess *store.Session) error {
	if sess == nil {
		return nil
	}

	switch sess.Harness {
	case msg.HarnessClaudeCode:
		return s.injectClaudeCodeHookSettings(sess)
	case msg.HarnessCodex:
		return s.injectCodexHookSettings(sess)
	case msg.HarnessDevin:
		return s.injectDevinHookSettings(sess)
	}
	return nil
}

func (s *Server) injectClaudeCodeHookSettings(sess *store.Session) error {
	cfg := map[string]json.RawMessage{}
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			return fmt.Errorf("harness_config of %s is unparseable: %w", sess.SessionID, err)
		}
	}
	if _, ok := cfg[harnessConfigKeySettings]; ok {
		// Explicit user override — don't clobber. But the Read deny rules
		// live in the settings this would have written, so a session whose
		// bundle denies paths cannot take an override.
		paths, err := deniedReadPathsPinnedOn(cfg)
		if err != nil {
			return err
		}
		if len(paths) > 0 {
			return fmt.Errorf("session %s set harness_config.settings itself, which would drop the Read deny rules for %v its bundle requires", sess.SessionID, paths)
		}
		return nil
	}

	settings, err := s.buildClaudeCodeSettings(sess)
	if err != nil {
		return fmt.Errorf("synthesize settings for %s: %w", sess.SessionID, err)
	}
	if settings == "" {
		return nil
	}
	encoded, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("encode settings for %s: %w", sess.SessionID, err)
	}
	cfg["settings"] = encoded

	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal harness_config for %s: %w", sess.SessionID, err)
	}
	sess.HarnessConfig = merged
	return nil
}

// injectCodexHookSettings writes a codex-shaped hooks tree into
// HarnessConfig.codex_hooks. The codex bridge translates this JSON to
// `-c hooks.<EventName>=<inline-toml>` arguments at app-server spawn
// (codex doesn't accept a hooks-file flag; the only injection path is
// per-key TOML overrides).
//
// Like the CC variant, if a caller has already populated codex_hooks
// (explicit user override) we leave it alone.
func (s *Server) injectCodexHookSettings(sess *store.Session) error {
	cfg := map[string]json.RawMessage{}
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			return fmt.Errorf("harness_config of %s is unparseable: %w", sess.SessionID, err)
		}
	}
	if _, ok := cfg["codex_hooks"]; ok {
		return nil
	}

	tree, err := s.buildCodexHookConfig(sess)
	if err != nil {
		return fmt.Errorf("synthesize codex hooks for %s: %w", sess.SessionID, err)
	}
	if tree == nil {
		return nil
	}
	encoded, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("encode codex hooks for %s: %w", sess.SessionID, err)
	}
	cfg["codex_hooks"] = encoded

	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal harness_config for %s: %w", sess.SessionID, err)
	}
	sess.HarnessConfig = merged
	return nil
}

// injectDevinHookSettings writes the hooks devin runs into
// HarnessConfig.devin_hooks, and the permission gate's URL into
// devin_permission_url. llm-bridge-devin puts the tree into the config file it
// starts `devin acp` with; devin's hooks.v1 file has the same shape as codex's
// hooks tree, and devin honors the hookSpecificOutput.permissionDecision
// answer the prehook gives, so the codex builder serves both with the route
// swapped. llm-bridge-devin refuses to start without devin_hooks, so a session
// can never run ungated. An explicit devin_hooks in harness_config is left
// alone, like the other harnesses' overrides.
func (s *Server) injectDevinHookSettings(sess *store.Session) error {
	cfg := map[string]json.RawMessage{}
	if len(sess.HarnessConfig) > 0 {
		if err := json.Unmarshal(sess.HarnessConfig, &cfg); err != nil {
			return fmt.Errorf("harness_config of %s is unparseable: %w", sess.SessionID, err)
		}
	}
	if _, ok := cfg["devin_hooks"]; ok {
		return nil
	}
	tree, err := s.buildPrehookCommandHookTree(sess, msg.HarnessDevin, "devin-prehook")
	if err != nil {
		return fmt.Errorf("synthesize devin hooks for %s: %w", sess.SessionID, err)
	}
	if tree == nil {
		return nil
	}
	encoded, err := json.Marshal(tree)
	if err != nil {
		return fmt.Errorf("encode devin hooks for %s: %w", sess.SessionID, err)
	}
	cfg["devin_hooks"] = encoded
	gateURL, err := json.Marshal(fmt.Sprintf("%s/permission/devin-prehook/%s", publicBaseURL(s.cfg.ListenAddr), escapePathSegment(sess.SessionID)))
	if err != nil {
		return err
	}
	cfg["devin_permission_url"] = gateURL
	merged, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("re-marshal harness_config for %s: %w", sess.SessionID, err)
	}
	sess.HarnessConfig = merged
	return nil
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

	// Permission gate: a PreToolUse command hook that posts the tool call to
	// /permission/cc-prehook and blocks the call when that post fails
	// (permissionGateCommand). It was a type:"http" hook until 2026-11-23, and
	// Claude Code ignores an http hook it cannot reach: measured on 2.1.330, a
	// dead port or a 503 let the tool run, and the wrapper starts every session
	// in bypassPermissions so this hook is the only gate.
	//
	// matcher "*" → fires on every tool call. Prepended to PreToolUse
	// so the permission decision lands before any snapshot hooks run;
	// CC's hook executor processes entries in the order we declare.
	if sess.SessionID != "" {
		permEntry := map[string]any{
			"matcher": "*",
			"hooks": []map[string]any{
				{
					"type":    "command",
					"command": permissionGateCommand(fmt.Sprintf("%s/permission/cc-prehook/%s", base, escapePathSegment(sess.SessionID))),
					"timeout": permissionGateHookTimeoutSeconds,
				},
			},
		}
		byEvent["PreToolUse"] = append(byEvent["PreToolUse"], permEntry)
	}

	// User-registered hooks from hook-store. These sit AFTER the
	// permission hook in PreToolUse so they only run when the call
	// was allowed.
	if s.hookStore != nil {
		applicable, err := s.collectApplicableHooks(sess, msg.HarnessClaudeCode)
		if err != nil {
			return "", err
		}
		for _, h := range applicable {
			cmd := hookCurlCommand(
				fmt.Sprintf("%s/hooks/exec/%s", base, escapePathSegment(h.ID)),
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

	settings := map[string]any{}
	if len(byEvent) > 0 {
		settings["hooks"] = byEvent
	}
	// Read deny rules for the paths the session's bundle denies. Claude Code
	// enforces them under every permission mode, bypassPermissions included.
	paths, err := sessionDeniedReadPaths(sess)
	if err != nil {
		return "", err
	}
	if len(paths) > 0 {
		rules, err := claudeCodeReadDenyRules(paths)
		if err != nil {
			return "", err
		}
		settings["permissions"] = map[string]any{"deny": rules}
	}
	if len(settings) == 0 {
		return "", nil
	}

	out, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// collectApplicableHooks runs three scoped queries and concatenates them.
// Instance and session filters are skipped when the session has no id for
// that scope, which avoids matching hooks registered with scope_id="".
//
// The harness parameter scopes the hook-store query: a hook registered for
// claude_code does not bleed into codex sessions and vice versa.
func (s *Server) collectApplicableHooks(sess *store.Session, harness msg.Harness) ([]msg.Hook, error) {
	filters := []hookstore.ListFilter{
		{Harness: harness, ScopeKind: msg.HookScopeGlobal, EnabledSet: true, Enabled: true},
	}
	if sess.InstanceID != "" {
		filters = append(filters, hookstore.ListFilter{
			Harness: harness, ScopeKind: msg.HookScopeInstance,
			ScopeID: sess.InstanceID, EnabledSet: true, Enabled: true,
		})
	}
	if sess.SessionID != "" {
		filters = append(filters, hookstore.ListFilter{
			Harness: harness, ScopeKind: msg.HookScopeSession,
			ScopeID: sess.SessionID, EnabledSet: true, Enabled: true,
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

// buildCodexHookConfig synthesizes a codex-shaped hooks tree:
//
//	{
//	  "PreToolUse": [
//	    { "matcher": ".*", "hooks": [{"type":"command","command":"curl …/permission/codex-prehook/<id>","timeout":86400}] }
//	  ]
//	}
//
// The codex bridge translates each top-level event key to a `-c
// hooks.<EventName>=<inline-toml>` argument on the `codex app-server`
// spawn. Codex doesn't accept type:"http" hooks natively (per the docs),
// so the bridge permission gate is wrapped as a `type: "command"` curl
// invocation that POSTs the stdin payload to the same prehook handler
// CC uses (the payload + response shapes are byte-identical).
//
// Returns nil when there's nothing to inject so the caller can skip the
// HarnessConfig write entirely.
func (s *Server) buildCodexHookConfig(sess *store.Session) (map[string][]any, error) {
	return s.buildPrehookCommandHookTree(sess, msg.HarnessCodex, "codex-prehook")
}

// buildPrehookCommandHookTree builds the {event: [{matcher, hooks:[{type:
// "command", command, timeout}]}]} tree codex and devin both read: the
// permission gate at /permission/<gateRoute>/<session> first, then the
// hook-store hooks scoped to harness.
func (s *Server) buildPrehookCommandHookTree(sess *store.Session, harness msg.Harness, gateRoute string) (map[string][]any, error) {
	base := publicBaseURL(s.cfg.ListenAddr)
	byEvent := make(map[string][]any, 4)

	// Permission gate. matcher ".*" → fires on every Bash / apply_patch /
	// MCP tool call codex routes through PreToolUse (per the codex docs,
	// not every tool fires the event — but for the gate we want to match
	// everything that does).
	if sess.SessionID != "" {
		permEntry := map[string]any{
			"matcher": ".*",
			"hooks": []map[string]any{
				{
					"type":    "command",
					"command": permissionGateCommand(fmt.Sprintf("%s/permission/%s/%s", base, gateRoute, escapePathSegment(sess.SessionID))),
					"timeout": permissionGateHookTimeoutSeconds,
				},
			},
		}
		byEvent["PreToolUse"] = append(byEvent["PreToolUse"], permEntry)
	}

	// User-registered hooks from hook-store scoped to codex. These run
	// AFTER the permission gate so denied calls never trigger them.
	if s.hookStore != nil {
		applicable, err := s.collectApplicableHooks(sess, harness)
		if err != nil {
			return nil, err
		}
		for _, h := range applicable {
			cmd := hookCurlCommand(
				fmt.Sprintf("%s/hooks/exec/%s", base, escapePathSegment(h.ID)),
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
		return nil, nil
	}
	return byEvent, nil
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

// escapePathSegment percent-encodes a value so it occupies exactly one path
// segment of the URL it is interpolated into. Without it a session id
// containing "/", "?" or "#" re-aims the request at a different endpoint
// entirely, and the id reaching here is caller-chosen: server/sessions.go
// takes req.SessionID off the POST body verbatim and checks it only for
// collision.
func escapePathSegment(segment string) string {
	return url.PathEscape(segment)
}

// shellSingleQuote wraps a value so a POSIX shell reads it as exactly one
// word. Every character inside single quotes is literal except the closing
// quote itself, which is written as: close, backslash-escaped quote, reopen.
//
// ⚠️ This is NOT redundant with escapePathSegment, and the difference is the
// whole reason this function exists. url.PathEscape encodes for a URL path
// segment, and "&" and "$" are legal sub-delims there (RFC 3986 §3.3), so it
// passes both through unchanged. In a URL that is correct and inert. In a
// shell command line "&" separates two commands, which turns a caller-chosen
// session id into a caller-chosen command. Measured before this was added:
// a session id of `a&touch /tmp/x` created /tmp/x when the generated hook ran.
func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// permissionGateHookTimeoutSeconds is how long a harness waits for the
// permission gate's hook: a day, so a person has time to answer a parked
// prompt, and far below the 24-day Node timer limit that broke the old MCP gate.
const permissionGateHookTimeoutSeconds = 86400

// permissionGateCurlMaxTimeSeconds ends the gate's curl five minutes before the
// harness would kill the hook. A hook the harness kills for running too long
// is a hook that failed in the harness's own way, which lets the tool run; a
// curl that gives up first takes the blocking exit below instead.
const permissionGateCurlMaxTimeSeconds = permissionGateHookTimeoutSeconds - 300

// permissionGateUnreachableMessage is what the agent reads when the gate's
// call fails. It holds no single quote, because it is written into the
// command inside one.
const permissionGateUnreachableMessage = "The bridge permission check could not be reached, so this tool call is blocked. Try again once the bridge answers."

// permissionGateCommand is the command every harness runs as its permission
// gate: post the tool call to targetURL and print the gate's answer, and when
// the post fails for any reason (no connection, a non-2xx answer, the time
// limit) exit 2. Exit 2 is the one hook exit that blocks the tool call in
// Claude Code, codex and devin; any other failing exit is logged and the tool
// runs. With the bridge's own permission prompts off (bypassPermissions), a
// gate that failed any other way let every tool call through unchecked.
//
// When the post succeeds the command's output and exit are curl's own, so
// a harness that reaches the gate sees exactly what it saw before.
func permissionGateCommand(targetURL string) string {
	return hookCurlCommand(targetURL, "--max-time", strconv.Itoa(permissionGateCurlMaxTimeSeconds)) +
		" || { printf '%s\\n' " + shellSingleQuote(permissionGateUnreachableMessage) + " >&2; exit 2; }"
}

// hookCurlCommand renders the curl invocation that a type:"command" hook
// runs. Both harnesses hand the string to a shell, so targetURL is quoted as
// one word here rather than at each call site -- the four sites that build
// one of these are otherwise four copies of the same string.
func hookCurlCommand(targetURL string, extraFlags ...string) string {
	flags := "-sfS"
	if len(extraFlags) > 0 {
		flags += " " + strings.Join(extraFlags, " ")
	}
	return fmt.Sprintf(
		"curl %s -X POST -H 'Content-Type: application/json' --data-binary @- %s",
		flags, shellSingleQuote(targetURL),
	)
}
