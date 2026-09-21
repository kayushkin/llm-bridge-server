package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"

	agentstore "github.com/kayushkin/agent-store"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
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

// injectPromptContext puts the host prompt, the prompt of every project the
// session's working directory sits in, and the context sections its card's
// tags select into the session's HarnessConfig. agent-store decides what that
// is, per harness: a harness that reads a rendered file itself is given only
// what that file does not already carry, so no harness gets the same text
// twice and none gets less than the others.
//
// Where it goes depends on the same delivery row. An inject harness has no
// prompt of its own, so the text is its whole system_prompt. A native_file
// harness already has one — Claude Code's own, plus the file it reads — and
// the text is an addition to it, so it goes in append_system_prompt: put in
// system_prompt it would replace Claude Code's default prompt outright.
//
// A caller that set system_prompt chose the whole prompt, and a caller that
// set the key this would write chose that key's text; either wins. Mutation
// is in-memory only (not persisted) so an edit to a section, or to the card's
// tags, takes effect on the next spawn.
func (s *Server) injectPromptContext(ctx context.Context, sess *store.Session, inst *msg.Instance) {
	if sess == nil || s.agentStore == nil {
		return
	}
	workDir, _ := harness.WorkingDirForSession(sess, inst)
	contextTags, err := s.sessionContextTags(ctx, sess)
	if err != nil {
		// Loud, and the session still starts, as below.
		log.Printf("[context] ERROR session %s starts WITHOUT its card's context sections: %v", sess.SessionID, err)
	}

	res, err := s.agentStore.ResolveContext(string(sess.Harness), workDir, contextTags)
	if err != nil {
		// Loud, and the session still starts: a session with no host prompt
		// is degraded, a session that cannot start is an outage.
		log.Printf("[context] ERROR session %s starts WITHOUT the host prompt: resolve for harness %q: %v", sess.SessionID, sess.Harness, err)
		return
	}
	if sess.CardID != "" {
		log.Printf("[context] session %s card %s tags %q in %q: context sections %s", sess.SessionID, sess.CardID, contextTags, workDir, describeMatchedContextSections(res))
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
	key := harnessConfigKeyForInjectedPrompt(res.Delivery)
	if _, ok := cfg["system_prompt"]; ok {
		return
	}
	if _, ok := cfg[key]; ok {
		return
	}

	encoded, err := json.Marshal(res.Content)
	if err != nil {
		log.Printf("[context] encode %s for %s: %v", key, sess.SessionID, err)
		return
	}
	cfg[key] = encoded

	merged, err := json.Marshal(cfg)
	if err != nil {
		log.Printf("[context] re-marshal HarnessConfig for %s: %v", sess.SessionID, err)
		return
	}
	sess.HarnessConfig = merged
}

// harnessConfigKeyForInjectedPrompt is the HarnessConfig key the injected text
// goes in for a delivery: see injectPromptContext.
func harnessConfigKeyForInjectedPrompt(delivery string) string {
	if delivery == agentstore.PromptDeliveryNativeFile {
		return "append_system_prompt"
	}
	return "system_prompt"
}

// sessionContextTags reads the tags of the session's card from kanban-store,
// or nil for a session with no card. It reads them at every spawn rather than
// keeping a copy, so a tag added to the card reaches the next spawn.
func (s *Server) sessionContextTags(ctx context.Context, sess *store.Session) ([]string, error) {
	if sess.CardID == "" {
		return nil, nil
	}
	if s.kanbanClient == nil {
		return nil, fmt.Errorf("session names card %s but this server has no kanban-store to read its tags from (LLMBRIDGE_KANBAN_STORE_URL)", sess.CardID)
	}
	tags, err := s.kanbanClient.CardTags(ctx, sess.CardID)
	if err != nil {
		return nil, fmt.Errorf("read the tags of card %s: %w", sess.CardID, err)
	}
	return tags, nil
}

// describeMatchedContextSections names the context sections a resolution
// injected, by id and title, for a log line.
func describeMatchedContextSections(res *agentstore.ResolvedContext) string {
	var matched []string
	for _, entry := range res.Manifest {
		for _, section := range entry.MatchedSections {
			matched = append(matched, fmt.Sprintf("%d %q", section.SectionID, section.Title))
		}
	}
	if len(matched) == 0 {
		return "none"
	}
	return strings.Join(matched, ", ")
}
