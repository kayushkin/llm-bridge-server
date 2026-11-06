package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/bundleclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// A harness's built-in tools (Claude Code's Read, Bash…; codex's shell_tool…)
// are registered in tool-store as kind "harness", on by default. Two things
// turn one off for a session, and both are unioned into harness_config's
// disabled_tools at create:
//
//   - its bundle denies it (bundle-store's denied_tools; a deny anywhere in the
//     resolved set wins, and a session's own list cannot give it back), and
//   - tool-store's master switch has it disabled, for every session.
//
// A bundle may also deny reading paths. Only Claude Code enforces that, with
// Read deny rules, and a shell reads around a Read rule, so such a session is
// refused unless every tool tool-store tags runs-commands is off.
//
// A deny list cannot turn off a tool nobody has named yet. A harness tool
// tool-store has never seen is on in the first session that reports it, and is
// registered switched off and unreviewed only once that session has started.
// So a session whose bundle denies anything (a restricted session) is also
// given an allow list: the harness tools tool-store has enabled, less every
// tool disabled above. A tool tool-store does not know, or has not enabled, is
// not on the list, and the harness does not offer it. Only claude_code takes an
// allow list (--tools); a restricted session of any other harness is refused.

const (
	harnessConfigKeyDeniedReadPaths = "denied_read_paths"
	// harnessConfigKeySettings is Claude Code's --settings, which carries the
	// Read deny rules and the hooks; a caller that sets it replaces both.
	harnessConfigKeySettings             = "settings"
	harnessConfigKeyDisabledToolsByLayer = "disabled_tools_by_layer"
	// harnessConfigKeyBuiltInToolAllowList is Claude Code's --tools: the only
	// built-in tools it may offer. The server pins it on a restricted session.
	harnessConfigKeyBuiltInToolAllowList = "tools"
	// harnessConfigKeyBuiltInToolAllowListPinnedFor names the bundle whose
	// restriction pinned the allow list. Harness bridges ignore it; the
	// server reads it to hold a started session to its list.
	harnessConfigKeyBuiltInToolAllowListPinnedFor = "built_in_tool_allow_list_pinned_for_bundle"
	harnessToolKind                               = "harness"
	harnessToolTagRunsCommands                    = "runs-commands"
)

// harnessTool is one tool-store row of kind harness.
type harnessTool struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	Harness         string   `json:"harness"`
	HarnessToolName string   `json:"harness_tool_name"`
	Enabled         bool     `json:"enabled"`
	Tags            []string `json:"tags"`
}

func (t harnessTool) runsCommands() bool {
	for _, tag := range t.Tags {
		if tag == harnessToolTagRunsCommands {
			return true
		}
	}
	return false
}

// errToolStoreUnavailable marks a create refused because tool-store could not
// say which harness tools are on. Without that read no deny or allow list can
// be decided, so the session is not created.
var errToolStoreUnavailable = errors.New("tool-store unavailable")

// harnessTools reads tool-store's harness tools, all of them when harness is
// empty. tool-store owns the list; nothing here restates it.
func (s *Server) harnessTools(ctx context.Context, harness msg.Harness) ([]harnessTool, error) {
	if s.cfg == nil || s.cfg.ToolStoreURL == "" {
		return nil, fmt.Errorf("this server has no tool-store (LLMBRIDGE_TOOL_STORE_URL) to read harness tools from")
	}
	query := url.Values{"kind": {harnessToolKind}}
	if harness != "" {
		query.Set("harness", string(harness))
	}
	requestURL := s.cfg.ToolStoreURL + "/tools?" + query.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET %s: %w", requestURL, err)
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		return nil, fmt.Errorf("%w: GET %s: %v", errToolStoreUnavailable, requestURL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: read the response to GET %s: %v", errToolStoreUnavailable, requestURL, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: GET %s returned %d: %s", errToolStoreUnavailable, requestURL, response.StatusCode, strings.TrimSpace(string(body)))
	}
	var tools []harnessTool
	if err := json.Unmarshal(body, &tools); err != nil {
		return nil, fmt.Errorf("%w: GET %s returned unparseable JSON: %v", errToolStoreUnavailable, requestURL, err)
	}
	for _, t := range tools {
		if t.HarnessToolName == "" || t.Harness == "" {
			return nil, fmt.Errorf("tool-store harness tool %d (%s) has no harness or harness_tool_name", t.ID, t.Name)
		}
	}
	return tools, nil
}

// harnessToolDenials is what tool-store and the bundle take away from a
// session of this harness, as the harness's own tool names.
type harnessToolDenials struct {
	Bundle              []string
	ToolStore           []string
	CommandRunningTools []string
	// EnabledInToolStore is this harness's tools that tool-store has
	// switched on, in tool-store's order: the most a restricted session's
	// allow list can hold.
	EnabledInToolStore []string
	Notes              []string
}

// decideHarnessToolDenials reads tool-store's harness tools for sess's harness
// and maps the bundle's denied ids onto them. A denied id that is not a
// harness tool was an MCP tool, which bundle-store already left out of the
// offer; a harness tool of another harness does not apply to this session.
func (s *Server) decideHarnessToolDenials(ctx context.Context, harness msg.Harness, bundle *bundleclient.Resolution) (*harnessToolDenials, error) {
	if s.cfg == nil || s.cfg.ToolStoreURL == "" {
		if bundle != nil && (len(bundle.DeniedTools) > 0 || len(bundle.DeniedReadPaths) > 0) {
			return nil, fmt.Errorf("the session's bundle denies tools or paths, and this server has no tool-store (LLMBRIDGE_TOOL_STORE_URL) to name them with")
		}
		return &harnessToolDenials{Notes: []string{"this server has no tool-store, so tool-store's master switch was not read"}}, nil
	}
	all, err := s.harnessTools(ctx, "")
	if err != nil {
		return nil, err
	}
	denials := &harnessToolDenials{}
	byID := make(map[int64]harnessTool, len(all))
	for _, t := range all {
		byID[t.ID] = t
		if t.Harness != string(harness) {
			continue
		}
		if t.Enabled {
			denials.EnabledInToolStore = append(denials.EnabledInToolStore, t.HarnessToolName)
		} else {
			denials.ToolStore = append(denials.ToolStore, t.HarnessToolName)
		}
		if t.runsCommands() {
			denials.CommandRunningTools = append(denials.CommandRunningTools, t.HarnessToolName)
		}
	}
	if bundle != nil {
		for _, denied := range bundle.DeniedTools {
			t, isHarnessTool := byID[denied.ID]
			switch {
			case !isHarnessTool:
				continue
			case t.Harness != string(harness):
				denials.Notes = append(denials.Notes, fmt.Sprintf("the bundle denies %s, a %s tool, which does not apply to a %s session", t.Name, t.Harness, harness))
			default:
				denials.Bundle = append(denials.Bundle, t.HarnessToolName)
			}
		}
	}
	return denials, nil
}

// harnessToolIDs is the ids of every tool-store harness tool, which a bundle
// may include but which tool-store cannot provision: the harness has them.
func (s *Server) harnessToolIDs(ctx context.Context) (map[int64]string, error) {
	tools, err := s.harnessTools(ctx, "")
	if err != nil {
		return nil, err
	}
	ids := make(map[int64]string, len(tools))
	for _, t := range tools {
		ids[t.ID] = t.Name
	}
	return ids, nil
}

// unionInOrder appends each list's names not already seen, first-seen order.
func unionInOrder(lists ...[]string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, list := range lists {
		for _, name := range list {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// bundleRestrictsSession reports whether a session's bundle denies any tool or
// read path. Such a session gets an allow list of built-in tools, not just a
// deny list, so a tool nobody has reviewed is off in it from the first session.
func bundleRestrictsSession(bundle *bundleclient.Resolution) bool {
	return bundle != nil && (len(bundle.DeniedTools) > 0 || len(bundle.DeniedReadPaths) > 0)
}

// errBuiltInToolAllowListUnenforceable marks a restricted session refused
// because the session as asked for could be offered a built-in tool that is not
// on its allow list.
var errBuiltInToolAllowListUnenforceable = errors.New("built-in tool allow list cannot be enforced")

// restrictedSessionBuiltInToolAllowList is the built-in tools a restricted
// session may be offered: the harness tools tool-store has enabled, less every
// disabled one. It is never nil; an empty list offers no built-in tool. It
// refuses a harness that takes no allow list, and a caller's own tools, which
// would replace the list.
func restrictedSessionBuiltInToolAllowList(harness msg.Harness, enabledInToolStore, disabled []string, callerSetTools bool) ([]string, error) {
	if harness != msg.HarnessClaudeCode {
		return nil, fmt.Errorf("%w: the session's bundle restricts its tools, and only claude_code can be held to an allow list; a %s session would be offered a tool tool-store has not reviewed", errBuiltInToolAllowListUnenforceable, harness)
	}
	if callerSetTools {
		return nil, fmt.Errorf("%w: the session's bundle restricts its tools, and harness_config.tools would replace the allow list tool-store decides; leave tools out", errBuiltInToolAllowListUnenforceable)
	}
	off := map[string]bool{}
	for _, name := range disabled {
		off[name] = true
	}
	allowed := []string{}
	for _, name := range enabledInToolStore {
		if !off[name] {
			allowed = append(allowed, name)
		}
	}
	return allowed, nil
}

// builtInToolAllowListPinnedOn reads a restricted session's allow list. It
// returns nil when the session has none: no bundle restricted it.
func builtInToolAllowListPinnedOn(cfg map[string]json.RawMessage) ([]string, error) {
	if _, pinned := cfg[harnessConfigKeyBuiltInToolAllowListPinnedFor]; !pinned {
		return nil, nil
	}
	raw, ok := cfg[harnessConfigKeyBuiltInToolAllowList]
	if !ok {
		return nil, fmt.Errorf("harness_config.%s is set but harness_config.tools is missing", harnessConfigKeyBuiltInToolAllowListPinnedFor)
	}
	allowed := []string{}
	if err := json.Unmarshal(raw, &allowed); err != nil {
		return nil, fmt.Errorf("harness_config.tools is not a string array: %w", err)
	}
	return allowed, nil
}

// errDeniedReadPathsUnenforceable marks a session refused because its
// bundle denies reading paths that the session as asked for could read anyway.
var errDeniedReadPathsUnenforceable = errors.New("denied read paths cannot be enforced")

// checkDeniedReadPathsAreEnforceable refuses a session whose bundle denies
// reading paths that its harness cannot keep it from reading: a harness other
// than claude_code, a command-running tool left on, or settings the caller set
// itself, which would replace the settings that carry the Read deny rules.
func checkDeniedReadPathsAreEnforceable(harness msg.Harness, paths, disabled, commandRunning []string, callerSetSettings bool) error {
	if len(paths) == 0 {
		return nil
	}
	if harness != msg.HarnessClaudeCode {
		return fmt.Errorf("%w: the session's bundle denies reading %v, which only claude_code enforces; a %s session reads files through its shell", errDeniedReadPathsUnenforceable, paths, harness)
	}
	if callerSetSettings {
		return fmt.Errorf("%w: the session's bundle denies reading %v, and harness_config.settings would replace the settings that carry those Read deny rules; leave settings out", errDeniedReadPathsUnenforceable, paths)
	}
	off := map[string]bool{}
	for _, name := range disabled {
		off[name] = true
	}
	var stillOn []string
	for _, name := range commandRunning {
		if !off[name] {
			stillOn = append(stillOn, name)
		}
	}
	if len(stillOn) > 0 {
		return fmt.Errorf("%w: the session's bundle denies reading %v but leaves %v on, and a command reads around a Read deny rule; deny them in the bundle", errDeniedReadPathsUnenforceable, paths, stillOn)
	}
	return nil
}

// claudeCodeReadDenyRules renders denied paths as Claude Code permission
// rules. Claude Code reads "/x" as relative to the settings file and matches
// an absolute path only as "//x" (measured on 2.1.280: "Read(/tmp/f)" let the
// read through, "Read(//tmp/f)" refused it, under bypassPermissions too). A
// Read rule also hides the path from Glob and Grep.
func claudeCodeReadDenyRules(paths []string) ([]string, error) {
	rules := make([]string, 0, len(paths))
	for _, p := range paths {
		switch {
		case strings.HasPrefix(p, "~/"):
			rules = append(rules, "Read("+p+")")
		case strings.HasPrefix(p, "/"):
			rules = append(rules, "Read(/"+p+")")
		default:
			return nil, fmt.Errorf("denied read path %q is neither absolute nor under ~/", p)
		}
	}
	return rules, nil
}

// deniedReadPathsPinnedOn reads harness_config.denied_read_paths.
func deniedReadPathsPinnedOn(cfg map[string]json.RawMessage) ([]string, error) {
	raw, ok := cfg[harnessConfigKeyDeniedReadPaths]
	if !ok {
		return nil, nil
	}
	var paths []string
	if err := json.Unmarshal(raw, &paths); err != nil {
		return nil, fmt.Errorf("harness_config.denied_read_paths is not a string array: %w", err)
	}
	return paths, nil
}

// sessionDeniedReadPaths is deniedReadPathsPinnedOn for a stored session.
func sessionDeniedReadPaths(sess *store.Session) ([]string, error) {
	cfg, err := parseHarnessConfig(sess)
	if err != nil {
		return nil, err
	}
	return deniedReadPathsPinnedOn(cfg)
}
