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

// harnessCapabilities defines what features each harness supports.
var harnessCapabilities = map[msg.Harness][]string{
	msg.HarnessClaudeCode: {"compact", "fork", "model", "effort", "tools", "budget", "system_prompt"},
	msg.HarnessCodex:      {"compact", "fork", "model", "effort", "system_prompt"},
	msg.HarnessOpenClaw:   {"compact", "model", "effort"},
	msg.HarnessInber:      {"compact", "fork", "model", "effort", "tools", "budget"},
	msg.HarnessHermes:     {"model", "fork", "effort", "tools", "system_prompt", "interrupt"},
	msg.HarnessAider:      {"model"},
	msg.HarnessGoose:      {"model"},
	msg.HarnessAutohand:   {"model"},
	msg.HarnessJig:        {"model"},
	msg.HarnessDexto:      {"model"},
	msg.HarnessCommander:  {"model"},
	msg.HarnessNanoClaw:   {"model"},
	msg.HarnessCline:      {"model"},
	msg.HarnessRooCode:    {"model"},
	msg.HarnessKiloCode:   {"model"},
	msg.HarnessOpenCode:   {"model"},
	msg.HarnessForgecode:  {"model"},
}

var allHarnesses = []msg.Harness{
	msg.HarnessClaudeCode,
	msg.HarnessCodex,
	msg.HarnessOpenClaw,
	msg.HarnessInber,
	msg.HarnessHermes,
	msg.HarnessAider,
	msg.HarnessGoose,
	msg.HarnessAutohand,
	msg.HarnessJig,
	msg.HarnessDexto,
	msg.HarnessCommander,
	msg.HarnessNanoClaw,
	msg.HarnessCline,
	msg.HarnessRooCode,
	msg.HarnessKiloCode,
	msg.HarnessOpenCode,
	msg.HarnessForgecode,
}

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
			if a.Orchestrator != string(name) || !a.Enabled {
				continue
			}
			id := a.OrchestratorName
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
			Name:               string(h),
			Label:              meta.Label,
			Emoji:              meta.Emoji,
			Image:              imageURL,
			Tint:               meta.Tint,
			Available:          available,
			Binary:             path,
			Capabilities:       caps,
			HookEvents:         harnessHookEvents[h],
			SupportedProviders: harnessSupportedProviders[h],
			PTY:                harnessSupportsPTY[h],
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
