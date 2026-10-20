package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/ids"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

// The two ways a surfacing worker's question reaches triage, and what each
// verdict does to the worker.
//
//   - Tool path: the worker called AskUserQuestion. The prehook is the moment
//     to answer it — a sign-off is allowed through with the answer filled in,
//     so the worker never notices; a real question is recorded on the card and
//     the tool is DENIED with a message telling the worker to stop. Nothing
//     parks: an unattended worker must not sit holding a process open for an
//     answer that may take a day, and POST /signals/{id}/answer restarts a
//     stopped session to deliver one.
//   - Derived path: the worker ended its turn on a question in prose. A
//     sign-off is answered by sending the worker its next message; a real
//     question is recorded on the card and the session left at awaiting_user,
//     which is the honest state for a session that ended its turn asking.

// surfaceUnattendedQuestion handles an AskUserQuestion prehook from a worker
// whose purpose surfaces questions.
//
// A request carries several questions and resolves once, so the verdict is
// per request: every question a sign-off → allow with every answer filled in;
// any question needing input → the whole request is surfaced, each question
// as its own signal row under one request id, the sign-offs among them
// included so the person sees what the worker asked in full.
func (s *Server) surfaceUnattendedQuestion(w http.ResponseWriter, bridgeID string, sess *store.Session, payload ccPrehookPayload) {
	var input askUserQuestionInput
	if err := json.Unmarshal(payload.ToolInput, &input); err != nil {
		writeHookDeny(w, "AskUserQuestion input could not be read: "+err.Error()+". Ask again with a well-formed question, or stop.")
		return
	}
	if len(input.Questions) == 0 {
		writeHookDeny(w, "AskUserQuestion carried no questions. Ask a specific question, or stop.")
		return
	}

	verdicts := make([]*questionTriageVerdict, len(input.Questions))
	var card cardContext
	allSignoff := true
	for i, question := range input.Questions {
		options := make([]string, 0, len(question.Options))
		for _, opt := range question.Options {
			options = append(options, opt.Label)
		}
		verdict, ctx, err := s.triageWithTimeout(triageQuestion{
			Question: question.Question,
			Context:  question.Header,
			Options:  options,
		}, bridgeID)
		if err != nil {
			// Untriaged is surfaced, not denied: the purpose said this worker's
			// questions go to a person, and triage only filters that. The
			// person sees a question with no audience judged for it.
			log.Printf("[triage] %s: triage failed, surfacing untriaged: %v", bridgeID, err)
			allSignoff = false
			continue
		}
		verdicts[i] = verdict
		card = ctx
		if verdict.Disposition != dispositionSignoff {
			allSignoff = false
		}
	}

	requestID := ids.NewHookRequestID()
	if allSignoff {
		answers := make(map[string]string, len(input.Questions))
		now := time.Now().UTC()
		for i, question := range input.Questions {
			answers[question.Question] = autoSignoffAnswer
			s.recordAutoAnsweredQuestion(bridgeID, sess, requestID, question, verdicts[i], now)
		}
		merged := map[string]any{}
		if err := json.Unmarshal(payload.ToolInput, &merged); err != nil {
			merged = map[string]any{}
		}
		merged["answers"] = answers
		updated, err := json.Marshal(merged)
		if err != nil {
			writeHookDeny(w, "could not encode the sign-off answer: "+err.Error())
			return
		}
		log.Printf("[triage] %s: %d sign-off question(s) answered automatically (%s)", bridgeID, len(input.Questions), verdicts[0].Reason)
		writeHookAllowWithInput(w, "sign-off answered by triage; no person was asked", updated)
		return
	}

	linkedTodoID := ""
	if card.Card != nil {
		linkedTodoID = card.Card.ID
	} else {
		linkedTodoID = s.linkedTodoForSession(bridgeID)
	}
	for i, question := range input.Questions {
		signal := askUserQuestionSignal(bridgeID, sess, requestID, question, linkedTodoID)
		if verdicts[i] != nil && verdicts[i].Disposition == dispositionNeedsInput {
			signal.Audience = verdicts[i].Audience
			signal.CustomerReplyDraft = customerReplyDraftFor(verdicts[i], card)
		}
		if err := s.store.CreateSignal(signal); err != nil {
			log.Printf("[signals] %s/%s: persist surfaced question: %v", bridgeID, requestID, err)
		}
	}
	s.recordCardWaiting(linkedTodoID, bridgeID, "Worker asked: "+strings.TrimSpace(input.Questions[0].Question))
	log.Printf("[triage] %s: %d question(s) surfaced to card %q under request %s", bridgeID, len(input.Questions), linkedTodoID, requestID)

	writeHookDeny(w, "Your question has been recorded on the ticket for the person working it. "+
		"Stop here and end your turn without guessing an answer; the answer will arrive as your next message.")
}

// recordAutoAnsweredQuestion writes the row for a sign-off triage answered,
// already answered, so the card's history shows what the worker asked and
// that a machine answered it.
func (s *Server) recordAutoAnsweredQuestion(bridgeID string, sess *store.Session, requestID string, question askUserQuestionQuestion, verdict *questionTriageVerdict, now time.Time) {
	linkedTodoID := s.linkedTodoForSession(bridgeID)
	signal := askUserQuestionSignal(bridgeID, sess, requestID, question, linkedTodoID)
	signal.State = msg.SignalStateAnswered
	signal.Answer = &msg.SignalAnswer{Text: autoSignoffAnswer}
	signal.ResolvedAt = &now
	if verdict != nil {
		signal.Body = strings.TrimSpace(strings.Join([]string{signal.Body, "Triage: " + verdict.Reason}, "\n\n"))
	}
	if err := s.store.CreateSignal(signal); err != nil {
		log.Printf("[signals] %s/%s: persist auto-answered question: %v", bridgeID, requestID, err)
	}
}

// askUserQuestionSignal builds the open row for one AskUserQuestion question,
// the same shape recordAskUserQuestionSignals mints for a parked ask.
func askUserQuestionSignal(bridgeID string, sess *store.Session, requestID string, question askUserQuestionQuestion, linkedTodoID string) *msg.Signal {
	options := make([]msg.SignalOption, 0, len(question.Options))
	for _, opt := range question.Options {
		options = append(options, msg.SignalOption{Label: opt.Label, Value: opt.Label, Description: opt.Description})
	}
	sessionType := msg.SessionType("")
	if sess != nil {
		sessionType = sess.Type
	}
	return &msg.Signal{
		ID:                   ids.NewSignalID(),
		SessionID:            bridgeID,
		SessionType:          sessionType,
		Kind:                 msg.SignalKindQuestion,
		Source:               msg.SignalSourceTool,
		RequestID:            requestID,
		Surface:              signalSurfaceForSession(sess),
		Title:                question.Question,
		Body:                 question.Header,
		Options:              options,
		AllowFreeform:        true,
		AllowMultipleOptions: question.MultiSelect,
		State:                msg.SignalStateOpen,
		LinkedTodoID:         linkedTodoID,
	}
}

// surfaceDerivedWorkerQuestions handles a turn-end the classifier judged a
// question, on a worker whose purpose surfaces questions.
//
// Triage judges each question, as surfaceUnattendedQuestion does for the
// questions of one AskUserQuestion request: every question a sign-off → the
// worker is told to carry on; any question needing input → every question is
// surfaced as one group, the sign-offs among them included so the person sees
// what the worker asked in full.
func (s *Server) surfaceDerivedWorkerQuestions(sess *store.Session, questions []classifiedQuestion) {
	triageByQuestion := make([]*questionTriageVerdict, len(questions))
	var card cardContext
	allSignoff := true
	for i, question := range questions {
		options := make([]string, 0, len(question.Options))
		for _, option := range question.Options {
			options = append(options, option.Label)
		}
		verdict, ctx, err := s.triageWithTimeout(triageQuestion{
			Question: question.Question,
			Context:  question.Context,
			Options:  options,
		}, sess.SessionID)
		if err != nil {
			log.Printf("[triage] %s: triage failed, surfacing untriaged: %v", sess.SessionID, err)
			allSignoff = false
			continue
		}
		triageByQuestion[i] = verdict
		card = ctx
		if verdict.Disposition != dispositionSignoff {
			allSignoff = false
		}
	}

	if allSignoff {
		count, err := s.autoContinueCount(sess.SessionID)
		if err != nil {
			log.Printf("[triage] %s: count auto-continues: %v", sess.SessionID, err)
		} else if count < maxAutoContinuesPerSession {
			if err := s.autoContinueWorker(sess, questions, triageByQuestion); err != nil {
				log.Printf("[triage] %s: auto-continue failed, surfacing instead: %v", sess.SessionID, err)
			} else {
				return
			}
		} else {
			log.Printf("[triage] %s: %d sign-offs already answered automatically; surfacing this one", sess.SessionID, count)
		}
	}

	// Only a question that needs input has an audience worth recording; a
	// sign-off surfaced alongside one, or past the cap, has none.
	for i, verdict := range triageByQuestion {
		if verdict != nil && verdict.Disposition != dispositionNeedsInput {
			triageByQuestion[i] = nil
		}
	}
	recorded := s.recordDerivedQuestions(sess, questions, triageByQuestion, card)
	if len(recorded) > 0 {
		s.recordCardWaiting(recorded[0].LinkedTodoID, sess.SessionID, "Worker asked: "+recorded[0].Title)
	}
	s.harness.ApplyDerivedSessionState(sess.SessionID, msg.SessionAwaitingUser,
		msg.ReasonTurnCompleteSignalQuestion, msg.SessionIdle, msg.SessionAwaitingUser)
}

// autoContinueWorker answers a turn of derived sign-offs by sending the worker
// its next message, and records each question as an already-answered signal,
// grouped as recordDerivedQuestions groups open ones.
//
// It sends first and records after. Recording first left answered rows behind
// whenever the send failed, and the caller then surfaced the same questions
// as open rows beside them. A row that fails to save after the send is logged
// and not returned: the worker has already carried on, and surfacing its
// questions now would ask a person about a turn that is over.
func (s *Server) autoContinueWorker(sess *store.Session, questions []classifiedQuestion, triageByQuestion []*questionTriageVerdict) error {
	now := time.Now().UTC()
	userEvent := msg.Event{
		Type:            msg.EventUserMessage,
		BridgeSessionID: sess.SessionID,
		Timestamp:       now,
		Result:          &msg.ResultEvent{Text: autoSignoffAnswer},
	}
	if _, err := s.harness.BroadcastEvent(&userEvent); err != nil {
		return fmt.Errorf("persist user_message: %w", err)
	}
	if err := s.harness.Send(sess.SessionID, autoSignoffAnswer, nil); err != nil {
		return err
	}

	requestID := ""
	if len(questions) > 1 {
		requestID = ids.NewDerivedQuestionGroupID()
	}
	for i, question := range questions {
		signal := s.derivedQuestionSignal(sess, question, requestID)
		signal.Body = strings.TrimSpace(strings.Join([]string{question.Context, "Triage: " + triageByQuestion[i].Reason}, "\n\n"))
		signal.State = msg.SignalStateAnswered
		signal.Answer = &msg.SignalAnswer{Text: autoSignoffAnswer}
		signal.ResolvedAt = &now
		if err := s.store.CreateSignal(signal); err != nil {
			log.Printf("[triage] %s: ERROR the worker continued but its auto-answered question %q was not recorded: %v", sess.SessionID, question.Question, err)
		}
	}
	log.Printf("[triage] %s: %d sign-off question(s) answered automatically and the worker continued (%s)", sess.SessionID, len(questions), triageByQuestion[0].Reason)
	return nil
}

// autoContinueCount is how many sign-offs triage has already answered on this
// session, read from the rows it left behind rather than from a counter that
// could drift from them.
func (s *Server) autoContinueCount(sessionID string) (int, error) {
	answered, err := s.store.ListSignals(store.SignalFilter{SessionID: sessionID, State: msg.SignalStateAnswered, Kind: msg.SignalKindQuestion})
	if err != nil {
		return 0, err
	}
	count := 0
	for _, sig := range answered {
		if sig.Answer != nil && sig.Answer.Text == autoSignoffAnswer {
			count++
		}
	}
	return count, nil
}
