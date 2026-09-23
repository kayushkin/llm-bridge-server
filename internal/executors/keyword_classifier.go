// Package executors holds the operation executors this server registers with
// the operations coordinator, one per operation type.
package executors

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/operations"
	"github.com/kayushkin/llm-bridge/msg"
)

// KeywordClassifier runs classification.run by matching each label's keywords
// against each item's text, case-insensitively. It calls no model: it stands
// in for the model-backed classifier so the operation path, the receipts and
// the pages that read them can be built and tested against a real server now.
// Its receipts say "keyword-classifier" in Executor, and a surface must not
// branch on that.
type KeywordClassifier struct {
	// DelayPerItem pauses before each item, so progress can be watched. Zero
	// in production.
	DelayPerItem time.Duration
}

// MaximumClassificationItems bounds one classification.run.
const MaximumClassificationItems = 1000

// Describe implements operations.Executor.
func (KeywordClassifier) Describe() msg.OperationTypeDescription {
	return msg.OperationTypeDescription{
		Type:     msg.OperationTypeClassificationRun,
		Executor: "keyword-classifier",
		Description: "Labels each item whose text contains one of a label's keywords. " +
			"A stand-in for the model-backed classifier; it calls no model.",
		MaximumAttempts: 3,
		TimeoutSeconds:  60,
	}
}

// Validate implements operations.Executor.
func (KeywordClassifier) Validate(intent msg.OperationIntent) error {
	_, err := decodeClassificationInput(intent.Input)
	return err
}

func decodeClassificationInput(raw json.RawMessage) (msg.ClassificationRunInput, error) {
	var input msg.ClassificationRunInput
	if len(raw) == 0 {
		return input, errors.New("classification.run needs input with labels and items")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("classification.run input: %w", err)
	}
	if len(input.Labels) == 0 {
		return input, errors.New("classification.run input needs at least one label")
	}
	if len(input.Items) == 0 {
		return input, errors.New("classification.run input needs at least one item")
	}
	if len(input.Items) > MaximumClassificationItems {
		return input, fmt.Errorf("classification.run takes at most %d items, got %d", MaximumClassificationItems, len(input.Items))
	}
	labelNames := map[string]bool{}
	for index, label := range input.Labels {
		if strings.TrimSpace(label.Name) == "" {
			return input, fmt.Errorf("label %d has no name", index)
		}
		if labelNames[label.Name] {
			return input, fmt.Errorf("label %q appears twice", label.Name)
		}
		labelNames[label.Name] = true
	}
	itemIDs := map[string]bool{}
	for index, item := range input.Items {
		if strings.TrimSpace(item.ID) == "" {
			return input, fmt.Errorf("item %d has no id", index)
		}
		if itemIDs[item.ID] {
			return input, fmt.Errorf("item id %q appears twice", item.ID)
		}
		itemIDs[item.ID] = true
	}
	return input, nil
}

// Execute implements operations.Executor.
func (classifier KeywordClassifier) Execute(ctx context.Context, intent msg.OperationIntent, receipt operations.ReceiptWriter) operations.Result {
	input, err := decodeClassificationInput(intent.Input)
	if err != nil {
		return operations.Result{State: msg.OperationStateFailed, Error: &msg.OperationError{Code: "invalid_input", Message: err.Error()}}
	}
	total := len(input.Items)
	if err := receipt.SetProgress(msg.OperationProgress{Completed: 0, Total: total}); err != nil {
		return writeFailure(err)
	}
	result := msg.ClassificationRunResult{Items: make([]msg.ClassificationItemResult, 0, total)}
	for index, item := range input.Items {
		if classifier.DelayPerItem > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(classifier.DelayPerItem):
			}
		}
		if ctx.Err() != nil {
			// Nothing left this process, so running it again is safe.
			return operations.Result{Error: &msg.OperationError{Code: "attempt_interrupted", Retryable: true,
				Message: fmt.Sprintf("stopped after %d of %d items: %v", index, total, ctx.Err())}}
		}
		result.Items = append(result.Items, classifyByKeywords(item, input.Labels))
		if err := receipt.SetProgress(msg.OperationProgress{Completed: index + 1, Total: total,
			Message: fmt.Sprintf("classified %d of %d", index+1, total)}); err != nil {
			return writeFailure(err)
		}
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return operations.Result{State: msg.OperationStateFailed, Error: &msg.OperationError{Code: "result_not_encodable", Message: err.Error()}}
	}
	return operations.Result{State: msg.OperationStateSucceeded, Result: encoded}
}

func writeFailure(err error) operations.Result {
	return operations.Result{Error: &msg.OperationError{Code: "receipt_write_failed", Retryable: true, Message: err.Error()}}
}

func classifyByKeywords(item msg.ClassificationItem, labels []msg.ClassificationLabel) msg.ClassificationItemResult {
	text := strings.ToLower(item.Text)
	answer := msg.ClassificationItemResult{ID: item.ID, Labels: []string{}}
	for _, label := range labels {
		for _, keyword := range label.Keywords {
			if keyword == "" || !strings.Contains(text, strings.ToLower(keyword)) {
				continue
			}
			detail, _ := json.Marshal(map[string]string{"label": label.Name, "keyword": keyword})
			answer.Labels = append(answer.Labels, label.Name)
			answer.Evidence = append(answer.Evidence, msg.OperationEvidence{
				Kind: "keyword_match", Summary: fmt.Sprintf("%q matched label %s", keyword, label.Name),
				Reference: item.Reference, Detail: detail})
			break
		}
	}
	if len(answer.Labels) > 0 {
		// A keyword hit is a strong but crude signal; the number says so and
		// no more.
		answer.Confidence = 0.6
	}
	return answer
}

// Reconcile implements operations.Executor. Classifying by keyword changes
// nothing outside this process, so running it again is always safe.
func (KeywordClassifier) Reconcile(context.Context, msg.OperationReceipt) operations.ReconcileResult {
	return operations.ReconcileResult{Action: operations.ReconcileRunAgain}
}

// Cancel implements operations.Executor. There is nothing outside this
// process to stop.
func (KeywordClassifier) Cancel(context.Context, msg.OperationReceipt) error { return nil }
