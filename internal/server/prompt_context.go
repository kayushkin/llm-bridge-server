package server

import (
	"encoding/json"
	"log"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// harnessesThatReadTheirOwnPromptFile are the harnesses that load a prompt
// file from the working directory and its ancestors without being told to,
// with the file they load. Everything else gets the prompt injected.
//
// This seeds agent-store's prompt_harness_deliveries and nothing reads it
// after that: the row is the truth, and the Files page edits it. jig is here
// because it launches the `claude` binary.
var harnessesThatReadTheirOwnPromptFile = map[msg.Harness]string{
	msg.HarnessClaudeCode: "CLAUDE.md",
	msg.HarnessJig:        "CLAUDE.md",
}

// syncPromptHarnessDeliveries makes sure every harness the bridge knows has a
// delivery row, without touching a row that already exists. agent-store
// refuses to resolve a prompt for a harness with no row, so a harness added
// to msg.AllHarnesses gets one here on the next start.
func (s *Server) syncPromptHarnessDeliveries() {
	if s.agentStore == nil {
		return
	}
	for _, harness := range msg.AllHarnesses {
		delivery := agentstore.PromptHarnessDelivery{Harness: string(harness), Delivery: agentstore.PromptDeliveryInject, Note: "recorded at bridge start: the bridge injects the prompt"}
		if nativeFile, ok := harnessesThatReadTheirOwnPromptFile[harness]; ok {
			delivery.Delivery = agentstore.PromptDeliveryNativeFile
			delivery.NativeRelativePath = nativeFile
			delivery.Note = "recorded at bridge start: this harness reads " + nativeFile + " itself"
		}
		if err := s.agentStore.RecordPromptHarnessDeliveryIfAbsent(delivery); err != nil {
			log.Printf("[context] record prompt delivery for %s: %v", harness, err)
		}
	}
}

// injectPromptContext puts the host prompt, plus the prompt of every project
// the session's working directory sits in, into the session's HarnessConfig as
// system_prompt. agent-store decides what that is, per harness: a harness that
// reads a rendered file itself is given only the collections that file does
// not already carry, so no harness gets the same text twice and none gets
// less than the others.
//
// If the caller already set a system_prompt on HarnessConfig, that wins —
// explicit overrides are respected. Mutation is in-memory only (not
// persisted) so an edit to the prompt takes effect on the next spawn.
func (s *Server) injectPromptContext(sess *store.Session) {
	if sess == nil || s.agentStore == nil {
		return
	}

	res, err := s.agentStore.ResolveContext(string(sess.Harness), workDirForSession(sess))
	if err != nil {
		// Loud, and the session still starts: a session with no host prompt
		// is degraded, a session that cannot start is an outage.
		log.Printf("[context] ERROR session %s starts WITHOUT the host prompt: resolve for harness %q: %v", sess.SessionID, sess.Harness, err)
		return
	}
	if res.Content == "" {
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
// here so resume picks up the same project prompt.
func workDirForSession(sess *store.Session) string {
	if sess == nil {
		return ""
	}
	if sess.DisplayName != "" && sess.DisplayName[0] == '/' {
		return sess.DisplayName
	}
	return ""
}
