package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/oneshotrouting"
	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge/msg"
)

type BoardClassificationStore interface {
	ClassificationSettings(context.Context, string, string) (msg.BoardClassification, error)
	ClassificationCard(context.Context, string, string, string) (json.RawMessage, error)
	PublishClassification(context.Context, msg.BoardClassificationDecisionPublication, http.Header) (json.RawMessage, error)
}

type BoardClassifier struct {
	Caller OneShotCaller
	Store  BoardClassificationStore
}

func (BoardClassifier) Describe() msg.OperationTypeDescription {
	return msg.OperationTypeDescription{Type: msg.OperationTypeBoardClassificationRun, Executor: "board-model-classifier", Description: "Classify one stored card and publish a reviewable board decision. Human review owns labels.", MaximumAttempts: 1, TimeoutSeconds: 900}
}
func boardInput(raw json.RawMessage) (msg.BoardClassificationRunInput, error) {
	var in msg.BoardClassificationRunInput
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return in, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return in, errors.New("input must contain exactly one JSON object")
	}
	if strings.TrimSpace(in.BoardID) == "" || strings.TrimSpace(in.CardID) == "" {
		return in, errors.New("board_id and card_id are required")
	}
	return in, nil
}
func (BoardClassifier) Validate(intent msg.OperationIntent) error {
	_, err := boardInput(intent.Input)
	return err
}

// captureCaller retains the route and the answer of the one routed call, so
// the decision records which candidate answered and what the harness said.
type captureCaller struct {
	OneShotCaller
	route  oneshotrouting.Route
	answer msg.OneShotResponse
}

func (c *captureCaller) OneShotRoute(model string) (oneshotrouting.Route, error) {
	route, err := c.OneShotCaller.OneShotRoute(model)
	c.route = route
	return route, err
}
func (c *captureCaller) RunOneShotRoute(ctx context.Context, route oneshotrouting.Route, req msg.OneShotRequest, admit func(oneshotrouting.Candidate) string) (msg.OneShotResponse, error) {
	r, err := c.OneShotCaller.RunOneShotRoute(ctx, route, req, admit)
	c.answer = r
	return r, err
}

// answeredCandidate is the route candidate that produced the answer: the
// router records one attempt (skipped or failed) per candidate before it.
func (c *captureCaller) answeredCandidate() (oneshotrouting.Candidate, error) {
	index := len(c.answer.Attempts)
	if index >= len(c.route.Candidates) {
		return oneshotrouting.Candidate{}, fmt.Errorf("answer follows %d attempts but the route has %d candidates", index, len(c.route.Candidates))
	}
	return c.route.Candidates[index], nil
}

func (c BoardClassifier) Execute(ctx context.Context, intent msg.OperationIntent, writer operations.ReceiptWriter) operations.Result {
	in, err := boardInput(intent.Input)
	if err != nil {
		return failed("invalid_input", err.Error())
	}
	if c.Store == nil {
		return failed("kanban_store_unavailable", "Board classification requires kanban-store.")
	}
	if intent.PrincipalID == "" {
		return failed("principal_required", "Board classification requires the initiating person's identity.")
	}
	settings, err := c.Store.ClassificationSettings(ctx, in.BoardID, intent.PrincipalID)
	if err != nil {
		return failed("board_unavailable", err.Error())
	}
	allowed := false
	for _, action := range settings.CallerActions {
		if action == msg.BoardClassificationActionClassify {
			allowed = true
		}
	}
	if !allowed {
		return failed("classification_not_allowed", "Board edit permission is required.")
	}
	if settings.OrganizationID == "" || settings.OrganizationID != intent.OrganizationID {
		return failed("organization_mismatch", "The operation must use the board's configured organization.")
	}
	if settings.Taxonomy == nil || settings.Policy == nil {
		return failed("classification_not_configured", "The board needs taxonomy and per-axis modes.")
	}
	taxonomy := settings.Taxonomy.Selectable()
	axes := []msg.ClassificationAxis{}
	for _, axis := range taxonomy.Axes {
		if settings.Policy.ModeOf(axis.ID) != msg.ClassificationAxisModeOff {
			axes = append(axes, axis)
		}
	}
	taxonomy.Axes = axes
	if len(axes) == 0 {
		return failed("classification_off", "All board classification axes are off.")
	}
	raw, err := c.Store.ClassificationCard(ctx, in.BoardID, in.CardID, intent.PrincipalID)
	if err != nil {
		return failed("card_unavailable", err.Error())
	}
	source, err := classificationSource(raw, in.BoardID, in.CardID)
	if err != nil {
		return failed("invalid_card_source", err.Error())
	}
	text, err := json.Marshal(source)
	if err != nil {
		return failed("invalid_card_source", err.Error())
	}
	input, err := json.Marshal(msg.ClassificationRunInput{Taxonomy: &taxonomy, Model: in.Model, Items: []msg.ClassificationItem{{ID: in.CardID, Text: string(text)}}})
	if err != nil {
		return failed("invalid_input", err.Error())
	}
	caller := &captureCaller{OneShotCaller: c.Caller}
	nested := intent
	nested.Input = input
	result := (ModelClassifier{Caller: caller}).Execute(ctx, nested, writer)
	if result.State != msg.OperationStateSucceeded {
		if result.Error != nil {
			result.Error.Retryable = false
		}
		return result
	}
	var classified msg.ClassificationRunResult
	if err := json.Unmarshal(result.Result, &classified); err != nil {
		return failed("invalid_result", err.Error())
	}
	if len(classified.Items) != 1 || classified.Items[0].ID != in.CardID {
		return failed("invalid_result", "Classifier did not return the requested card.")
	}
	answer := classified.Items[0]
	// Keep omitted confidence absent, and preserve the model's reported value.
	var reported struct {
		Items []struct {
			ID         string   `json:"id"`
			Confidence *float64 `json:"confidence"`
		} `json:"items"`
	}
	if err := json.Unmarshal(caller.answer.Parsed, &reported); err != nil {
		return failed("invalid_result", err.Error())
	}
	var confidence *float64
	for _, item := range reported.Items {
		if item.ID == in.CardID {
			confidence = item.Confidence
		}
	}
	selections := map[string][]string{}
	for _, axis := range taxonomy.Axes {
		selections[axis.ID] = []string{}
		for _, name := range answer.Values[axis.Name] {
			found := false
			for _, value := range axis.Values {
				if value.Name == name {
					selections[axis.ID] = append(selections[axis.ID], value.ID)
					found = true
					break
				}
			}
			if !found {
				return failed("invalid_result", "Classifier selected a value outside the taxonomy.")
			}
		}
	}
	answered, err := caller.answeredCandidate()
	if err != nil {
		return failed("invalid_result", err.Error())
	}
	digest, err := source.Digest()
	if err != nil {
		return failed("source_digest_failed", err.Error())
	}
	publication := msg.BoardClassificationDecisionPublication{BoardID: in.BoardID, CardID: in.CardID, OperationID: writer.OperationID(), CheckpointID: writer.OperationID() + ":card:" + in.CardID, AttemptNumber: 1, InitiatingPrincipalID: intent.PrincipalID,
		Source: source, SourceDigest: digest, TaxonomyRevision: settings.TaxonomyRevision, TaxonomyDigest: settings.TaxonomyDigest, PolicyRevision: settings.Policy.Revision, PromptRevision: "board-model-classifier-v1", Origin: msg.ClassificationDecisionOriginModel,
		Model: msg.ClassificationModelEvidence{RequestedModel: in.Model, ResolvedModelID: answered.ModelID, Provider: answered.Provider, AnsweredByModel: caller.answer.Model}, Selections: selections, Rationale: answer.Rationale, ModelReportedConfidence: confidence, ReviewReasons: answer.Evidence, NeedsReview: answer.NeedsReview, CompletedAt: time.Now().UTC()}
	checkpoint, err := json.Marshal(publication)
	if err != nil {
		return failed("checkpoint_failed", err.Error())
	}
	// Persist the exact proposal before its publication leaves this process.
	if err := writer.AddEvidence(msg.OperationEvidence{Kind: "classification_checkpoint", Summary: "Completed model proposal awaiting board publication", Detail: checkpoint}); err != nil {
		return writeFailure(err)
	}
	target := msg.OperationReference{EntityType: "board", EntityID: in.BoardID}
	number, err := writer.RecordEffect(target, "publish_classification_decision")
	if err != nil {
		return writeFailure(err)
	}
	decision, err := c.Store.PublishClassification(ctx, publication, writer.EffectHeaders(number))
	if err != nil {
		return operations.Result{State: msg.OperationStateUnknown, Error: &msg.OperationError{Code: "publication_unconfirmed", Message: err.Error()}}
	}
	if err := writer.SettleEffect(number, msg.OperationEffectOutcomeApplied, ""); err != nil {
		return writeFailure(err)
	}
	return operations.Result{State: msg.OperationStateSucceeded, Result: decision}
}

// Decode the owner's raw response without maintaining a second card/page type.
func classificationSource(raw json.RawMessage, boardID, cardID string) (msg.ClassificationSourceSnapshot, error) {
	var source msg.ClassificationSourceSnapshot
	var page map[string]json.RawMessage
	if err := json.Unmarshal(raw, &page); err != nil {
		return source, err
	}
	var board map[string]json.RawMessage
	if err := json.Unmarshal(page["board"], &board); err != nil {
		return source, err
	}
	var got string
	if err := json.Unmarshal(board["id"], &got); err != nil || got != boardID {
		return source, errors.New("card page belongs to another board")
	}
	var cards []map[string]json.RawMessage
	if err := json.Unmarshal(page["cards"], &cards); err != nil {
		return source, err
	}
	if len(cards) != 1 {
		return source, errors.New("card is not on the selected board")
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(cards[0]["item"], &item); err != nil {
		return source, err
	}
	if err := json.Unmarshal(item["id"], &got); err != nil || got != cardID {
		return source, errors.New("card page returned another item")
	}
	for key, dest := range map[string]*string{"title": &source.Subject, "body": &source.Body, "updated_at": &source.SourceRevision} {
		if err := json.Unmarshal(item[key], dest); err != nil {
			return source, fmt.Errorf("card %s: %w", key, err)
		}
	}
	if strings.TrimSpace(source.Body) == "" {
		return source, errors.New("card body is empty")
	}
	return source, nil
}
func (BoardClassifier) Reconcile(context.Context, msg.OperationReceipt) operations.ReconcileResult {
	return operations.ReconcileResult{Action: operations.ReconcileSettle, State: msg.OperationStateUnknown, Error: &msg.OperationError{Code: "classification_interrupted", Message: "Inspect the durable checkpoint and board decisions. An interrupted model call or publication is never automatically repeated."}}
}
func (BoardClassifier) Cancel(context.Context, msg.OperationReceipt) error { return nil }
