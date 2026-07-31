package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge/msg"
)

// Response types are canonical — defined in llm-bridge/msg/server.go.
// DO NOT define new request/response types here. Add them to msg/ instead,
// then run generate-ts.sh so the TypeScript frontend stays in sync.
type (
	HealthResponse = msg.HealthResponse
	HarnessStatus  = msg.HarnessInfo
	SessionCounts  = msg.SessionCounts
)

// harnessMeta holds display metadata for each harness type. Tint is the
// canonical sRGB hex UIs key chrome to (header washes, chips). Add a brand
// color when the harness has one; otherwise leave empty and the UI falls
// through to its theme accent.
type harnessMeta struct {
	Label string
	Emoji string
	Image string // filename in images/harnesses/, empty if none
	Tint  string // sRGB hex like "#d97757", empty if none
}

var harnessMetadata = map[msg.Harness]harnessMeta{
	msg.HarnessClaudeCode: {Label: "Claude Code", Emoji: "💻", Image: "claude_code.png", Tint: "#d97757"},
	msg.HarnessCodex:      {Label: "Codex", Emoji: "📖", Image: "codex.svg", Tint: "#10a37f"},
	msg.HarnessOpenClaw:   {Label: "OpenClaw", Emoji: "🦀", Tint: "#dc2626"},
	msg.HarnessInber:      {Label: "Inber", Emoji: "🌿", Tint: "#22c55e"},
	msg.HarnessHermes:     {Label: "Hermes", Emoji: "📨", Tint: "#eab308"},
	msg.HarnessAider:      {Label: "Aider", Emoji: "🛠️", Image: "aider.png", Tint: "#f97316"},
	msg.HarnessGoose:      {Label: "Goose", Emoji: "🪿", Image: "goose.png", Tint: "#84cc16"},
	msg.HarnessAutohand:   {Label: "Autohand", Emoji: "🤖", Tint: "#94a3b8"},
	msg.HarnessJig:        {Label: "Jig", Emoji: "🧩", Tint: "#a855f7"},
	msg.HarnessDexto:      {Label: "Dexto", Emoji: "🎯", Tint: "#ec4899"},
	msg.HarnessCommander:  {Label: "Commander", Emoji: "🎖️", Tint: "#64748b"},
	msg.HarnessNanoClaw:   {Label: "NanoClaw", Emoji: "🔬", Tint: "#06b6d4"},
	msg.HarnessCline:      {Label: "Cline", Emoji: "📝", Image: "cline.png", Tint: "#3b82f6"},
	msg.HarnessRooCode:    {Label: "Roo Code", Emoji: "🦘", Image: "roo_code.svg", Tint: "#fb7185"},
	msg.HarnessKiloCode:   {Label: "Kilo Code", Emoji: "⚡", Image: "kilo_code.png", Tint: "#f59e0b"},
	msg.HarnessOpenCode:   {Label: "OpenCode", Emoji: "🔓", Image: "opencode.svg", Tint: "#8b5cf6"},
	msg.HarnessForgecode:  {Label: "ForgeCode", Emoji: "🔥", Image: "forgecode.png", Tint: "#ef4444"},
	msg.HarnessGemini:     {Label: "Gemini", Emoji: "✨", Tint: "#4285f4"},
	msg.HarnessMock:       {Label: "Mock", Emoji: "🧪", Tint: "#6b7280"},
}

// harnessSupportedProviders defines which model providers each harness accepts.
// nil means all providers are valid (framework-managed or multi-provider).
var harnessSupportedProviders = map[msg.Harness][]string{
	msg.HarnessClaudeCode: {"anthropic"},
	msg.HarnessCodex:      {"openai"},
	msg.HarnessJig:        {"anthropic"},
	msg.HarnessAutohand:   {"anthropic"},
}

// harnessHookEvents lists the hook lifecycle events each harness can register
// handlers for via the bridge. Claude Code has the full lifecycle because its
// native hook engine runs in-process and supports deny/modify. Harnesses that
// only emit observation-style lifecycle notifications (e.g. Codex) or run
// agents remotely without any local hook point are absent here.
var harnessHookEvents = map[msg.Harness][]string{
	msg.HarnessClaudeCode: {
		"PreToolUse",
		"PostToolUse",
		"UserPromptSubmit",
		"Notification",
		"Stop",
		"SubagentStop",
		"PreCompact",
		"SessionStart",
		"SessionEnd",
	},
}

// harnessSupportsPTY reports whether each harness can run inside a
// pseudoterminal (pty session mode). Mirrors the bridge.PTYCapableHarness
// optional interface declared in llm-bridge — actual subprocess plumbing
// lands per-harness in later children of the pty-mode roadmap. Until a
// harness flips its entry here AND implements SupportsPTY() on its bridge,
// the server keeps reporting false.
//
// claude_code lit up in pty-mode child 2 (this commit): the harness binary
// detects LLMBRIDGE_PTY_MODE in env at startup and execs into the upstream
// `claude` CLI so the pty fd is wired straight through to its TUI.
//
// codex lit up in pty-mode child 5: the codex harness binary follows the
// same env-var hand-off and execs into the upstream `codex` CLI's
// interactive mode. The existing AppServer/WebSocket path coexists for
// events-mode sessions; pty mode opts out at spawn time.
var harnessSupportsPTY = map[msg.Harness]bool{
	msg.HarnessClaudeCode: true,
	msg.HarnessCodex:      true,
}

// harnessSupportsDisableNetwork reports whether the harness can enforce
// "no outbound network" at the sandbox layer. The bridge surfaces this
// as a checkbox alongside the permission-mode dropdown; greyed out for
// harnesses without sandbox-level network gating.
//
// codex maps to sandbox_workspace_write.network_access=false (passed as
// a `-c` override on app-server spawn). claude_code has no sandbox-level
// network knob today; a future hook-store rule could implement the same
// gate, at which point we flip its entry to true.
var harnessSupportsDisableNetwork = map[msg.Harness]bool{
	msg.HarnessCodex: true,
}

// harnessSupportedPermissionModes lists the permission modes each harness
// knows how to express in its own start params. Every harness implicitly
// supports "ask" and "bypass" via the universal prehook gate; the
// restrictive modes (block_all / plan / read / ask_all) also work for
// every harness since they're enforced bridge-side. Auto is harness-aware
// (the harness translates to its native vocab). Custom is opt-in per
// harness since it surfaces raw harness-specific knobs.
var harnessSupportedPermissionModes = map[msg.Harness][]string{
	msg.HarnessClaudeCode: {
		msg.PermissionModeBlockAll,
		msg.PermissionModePlan,
		msg.PermissionModeRead,
		msg.PermissionModeAskAll,
		msg.PermissionModeAsk,
		msg.PermissionModeAuto,
		msg.PermissionModeBypass,
	},
	// Codex supports all modes including Custom, which exposes the raw
	// approval_policy + sandbox_mode pair through bridge-ui's Custom panel.
	msg.HarnessCodex: {
		msg.PermissionModeBlockAll,
		msg.PermissionModePlan,
		msg.PermissionModeRead,
		msg.PermissionModeAskAll,
		msg.PermissionModeAsk,
		msg.PermissionModeAuto,
		msg.PermissionModeBypass,
		msg.PermissionModeCustom,
	},
}

// supportedPermissionModesFor returns the modes for a harness, defaulting
// to the prehook-enforced subset for any harness without an explicit entry.
// The restrictive modes (block_all / plan / read / ask_all) and the always-
// available rule modes (ask / bypass) all work via the universal prehook
// gate without any per-harness translation. Auto and Custom require
// harness opt-in (auto needs a translation; custom needs a UI panel).
func supportedPermissionModesFor(h msg.Harness) []string {
	if modes, ok := harnessSupportedPermissionModes[h]; ok {
		return modes
	}
	return []string{
		msg.PermissionModeBlockAll,
		msg.PermissionModePlan,
		msg.PermissionModeRead,
		msg.PermissionModeAskAll,
		msg.PermissionModeAsk,
		msg.PermissionModeBypass,
	}
}

// harnessCapabilities defines what features each harness supports. The chat
// UI reads it over GET /harnesses/{name}/capabilities and shows or hides its
// Compact, Fork, Model, Effort, Tools and System-prompt controls accordingly
// (bridge-ui Workspace.tsx), so a wrong entry here is directly visible to the
// user in both directions: a granted capability the harness drops is a button
// that silently does nothing, and a withheld one is a working feature the user
// cannot reach.
//
// It is hand-maintained, which is the standing hazard — the harnesses live in
// their own repos and nothing here re-reads them. The "compact" column was
// audited against every harness's dispatcher on 2026-07-31 (noteboard todo
// f8035505 tracks deriving this table instead of writing it down; 8fbaf27d
// tracks the two harnesses that answer compact without compacting):
//
//   - real: claude_code (/compact), codex (HandleCompact), inber (POST
//     /sessions/{id}/compact), kilo_code (server.Summarize), jig (/compact,
//     llm-bridge-jig 9ac0b0e)
//   - honest refusal, correctly withheld: hermes (UNSUPPORTED error), cline,
//     aider, forgecode (no-op acks that say so)
//   - claims a delegation that never happens: openclaw, nanoclaw. Both emit
//     "compaction delegated to X" and write nothing. openclaw is withheld
//     here rather than left showing a button that does nothing; nanoclaw was
//     already withheld. Fixing either needs its own protocol work — nanoclaw's
//     container contract does not cover compaction and no image implements it.
//
// The remaining six columns were audited the same way on 2026-07-31, against
// each harness's origin/main (not whatever branch its checkout happened to
// have out). What the audit had to establish first is that only two things
// reach a running harness from this server: the bare "compact" / "compact:
// <summary>" method, and "config:<json>" carrying msg.ConfigSessionRequest —
// its four fields model / effort / disabled_tools / max_budget are the entire
// mid-session vocabulary. Manager.SendJSONRPC has no caller here, so every
// harness's set_model, fork and interrupt JSON-RPC method is unreachable from
// this gateway however well it is written, and POST /sessions/{id}/interrupt
// is SIGINT (process.go Interrupt), not a request. "fork" is start-params
// only: buildStartParams sets params.Fork from the parent's harness UUID.
//
// The grading rule, so a future entry is decidable rather than a matter of
// taste: a capability is granted when the control it gates changes something
// for a session that is already running. Storing a value and applying it at
// the next spawn counts (jig) — the user's choice takes effect. Refusing it
// and keeping the old value does not (claude_code effort / max_budget /
// disabled_tools, which answer "spawn-time only, unchanged" and return an
// error), and neither does parsing a field and never reading it (mock's fork,
// nanoclaw's, aider's, forgecode's).
//
// Per column, with the implementation each claim names:
//
//   - model: claude_code (handleSessionConfig -> handleSetModel, live),
//     codex (HandleConfig -> cfg.CodexModel, next turn), inber (POST
//     /sessions/{id}/config -> Engine.SetModel), cline (-m on the next
//     one-shot turn), jig (patches the profile, next spawn). Withheld from
//     everything else, and the reasons differ: openclaw hardcodes the literal
//     model "openclaw", kilo_code and nanoclaw plumb no model at all,
//     forgecode's own README says the -m it passes is inert, aider fixes the
//     model at start, hermes and the six scaffolds have no config: branch.
//   - effort: codex, inber, jig. claude_code takes --effort at spawn and says
//     so; cline refuses it by name.
//   - tools: this one column gates two different controls, and no harness
//     earns both halves — the chat pane's Tools button reads SessionInfo.Tools,
//     the settings pane's grid writes disabled_tools. Granted where either
//     half is real, and which half differs: claude_code and mock report a tool
//     list and refuse to disable; inber and jig disable for real and report
//     nothing. Splitting the column is noteboard todo f8035505's sibling.
//   - budget: jig only. inber refuses it outright and explains why (the bridge
//     sends dollars, inber's config takes input tokens); claude_code takes
//     --max-budget-usd at spawn; nobody else parses the field.
//   - fork: claude_code and jig (--resume <uuid> --fork-session), codex
//     (thread/fork), inber (POST /sessions/{id}/fork, deep-copies messages),
//     kilo_code (POST /session/{id}/fork). Withheld from hermes, which fails
//     the start outright with FORK_UNSUPPORTED, and from cline, whose "fork"
//     passes the parent id to the same -T flag resume uses, so both sessions
//     write into one task and nothing branches.
//   - system_prompt: claude_code and mock. It is a reporting capability — the
//     button renders SessionInfo.SystemPrompt — and codex, hermes, inber and
//     jig emit no SessionInfo event at all, so the button had nothing to show.
//
// "interrupt" is gone: it was on hermes alone, it named a JSON-RPC method this
// server never sends, and no UI reads the column.
var harnessCapabilities = map[msg.Harness][]string{
	msg.HarnessClaudeCode: {"compact", "fork", "model", "tools", "system_prompt"},
	msg.HarnessCodex:      {"compact", "fork", "model", "effort"},
	msg.HarnessOpenClaw:   {},
	msg.HarnessInber:      {"compact", "fork", "model", "effort", "tools"},
	msg.HarnessHermes:     {},
	msg.HarnessAider:      {},
	msg.HarnessGoose:      {},
	msg.HarnessAutohand:   {},
	msg.HarnessJig:        {"compact", "fork", "model", "effort", "tools", "budget"},
	msg.HarnessDexto:      {},
	msg.HarnessCommander:  {},
	msg.HarnessNanoClaw:   {},
	msg.HarnessCline:      {"model"},
	msg.HarnessRooCode:    {},
	msg.HarnessKiloCode:   {"compact", "fork"},
	msg.HarnessOpenCode:   {},
	msg.HarnessForgecode:  {},
	msg.HarnessGemini:     {},
	msg.HarnessMock:       {"compact", "tools", "system_prompt"},
}

// disabledHarnesses names canonical harnesses (msg.AllHarnesses) that this
// gateway deliberately does not surface, validate, or spawn. The value is the
// reason, so the omission stays an explicit decision rather than an oversight.
//
// Every canonical harness must be either listed here or fully described by the
// tables above; TestHarnessTablesCoverCanonicalSet enforces that, so a harness
// added to llm-bridge cannot silently go missing here again.
var disabledHarnesses = map[msg.Harness]string{
	// The llm-bridge-copilotcli repo is still an un-retargeted clone of
	// llm-bridge-claudecode (its README says status: planning, and its code
	// spawns `claude`), and the llm-bridge-copilotcli binary sitting on PATH is
	// a stale 2026-05-08 build of an abandoned prototype that links the retired
	// aiauth. Enabling this harness would spawn that binary. Whether to
	// re-target, publish privately, or drop the scaffold is an open decision —
	// noteboard todo 2d8b6d10.
	msg.HarnessCopilotCLI: "scaffold not yet re-targeted from claudecode; see noteboard todo 2d8b6d10",
}

// allHarnesses is the set this gateway surfaces, validates and spawns: the
// canonical list minus disabledHarnesses. Derived, never hand-maintained — a
// hand-copied duplicate of msg.AllHarnesses is what let copilot_cli drift out
// of this file unnoticed.
var allHarnesses = func() []msg.Harness {
	enabled := make([]msg.Harness, 0, len(msg.AllHarnesses))
	for _, h := range msg.AllHarnesses {
		if _, off := disabledHarnesses[h]; off {
			continue
		}
		enabled = append(enabled, h)
	}
	return enabled
}()

// isValidHarness checks whether a harness type is in the known set.
func isValidHarness(h msg.Harness) bool {
	for _, known := range allHarnesses {
		if h == known {
			return true
		}
	}
	return false
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	harnesses := s.discoverHarnesses()
	counts := s.sessionCounts()

	status := "ok"
	if counts.Running == 0 && !anyAvailable(harnesses) {
		status = "degraded"
	}

	writeJSON(w, HealthResponse{
		Status:    status,
		Harnesses: harnesses,
		Sessions:  counts,
	})
}

func (s *Server) handleHarnesses(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.discoverHarnesses())
}

// handleHarnessCapabilities returns the capability summary for a single
// harness: features, hook events, supported providers, plus the metadata
// already on HarnessInfo. Kept as a dedicated endpoint so clients wiring the
// hook UI don't have to filter the full /harnesses list themselves.
func (s *Server) handleHarnessCapabilities(w http.ResponseWriter, r *http.Request) {
	name := msg.Harness(r.PathValue("name"))
	if !isValidHarness(name) {
		http.Error(w, "unknown harness", http.StatusNotFound)
		return
	}
	for _, info := range s.discoverHarnesses() {
		if info.Name == string(name) {
			writeJSON(w, info)
			return
		}
	}
	http.Error(w, "unknown harness", http.StatusNotFound)
}

// handleHarnessAgents returns the agents registered for a harness, sourced
// from agent-store filtered by orchestrator id == harness name. Empty array
// when no agents are configured (or agent-store is unavailable) — that's a
// valid state for harnesses without a named-agent concept.
func (s *Server) handleHarnessAgents(w http.ResponseWriter, r *http.Request) {
	name := msg.Harness(r.PathValue("name"))
	if !isValidHarness(name) {
		http.Error(w, "unknown harness", http.StatusNotFound)
		return
	}
	agents := []msg.HarnessAgent{}
	if s.agentStore != nil {
		expanded, err := s.agentStore.ListAgentsExpanded()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range expanded {
			if a.Harness != string(name) || !a.Enabled {
				continue
			}
			id := a.HarnessName
			if id == "" {
				id = a.Slug
			}
			agents = append(agents, msg.HarnessAgent{
				Name:        id,
				DisplayName: a.DisplayName,
				Description: a.Description,
				IsDefault:   a.IsDefault,
			})
		}
	}
	writeJSON(w, agents)
}

func (s *Server) discoverHarnesses() []HarnessStatus {
	var statuses []HarnessStatus
	for _, h := range allHarnesses {
		path, available := harness.Available(h)
		caps := harnessCapabilities[h]
		if caps == nil {
			caps = []string{}
		}
		meta := harnessMetadata[h]
		var imageURL string
		if meta.Image != "" {
			imageURL = "/images/harnesses/" + meta.Image
			if st, err := os.Stat(filepath.Join(s.cfg.ImagesDir, "harnesses", meta.Image)); err == nil {
				imageURL += fmt.Sprintf("?v=%d", st.ModTime().Unix())
			}
		}
		statuses = append(statuses, HarnessStatus{
			Name:                     string(h),
			Label:                    meta.Label,
			Emoji:                    meta.Emoji,
			Image:                    imageURL,
			Tint:                     meta.Tint,
			Available:                available,
			Binary:                   path,
			Capabilities:             caps,
			HookEvents:               harnessHookEvents[h],
			SupportedProviders:       harnessSupportedProviders[h],
			SupportedPermissionModes: supportedPermissionModesFor(h),
			PTY:                      harnessSupportsPTY[h],
			SupportsDisableNetwork:   harnessSupportsDisableNetwork[h],
		})
	}
	return statuses
}

func (s *Server) sessionCounts() SessionCounts {
	var counts SessionCounts

	if sessions, err := s.store.ListSessionsByState(string(msg.SessionRunning)); err == nil {
		counts.Running = len(sessions)
	}
	if sessions, err := s.store.ListSessionsByState(string(msg.SessionIdle)); err == nil {
		counts.Idle = len(sessions)
	}
	if sessions, err := s.store.ListSessionsByState(string(msg.SessionCompleted)); err == nil {
		counts.Completed = len(sessions)
	}

	return counts
}

func anyAvailable(harnesses []HarnessStatus) bool {
	for _, h := range harnesses {
		if h.Available {
			return true
		}
	}
	return false
}
