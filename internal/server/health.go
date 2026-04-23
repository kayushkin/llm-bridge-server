package server

import (
	"net/http"

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

// harnessMeta holds display metadata for each harness type.
type harnessMeta struct {
	Label string
	Emoji string
	Image string // filename in images/harnesses/, empty if none
}

var harnessMetadata = map[msg.Harness]harnessMeta{
	msg.HarnessClaudeCode: {Label: "Claude Code", Emoji: "💻", Image: "claude_code.png"},
	msg.HarnessCodex:      {Label: "Codex", Emoji: "📖", Image: "codex.png"},
	msg.HarnessOpenClaw:   {Label: "OpenClaw", Emoji: "🦀"},
	msg.HarnessInber:      {Label: "Inber", Emoji: "🌿"},
	msg.HarnessHermes:     {Label: "Hermes", Emoji: "📨"},
	msg.HarnessAider:      {Label: "Aider", Emoji: "🛠️", Image: "aider.png"},
	msg.HarnessGoose:      {Label: "Goose", Emoji: "🪿", Image: "goose.png"},
	msg.HarnessAutohand:   {Label: "Autohand", Emoji: "🤖"},
	msg.HarnessJig:        {Label: "Jig", Emoji: "🧩"},
	msg.HarnessDexto:      {Label: "Dexto", Emoji: "🎯"},
	msg.HarnessCommander:  {Label: "Commander", Emoji: "🎖️"},
	msg.HarnessNanoClaw:   {Label: "NanoClaw", Emoji: "🔬"},
	msg.HarnessCline:      {Label: "Cline", Emoji: "📝", Image: "cline.png"},
	msg.HarnessRooCode:    {Label: "Roo Code", Emoji: "🦘", Image: "roo_code.svg"},
	msg.HarnessKiloCode:   {Label: "Kilo Code", Emoji: "⚡", Image: "kilo_code.png"},
	msg.HarnessOpenCode:   {Label: "OpenCode", Emoji: "🔓"},
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
		}
		statuses = append(statuses, HarnessStatus{
			Name:               string(h),
			Label:              meta.Label,
			Emoji:              meta.Emoji,
			Image:              imageURL,
			Available:          available,
			Binary:             path,
			Capabilities:       caps,
			HookEvents:         harnessHookEvents[h],
			SupportedProviders: harnessSupportedProviders[h],
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
