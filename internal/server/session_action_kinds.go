package server

// The session actions that do work of their own rather than calling another
// store: run_command (a shell command, reviewed by a model when offered),
// model_call (one model question) and background_agent (a capped session
// whose final reply is the action's output).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/childprocessenv"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/oneshotrouting"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge/msg"
)

const (
	// sessionActionCommandTimeout is how long a run_command or a deploy may
	// run before it is killed and recorded as failed.
	sessionActionCommandTimeout = 30 * time.Minute
	// sessionActionBackgroundAgentTimeout is how long a background_agent's
	// session may take to give its final reply before the action stops
	// waiting. The session itself carries on.
	sessionActionBackgroundAgentTimeout = 60 * time.Minute
	// sessionActionModelCallMaximumOutputTokens bounds a model_call's answer
	// whatever its cap would allow.
	sessionActionModelCallMaximumOutputTokens = 32000
	// sessionActionModelCallMinimumOutputTokens is the least worth asking for;
	// a cap that buys fewer is refused at offer.
	sessionActionModelCallMinimumOutputTokens = 64
	// sessionActionReviewTimeout bounds the reviewer's call, which the agent's
	// offer waits on.
	sessionActionReviewTimeout = 3 * time.Minute
)

// sessionActionModels is what model_call and the command reviewer need from
// the model side: resolve a role or model to a route, call it, and price it. The server is
// its own; a test hands in a fake.
type sessionActionModels interface {
	executors.OneShotCaller
	ModelListPrice(model string) (inputPerMillion, outputPerMillion float64, known bool)
}

// sessionActionWorkingDirectory is where a run_command runs: the offer's
// directory, or the session's own working directory as its spawn resolves it.
func (s *Server) sessionActionWorkingDirectory(session *store.Session, offer msg.SessionActionOffer) (string, *sessionActionRefusal) {
	directory := offer.WorkingDirectory
	if directory == "" {
		var instance *msg.Instance
		if session.InstanceID != "" && s.harnessStore != nil {
			found, err := s.harnessStore.GetInstance(session.InstanceID)
			if err != nil {
				return "", &sessionActionRefusal{http.StatusBadGateway, "instance_unreadable", fmt.Sprintf("instance %s: %v", session.InstanceID, err)}
			}
			instance = found
		}
		directory, _ = harness.WorkingDirForSession(session, instance)
	}
	if directory == "" {
		return "", &sessionActionRefusal{http.StatusUnprocessableEntity, "no_working_directory",
			fmt.Sprintf("session %s has no working directory of its own; say working_directory", session.SessionID)}
	}
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return "", &sessionActionRefusal{http.StatusUnprocessableEntity, "working_directory_missing", fmt.Sprintf("%s is not a directory", directory)}
	}
	return directory, nil
}

// runCommandAction runs a run_command's shell command in its directory.
func (s *Server) runCommandAction(session *store.Session, action store.SessionAction) sessionActionOutcome {
	directory, refusal := s.sessionActionWorkingDirectory(session, action.Offer)
	if refusal != nil {
		return sessionActionOutcome{err: errors.New(refusal.message)}
	}
	return s.runShellForSessionAction(session, action, directory, action.Offer.ShellCommand)
}

// runShellForSessionAction runs script with bash -l -c in directory. The child
// gets this server's environment without its secrets, as a harness child
// does, plus LLM_BRIDGE_SESSION_ID — so a deploy that detaches, as
// llm-bridge-server's own does, reports its outcome to the session — and
// AI_AGENT, which deploy-gate writes into the deploy ledger as who deployed.
func (s *Server) runShellForSessionAction(session *store.Session, action store.SessionAction, directory, script string) sessionActionOutcome {
	ctx, cancel := context.WithTimeout(context.Background(), sessionActionCommandTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "bash", "-l", "-c", script)
	command.Dir = directory
	command.Env = append(childprocessenv.EnvironmentWithoutServerSecrets(),
		"LLM_BRIDGE_SESSION_ID="+session.SessionID,
		fmt.Sprintf("AI_AGENT=chat button %s confirmed by %s", action.ActionID, describePrincipalForLog(action.RunByPrincipalID)),
	)
	output, err := command.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return sessionActionOutcome{output: string(output), err: fmt.Errorf("it ran longer than %s and was killed", sessionActionCommandTimeout)}
	}
	if err != nil {
		return sessionActionOutcome{output: string(output), err: fmt.Errorf("it failed in %s: %w", directory, err)}
	}
	return sessionActionOutcome{output: string(output)}
}

// sessionActionModelCallPlan is a model_call resolved: the models it may
// try, their prices, and how many output tokens its cap buys on the dearest.
type sessionActionModelCallPlan struct {
	route               oneshotrouting.Route
	listPricesByModel   map[string][2]float64
	maximumOutputTokens int
}

// sessionActionModelCall resolves a model_call's model — a role, id or alias —
// and turns its dollar cap into an output-token limit at list price. A role
// may fall back to another model, so every model on the route must have a
// price and the limit is what the cap buys on the dearest of them. The
// prompt's input is counted at one token per three bytes, which overcounts
// English, so the cap holds.
func (s *Server) sessionActionModelCall(offer msg.SessionActionOffer) (sessionActionModelCallPlan, *sessionActionRefusal) {
	route, err := s.sessionActionModels.OneShotRoute(offer.Model)
	if err != nil {
		var targetError *executors.TargetError
		if errors.As(err, &targetError) {
			return sessionActionModelCallPlan{}, &sessionActionRefusal{http.StatusUnprocessableEntity, targetError.Code, targetError.Message}
		}
		return sessionActionModelCallPlan{}, &sessionActionRefusal{http.StatusServiceUnavailable, "model_unresolvable", err.Error()}
	}
	plan := sessionActionModelCallPlan{route: route, listPricesByModel: map[string][2]float64{}, maximumOutputTokens: sessionActionModelCallMaximumOutputTokens}
	estimatedInputTokens := float64(len(offer.Message))/3 + 1
	for _, candidate := range route.Candidates {
		inputPrice, outputPrice, known := s.sessionActionModels.ModelListPrice(candidate.ModelID)
		if !known || outputPrice <= 0 {
			return sessionActionModelCallPlan{}, &sessionActionRefusal{http.StatusUnprocessableEntity, "model_has_no_price",
				fmt.Sprintf("model-store has no list price for %s, so a spending limit on it cannot be kept", candidate.ModelID)}
		}
		plan.listPricesByModel[candidate.ModelID] = [2]float64{inputPrice, outputPrice}
		remainingUSD := offer.MaximumCostUSD - estimatedInputTokens*inputPrice/1e6
		outputTokens := int(math.Floor(remainingUSD * 1e6 / outputPrice))
		if outputTokens < sessionActionModelCallMinimumOutputTokens {
			return sessionActionModelCallPlan{}, &sessionActionRefusal{http.StatusUnprocessableEntity, "cost_limit_too_low",
				fmt.Sprintf("$%.4f buys fewer than %d output tokens of %s after this prompt; raise maximum_cost_usd", offer.MaximumCostUSD, sessionActionModelCallMinimumOutputTokens, candidate.ModelID)}
		}
		plan.maximumOutputTokens = min(plan.maximumOutputTokens, outputTokens)
	}
	return plan, nil
}

// listPriceOf is what a call's tokens cost at list price on the model that
// answered. Cache tokens are priced as input: model-store holds no cache
// prices.
func (plan sessionActionModelCallPlan) listPriceOf(modelID string, usage msg.TokenUsage) (float64, error) {
	prices, known := plan.listPricesByModel[modelID]
	if !known {
		return 0, fmt.Errorf("%s answered, and it is not a model this call was priced for", modelID)
	}
	inputTokens := usage.InputTokens + usage.CacheReadTokens + usage.CacheWriteTokens
	return float64(inputTokens)*prices[0]/1e6 + float64(usage.OutputTokens)*prices[1]/1e6, nil
}

func (s *Server) modelCallAction(offer msg.SessionActionOffer) sessionActionOutcome {
	plan, refusal := s.sessionActionModelCall(offer)
	if refusal != nil {
		return sessionActionOutcome{err: errors.New(refusal.message)}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	response, err := s.sessionActionModels.RunOneShotRoute(ctx, plan.route, msg.OneShotRequest{
		Prompt:    offer.Message,
		MaxTokens: plan.maximumOutputTokens,
		Caller:    oneShotCallerSessionActionModel,
	}, nil)
	if err != nil {
		return sessionActionOutcome{err: err}
	}
	costUSD, err := plan.listPriceOf(response.Model, response.Usage)
	if err != nil {
		return sessionActionOutcome{output: response.Text, err: err}
	}
	return sessionActionOutcome{output: response.Text, costUSD: costUSD}
}

// sessionActionReviewSchema forces the reviewer's answer into a verdict and
// its reasons.
var sessionActionReviewSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "verdict": {"type": "string", "enum": ["approve", "caution", "reject"]},
    "reasons": {"type": "string"}
  },
  "required": ["verdict", "reasons"],
  "additionalProperties": false
}`)

const sessionActionReviewInstructions = `You review a shell command an AI agent wants to put behind a button in a chat. A person will see the button's label, your verdict and your reasons, and may then press it; the command then runs with bash -l -c, as the host's user, in the directory given, with access to every repo and service on the machine.

Judge two things: does the command do what the label says, and could it do harm the label does not admit.

- approve: it does what the label says, and nothing it does is hard to undo (reading, listing, querying, building, testing).
- caution: it does what the label says, but it changes something that matters — commits, pushes, deploys, deletes, restarts a service, sends a message to a person, spends money. Say exactly what it changes.
- reject: it does not do what the label says, hides what it does, fetches and runs code from the internet, reads or prints credentials, deletes data it has no reason to, or you cannot tell what it does.

Reasons: two or three plain sentences a busy person reads in ten seconds.`

// reviewSessionActionCommand asks the reviewer model what it makes of a
// run_command. A reviewer that cannot be reached refuses the offer: a command
// shown with no review would look reviewed.
func (s *Server) reviewSessionActionCommand(ctx context.Context, session *store.Session, offer msg.SessionActionOffer, directory string) (*msg.SessionActionReview, *sessionActionRefusal) {
	modelRole := s.settings.ModelRole(config.SettingSessionActionsReviewModelRole)
	if modelRole == "" {
		return nil, &sessionActionRefusal{http.StatusServiceUnavailable, "no_review_model",
			"session_actions.review_model_role is empty, so no command can be reviewed, and an unreviewed command is not offered"}
	}
	route, err := s.sessionActionModels.OneShotRoute(modelRole)
	if err != nil {
		return nil, &sessionActionRefusal{http.StatusServiceUnavailable, "review_model_unresolvable", err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, sessionActionReviewTimeout)
	defer cancel()
	prompt := fmt.Sprintf("Label: %s\nDirectory: %s\nSession: %s\nCommand:\n%s", offer.Label, directory, session.SessionID, offer.ShellCommand)
	response, err := s.sessionActionModels.RunOneShotRoute(ctx, route, msg.OneShotRequest{
		Prompt:       prompt,
		SystemPrompt: sessionActionReviewInstructions,
		Schema:       sessionActionReviewSchema,
		Caller:       oneShotCallerSessionActionsReview,
	}, nil)
	if err != nil {
		return nil, &sessionActionRefusal{http.StatusBadGateway, "review_failed", fmt.Sprintf("the reviewer (role %s) could not be asked: %v", modelRole, err)}
	}
	var answer struct {
		Verdict msg.SessionActionReviewVerdict `json:"verdict"`
		Reasons string                         `json:"reasons"`
	}
	if err := json.Unmarshal(response.Parsed, &answer); err != nil {
		return nil, &sessionActionRefusal{http.StatusBadGateway, "review_unreadable", fmt.Sprintf("the reviewer's answer is not a verdict: %v (%s)", err, response.Text)}
	}
	switch answer.Verdict {
	case msg.SessionActionReviewApprove, msg.SessionActionReviewCaution, msg.SessionActionReviewReject:
	default:
		return nil, &sessionActionRefusal{http.StatusBadGateway, "review_unreadable", fmt.Sprintf("the reviewer answered verdict %q", answer.Verdict)}
	}
	return &msg.SessionActionReview{Verdict: answer.Verdict, Reasons: strings.TrimSpace(answer.Reasons), Model: response.Model, ReviewedAt: time.Now().UTC()}, nil
}

// backgroundAgentAction starts a session set up as this one is — harness,
// instance, agent, principal, bundle, working directory, harness config — with
// the offer's spend ceiling and, when it names one, its model; sends it the
// task; and waits for its final reply, which becomes the action's output.
// The session's id is recorded as soon as it exists, so the chat can show it
// while it works.
func (s *Server) backgroundAgentAction(session *store.Session, action store.SessionAction) sessionActionOutcome {
	offer := action.Offer
	harnessConfig, err := harnessConfigWithModel(session.HarnessConfig, offer.Model)
	if err != nil {
		return sessionActionOutcome{err: err}
	}
	ceiling := offer.MaximumCostUSD
	request := CreateSessionRequest{
		Harness:       session.Harness,
		InstanceID:    session.InstanceID,
		DisplayName:   offer.Label,
		AgentID:       session.AgentID,
		PrincipalID:   session.PrincipalID,
		BundleID:      session.BundleID,
		Type:          msg.SessionTypeAutonomous,
		Purpose:       msg.PurposeSessionAction,
		Origin:        "llm-bridge-server",
		HarnessConfig: harnessConfig,
		WorkingDir:    session.WorkingDir,
		MaxBudget:     &ceiling,
	}
	status, body := s.serveInternally(http.MethodPost, "/sessions", request)
	if status != http.StatusCreated && status != http.StatusOK {
		return sessionActionOutcome{output: string(body), err: fmt.Errorf("the background session was refused with %d", status)}
	}
	var created store.Session
	if err := json.Unmarshal(body, &created); err != nil || created.SessionID == "" {
		return sessionActionOutcome{output: string(body), err: fmt.Errorf("the created session's record does not name it: %v", err)}
	}

	// Subscribed before the task is sent, so its reply cannot slip past.
	events := s.harness.Subscribe(created.SessionID)
	defer s.harness.Unsubscribe(created.SessionID, events)

	if recorded, err := s.store.SetSessionActionResultSession(session.SessionID, action.ActionID, created.SessionID); err != nil {
		log.Printf("[session-actions] ERROR %s: %v", action.ActionID, err)
	} else if err := s.broadcastSessionAction(recorded); err != nil {
		log.Printf("[session-actions] ERROR %s: started-session event not written: %v", action.ActionID, err)
	}
	if output, err := s.sendMessageInternally(created.SessionID, offer.Message); err != nil {
		return sessionActionOutcome{output: output, resultSessionID: created.SessionID, err: fmt.Errorf("session %s was created but the task was not sent: %w", created.SessionID, err)}
	}
	return s.waitForFinalReply(created.SessionID, events)
}

// waitForFinalReply reads a session's events until its turn ends. The reading
// is fast on purpose: the subscription drops events a slow reader misses.
func (s *Server) waitForFinalReply(sessionID string, events chan harness.StoredEvent) sessionActionOutcome {
	deadline := time.After(sessionActionBackgroundAgentTimeout)
	for {
		select {
		case event, open := <-events:
			if !open {
				return sessionActionOutcome{resultSessionID: sessionID, err: errors.New("the session's event stream closed before its final reply")}
			}
			switch event.Type {
			case msg.EventResult:
				if event.Result == nil {
					continue
				}
				if event.Result.IsError {
					return sessionActionOutcome{output: event.Result.Text, resultSessionID: sessionID, err: errors.New("the session's turn ended in an error")}
				}
				return sessionActionOutcome{output: event.Result.Text, resultSessionID: sessionID}
			case msg.EventError:
				if event.Error != nil && !event.Error.Retryable {
					return sessionActionOutcome{output: event.Error.Message, resultSessionID: sessionID, err: fmt.Errorf("the session failed: %s", event.Error.Code)}
				}
			case msg.EventTurnComplete:
				return sessionActionOutcome{resultSessionID: sessionID,
					err: errors.New("the session's turn ended without a final reply this server saw; open the session to read it")}
			}
		case <-deadline:
			return sessionActionOutcome{resultSessionID: sessionID,
				err: fmt.Errorf("no final reply within %s; the session is still open", sessionActionBackgroundAgentTimeout)}
		}
	}
}

// harnessConfigWithModel is config with its model replaced by model, and the
// pinned selection dropped so the create resolves the new one. An empty model
// keeps config as it is.
func harnessConfigWithModel(config json.RawMessage, model string) (json.RawMessage, error) {
	if model == "" {
		return config, nil
	}
	fields := map[string]json.RawMessage{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &fields); err != nil {
			return nil, fmt.Errorf("this session's harness_config is not an object: %w", err)
		}
	}
	encodedModel, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	fields[harnessConfigKeyModel] = encodedModel
	delete(fields, harnessConfigKeyModelSelection)
	return json.Marshal(fields)
}
