package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The turn-end classifier is the derived producer described in
// SESSION-SIGNALS.md: a cheap-model pass that sorts a finished turn's final
// text into question | notification | neither and, for the first two, writes
// a source:"derived" signal.
//
// It generalizes looksLikeQuestion, which only ever detected questions and
// only ever produced a state flag. The heuristic stays in place as the
// immediate provisional answer (see its comment in internal/harness); this
// pass is the authoritative one and runs after the turn's events are out.

const (
	// classifierToolName is the single tool the classifier is forced to
	// call, so the verdict arrives as a validated object rather than prose
	// we would have to parse.
	classifierToolName = "report_turn_signal"

	// classifyMinChars skips turns too short to carry either a question or
	// a notification worth surfacing ("ok", "done."). Below this the call
	// costs more than the answer is worth.
	classifyMinChars = 12

	// classifierAnthropicVersion is the API version header every Anthropic
	// Messages request carries.
	classifierAnthropicVersion = "2023-06-01"

	// classifierMaxTokens bounds the verdict, and the cap covers the
	// model's thinking as well as its answer: the harness passes it as
	// CLAUDE_CODE_MAX_OUTPUT_TOKENS. At 700 the classifier failed one to
	// two turns an hour from 2026-09-02 22:00 with "response exceeded the
	// 700 output token maximum" — measured on the failing calls, thinking
	// alone ran 460–704 tokens before the verdict started, and the
	// successful calls peaked at 660. The verdict itself is small (a kind, a
	// headline, a short body, a handful of options), so this is headroom for
	// the thinking, not the answer. A classifier with no tools cannot loop on
	// tokens, so generous costs nothing when the model is brief.
	classifierMaxTokens = 4000
)

// turnSignalKind is the classifier's three-way verdict. It is deliberately
// wider than msg.SignalKind: "neither" is the common case and has no record.
type turnSignalKind string

const (
	turnSignalQuestion     turnSignalKind = "question"
	turnSignalNotification turnSignalKind = "notification"
	turnSignalNeither      turnSignalKind = "neither"
)

// turnClassification is the verdict for one turn-end.
type turnClassification struct {
	Kind    turnSignalKind `json:"kind"`
	Title   string         `json:"title"`
	Body    string         `json:"body"`
	Options []struct {
		Label       string `json:"label"`
		Description string `json:"description"`
	} `json:"options"`
	Severity string `json:"severity"`
}

// signalClassifier calls a cheap model to classify a turn-end. Zero value is
// unusable; build it with newSignalClassifier.
type signalClassifier struct {
	// runOneShot executes one schema-forced call on a harness instance. Injected
	// rather than reached for, so this type still knows nothing about the
	// server that owns it.
	runOneShot func(context.Context, msg.OneShotRequest) ([]byte, error)

	model    string
	timeout  time.Duration
	maxChars int
	optOut   map[msg.Harness]bool

	// baseURL overrides the Anthropic endpoint. Empty means "take it from
	// the resolved credential, falling back to the public API" — the
	// override exists so tests can point the classifier at a local stub
	// without a credential or a network.
	baseURL string
}

func newSignalClassifier(model string, timeout time.Duration, maxChars int, optOut map[msg.Harness]bool, runOneShot func(context.Context, msg.OneShotRequest) ([]byte, error)) *signalClassifier {
	return &signalClassifier{
		model:      model,
		timeout:    timeout,
		maxChars:   maxChars,
		optOut:     optOut,
		runOneShot: runOneShot,
	}
}

// enabledFor reports whether the classifier runs for this harness. An empty
// model disables it everywhere; the opt-out set is the per-harness escape
// hatch the on-by-default decision was taken with.
func (c *signalClassifier) enabledFor(harness msg.Harness) bool {
	if c == nil || c.model == "" {
		return false
	}
	return !c.optOut[harness]
}

// classifierSystemPrompt tells the model what the three verdicts mean. The
// bar for a signal is deliberately high: a false question parks a session at
// awaiting_user, and a false notification puts an unread badge in front of
// the user for a turn that just finished normally.
const classifierSystemPrompt = `You classify the final message of a finished AI coding-assistant turn, deciding whether it needs to reach the human who owns the session.

Return exactly one kind:

- "question" — the assistant is genuinely waiting on the human before it can continue. It asked something it cannot answer itself, or offered a choice it will not make alone. A rhetorical question, a question the assistant then answers itself, and a closing "let me know if you need anything else" are NOT questions.
- "notification" — the turn finished without needing an answer, but it says something the human should see unprompted: work completed that they were waiting on, something that failed, a warning, a change with consequences they did not ask for.
- "neither" — everything else, and this is the common case. Ordinary progress reports, answers to what the human just asked, and routine completions are "neither". Prefer "neither" whenever you hesitate.

For a question: title is the question itself, in the assistant's own words. options are the choices the assistant offered, if any — do not invent options it did not offer. body is any short context needed to understand the question.

For a notification: title is a one-line headline. body is the detail. severity is "warn" for a failure, a risk, or anything the human may need to act on, and "info" otherwise.

For "neither": leave every other field empty.`

// classifierToolSchema is the forced tool. Enum constraints keep the verdict
// inside the vocabulary the record understands, so an unexpected value is a
// schema violation the model retries rather than a bad row.
// classifierOutputSchema is the bare JSON Schema the oneshot path takes.
//
// Derived from the tool schema rather than written out a second time: two
// copies of one shape drift, and the failure that follows is a model answering
// a question nobody asked.
func classifierOutputSchema() map[string]any {
	tool := classifierToolSchema()
	if inner, ok := tool["input_schema"].(map[string]any); ok {
		return inner
	}
	return map[string]any{"type": "object"}
}

func classifierToolSchema() map[string]any {
	return map[string]any{
		"name":        classifierToolName,
		"description": "Report the classification of this turn-end.",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind": map[string]any{
					"type": "string",
					"enum": []string{"question", "notification", "neither"},
				},
				"title": map[string]any{
					"type":        "string",
					"description": "The question, or the notification headline. Empty when kind is neither.",
					"maxLength":   300,
				},
				"body": map[string]any{
					"type":        "string",
					"description": "Optional supporting detail.",
				},
				"options": map[string]any{
					"type":        "array",
					"description": "Choices the assistant actually offered. Empty unless it offered some.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"label":       map[string]any{"type": "string"},
							"description": map[string]any{"type": "string"},
						},
						"required": []string{"label"},
					},
				},
				"severity": map[string]any{
					"type":        "string",
					"enum":        []string{"info", "warn"},
					"description": "Notifications only.",
				},
			},
			"required": []string{"kind"},
		},
	}
}

// classify sends one turn's final text and returns the verdict. A nil
// verdict with a nil error is impossible — every path either yields a
// classification or explains why it could not.
// classify asks the model what a finished turn was.
//
// It goes through the harness oneshot path, not api.anthropic.com. That call
// used to resolve an API key from auth-store on EVERY turn end, which the audit
// log showed was the largest remaining drain on that key once the scheduler's
// classifiers had moved off it. The harness runs on the Claude Code
// subscription login, so no credential passes through here at all — the
// endpoint/auth-header logic this replaced is gone with it.
func (c *signalClassifier) classify(ctx context.Context, text string) (*turnClassification, error) {
	if c.runOneShot == nil {
		return nil, fmt.Errorf("classify: no oneshot runner wired")
	}
	schema, err := json.Marshal(classifierOutputSchema())
	if err != nil {
		return nil, fmt.Errorf("marshal classify schema: %w", err)
	}

	raw, err := c.runOneShot(ctx, msg.OneShotRequest{
		Prompt:       "Final message of the finished turn:\n\n" + c.trim(text),
		SystemPrompt: classifierSystemPrompt,
		Model:        c.model,
		Schema:       schema,
		MaxTokens:    classifierMaxTokens,
	})
	if err != nil {
		return nil, err
	}

	var reply msg.OneShotResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decode oneshot reply: %w", err)
	}
	if len(reply.Parsed) == 0 {
		// No parsed object means the schema was not satisfied. Reported rather
		// than guessed at: a classification invented here would be indis-
		// tinguishable from one the model actually made.
		return nil, fmt.Errorf("classify: oneshot returned no schema-conformant output (stop_reason=%q)", reply.StopReason)
	}

	var out turnClassification
	if err := json.Unmarshal(reply.Parsed, &out); err != nil {
		return nil, fmt.Errorf("decode classification: %w", err)
	}
	switch out.Kind {
	case turnSignalQuestion, turnSignalNotification, turnSignalNeither:
		return &out, nil
	default:
		// Refused rather than treated as "neither". A kind outside the
		// vocabulary means the model did not answer the question asked, and
		// silently reading that as "no signal" would hide every such failure.
		return nil, fmt.Errorf("classify returned unknown kind %q", out.Kind)
	}
}

// trim caps the text sent to the model, keeping the TAIL. A turn that ends
// with a question preceded by a long file dump would otherwise be classified
// from the dump; the question the human has to answer is always at the end.
func (c *signalClassifier) trim(text string) string {
	limit := c.maxChars
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return "…" + string(runes[len(runes)-limit:])
}

// onTurnEnd is the Manager's turn-end observer. It classifies the finished
// turn, records a derived signal for a question or a notification, and
// reconciles the session state the heuristic set provisionally.
//
// Everything here is best-effort: a classifier that errors, times out or is
// switched off leaves the session exactly as the heuristic left it. A signal
// is an extra surface on top of a turn that already completed, and no
// failure to produce one may change how the turn itself resolved.
func (s *Server) onTurnEnd(bridgeID string, ev *msg.Event, state msg.SessionState) {
	if s.signalClassifier == nil || ev == nil || ev.Result == nil {
		return
	}
	sess, err := s.store.GetSession(bridgeID)
	if err != nil {
		return
	}
	if !s.signalClassifier.enabledFor(sess.Harness) {
		return
	}
	if skip := skipClassifyReason(sess, ev.Result.Text, state); skip != "" {
		return
	}

	// A turn-end supersedes this session's earlier derived signals whatever
	// the new verdict is: the assistant has spoken again, so a free-text
	// ask from a previous turn is either answered or moot. Without this a
	// derived row would sit open forever — nothing else resolves one until
	// P4 adds the signal-level resolve verb.
	s.supersedeStaleQuestions(bridgeID)

	// The session already has a parked tool question open. That is a
	// structured ask with a real resolve verb behind it; a derived row
	// alongside it would be a second, weaker copy of the same demand on the
	// user's attention. A tool NOTIFICATION does not count — it makes no
	// demand, and letting it count would end classification for the session.
	if s.hasOpenToolQuestion(bridgeID) {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.signalClassifier.timeout)
	defer cancel()

	verdict, err := s.signalClassifier.classify(ctx, ev.Result.Text)
	if err != nil {
		log.Printf("[signals] %s: classify turn-end: %v", bridgeID, err)
		return
	}

	switch verdict.Kind {
	case turnSignalQuestion:
		s.recordDerivedSignal(sess, verdict, msg.SignalKindQuestion)
		// Promote: the classifier saw a question the heuristic's string
		// match did not. Bounded to the states this verdict was formed
		// about, so a turn that has already started again is left alone.
		s.harness.ApplyDerivedSessionState(bridgeID, msg.SessionAwaitingUser,
			"turn_complete_signal_question", msg.SessionIdle, msg.SessionAwaitingUser)
	case turnSignalNotification:
		s.recordDerivedSignal(sess, verdict, msg.SignalKindNotification)
		// A notification does not block, so a session the heuristic parked
		// at awaiting_user goes back to idle: nobody is being asked
		// anything.
		s.harness.ApplyDerivedSessionState(bridgeID, msg.SessionIdle,
			"turn_complete_signal_notification", msg.SessionAwaitingUser)
	case turnSignalNeither:
		// Demote a heuristic false positive. This is the only path that
		// makes an existing behaviour stricter, and it is why the
		// classifier is worth its cost: today a turn ending in "does that
		// make sense?" parks the session with nothing to answer.
		s.harness.ApplyDerivedSessionState(bridgeID, msg.SessionIdle,
			"turn_complete_signal_neither", msg.SessionAwaitingUser)
	}
}

// skipClassifyReason returns why this turn should not be classified, or ""
// when it should be. Split out so the skip rules are testable without a
// model, a store or a network.
func skipClassifyReason(sess *store.Session, text string, state msg.SessionState) string {
	// A signal exists to put something in front of a PERSON: a question they
	// have to answer, a notification worth their attention. An autonomous
	// session has nobody watching it, so a signal raised on one is a row
	// nobody reads, minted at the cost of a model call on every turn end.
	//
	// This is most of the traffic. The fleet runs autoworker, dispatcher and
	// demo sessions continuously and interactive chat rarely, so classifying
	// everything meant nearly every call was spent on a turn with no reader.
	if sess != nil && sess.Type != "" && sess.Type != msg.SessionTypeInteractive {
		return "not an interactive session: " + string(sess.Type)
	}
	if sess != nil && sess.Purpose == renamerSourceTag {
		// The renamer is our own helper. Classifying its turns spends money
		// on a session no human reads, and its sign-off looks enough like a
		// notification to mint one every time.
		return "renamer session"
	}
	if strings.TrimSpace(text) == "" {
		return "empty final message"
	}
	if len([]rune(strings.TrimSpace(text))) < classifyMinChars {
		return "final message too short to carry a signal"
	}
	switch state {
	case msg.SessionIdle, msg.SessionAwaitingUser:
		return ""
	default:
		// The turn did not end on its own terms — it errored, was aborted,
		// or another turn opened before the observer ran. Nothing to
		// surface, and nothing whose state we may touch.
		return "turn did not settle: " + string(state)
	}
}

// hasOpenToolQuestion reports whether a structured tool ask is already open on
// this session.
//
// Kind matters, and narrowing to it is what keeps the notification producer
// from switching the classifier off. The rule this guards is "do not raise a
// weaker copy of a demand the user already has in front of them", and a demand
// is a question: a parked AskUserQuestion holds the session at awaiting_user
// with a resolve verb behind it. A tool notification (POST
// /sessions/{id}/signals) asks for nothing and blocks nothing, but it stays
// open until somebody clicks Acknowledge — which on an unattended worker's
// kanban card may be never. Counting it here would mean one notification
// silently ends classification for the rest of that session's life, so the
// genuine blocker it raised three turns later would never be surfaced at all.
func (s *Server) hasOpenToolQuestion(bridgeID string) bool {
	open, err := s.store.ListSignals(store.SignalFilter{SessionID: bridgeID, State: msg.SignalStateOpen})
	if err != nil {
		log.Printf("[signals] %s: look up open signals: %v", bridgeID, err)
		// Fail closed: an unreadable store must not become a licence to
		// mint duplicates on top of a park we could not see.
		return true
	}
	for _, sig := range open {
		if sig.Source == msg.SignalSourceTool && sig.Kind == msg.SignalKindQuestion {
			return true
		}
	}
	return false
}

// answerableInPlace reports whether a question can still be delivered to the
// tool call that asked it — that is, whether its park is alive right now.
//
// A request_id is not enough to answer this: it says a park EXISTED. The park
// may have drained, or died with its process. Only the live registry knows.
func (s *Server) answerableInPlace(sig store.Signal) bool {
	return sig.RequestID != "" && s.parkedAsks.isParked(sig.SessionID, sig.RequestID)
}

// supersedeStaleQuestions closes every open question the thread has moved past.
// Called at the top of a turn-end.
//
// A session is a THREAD, and that is the whole reason this exists. Two
// questions cannot sit open on one thread waiting to be answered at different
// times: an answer sent now lands at the end of the conversation, so answering
// the older one later drops a reply to a question three turns back into a
// context that has moved on. The invariant is at most ONE open question per
// session, and a new turn is what retires the previous one.
//
// The test is whether the question is still answerable IN PLACE, not what
// produced it. That distinction used to be Source == derived, which was right
// only while a parked question could not outlive its park — it was dismissed
// when the process died. Now that a question survives its session, an orphaned
// parked question is exactly as un-answerable-later as a derived one, and
// leaving it open would let a thread accumulate the very backlog this prevents.
//
// A LIVE park is never superseded: something is still blocked on it, and
// closing it would destroy a real pending tool call. In practice a live park
// blocks mid-turn, so a turn-end cannot arrive while one is outstanding.
//
// Notifications keep the narrower rule. They are read, not answered, so the
// thread argument does not reach them and this is deliberately not the commit
// that changes when an FYI disappears.
func (s *Server) supersedeStaleQuestions(bridgeID string) {
	open, err := s.store.ListSignals(store.SignalFilter{SessionID: bridgeID, State: msg.SignalStateOpen})
	if err != nil {
		log.Printf("[signals] %s: look up signals to supersede: %v", bridgeID, err)
		return
	}
	for _, sig := range open {
		stale := sig.Kind == msg.SignalKindQuestion && !s.answerableInPlace(sig)
		if sig.Kind != msg.SignalKindQuestion {
			stale = sig.Source == msg.SignalSourceDerived
		}
		if !stale {
			continue
		}
		if err := s.store.ResolveSignal(sig.ID, msg.SignalStateDismissed, nil); err != nil {
			log.Printf("[signals] %s: supersede signal %s: %v", bridgeID, sig.ID, err)
		}
	}
}

// closeQuestionsAnsweredByMessage closes the session's open questions with the
// message the user just sent.
//
// Any message is an answer here, whether or not the sender meant it as one:
// the reply lands at the end of the thread either way, so a question left open
// behind it could never be answered in its own context again. Closing it is
// the honest record.
//
// Skips a question whose park is still live — that one has a channel waiting
// and resolves through it, not through the next message. A notification is
// left alone: it is acknowledged, not answered.
func (s *Server) closeQuestionsAnsweredByMessage(bridgeID, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	open, err := s.store.ListSignals(store.SignalFilter{SessionID: bridgeID, State: msg.SignalStateOpen})
	if err != nil {
		log.Printf("[signals] %s: look up questions to answer: %v", bridgeID, err)
		return
	}
	for _, sig := range open {
		if sig.Kind != msg.SignalKindQuestion || s.answerableInPlace(sig) {
			continue
		}
		if err := s.store.ResolveSignal(sig.ID, msg.SignalStateAnswered, &msg.SignalAnswer{Text: text}); err != nil {
			log.Printf("[signals] %s: answer signal %s: %v", bridgeID, sig.ID, err)
		}
	}
}

// recordDerivedSignal persists the classifier's verdict as a signal row.
// Derived rows carry no RequestID — there is no parked hook behind them —
// which is exactly how a consumer tells the two resolve paths apart.
func (s *Server) recordDerivedSignal(sess *store.Session, verdict *turnClassification, kind msg.SignalKind) {
	title := strings.TrimSpace(verdict.Title)
	if title == "" {
		log.Printf("[signals] %s: classifier returned %s with no title, no signal recorded", sess.SessionID, kind)
		return
	}

	signal := &msg.Signal{
		ID:           ids.NewSignalID(),
		SessionID:    sess.SessionID,
		SessionType:  sess.Type,
		Kind:         kind,
		Source:       msg.SignalSourceDerived,
		Surface:      signalSurfaceForSession(sess),
		Title:        title,
		Body:         strings.TrimSpace(verdict.Body),
		State:        msg.SignalStateOpen,
		LinkedTodoID: s.linkedTodoForSession(sess.SessionID),
	}

	if kind == msg.SignalKindQuestion {
		for _, opt := range verdict.Options {
			label := strings.TrimSpace(opt.Label)
			if label == "" {
				continue
			}
			signal.Options = append(signal.Options, msg.SignalOption{
				// A derived option has no machine value distinct from its
				// label: the resolve path sends the text on to /send, and
				// the label is the text.
				Label:       label,
				Value:       label,
				Description: strings.TrimSpace(opt.Description),
			})
		}
		// A derived question resolves by sending a message, and a message
		// is free text whether or not the assistant listed choices.
		signal.AllowFreeform = true
	} else {
		switch verdict.Severity {
		case string(msg.SignalSeverityWarn):
			signal.Severity = msg.SignalSeverityWarn
		default:
			signal.Severity = msg.SignalSeverityInfo
		}
	}

	if err := s.store.CreateSignal(signal); err != nil {
		log.Printf("[signals] %s: persist derived signal: %v", sess.SessionID, err)
		return
	}
	log.Printf("[signals] %s: derived %s signal %s (surface=%s)", sess.SessionID, kind, signal.ID, signal.Surface)
}
