package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The session brief pass: at the end of each chat turn, one cheap-model call
// reads the brief as it stood, the loose ends still open, what the person said
// and everything the assistant wrote this turn, and returns the brief as the
// turn left it (msg.SessionBrief, msg/session_brief.go).
//
// It rides on the turn-end classifier: same trigger, same model role
// (signal_classifier.model_role, so an empty role switches both off) and the
// same per-harness opt-out. It is a second call rather than more fields on the
// classifier's, because the classifier sees only the final message and its
// verdict moves the session's state; the brief needs the whole turn and moves
// nothing.
//
// Only chats get a brief. An autonomous worker has a kanban card for the same
// purpose, and briefing the fleet's workers would multiply the calls for
// summaries nobody reads.

const (
	// sessionBriefUserMessageMaximumCharacters caps what the person said. A
	// pasted log can run to megabytes; the head says what they asked.
	sessionBriefUserMessageMaximumCharacters = 6000
	// sessionBriefAssistantTextMaximumCharacters caps the assistant's text for
	// the turn. A long turn keeps its tail, which holds the reply the person
	// reads, and its head, which holds what it set out to do.
	sessionBriefAssistantTextMaximumCharacters = 24000
	// sessionBriefMaximumNewLooseEnds bounds what one turn may raise. More
	// than a handful from one turn means the model is listing everything.
	sessionBriefMaximumNewLooseEnds = 5
	// sessionBriefMaximumTokens bounds the model's answer and its thinking.
	sessionBriefMaximumTokens = 4000
)

const sessionBriefSystemPrompt = `You keep a short brief for one chat between a person and an AI coding assistant. After each turn you are shown the brief as it stood, the loose ends still open, what the person said to start this turn, and everything the assistant wrote during it. Return the brief as this turn leaves it.

Fields:

- overall_goal: what the person wants from this chat, in one plain sentence. Keep it unless the person changed what they want; a new step toward the same goal is not a change. On the first turn, write it from the person's message.
- goal_changed: true only when this turn's message changed the overall goal.
- current_step: what the assistant is doing now or waiting on, in one sentence. When it finished and waits for nothing, say what it finished.
- turn_change: one line naming what this turn changed outside the conversation — a commit, a merge, a push, a deploy, files written, a setting or record changed — using the names, ids and commit hashes the assistant gave. Empty when the turn only read, explained or asked.
- settled: the open loose ends this turn dealt with, each by its id with a few words on how. A loose end is settled when the person's message answered it, when the assistant did or dropped the thing on purpose, or when it no longer matters. Never name an id you were not shown.
- new_loose_ends: things the assistant said this turn that want the person's reply and that the end of the turn does not already ask. The bar is high, and an empty list is the common answer.
  - "question": a question to the person asked in the middle of the turn and then left behind as the work went on.
  - "offer": something the assistant offered to do ("I can also…", "want me to…") that the person has not taken up.
  - "dropped_problem": a failure, risk or oddity the assistant noticed and did not follow up or fix.
  Do not raise: the question or choice the final message of the turn asks (the person sees that one already), anything the person's message already dealt with, rhetorical questions, "let me know if…", or progress notes. Do not raise a loose end already open under another id. text is the remark in the assistant's own words where possible; context is the one detail needed to act on it, or empty.

Write plain, short sentences. Use the names and ids the conversation uses.`

func sessionBriefOutputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"overall_goal": map[string]any{"type": "string", "maxLength": 400},
			"goal_changed": map[string]any{"type": "boolean"},
			"current_step": map[string]any{"type": "string", "maxLength": 400},
			"turn_change":  map[string]any{"type": "string", "maxLength": 400},
			"settled": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":  map[string]any{"type": "string"},
						"how": map[string]any{"type": "string", "maxLength": 300},
					},
					"required": []string{"id", "how"},
				},
			},
			"new_loose_ends": map[string]any{
				"type":     "array",
				"maxItems": sessionBriefMaximumNewLooseEnds,
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"kind":    map[string]any{"type": "string", "enum": msg.LooseEndKinds},
						"text":    map[string]any{"type": "string", "maxLength": 400},
						"context": map[string]any{"type": "string", "maxLength": 600},
					},
					"required": []string{"kind", "text"},
				},
			},
		},
		"required": []string{"overall_goal", "goal_changed", "current_step", "turn_change", "settled", "new_loose_ends"},
	}
}

// sessionBriefVerdict is the model's answer.
type sessionBriefVerdict struct {
	OverallGoal string `json:"overall_goal"`
	GoalChanged bool   `json:"goal_changed"`
	CurrentStep string `json:"current_step"`
	TurnChange  string `json:"turn_change"`
	Settled     []struct {
		ID  string `json:"id"`
		How string `json:"how"`
	} `json:"settled"`
	NewLooseEnds []struct {
		Kind    msg.LooseEndKind `json:"kind"`
		Text    string           `json:"text"`
		Context string           `json:"context"`
	} `json:"new_loose_ends"`
}

// sessionBriefLocks holds one lock per session, so two turn-ends of one
// session in quick succession update the brief one after the other rather
// than both from the same old brief.
var sessionBriefLocks sync.Map

// skipSessionBriefReason says why a session's turn gets no brief update, or ""
// when it does. Split out so the rules are testable without a model.
func skipSessionBriefReason(sess *store.Session, state msg.SessionState) string {
	if sess == nil {
		return "no session"
	}
	if sess.Type != msg.SessionTypeInteractive {
		return "not a chat: " + string(sess.Type)
	}
	if sess.Purpose == renamerSourceTag {
		return "renamer session"
	}
	switch state {
	case msg.SessionIdle, msg.SessionAwaitingUser, msg.SessionBackgroundTasksRunning:
		return ""
	default:
		return "turn did not settle: " + string(state)
	}
}

// updateSessionBriefAtTurnEnd runs the brief pass for one finished turn. Like
// the classifier it is best effort: a failure is logged and the brief stays as
// the previous turn left it.
func (s *Server) updateSessionBriefAtTurnEnd(bridgeID string, state msg.SessionState) {
	if s.signalClassifier == nil {
		return
	}
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		return
	}
	if !s.signalClassifier.enabledFor(sess.Harness) {
		return
	}
	if skipSessionBriefReason(sess, state) != "" {
		return
	}

	lock, _ := sessionBriefLocks.LoadOrStore(bridgeID, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	userMessage, assistantTexts, err := s.store.CurrentTurnText(bridgeID)
	if err != nil {
		log.Printf("[session-brief] %s: ERROR read the turn: %v", bridgeID, err)
		return
	}
	if len(assistantTexts) == 0 {
		return
	}
	previous, err := s.store.GetSessionBriefView(bridgeID)
	if err != nil {
		log.Printf("[session-brief] %s: ERROR read the brief: %v", bridgeID, err)
		return
	}
	openLooseEnds := make([]msg.LooseEnd, 0, len(previous.LooseEnds))
	for _, looseEnd := range previous.LooseEnds {
		if looseEnd.State == msg.LooseEndOpen {
			openLooseEnds = append(openLooseEnds, looseEnd)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.signalClassifier.currentTuning().timeout)
	defer cancel()
	verdict, err := s.askForSessionBrief(ctx, previous.Brief, openLooseEnds, userMessage, assistantTexts)
	if err != nil {
		log.Printf("[session-brief] %s: ERROR %v", bridgeID, err)
		return
	}
	update, err := sessionBriefUpdateFromVerdict(bridgeID, verdict, openLooseEnds)
	if err != nil {
		log.Printf("[session-brief] %s: ERROR %v", bridgeID, err)
		return
	}
	if err := s.store.ApplySessionBriefUpdate(update); err != nil {
		log.Printf("[session-brief] %s: ERROR save: %v", bridgeID, err)
		return
	}
	log.Printf("[session-brief] %s: revision %s, %d loose ends settled, %d raised", bridgeID, update.Revision.ID, len(update.Settled), len(update.Raised))
}

// askForSessionBrief makes the one model call.
func (s *Server) askForSessionBrief(ctx context.Context, previous *msg.SessionBrief, openLooseEnds []msg.LooseEnd, userMessage string, assistantTexts []string) (*sessionBriefVerdict, error) {
	if s.signalClassifier.runOneShot == nil {
		return nil, fmt.Errorf("no oneshot runner wired")
	}
	schema, err := json.Marshal(sessionBriefOutputSchema())
	if err != nil {
		return nil, fmt.Errorf("marshal brief schema: %w", err)
	}
	raw, err := s.signalClassifier.runOneShot(ctx, msg.OneShotRequest{
		Prompt:       sessionBriefPrompt(previous, openLooseEnds, userMessage, assistantTexts),
		SystemPrompt: sessionBriefSystemPrompt,
		ModelRole:    s.signalClassifier.currentTuning().modelRole,
		Caller:       oneShotCallerSessionBrief,
		Schema:       schema,
		MaxTokens:    sessionBriefMaximumTokens,
	})
	if err != nil {
		return nil, err
	}
	var reply msg.OneShotResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decode oneshot reply: %w", err)
	}
	if len(reply.Parsed) == 0 {
		return nil, fmt.Errorf("oneshot returned no schema-conformant output (stop_reason=%q)", reply.StopReason)
	}
	var verdict sessionBriefVerdict
	if err := json.Unmarshal(reply.Parsed, &verdict); err != nil {
		return nil, fmt.Errorf("decode brief: %w", err)
	}
	return &verdict, nil
}

// sessionBriefPrompt lays out what the model reads.
func sessionBriefPrompt(previous *msg.SessionBrief, openLooseEnds []msg.LooseEnd, userMessage string, assistantTexts []string) string {
	var prompt strings.Builder
	prompt.WriteString("## The brief before this turn\n\n")
	if previous == nil {
		prompt.WriteString("None: this is the chat's first brief.\n")
	} else {
		fmt.Fprintf(&prompt, "overall_goal: %s\ncurrent_step: %s\n", previous.OverallGoal, previous.CurrentStep)
	}
	prompt.WriteString("\n## Open loose ends\n\n")
	if len(openLooseEnds) == 0 {
		prompt.WriteString("None.\n")
	}
	for _, looseEnd := range openLooseEnds {
		fmt.Fprintf(&prompt, "- id %s (%s, raised %s): %s", looseEnd.ID, looseEnd.Kind, looseEnd.CreatedAt.UTC().Format("2006-01-02 15:04Z"), looseEnd.Text)
		if looseEnd.Context != "" {
			fmt.Fprintf(&prompt, " — %s", looseEnd.Context)
		}
		prompt.WriteString("\n")
	}
	prompt.WriteString("\n## What the person said to start this turn\n\n")
	if strings.TrimSpace(userMessage) == "" {
		prompt.WriteString("Nothing: the assistant started this turn by itself.\n")
	} else {
		prompt.WriteString(keepHead(userMessage, sessionBriefUserMessageMaximumCharacters))
		prompt.WriteString("\n")
	}
	prompt.WriteString("\n## What the assistant wrote this turn, in order\n\n")
	prompt.WriteString(keepHeadAndTail(strings.Join(assistantTexts, "\n\n---\n\n"), sessionBriefAssistantTextMaximumCharacters))
	prompt.WriteString("\n")
	return prompt.String()
}

// sessionBriefUpdateFromVerdict checks the model's answer and turns it into a
// store update. A loose end it settles must be one it was shown; one it raises
// must have a known kind and some text.
func sessionBriefUpdateFromVerdict(sessionID string, verdict *sessionBriefVerdict, openLooseEnds []msg.LooseEnd) (store.SessionBriefUpdate, error) {
	overallGoal := strings.TrimSpace(verdict.OverallGoal)
	if overallGoal == "" {
		return store.SessionBriefUpdate{}, fmt.Errorf("the model returned no overall goal")
	}
	update := store.SessionBriefUpdate{
		SessionID: sessionID,
		Revision: msg.SessionBriefRevision{
			ID:          ids.NewSessionBriefRevisionID(),
			SessionID:   sessionID,
			OverallGoal: overallGoal,
			CurrentStep: strings.TrimSpace(verdict.CurrentStep),
			GoalChanged: verdict.GoalChanged,
			TurnChange:  strings.TrimSpace(verdict.TurnChange),
		},
		Settled: map[string]string{},
	}
	for _, settled := range verdict.Settled {
		shown := slices.ContainsFunc(openLooseEnds, func(looseEnd msg.LooseEnd) bool { return looseEnd.ID == settled.ID })
		if !shown {
			log.Printf("[session-brief] %s: the model settled %q, which it was not shown; ignored", sessionID, settled.ID)
			continue
		}
		update.Settled[settled.ID] = strings.TrimSpace(settled.How)
	}
	for _, raised := range verdict.NewLooseEnds {
		text := strings.TrimSpace(raised.Text)
		if text == "" || !slices.Contains(msg.LooseEndKinds, raised.Kind) {
			log.Printf("[session-brief] %s: dropped a loose end with kind %q and text %q", sessionID, raised.Kind, text)
			continue
		}
		if len(update.Raised) == sessionBriefMaximumNewLooseEnds {
			break
		}
		update.Raised = append(update.Raised, msg.LooseEnd{
			ID:        ids.NewLooseEndID(),
			SessionID: sessionID,
			Kind:      raised.Kind,
			Text:      text,
			Context:   strings.TrimSpace(raised.Context),
			State:     msg.LooseEndOpen,
		})
	}
	return update, nil
}

func keepHead(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// keepHeadAndTail keeps a quarter of the limit from the start and the rest
// from the end.
func keepHeadAndTail(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	head := limit / 4
	tail := limit - head
	return string(runes[:head]) + "\n\n[… middle of the turn left out …]\n\n" + string(runes[len(runes)-tail:])
}
