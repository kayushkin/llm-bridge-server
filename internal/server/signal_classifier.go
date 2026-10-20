package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The turn-end classifier is the derived producer described in
// docs/SESSION-SIGNALS.md: a cheap-model pass that sorts a finished turn's final
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

// turnClassification is the verdict for one turn-end. A question verdict
// carries its questions in Questions and leaves Title, Body and Severity
// empty; a notification verdict is the other way round.
type turnClassification struct {
	Kind      turnSignalKind       `json:"kind"`
	Title     string               `json:"title"`
	Body      string               `json:"body"`
	Questions []classifiedQuestion `json:"questions"`
	Severity  string               `json:"severity"`
}

// classifiedQuestion is one decision a turn asked the human to make. A turn
// may ask several, and each is its own entry: on 2026-10-20 a turn asking how
// to handle pull requests (a, b or c) and how to handle next steps (a, b or c)
// came back as ONE question whose options were "PRs: choose a, b, or c" and
// "Next steps: choose a, b, or c" (sig_01M5D1003TG9GXK3DZPEWYPG57), and with
// one list of options a person could answer only one of the two anyway.
type classifiedQuestion struct {
	Question string             `json:"question"`
	Context  string             `json:"context"`
	Options  []classifiedOption `json:"options"`
}

// classifiedOption is one choice the assistant offered for a question.
type classifiedOption struct {
	Label       string `json:"label"`
	Description string `json:"description"`
}

// signalClassifier calls a cheap model to classify a turn-end. Zero value is
// unusable; build it with newSignalClassifier.
type signalClassifier struct {
	// runOneShot executes one schema-forced call through the server's one-shot
	// router. Injected rather than reached for, so this type still knows
	// nothing about the server that owns it.
	runOneShot func(context.Context, msg.OneShotRequest) ([]byte, error)

	// tuning is what the operator may change while the server runs (the
	// signal_classifier.* settings). Read it through currentTuning, once per
	// use, so one classification never mixes two settings.
	tuningMutex sync.RWMutex
	tuning      signalClassifierTuning

	// baseURL overrides the Anthropic endpoint. Empty means "take it from
	// the resolved credential, falling back to the public API" — the
	// override exists so tests can point the classifier at a local stub
	// without a credential or a network.
	baseURL string
}

// signalClassifierTuning is the classifier's changeable part: the model-store
// role it asks (empty switches it off everywhere), how long one call may take,
// how much of a turn's final text it sends, and the harnesses it skips.
type signalClassifierTuning struct {
	modelRole string
	timeout   time.Duration
	maxChars  int
	optOut    map[msg.Harness]bool
}

func newSignalClassifier(modelRole string, timeout time.Duration, maxChars int, optOut map[msg.Harness]bool, runOneShot func(context.Context, msg.OneShotRequest) ([]byte, error)) *signalClassifier {
	return &signalClassifier{
		tuning:     signalClassifierTuning{modelRole: modelRole, timeout: timeout, maxChars: maxChars, optOut: optOut},
		runOneShot: runOneShot,
	}
}

func (c *signalClassifier) currentTuning() signalClassifierTuning {
	c.tuningMutex.RLock()
	defer c.tuningMutex.RUnlock()
	return c.tuning
}

// retune puts new settings in force for the next classification.
func (c *signalClassifier) retune(tuning signalClassifierTuning) {
	c.tuningMutex.Lock()
	defer c.tuningMutex.Unlock()
	c.tuning = tuning
}

// enabledFor reports whether the classifier runs for this harness. An empty
// model role disables it everywhere; the opt-out set is the per-harness escape
// hatch the on-by-default decision was taken with.
func (c *signalClassifier) enabledFor(harness msg.Harness) bool {
	if c == nil {
		return false
	}
	tuning := c.currentTuning()
	if tuning.modelRole == "" {
		return false
	}
	return !tuning.optOut[harness]
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

For a question: fill questions, one entry for each separate decision the assistant is waiting on, and leave title, body and severity empty.
- Two questions that can be answered independently are two entries, even when the assistant asked them in one sentence ("Which do you want for X and for Y?"). Never merge them into one entry, because a person can click only one option of an entry.
- question is that one question, in the assistant's own words where possible. context is any short detail needed to answer it, or empty.
- options are the choices the assistant offered for that question, in its order — do not invent choices it did not offer. A question that asks for a free answer ("What's your upload speed?") has no options.
- A plain yes/no question ("Do you want me to make them start on their own?") gets exactly two options, the yes and the no, each saying what it means ("Yes, make them start automatically", "No, leave them as they are"). Never give a question a single option: one button is no choice.
- Each option's label is a complete answer a person can click on its own: name the choice itself ("Add PRs to repo-store"), never a bare letter or number ("c", "Option 2") and never an instruction ("Choose a, b, or c"). description is the rest of what the assistant said about that choice; when the assistant said it would pick that option, say so there.

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
					"description": "The notification headline. Empty unless kind is notification.",
					"maxLength":   300,
				},
				"body": map[string]any{
					"type":        "string",
					"description": "The notification detail. Empty unless kind is notification.",
				},
				"questions": map[string]any{
					"type":        "array",
					"description": "One entry per separate decision the assistant is waiting on. Empty unless kind is question.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"question": map[string]any{"type": "string", "maxLength": 300},
							"context":  map[string]any{"type": "string"},
							"options": map[string]any{
								"type":        "array",
								"description": "Choices the assistant actually offered for this question. Empty unless it offered some.",
								"items": map[string]any{
									"type": "object",
									"properties": map[string]any{
										"label":       map[string]any{"type": "string"},
										"description": map[string]any{"type": "string"},
									},
									"required": []string{"label"},
								},
							},
						},
						"required": []string{"question"},
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
		ModelRole:    c.currentTuning().modelRole,
		Caller:       oneShotCallerSignalClassifier,
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
	limit := c.currentTuning().maxChars
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return "…" + string(runes[len(runes)-limit:])
}

// classifyTurnEnd classifies the finished turn, records a derived signal for a question or a notification, and
// reconciles the session state the heuristic set provisionally.
//
// Everything here is best-effort: a classifier that errors, times out or is
// switched off leaves the session exactly as the heuristic left it. A signal
// is an extra surface on top of a turn that already completed, and no
// failure to produce one may change how the turn itself resolved.
func (s *Server) classifyTurnEnd(bridgeID string, ev *msg.Event, state msg.SessionState) {
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

	ctx, cancel := context.WithTimeout(context.Background(), s.signalClassifier.currentTuning().timeout)
	defer cancel()

	verdict, err := s.signalClassifier.classify(ctx, ev.Result.Text)
	if err != nil {
		log.Printf("[signals] %s: classify turn-end: %v", bridgeID, err)
		return
	}

	switch verdict.Kind {
	case turnSignalQuestion:
		questions := usableQuestions(verdict.Questions)
		if len(questions) == 0 {
			log.Printf("[signals] %s: classifier returned question with no question text, no signal recorded", bridgeID)
			return
		}
		if sessionSurfacesQuestions(sess) {
			s.surfaceDerivedWorkerQuestions(sess, questions)
			return
		}
		s.recordDerivedQuestions(sess, questions, nil, cardContext{})
		// Promote: the classifier saw a question the heuristic's string
		// match did not. Bounded to the states this verdict was formed
		// about, so a turn that has already started again is left alone.
		s.harness.ApplyDerivedSessionState(bridgeID, msg.SessionAwaitingUser,
			msg.ReasonTurnCompleteSignalQuestion, msg.SessionIdle, msg.SessionAwaitingUser)
	case turnSignalNotification:
		s.recordDerivedNotification(sess, verdict)
		// A notification does not block, so a session the heuristic parked
		// at awaiting_user goes back to idle: nobody is being asked
		// anything.
		s.harness.ApplyDerivedSessionState(bridgeID, msg.SessionIdle,
			msg.ReasonTurnCompleteSignalNotification, msg.SessionAwaitingUser)
	case turnSignalNeither:
		// Demote a heuristic false positive. This is the only path that
		// makes an existing behaviour stricter, and it is why the
		// classifier is worth its cost: today a turn ending in "does that
		// make sense?" parks the session with nothing to answer.
		s.harness.ApplyDerivedSessionState(bridgeID, msg.SessionIdle,
			msg.ReasonTurnCompleteSignalNeither, msg.SessionAwaitingUser)
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
	//
	// The exception is an autonomous session whose purpose surfaces
	// questions: a worker on a kanban card has a reader — the person on the
	// card — and a question it ends its turn on goes there through triage.
	if sess != nil && sess.Type != "" && sess.Type != msg.SessionTypeInteractive && !sessionSurfacesQuestions(sess) {
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
	case msg.SessionIdle, msg.SessionAwaitingUser, msg.SessionBackgroundTasksRunning:
		// background_tasks_running is a turn that settled with subagents or
		// backgrounded commands still going. Its final message can ask a
		// question as well as any other, and the verdict is held under the
		// running tasks (derivationState.applyExternalState).
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

// onUnpromptedTurnStart retires the questions a turn the harness opened by
// itself has moved past.
//
// Measured on br_1790362479778008348 (2026-09-25): a turn settled with a
// background agent still running, the classifier raised "Ready for
// dialog-wording check" from its last message, and two minutes later the
// agent finished and Claude Code opened a new turn with its
// <task-notification>. Nothing closed the question until that turn ENDED, so
// for the whole of a long turn the user was asked about a state the session
// had already left. An answer sent now would land after the new turn's output,
// which is the thread argument supersedeStaleQuestions makes for a turn-end.
func (s *Server) onUnpromptedTurnStart(bridgeID string, _ *msg.Event) {
	s.supersedeStaleQuestions(bridgeID)
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

// usableQuestions keeps the questions that have question text, trimmed, and
// drops options with no label. A question with no text cannot be shown, and
// an option with no label cannot be clicked.
func usableQuestions(questions []classifiedQuestion) []classifiedQuestion {
	usable := make([]classifiedQuestion, 0, len(questions))
	for _, question := range questions {
		text := strings.TrimSpace(question.Question)
		if text == "" {
			continue
		}
		cleaned := classifiedQuestion{Question: text, Context: strings.TrimSpace(question.Context)}
		for _, option := range question.Options {
			label := strings.TrimSpace(option.Label)
			if label == "" {
				continue
			}
			cleaned.Options = append(cleaned.Options, classifiedOption{Label: label, Description: strings.TrimSpace(option.Description)})
		}
		usable = append(usable, cleaned)
	}
	return usable
}

// derivedQuestionSignal builds the open row for one question the classifier
// found. requestID ties it to the other questions of the same turn, and is
// empty when the turn asked only one.
func (s *Server) derivedQuestionSignal(sess *store.Session, question classifiedQuestion, requestID string) *msg.Signal {
	signal := &msg.Signal{
		ID:           ids.NewSignalID(),
		SessionID:    sess.SessionID,
		SessionType:  sess.Type,
		Kind:         msg.SignalKindQuestion,
		Source:       msg.SignalSourceDerived,
		RequestID:    requestID,
		Surface:      signalSurfaceForSession(sess),
		Title:        question.Question,
		Body:         question.Context,
		State:        msg.SignalStateOpen,
		LinkedTodoID: s.linkedTodoForSession(sess.SessionID),
		// A derived question resolves by sending a message, and a message
		// is free text whether or not the assistant listed choices.
		AllowFreeform: true,
	}
	for _, option := range question.Options {
		signal.Options = append(signal.Options, msg.SignalOption{
			// A derived option has no machine value distinct from its
			// label: the answer is sent on as a message, and the label is
			// the text.
			Label:       option.Label,
			Value:       option.Label,
			Description: option.Description,
		})
	}
	return signal
}

// recordDerivedQuestions writes one open question row per question the turn
// asked. Several questions share one request id, which is what makes them one
// group: the chat draws them together, POST /signals/{id}/answer refuses an
// answer that leaves one of them blank, and the answers reach the session as
// one message with each answer under its question (answerTextForMessage). A
// lone question carries no request id, as derived rows always did.
//
// A derived request id never names a parked hook, so the answer path, which
// asks parkedAsks before it treats a request id as a park, delivers the
// answers as a message.
//
// triageByQuestion holds the question triage's verdict for each question, in
// order, for a surfacing worker; nil for every other session. card is what
// triage knew about the card. Returns the rows it wrote.
func (s *Server) recordDerivedQuestions(sess *store.Session, questions []classifiedQuestion, triageByQuestion []*questionTriageVerdict, card cardContext) []*msg.Signal {
	requestID := ""
	if len(questions) > 1 {
		requestID = ids.NewDerivedQuestionGroupID()
	}
	recorded := make([]*msg.Signal, 0, len(questions))
	for i, question := range questions {
		signal := s.derivedQuestionSignal(sess, question, requestID)
		if i < len(triageByQuestion) && triageByQuestion[i] != nil {
			signal.Audience = triageByQuestion[i].Audience
			signal.CustomerReplyDraft = customerReplyDraftFor(triageByQuestion[i], card)
		}
		if err := s.store.CreateSignal(signal); err != nil {
			log.Printf("[signals] %s: persist derived question: %v", sess.SessionID, err)
			continue
		}
		log.Printf("[signals] %s: derived question signal %s (surface=%s audience=%s request=%s)", sess.SessionID, signal.ID, signal.Surface, signal.Audience, requestID)
		recorded = append(recorded, signal)
	}
	return recorded
}

// recordDerivedNotification persists a notification verdict as a signal row.
// Returns the row it wrote, or nil when it wrote none.
func (s *Server) recordDerivedNotification(sess *store.Session, verdict *turnClassification) *msg.Signal {
	title := strings.TrimSpace(verdict.Title)
	if title == "" {
		log.Printf("[signals] %s: classifier returned notification with no title, no signal recorded", sess.SessionID)
		return nil
	}
	signal := &msg.Signal{
		ID:           ids.NewSignalID(),
		SessionID:    sess.SessionID,
		SessionType:  sess.Type,
		Kind:         msg.SignalKindNotification,
		Source:       msg.SignalSourceDerived,
		Surface:      signalSurfaceForSession(sess),
		Title:        title,
		Body:         strings.TrimSpace(verdict.Body),
		State:        msg.SignalStateOpen,
		LinkedTodoID: s.linkedTodoForSession(sess.SessionID),
		Severity:     msg.SignalSeverityInfo,
	}
	if verdict.Severity == string(msg.SignalSeverityWarn) {
		signal.Severity = msg.SignalSeverityWarn
	}
	if err := s.store.CreateSignal(signal); err != nil {
		log.Printf("[signals] %s: persist derived notification: %v", sess.SessionID, err)
		return nil
	}
	log.Printf("[signals] %s: derived notification signal %s (surface=%s)", sess.SessionID, signal.ID, signal.Surface)
	return signal
}
