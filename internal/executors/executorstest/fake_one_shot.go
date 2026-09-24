// Package executorstest holds fakes for testing code that runs the model
// executors without a harness.
package executorstest

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/executors"
	"github.com/kayushkin/llm-bridge-server/internal/kanbanclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// FakeOneShot stands in for a harness instance. A classification request —
// recognised by its schema — is answered item by item: on each axis the item
// gets the values whose names appear in its text, or the axis's first value
// when the axis is required and none appears. Any other request is answered
// with Text.
type FakeOneShot struct {
	InstanceID   string
	DefaultModel string
	Text         string
	Usage        msg.TokenUsage
	// DelayPerCall pauses before answering, so progress can be watched.
	DelayPerCall time.Duration
	// Err, when set, fails every call.
	Err error

	mutex sync.Mutex
	calls []msg.OneShotRequest
}

// CompletionTarget implements executors.OneShotCaller: the requested model,
// or DefaultModel, sent to InstanceID. It resolves nothing — a name is its
// own id — so a test of resolution uses the server's.
func (f *FakeOneShot) CompletionTarget(requestedModel string) (executors.CompletionTarget, error) {
	model := requestedModel
	if model == "" {
		model = f.DefaultModel
	}
	if model == "" {
		return executors.CompletionTarget{}, &executors.TargetError{Code: "no_model", Message: "the fake has no default model"}
	}
	return executors.CompletionTarget{RequestedModel: model, ModelID: model, Provider: "test", InstanceID: f.InstanceID}, nil
}

// Calls returns the requests made so far.
func (f *FakeOneShot) Calls() []msg.OneShotRequest {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]msg.OneShotRequest(nil), f.calls...)
}

// RunOneShot implements executors.OneShotCaller.
func (f *FakeOneShot) RunOneShot(ctx context.Context, instanceID string, request msg.OneShotRequest) (msg.OneShotResponse, error) {
	f.mutex.Lock()
	f.calls = append(f.calls, request)
	f.mutex.Unlock()
	if instanceID != f.InstanceID {
		return msg.OneShotResponse{}, errors.New("no such instance " + instanceID)
	}
	if f.DelayPerCall > 0 {
		select {
		case <-ctx.Done():
			return msg.OneShotResponse{}, ctx.Err()
		case <-time.After(f.DelayPerCall):
		}
	}
	if f.Err != nil {
		return msg.OneShotResponse{}, f.Err
	}
	response := msg.OneShotResponse{Model: request.Model, Usage: f.Usage, StopReason: "end_turn"}
	if parsed, ok := answerClassification(request); ok {
		response.Parsed = parsed
		return response, nil
	}
	response.Text = f.Text
	return response, nil
}

func answerClassification(request msg.OneShotRequest) (json.RawMessage, bool) {
	var schema struct {
		Properties struct {
			Items struct {
				Items struct {
					Properties struct {
						Values struct {
							Properties map[string]struct {
								Items struct {
									Enum []string `json:"enum"`
								} `json:"items"`
							} `json:"properties"`
						} `json:"values"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"items"`
		} `json:"properties"`
	}
	if len(request.Schema) == 0 || json.Unmarshal(request.Schema, &schema) != nil {
		return nil, false
	}
	axes := schema.Properties.Items.Items.Properties.Values.Properties
	if len(axes) == 0 {
		return nil, false
	}
	_, itemsJSON, found := strings.Cut(request.Prompt, "\n")
	var items []struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if !found || json.Unmarshal([]byte(itemsJSON), &items) != nil {
		return nil, false
	}
	type answer struct {
		ID         string              `json:"id"`
		Values     map[string][]string `json:"values"`
		Confidence float64             `json:"confidence"`
		Rationale  string              `json:"rationale"`
	}
	answers := []answer{}
	for _, item := range items {
		text := strings.ToLower(item.Text)
		values := map[string][]string{}
		for axisName, axis := range axes {
			values[axisName] = []string{}
			for _, value := range axis.Items.Enum {
				if strings.Contains(text, strings.ToLower(value)) {
					values[axisName] = append(values[axisName], value)
				}
			}
			if len(values[axisName]) > 1 {
				values[axisName] = values[axisName][:1]
			}
		}
		answers = append(answers, answer{ID: item.ID, Values: values, Confidence: 0.8, Rationale: "the text names it"})
	}
	encoded, _ := json.Marshal(map[string]any{"items": answers})
	return encoded, true
}

// FakeTaxonomies is a set of boards and the taxonomies they keep. A board
// missing from Boards is kanbanclient.ErrNotFound; one mapped to nil keeps
// no taxonomy. ViewersByBoard, when set for a board, limits which principals
// may read it.
type FakeTaxonomies struct {
	Boards         map[string]*msg.ClassificationTaxonomy
	ViewersByBoard map[string][]string
}

// BoardTaxonomy implements executors.TaxonomyReader.
func (f FakeTaxonomies) BoardTaxonomy(_ context.Context, boardID, principalID string) (msg.ClassificationTaxonomy, string, error) {
	taxonomy, known := f.Boards[boardID]
	if viewers, limited := f.ViewersByBoard[boardID]; limited && principalID != "" {
		allowed := false
		for _, viewer := range viewers {
			allowed = allowed || viewer == principalID
		}
		if !allowed {
			return msg.ClassificationTaxonomy{}, "", kanbanclient.ErrNotFound
		}
	}
	switch {
	case !known:
		return msg.ClassificationTaxonomy{}, "", kanbanclient.ErrNotFound
	case taxonomy == nil:
		return msg.ClassificationTaxonomy{}, "", kanbanclient.ErrBoardHasNoTaxonomy
	}
	return *taxonomy, "2026-09-23T10:00:00Z", nil
}
