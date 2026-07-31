package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
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

	// classifierMaxTokens bounds the verdict. The tool schema is small —
	// a kind, a headline, a short body and a handful of options — so this
	// is generous rather than tight.
	classifierMaxTokens = 700
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
	model      string
	timeout    time.Duration
	maxChars   int
	optOut     map[msg.Harness]bool
	authClient *authstoreclient.Client
	httpClient *http.Client

	// baseURL overrides the Anthropic endpoint. Empty means "take it from
	// the resolved credential, falling back to the public API" — the
	// override exists so tests can point the classifier at a local stub
	// without a credential or a network.
	baseURL string
}

func newSignalClassifier(model string, timeout time.Duration, maxChars int, optOut map[msg.Harness]bool, authClient *authstoreclient.Client) *signalClassifier {
	return &signalClassifier{
		model:      model,
		timeout:    timeout,
		maxChars:   maxChars,
		optOut:     optOut,
		authClient: authClient,
		httpClient: &http.Client{Timeout: timeout},
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
func (c *signalClassifier) classify(ctx context.Context, text string) (*turnClassification, error) {
	endpoint, authHeader, authValue, extraHeaders, err := c.endpoint(ctx)
	if err != nil {
		return nil, err
	}

	body, err := json.Marshal(map[string]any{
		"model":       c.model,
		"max_tokens":  classifierMaxTokens,
		"system":      classifierSystemPrompt,
		"tools":       []any{classifierToolSchema()},
		"tool_choice": map[string]any{"type": "tool", "name": classifierToolName},
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "Final message of the finished turn:\n\n" + c.trim(text),
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("marshal classify request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", classifierAnthropicVersion)
	req.Header.Set(authHeader, authValue)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("classify: %s — %s", resp.Status, string(raw))
	}

	var parsed struct {
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("decode classify response: %w", err)
	}
	for _, block := range parsed.Content {
		if block.Type != "tool_use" || block.Name != classifierToolName {
			continue
		}
		var verdict turnClassification
		if err := json.Unmarshal(block.Input, &verdict); err != nil {
			return nil, fmt.Errorf("decode %s input: %w", classifierToolName, err)
		}
		switch verdict.Kind {
		case turnSignalQuestion, turnSignalNotification, turnSignalNeither:
			return &verdict, nil
		default:
			return nil, fmt.Errorf("classify returned unknown kind %q", verdict.Kind)
		}
	}
	return nil, fmt.Errorf("classify response carried no %s call", classifierToolName)
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

// endpoint resolves the Anthropic credential and returns everything the
// request needs to authenticate. auth_type decides the header: an API key
// goes in x-api-key, an OAuth access token in Authorization, and sending
// one in the other's header is a 401 that reads like a bad credential.
func (c *signalClassifier) endpoint(ctx context.Context) (url, authHeader, authValue string, extra map[string]string, err error) {
	base := c.baseURL
	if base != "" {
		// Test stub: no credential, no auth-store round trip.
		return strings.TrimRight(base, "/") + "/v1/messages", "x-api-key", "stub", nil, nil
	}
	if c.authClient == nil {
		return "", "", "", nil, fmt.Errorf("no auth-store client")
	}
	cred, err := c.authClient.ResolveByProvider(ctx, "anthropic", "", "llm-bridge-server", "signal-classifier")
	if err != nil {
		return "", "", "", nil, fmt.Errorf("resolve anthropic credential: %w", err)
	}
	secret := cred.Secret()
	if secret == "" {
		return "", "", "", nil, fmt.Errorf("anthropic credential %s has no usable secret", cred.ID)
	}
	base = cred.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	switch cred.AuthType {
	case "oauth", "token":
		return strings.TrimRight(base, "/") + "/v1/messages", "Authorization", "Bearer " + secret,
			map[string]string{"anthropic-beta": "oauth-2025-04-20"}, nil
	default:
		return strings.TrimRight(base, "/") + "/v1/messages", "x-api-key", secret, nil, nil
	}
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
	s.supersedeDerivedSignals(bridgeID)

	// The session already has a parked tool question open. That is a
	// structured ask with a real resolve verb behind it; a derived row
	// alongside it would be a second, weaker copy of the same demand on the
	// user's attention.
	if s.hasOpenToolSignal(bridgeID) {
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

// hasOpenToolSignal reports whether a structured tool ask is already open on
// this session.
func (s *Server) hasOpenToolSignal(bridgeID string) bool {
	open, err := s.store.ListSignals(store.SignalFilter{SessionID: bridgeID, State: msg.SignalStateOpen})
	if err != nil {
		log.Printf("[signals] %s: look up open signals: %v", bridgeID, err)
		// Fail closed: an unreadable store must not become a licence to
		// mint duplicates on top of a park we could not see.
		return true
	}
	for _, sig := range open {
		if sig.Source == msg.SignalSourceTool {
			return true
		}
	}
	return false
}

// supersedeDerivedSignals dismisses every still-open derived signal on the
// session. Called at the top of a turn-end, so at most one derived row per
// session is ever open: the one from the most recent turn.
func (s *Server) supersedeDerivedSignals(bridgeID string) {
	open, err := s.store.ListSignals(store.SignalFilter{SessionID: bridgeID, State: msg.SignalStateOpen})
	if err != nil {
		log.Printf("[signals] %s: look up derived signals to supersede: %v", bridgeID, err)
		return
	}
	for _, sig := range open {
		if sig.Source != msg.SignalSourceDerived {
			continue
		}
		if err := s.store.ResolveSignal(sig.ID, msg.SignalStateDismissed, nil); err != nil {
			log.Printf("[signals] %s: supersede signal %s: %v", bridgeID, sig.ID, err)
		}
	}
}

// answerDerivedQuestions closes the session's open derived questions with the
// message the user just sent.
//
// A derived question has no parked hook behind it, so the hook-resolve verb
// cannot reach it; the design's answer is that sending a message resolves it,
// and the send handler is the one place every surface's answer passes through.
// A notification is left alone — it is acknowledged, not answered, and the ack
// verb lands with P4.
func (s *Server) answerDerivedQuestions(bridgeID, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	open, err := s.store.ListSignals(store.SignalFilter{SessionID: bridgeID, State: msg.SignalStateOpen})
	if err != nil {
		log.Printf("[signals] %s: look up derived questions to answer: %v", bridgeID, err)
		return
	}
	for _, sig := range open {
		if sig.Source != msg.SignalSourceDerived || sig.Kind != msg.SignalKindQuestion {
			continue
		}
		if err := s.store.ResolveSignal(sig.ID, msg.SignalStateAnswered, &msg.SignalAnswer{Text: text}); err != nil {
			log.Printf("[signals] %s: answer derived signal %s: %v", bridgeID, sig.ID, err)
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
		ID:          ids.NewSignalID(),
		SessionID:   sess.SessionID,
		SessionType: sess.Type,
		Kind:        kind,
		Source:      msg.SignalSourceDerived,
		Surface:     signalSurfaceForSession(sess),
		Title:       title,
		Body:        strings.TrimSpace(verdict.Body),
		State:       msg.SignalStateOpen,
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
