package server

// Session actions: buttons a session's agent puts in the chat, which this
// server runs when a person confirms one.
//
// The agent offers one of a fixed set of action types (msg.SessionActionTypes)
// with its label and the one argument its type needs; it never supplies a
// command. This server writes Command — exactly what a confirm runs — from the
// offer, and the chat shows it under the button. Confirming takes the
// session's owner: the agent's session posting token opens the offer route
// and not the run route, so an agent cannot press its own button.
//
// Every change to an action is a row update in session_actions, which is the
// log of each press, and an EventSessionAction carrying the whole record, so
// the chat shows the button where it was offered and each later step where it
// happened.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

const (
	// maximumSessionActionLabelCharacters keeps a label to what fits on a
	// button; the detail belongs in the message or in Command.
	maximumSessionActionLabelCharacters = 120
	// maximumSessionActionOfferBytes bounds an offer's body. A message to
	// send is the only long field.
	maximumSessionActionOfferBytes = 256 << 10
	// sessionActionDeployTimeout is how long a deploy may run before it is
	// killed and recorded as failed.
	sessionActionDeployTimeout = 30 * time.Minute
	// sessionActionStoreRequestTimeout bounds each call to repo-store and the
	// scheduler.
	sessionActionStoreRequestTimeout = 30 * time.Second
)

// handleOfferSessionAction is POST /sessions/{id}/actions. The body is a
// msg.SessionActionOffer. Answers 201 and the msg.SessionAction, waiting for a
// person to confirm it.
func (s *Server) handleOfferSessionAction(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	var offer msg.SessionActionOffer
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maximumSessionActionOfferBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&offer); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_session_action_offer", "body is not a session action offer: "+err.Error())
		return
	}
	if problem := sessionActionOfferProblem(offer); problem != "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_session_action_offer", problem)
		return
	}
	command, refusal := s.describeSessionAction(r.Context(), session, offer)
	if refusal != nil {
		refusal.write(w)
		return
	}
	action := store.SessionAction{
		SessionID: session.SessionID,
		Offer:     offer,
		Command:   command,
		State:     msg.SessionActionOffered,
		OfferedAt: time.Now().UTC(),
	}
	if err := s.store.InsertSessionAction(&action); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "record_session_action", err.Error())
		return
	}
	log.Printf("[session-actions] %s offered in %s: %q, %s", action.ActionID, session.SessionID, offer.Label, command)
	if err := s.broadcastSessionAction(action); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "record_session_action_event", fmt.Sprintf(
			"%s is stored and listed at GET /sessions/%s/actions, but its session_action event was not written, so the chat does not show it: %v",
			action.ActionID, session.SessionID, err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	if err := json.NewEncoder(w).Encode(action); err != nil {
		log.Printf("[session-actions] write %s answer: %v", action.ActionID, err)
	}
}

// handleListSessionActions is GET /sessions/{id}/actions: every action offered
// in the session and how each run went, oldest first.
func (s *Server) handleListSessionActions(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	actions, err := s.store.ListSessionActions(session.SessionID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "list_session_actions", err.Error())
		return
	}
	writeJSON(w, actions)
}

// handleRunSessionAction is POST /sessions/{id}/actions/{action_id}/run: a
// person confirmed the action. It is started and answered 202 with the record
// as running; how the run ends arrives as a session_action event and in the
// record. An action runs once: a second confirm is 409.
func (s *Server) handleRunSessionAction(w http.ResponseWriter, r *http.Request) {
	session, err := s.store.GetSession(r.PathValue("id"))
	if err != nil {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	actionID := r.PathValue("action_id")
	offered, err := s.store.GetSessionAction(session.SessionID, actionID)
	if errors.Is(err, store.ErrSessionActionNotFound) {
		http.Error(w, "session action not found", http.StatusNotFound)
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "read_session_action", err.Error())
		return
	}
	// What a confirm runs is what the person was shown. Described again now,
	// so a repo that moved or a scheduler job that was changed since the offer
	// is refused rather than run under the old description.
	command, refusal := s.describeSessionAction(r.Context(), session, offered.Offer)
	if refusal != nil {
		refusal.write(w)
		return
	}
	if command != offered.Command {
		writeJSONError(w, http.StatusConflict, "session_action_changed", fmt.Sprintf(
			"what %s would run has changed since it was offered, so it was not run. It was offered as: %s. It would now run: %s. Ask the agent to offer it again.",
			actionID, offered.Command, command))
		return
	}

	runByPrincipalID, _ := principalIdentityOfRequest(r)
	running, err := s.store.StartSessionAction(session.SessionID, actionID, runByPrincipalID, time.Now().UTC())
	if errors.Is(err, store.ErrSessionActionNotOffered) {
		current, readErr := s.store.GetSessionAction(session.SessionID, actionID)
		if readErr != nil {
			writeJSONError(w, http.StatusInternalServerError, "read_session_action", readErr.Error())
			return
		}
		writeJSONError(w, http.StatusConflict, "session_action_already_run", fmt.Sprintf(
			"%s is %s, not waiting to be confirmed; an action runs once", actionID, current.State))
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "start_session_action", err.Error())
		return
	}
	log.Printf("[session-actions] %s in %s confirmed by %s: running %s", actionID, session.SessionID, describePrincipalForLog(runByPrincipalID), running.Command)
	if err := s.broadcastSessionAction(running); err != nil {
		log.Printf("[session-actions] ERROR %s: running event not written: %v", actionID, err)
	}

	go s.runSessionAction(session, running)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	if err := json.NewEncoder(w).Encode(running); err != nil {
		log.Printf("[session-actions] write %s answer: %v", actionID, err)
	}
}

// runSessionAction does a confirmed action's work and records how it ended.
func (s *Server) runSessionAction(session *store.Session, action store.SessionAction) {
	outcome := s.performSessionAction(session, action)
	state := msg.SessionActionSucceeded
	if outcome.err != nil {
		state = msg.SessionActionFailed
	}
	errorText := ""
	if outcome.err != nil {
		errorText = outcome.err.Error()
	}
	finished, err := s.store.FinishSessionAction(session.SessionID, action.ActionID, state, outcome.output, errorText, outcome.resultSessionID, time.Now().UTC())
	if err != nil {
		log.Printf("[session-actions] ERROR %s in %s ended %s (%s) but the end was not recorded: %v", action.ActionID, session.SessionID, state, errorText, err)
		return
	}
	log.Printf("[session-actions] %s in %s %s%s", action.ActionID, session.SessionID, state, suffixIfNotEmpty(": ", errorText))
	if err := s.broadcastSessionAction(finished); err != nil {
		log.Printf("[session-actions] ERROR %s: %s event not written: %v", action.ActionID, state, err)
	}
}

type sessionActionOutcome struct {
	output          string
	resultSessionID string
	err             error
}

func (s *Server) performSessionAction(session *store.Session, action store.SessionAction) sessionActionOutcome {
	offer := action.Offer
	switch offer.Type {
	case msg.SessionActionDeploy:
		return s.runDeployAction(session, action)
	case msg.SessionActionRunSchedulerJob:
		return s.runSchedulerJobAction(offer.SchedulerJobID)
	case msg.SessionActionSendMessage:
		output, err := s.sendMessageInternally(session.SessionID, offer.Message)
		return sessionActionOutcome{output: output, err: err}
	case msg.SessionActionForkAndSend:
		return s.forkAndSendAction(session, offer)
	case msg.SessionActionNewSessionAndSend:
		return s.newSessionAndSendAction(session, offer)
	}
	return sessionActionOutcome{err: fmt.Errorf("unknown action type %q", offer.Type)}
}

// sessionActionOfferProblem says what is wrong with an offer on its own, or ""
// when nothing is. Whether the repo or job it names exists is asked of its
// owner afterwards.
func sessionActionOfferProblem(offer msg.SessionActionOffer) string {
	label := strings.TrimSpace(offer.Label)
	switch {
	case label == "":
		return "label is required: it is the button's text"
	case len([]rune(offer.Label)) > maximumSessionActionLabelCharacters:
		return fmt.Sprintf("label is longer than %d characters; put the detail in the message", maximumSessionActionLabelCharacters)
	case strings.IndexFunc(offer.Label, unicode.IsControl) >= 0:
		return "label cannot contain a control character"
	}
	if !slices.Contains(msg.SessionActionTypes, offer.Type) {
		names := make([]string, len(msg.SessionActionTypes))
		for index, actionType := range msg.SessionActionTypes {
			names[index] = string(actionType)
		}
		return fmt.Sprintf("type %q is not an action type; it must be one of %s", offer.Type, strings.Join(names, ", "))
	}
	needsRepo := offer.Type == msg.SessionActionDeploy
	needsJob := offer.Type == msg.SessionActionRunSchedulerJob
	needsMessage := !needsRepo && !needsJob
	switch {
	case needsRepo != (offer.RepoID != 0):
		if needsRepo {
			return "a deploy action needs repo_id, repo-store's id of the repo"
		}
		return fmt.Sprintf("repo_id is only for a deploy action, not %s", offer.Type)
	case needsJob != (offer.SchedulerJobID != 0):
		if needsJob {
			return "a run_scheduler_job action needs scheduler_job_id, the scheduler's id of the job"
		}
		return fmt.Sprintf("scheduler_job_id is only for a run_scheduler_job action, not %s", offer.Type)
	case needsMessage != (strings.TrimSpace(offer.Message) != ""):
		if needsMessage {
			return fmt.Sprintf("a %s action needs message, the text to send", offer.Type)
		}
		return fmt.Sprintf("message is not used by a %s action", offer.Type)
	case offer.RepoID < 0 || offer.SchedulerJobID < 0:
		return "repo_id and scheduler_job_id are positive ids"
	}
	return ""
}

// sessionActionRefusal is why an action cannot be described, and so cannot be
// offered or run.
type sessionActionRefusal struct {
	status  int
	code    string
	message string
}

func (refusal *sessionActionRefusal) write(w http.ResponseWriter) {
	writeJSONError(w, refusal.status, refusal.code, refusal.message)
}

// describeSessionAction writes Command for an offer: exactly what confirming
// it runs, read from the stores that own the repo and the job.
func (s *Server) describeSessionAction(ctx context.Context, session *store.Session, offer msg.SessionActionOffer) (string, *sessionActionRefusal) {
	switch offer.Type {
	case msg.SessionActionDeploy:
		repo, refusal := s.deployableRepo(ctx, offer.RepoID)
		if refusal != nil {
			return "", refusal
		}
		return fmt.Sprintf("run `bash -l -c ./deploy.sh` in %s (repo-store repo %d, %s)", repo.Path, repo.ID, repo.Name), nil
	case msg.SessionActionRunSchedulerJob:
		job, refusal := s.schedulerJob(ctx, offer.SchedulerJobID)
		if refusal != nil {
			return "", refusal
		}
		what := job.Command
		if job.Type == "agent" {
			what = fmt.Sprintf("the %s agent with this prompt: %s", job.Agent, job.Prompt)
		}
		return fmt.Sprintf("POST %s — start scheduler job %d, %s (a %s job), which runs %s",
			s.schedulerJobRunURL(job.ID), job.ID, job.Name, job.Type, what), nil
	case msg.SessionActionSendMessage:
		return fmt.Sprintf("send session %s this message: %s", session.SessionID, offer.Message), nil
	case msg.SessionActionForkAndSend:
		return fmt.Sprintf("fork session %s into a new session named %q, and send the fork this message: %s", session.SessionID, offer.Label, offer.Message), nil
	case msg.SessionActionNewSessionAndSend:
		return fmt.Sprintf("start a new session named %q — %s harness, instance %s, agent %s, principal %s, working directory %s, none of session %s's history — and send it this message: %s",
			offer.Label, session.Harness, orNone(session.InstanceID), orNone(session.AgentID), orNone(session.PrincipalID), orInstanceDefault(session.WorkingDir),
			session.SessionID, offer.Message), nil
	}
	return "", &sessionActionRefusal{http.StatusBadRequest, "invalid_session_action_offer", fmt.Sprintf("type %q is not an action type", offer.Type)}
}

// repoStoreRepo is the part of repo-store's repo record a deploy reads.
type repoStoreRepo struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"`
}

// deployableRepo reads a repo from repo-store and checks it has a deploy.sh
// this server can run.
func (s *Server) deployableRepo(ctx context.Context, repoID int64) (repoStoreRepo, *sessionActionRefusal) {
	if s.cfg.RepoStoreURL == "" {
		return repoStoreRepo{}, &sessionActionRefusal{http.StatusServiceUnavailable, "repo_store_not_configured",
			"LLMBRIDGE_REPO_STORE_URL is not set, so this server cannot find a repo to deploy"}
	}
	var repo repoStoreRepo
	if refusal := s.getJSONForSessionAction(ctx, fmt.Sprintf("%s/repos/%d", strings.TrimRight(s.cfg.RepoStoreURL, "/"), repoID), "repo-store", &repo); refusal != nil {
		return repoStoreRepo{}, refusal
	}
	if repo.ID != repoID || repo.Path == "" {
		return repoStoreRepo{}, &sessionActionRefusal{http.StatusBadGateway, "repo_store_answer_unusable",
			fmt.Sprintf("repo-store answered repo %d with id %d and path %q", repoID, repo.ID, repo.Path)}
	}
	info, err := os.Stat(filepath.Join(repo.Path, "deploy.sh"))
	if err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
		return repoStoreRepo{}, &sessionActionRefusal{http.StatusUnprocessableEntity, "repo_has_no_deploy_script",
			fmt.Sprintf("repo %d (%s) has no executable deploy.sh in %s", repo.ID, repo.Name, repo.Path)}
	}
	return repo, nil
}

// schedulerJobRecord is the part of the scheduler's job record an action reads.
type schedulerJobRecord struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Command string `json:"command"`
	Agent   string `json:"agent"`
	Prompt  string `json:"prompt"`
}

func (s *Server) schedulerJob(ctx context.Context, jobID int64) (schedulerJobRecord, *sessionActionRefusal) {
	if s.cfg.SchedulerURL == "" {
		return schedulerJobRecord{}, &sessionActionRefusal{http.StatusServiceUnavailable, "scheduler_not_configured",
			"LLMBRIDGE_SCHEDULER_URL is not set, so this server cannot run a scheduler job"}
	}
	var job schedulerJobRecord
	if refusal := s.getJSONForSessionAction(ctx, fmt.Sprintf("%s/api/jobs/%d", strings.TrimRight(s.cfg.SchedulerURL, "/"), jobID), "the scheduler", &job); refusal != nil {
		return schedulerJobRecord{}, refusal
	}
	if job.ID != jobID {
		return schedulerJobRecord{}, &sessionActionRefusal{http.StatusBadGateway, "scheduler_answer_unusable",
			fmt.Sprintf("the scheduler answered job %d with id %d", jobID, job.ID)}
	}
	return job, nil
}

func (s *Server) schedulerJobRunURL(jobID int64) string {
	return fmt.Sprintf("%s/api/jobs/%d/run", strings.TrimRight(s.cfg.SchedulerURL, "/"), jobID)
}

// getJSONForSessionAction reads one record from a store. A 404 is the
// caller's mistake (422: the offer names a record that does not exist); any
// other failure is the store's (502).
func (s *Server) getJSONForSessionAction(ctx context.Context, address, storeName string, into any) *sessionActionRefusal {
	ctx, cancel := context.WithTimeout(ctx, sessionActionStoreRequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return &sessionActionRefusal{http.StatusInternalServerError, "store_request", err.Error()}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return &sessionActionRefusal{http.StatusBadGateway, "store_unavailable", fmt.Sprintf("%s did not answer GET %s: %v", storeName, address, err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return &sessionActionRefusal{http.StatusBadGateway, "store_unavailable", fmt.Sprintf("%s answer to GET %s was cut off: %v", storeName, address, err)}
	}
	if response.StatusCode == http.StatusNotFound {
		return &sessionActionRefusal{http.StatusUnprocessableEntity, "unknown_record", fmt.Sprintf("%s has no record at %s: %s", storeName, address, strings.TrimSpace(string(body)))}
	}
	if response.StatusCode != http.StatusOK {
		return &sessionActionRefusal{http.StatusBadGateway, "store_refused", fmt.Sprintf("%s answered GET %s with %d: %s", storeName, address, response.StatusCode, strings.TrimSpace(string(body)))}
	}
	if err := json.Unmarshal(body, into); err != nil {
		return &sessionActionRefusal{http.StatusBadGateway, "store_answer_unusable", fmt.Sprintf("%s answer to GET %s is not the record expected: %v", storeName, address, err)}
	}
	return nil
}

// runDeployAction runs the repo's deploy.sh, which runs deploy-gate like any
// other deploy. The child gets this server's environment without its secrets,
// as a harness child does, plus LLM_BRIDGE_SESSION_ID — so a deploy that
// detaches, as llm-bridge-server's own does, reports its outcome to the
// session — and AI_AGENT, which deploy-gate writes into the deploy ledger as
// who deployed.
func (s *Server) runDeployAction(session *store.Session, action store.SessionAction) sessionActionOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), sessionActionDeployTimeout)
	defer cancel()
	repo, refusal := s.deployableRepo(ctx, action.Offer.RepoID)
	if refusal != nil {
		return sessionActionOutcome{err: errors.New(refusal.message)}
	}
	command := exec.CommandContext(ctx, "bash", "-l", "-c", "./deploy.sh")
	command.Dir = repo.Path
	command.Env = append(childprocessenv.EnvironmentWithoutServerSecrets(),
		"LLM_BRIDGE_SESSION_ID="+session.SessionID,
		fmt.Sprintf("AI_AGENT=chat button %s confirmed by %s", action.ActionID, describePrincipalForLog(action.RunByPrincipalID)),
	)
	output, err := command.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return sessionActionOutcome{output: string(output), err: fmt.Errorf("deploy.sh ran longer than %s and was killed", sessionActionDeployTimeout)}
	}
	if err != nil {
		return sessionActionOutcome{output: string(output), err: fmt.Errorf("deploy.sh in %s failed: %w", repo.Path, err)}
	}
	return sessionActionOutcome{output: string(output)}
}

func (s *Server) runSchedulerJobAction(jobID int64) sessionActionOutcome {
	if s.cfg.SchedulerURL == "" {
		return sessionActionOutcome{err: errors.New("LLMBRIDGE_SCHEDULER_URL is not set, so this server cannot run a scheduler job")}
	}
	ctx, cancel := context.WithTimeout(context.Background(), sessionActionStoreRequestTimeout)
	defer cancel()
	address := s.schedulerJobRunURL(jobID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, nil)
	if err != nil {
		return sessionActionOutcome{err: err}
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return sessionActionOutcome{err: fmt.Errorf("the scheduler did not answer POST %s: %w", address, err)}
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return sessionActionOutcome{output: string(body), err: fmt.Errorf("the scheduler's answer to POST %s was cut off: %w", address, err)}
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return sessionActionOutcome{output: string(body), err: fmt.Errorf("the scheduler answered POST %s with %d", address, response.StatusCode)}
	}
	return sessionActionOutcome{output: string(body)}
}

func (s *Server) forkAndSendAction(session *store.Session, offer msg.SessionActionOffer) sessionActionOutcome {
	status, body := s.serveInternally(http.MethodPost, "/sessions/"+url.PathEscape(session.SessionID)+"/fork", ForkSessionRequest{DisplayName: offer.Label})
	if status != http.StatusCreated {
		return sessionActionOutcome{output: string(body), err: fmt.Errorf("fork of %s refused with %d", session.SessionID, status)}
	}
	return s.sendToCreatedSession(body, offer.Message)
}

// newSessionAndSendAction starts a session set up as this one is — harness,
// instance, agent, principal, bundle, working directory, harness config and
// spend ceiling — with none of its history. The bundle and the ceiling are
// carried for the reason a fork carries them: otherwise a button would be how
// a session sheds its restrictions.
func (s *Server) newSessionAndSendAction(session *store.Session, offer msg.SessionActionOffer) sessionActionOutcome {
	request := CreateSessionRequest{
		Harness:       session.Harness,
		InstanceID:    session.InstanceID,
		DisplayName:   offer.Label,
		AgentID:       session.AgentID,
		PrincipalID:   session.PrincipalID,
		BundleID:      session.BundleID,
		Type:          session.Type,
		Purpose:       session.Purpose,
		Origin:        session.Origin,
		HarnessConfig: session.HarnessConfig,
		WorkingDir:    session.WorkingDir,
	}
	if session.MaxBudgetUSD > 0 {
		ceiling := session.MaxBudgetUSD
		request.MaxBudget = &ceiling
	}
	status, body := s.serveInternally(http.MethodPost, "/sessions", request)
	if status != http.StatusCreated && status != http.StatusOK {
		return sessionActionOutcome{output: string(body), err: fmt.Errorf("new session refused with %d", status)}
	}
	return s.sendToCreatedSession(body, offer.Message)
}

// sendToCreatedSession sends message to the session a create or fork
// answered with.
func (s *Server) sendToCreatedSession(createdBody []byte, message string) sessionActionOutcome {
	var created store.Session
	if err := json.Unmarshal(createdBody, &created); err != nil || created.SessionID == "" {
		return sessionActionOutcome{output: string(createdBody), err: fmt.Errorf("the created session's record does not name it: %v", err)}
	}
	output, err := s.sendMessageInternally(created.SessionID, message)
	if err != nil {
		return sessionActionOutcome{output: output, resultSessionID: created.SessionID, err: fmt.Errorf("session %s was created but the message was not sent: %w", created.SessionID, err)}
	}
	return sessionActionOutcome{output: output, resultSessionID: created.SessionID}
}

func (s *Server) sendMessageInternally(sessionID, message string) (string, error) {
	status, body := s.serveInternally(http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/send", SendMessageRequest{Message: message})
	if status != http.StatusOK {
		return string(body), fmt.Errorf("send to %s refused with %d: %s", sessionID, status, strings.TrimSpace(string(body)))
	}
	return string(body), nil
}

// serveInternally runs one request through this server's own routes, past
// request authorization — the person who confirmed the action was authorized
// for its session already — so an action sends, forks and creates exactly as
// those routes do when a person calls them.
func (s *Server) serveInternally(method, path string, body any) (int, []byte) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return http.StatusInternalServerError, []byte(err.Error())
	}
	request := httptest.NewRequest(method, path, strings.NewReader(string(encoded)))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.mux.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.Bytes()
}

func (s *Server) broadcastSessionAction(action store.SessionAction) error {
	event := msg.Event{
		Type:            msg.EventSessionAction,
		BridgeSessionID: action.SessionID,
		Timestamp:       time.Now().UTC(),
		SessionAction:   &action,
	}
	_, err := s.harness.BroadcastEvent(&event)
	return err
}

// settleSessionActionsLeftRunning marks every action a previous process left
// running as outcome_unknown, since nobody saw its run end, and tells each
// session.
func (s *Server) settleSessionActionsLeftRunning() {
	settled, err := s.store.MarkRunningSessionActionsOutcomeUnknown(
		"llm-bridge-server stopped while this ran, so how it ended was not seen", time.Now().UTC())
	if err != nil {
		log.Printf("[session-actions] ERROR settling actions left running: %v", err)
	}
	for _, action := range settled {
		log.Printf("[session-actions] %s in %s was running when the server stopped: outcome unknown", action.ActionID, action.SessionID)
		if err := s.broadcastSessionAction(action); err != nil {
			log.Printf("[session-actions] ERROR %s: outcome_unknown event not written: %v", action.ActionID, err)
		}
	}
}

func describePrincipalForLog(principalID string) string {
	if principalID == "" {
		return "the internal service"
	}
	return principalID
}

func suffixIfNotEmpty(separator, text string) string {
	if text == "" {
		return ""
	}
	return separator + text
}

func orInstanceDefault(workingDirectory string) string {
	if workingDirectory == "" {
		return "the instance's default"
	}
	return workingDirectory
}
