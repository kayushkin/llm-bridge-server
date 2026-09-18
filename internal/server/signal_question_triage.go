package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"
	"github.com/kayushkin/llm-bridge-server/internal/mailstackclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// Question triage is what stands between an unattended worker's question and
// the person on its card.
//
// A worker whose purpose surfaces questions (msg.PurposeSurfacesQuestions) is
// allowed to ask. It is not allowed to bother a person with every ask: most
// of what a coding agent asks is a sign-off — "shall I proceed?", "is this
// plan okay?" — that the person would answer "yes" to without reading. Triage
// answers those on the spot and the worker continues. What reaches the card is
// the remainder: a question that needs the assignee's judgement, or one only
// the customer who asked for the work can answer. For the second kind triage
// also drafts the reply to the customer, so the person on the card forwards
// rather than composes.
//
// One cheap-model call per question, through the same oneshot path as the
// turn-end classifier, on the same instance and the same subscription login.

const (
	// triageMaxTokens bounds the verdict plus the model's thinking, for the
	// same reason classifierMaxTokens does. The verdict here is larger — it
	// can carry a drafted email — so the headroom is larger too.
	triageMaxTokens = 6000

	// autoSignoffAnswer is what the worker is told when triage judges its
	// question a sign-off. It is deliberately more than "yes": the worker
	// should know nobody read the question, so it does not lean on the answer
	// as if a person had weighed it.
	autoSignoffAnswer = "Proceed. This sign-off was answered automatically because nobody needs to weigh it; if you actually need a decision from a person, ask a specific question that states the choice."

	// autoSignoffResolvedBy is the resolved_by stamped on a signal triage
	// closed itself, so the record says a machine answered.
	autoSignoffResolvedBy = "auto:signoff-triage"

	// maxAutoContinuesPerSession caps how many derived sign-offs are answered
	// automatically on one session. A worker that keeps ending its turn on
	// "shall I continue?" is looping, and answering it forever would spend
	// the budget on a conversation with itself. Past the cap the question
	// surfaces to the card like any other.
	maxAutoContinuesPerSession = 3
)

// questionDisposition is triage's first verdict: does anyone need to see this?
type questionDisposition string

const (
	// dispositionSignoff — a confirmation the worker could take as given.
	dispositionSignoff questionDisposition = "signoff"
	// dispositionNeedsInput — a real question with a real choice in it.
	dispositionNeedsInput questionDisposition = "needs_input"
)

// questionTriageVerdict is the triage of one question.
type questionTriageVerdict struct {
	Disposition questionDisposition `json:"disposition"`
	// Reason is one sentence on why, kept for the log and for a person who
	// wonders why a question did or did not reach them.
	Reason string `json:"reason"`
	// Audience is set when Disposition is needs_input.
	Audience msg.SignalAudience `json:"audience"`
	// CustomerReply is set when Audience is customer: the drafted reply,
	// minus the recipient, which triage resolves from the card's mail rather
	// than asking the model to guess.
	CustomerReply *struct {
		Subject string `json:"subject"`
		Body    string `json:"body"`
	} `json:"customer_reply"`
}

// triageQuestion is one question as triage sees it, whichever producer
// raised it.
type triageQuestion struct {
	Question string
	Context  string
	Options  []string
}

// cardContext is what triage knows about the card the worker is on: its text,
// and the mail it was filed from when that can be resolved.
type cardContext struct {
	Card *kanbanclient.LinkedCard
	// Customer is the sender of the card's oldest linked mail — the person
	// who asked for the work. Nil when the card has no mail link or mailstack
	// could not be asked.
	Customer *mailstackclient.MessageHeaders
}

// questionTriage runs the triage call. Zero value is unusable; build it with
// newQuestionTriage. runOneShot is injected like the classifier's, so the
// type knows nothing about the server that owns it.
type questionTriage struct {
	runOneShot func(context.Context, msg.OneShotRequest) ([]byte, error)

	// model and timeout follow the signal_classifier.* settings, which the
	// operator may change while the server runs. Read them through current.
	tuningMutex sync.RWMutex
	model       string
	timeout     time.Duration
}

func (q *questionTriage) current() (model string, timeout time.Duration) {
	q.tuningMutex.RLock()
	defer q.tuningMutex.RUnlock()
	return q.model, q.timeout
}

// retune puts a new model and timeout in force for the next triage.
func (q *questionTriage) retune(model string, timeout time.Duration) {
	q.tuningMutex.Lock()
	defer q.tuningMutex.Unlock()
	q.model, q.timeout = model, timeout
}

func newQuestionTriage(model string, timeout time.Duration, runOneShot func(context.Context, msg.OneShotRequest) ([]byte, error)) *questionTriage {
	return &questionTriage{model: model, timeout: timeout, runOneShot: runOneShot}
}

// enabled reports whether triage can run at all. Off when there is no model:
// the same switch that turns the turn-end classifier off turns this off, and
// with it off a surfacing worker's question reaches the card untriaged rather
// than being denied — surfacing is the purpose's decision, triage only
// filters.
func (q *questionTriage) enabled() bool {
	if q == nil || q.runOneShot == nil {
		return false
	}
	model, _ := q.current()
	return model != ""
}

const triageSystemPrompt = `You triage a question that an autonomous coding agent has asked while working a ticket. Nobody is watching the agent. Decide whether a person needs to see the question, and if so, who.

Return exactly one disposition:

- "signoff" — the agent is asking for permission or confirmation it could take as given: "shall I proceed?", "is this plan okay?", "should I go ahead and open the pull request?", "do you want me to continue?". The safe answer is "yes, proceed", and a person would give it without reading. Also "signoff" when the agent asks which of two ways to do something that the ticket does not care about and either way is fine.
- "needs_input" — the question has a real choice in it that the agent should not make alone: two readings of the request that lead to different work, a missing fact the agent cannot find in the code, a change that would be hard to undo, anything about what the requester actually wants. Prefer "needs_input" when in doubt; a wrong sign-off costs more than a question asked.

For "needs_input", also say who can answer it:

- "assignee" — the person working the ticket can answer from what they know: a design choice, a repository convention, a priority call, whether to do something the ticket did not ask for.
- "customer" — only the person who asked for the work can answer: what they meant, which of two readings of their request is right, what they want when the request is ambiguous or contradicts what exists.

For "customer", draft the reply to them. Write as the person working the ticket, in plain prose, addressed to the requester by name when the card names them. State the question and the choices in the requester's terms, not in code terms; a customer does not know the repository's file names. Keep it short. subject should read as a reply on the requester's thread ("Re: ..."). Do not invent facts about the code that are not in the question or the card.

reason is one sentence for the log.`

func triageToolSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"disposition": map[string]any{
				"type": "string",
				"enum": []string{string(dispositionSignoff), string(dispositionNeedsInput)},
			},
			"reason": map[string]any{
				"type":      "string",
				"maxLength": 300,
			},
			"audience": map[string]any{
				"type":        "string",
				"enum":        []string{string(msg.SignalAudienceAssignee), string(msg.SignalAudienceCustomer)},
				"description": "Who can answer. Required when disposition is needs_input.",
			},
			"customer_reply": map[string]any{
				"type":        "object",
				"description": "The drafted reply. Required when audience is customer.",
				"properties": map[string]any{
					"subject": map[string]any{"type": "string", "maxLength": 200},
					"body":    map[string]any{"type": "string"},
				},
				"required": []string{"subject", "body"},
			},
		},
		"required": []string{"disposition", "reason"},
	}
}

// triage asks the model about one question and returns its verdict. A nil
// verdict with a nil error is impossible.
func (q *questionTriage) triage(ctx context.Context, question triageQuestion, card cardContext) (*questionTriageVerdict, error) {
	if !q.enabled() {
		return nil, fmt.Errorf("triage: not enabled")
	}
	schema, err := json.Marshal(triageToolSchema())
	if err != nil {
		return nil, fmt.Errorf("marshal triage schema: %w", err)
	}
	triageModel, _ := q.current()
	raw, err := q.runOneShot(ctx, msg.OneShotRequest{
		Prompt:       triagePrompt(question, card),
		SystemPrompt: triageSystemPrompt,
		Model:        triageModel,
		Schema:       schema,
		MaxTokens:    triageMaxTokens,
	})
	if err != nil {
		return nil, err
	}
	var reply msg.OneShotResponse
	if err := json.Unmarshal(raw, &reply); err != nil {
		return nil, fmt.Errorf("decode oneshot reply: %w", err)
	}
	if len(reply.Parsed) == 0 {
		return nil, fmt.Errorf("triage: oneshot returned no schema-conformant output (stop_reason=%q)", reply.StopReason)
	}
	var verdict questionTriageVerdict
	if err := json.Unmarshal(reply.Parsed, &verdict); err != nil {
		return nil, fmt.Errorf("decode triage verdict: %w", err)
	}
	switch verdict.Disposition {
	case dispositionSignoff:
		return &verdict, nil
	case dispositionNeedsInput:
	default:
		return nil, fmt.Errorf("triage returned unknown disposition %q", verdict.Disposition)
	}
	switch verdict.Audience {
	case msg.SignalAudienceAssignee:
	case msg.SignalAudienceCustomer:
		if verdict.CustomerReply == nil || strings.TrimSpace(verdict.CustomerReply.Body) == "" {
			return nil, fmt.Errorf("triage judged the question for the customer and drafted no reply")
		}
	default:
		return nil, fmt.Errorf("triage returned needs_input with audience %q", verdict.Audience)
	}
	return &verdict, nil
}

// triagePrompt renders the question and what is known about the card.
func triagePrompt(question triageQuestion, card cardContext) string {
	var b strings.Builder
	b.WriteString("THE QUESTION THE AGENT ASKED\n\n")
	b.WriteString(strings.TrimSpace(question.Question))
	b.WriteString("\n")
	if c := strings.TrimSpace(question.Context); c != "" {
		b.WriteString("\nContext the agent gave:\n")
		b.WriteString(c)
		b.WriteString("\n")
	}
	if len(question.Options) > 0 {
		b.WriteString("\nChoices the agent offered:\n")
		for _, opt := range question.Options {
			b.WriteString("- ")
			b.WriteString(opt)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nTHE TICKET\n\n")
	if card.Card == nil {
		b.WriteString("(the agent is not linked to a ticket)\n")
	} else {
		b.WriteString(strings.TrimSpace(card.Card.Title))
		b.WriteString("\n")
		if body := strings.TrimSpace(card.Card.Body); body != "" {
			b.WriteString("\n")
			b.WriteString(body)
			b.WriteString("\n")
		}
	}
	b.WriteString("\nTHE REQUESTER\n\n")
	if card.Customer == nil {
		b.WriteString("(no mail is linked to the ticket, or it could not be read; if the reply must go to a customer, draft it without a name)\n")
	} else {
		name := strings.TrimSpace(card.Customer.From.Name)
		if name == "" {
			name = card.Customer.From.Email
		}
		fmt.Fprintf(&b, "%s <%s>\nTheir subject line: %s\n", name, card.Customer.From.Email, card.Customer.Subject)
	}
	return b.String()
}

// cardContextForSession gathers what triage may know about the card a session
// works: the card's text, and the sender of the oldest mail linked to it.
//
// Every miss is a legible outcome, not a failure: a session with no card is
// triaged against nothing, a card with no mail link has no requester to name,
// and a mailstack that cannot be asked leaves the requester unresolved. The
// prompt says which of these happened, so the model drafts accordingly rather
// than inventing a name.
func (s *Server) cardContextForSession(ctx context.Context, sessionID string) cardContext {
	var out cardContext
	if s.kanbanClient == nil {
		return out
	}
	card, err := s.kanbanClient.LinkedCardForSession(ctx, sessionID)
	if err != nil {
		log.Printf("[triage] %s: resolve linked card: %v", sessionID, err)
		return out
	}
	if card == nil {
		return out
	}
	out.Card = card
	if s.mailstackClient == nil {
		return out
	}
	links, err := s.kanbanClient.CardLinks(ctx, card.ID)
	if err != nil {
		log.Printf("[triage] %s: read card %s links: %v", sessionID, card.ID, err)
		return out
	}
	for _, link := range links {
		if link.EntityType != "email" {
			continue
		}
		accountID, providerID, err := mailstackclient.ParseLocator(link.EntityRef)
		if err != nil {
			log.Printf("[triage] %s: card %s: %v", sessionID, card.ID, err)
			continue
		}
		headers, err := s.mailstackClient.GetMessageHeaders(ctx, accountID, providerID)
		if err != nil {
			log.Printf("[triage] %s: card %s: read mail %s: %v", sessionID, card.ID, link.EntityRef, err)
			continue
		}
		out.Customer = headers
		break
	}
	return out
}

// customerReplyDraftFor turns a needs_input/customer verdict into the draft
// stored on the signal. To comes from the resolved mail, never from the model.
func customerReplyDraftFor(verdict *questionTriageVerdict, card cardContext) *msg.SignalCustomerReplyDraft {
	if verdict == nil || verdict.Audience != msg.SignalAudienceCustomer || verdict.CustomerReply == nil {
		return nil
	}
	draft := &msg.SignalCustomerReplyDraft{
		Subject: strings.TrimSpace(verdict.CustomerReply.Subject),
		Body:    strings.TrimSpace(verdict.CustomerReply.Body),
	}
	if card.Customer != nil {
		draft.To = card.Customer.From.Email
	}
	return draft
}

// sessionSurfacesQuestions reports whether this session is one whose questions
// go to a person instead of being denied: unattended, and of a purpose the
// registry says has somewhere for a question to land.
func sessionSurfacesQuestions(sess *store.Session) bool {
	return sess != nil && sess.Type == msg.SessionTypeAutonomous && msg.PurposeSurfacesQuestions(sess.Purpose)
}

// triageWithTimeout runs triage under the configured timeout. Errors are
// returned, not swallowed: the caller decides what an untriaged question
// means for its producer.
func (s *Server) triageWithTimeout(question triageQuestion, sessionID string) (*questionTriageVerdict, cardContext, error) {
	if !s.questionTriage.enabled() {
		return nil, cardContext{}, fmt.Errorf("triage is not enabled")
	}
	_, triageTimeout := s.questionTriage.current()
	ctx, cancel := context.WithTimeout(context.Background(), triageTimeout)
	defer cancel()
	card := s.cardContextForSession(ctx, sessionID)
	verdict, err := s.questionTriage.triage(ctx, question, card)
	return verdict, card, err
}

// recordCardWaiting writes waiting_started onto the card a surfaced question
// landed on, so the card's budget clock pauses while the ball is with the
// person answering. Best-effort: the timeline is a view of the work, and a
// store that cannot be reached must not cost the question itself.
func (s *Server) recordCardWaiting(cardID, sessionID, summary string) {
	if s.kanbanClient == nil || cardID == "" {
		return
	}
	detail, _ := json.Marshal(map[string]string{"entity_type": "session", "entity_ref": sessionID})
	err := s.kanbanClient.PostCardEvent(context.Background(), cardID, kanbanclient.CardEvent{
		Kind:    "waiting_started",
		Actor:   "service:llm-bridge-server",
		Summary: summary,
		Detail:  detail,
	})
	if err != nil {
		log.Printf("[triage] %s: record waiting_started on card %s: %v", sessionID, cardID, err)
	}
}

// recordCardWaitingEnded is the other half: the answer arrived, the worker
// has the ball again.
func (s *Server) recordCardWaitingEnded(cardID, sessionID, summary string) {
	if s.kanbanClient == nil || cardID == "" {
		return
	}
	detail, _ := json.Marshal(map[string]string{"entity_type": "session", "entity_ref": sessionID})
	err := s.kanbanClient.PostCardEvent(context.Background(), cardID, kanbanclient.CardEvent{
		Kind:    "waiting_ended",
		Actor:   "service:llm-bridge-server",
		Summary: summary,
		Detail:  detail,
	})
	if err != nil {
		log.Printf("[triage] %s: record waiting_ended on card %s: %v", sessionID, cardID, err)
	}
}
