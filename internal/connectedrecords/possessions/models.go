package possessions

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/kayushkin/llm-bridge/msg"
)

// ModelAnswer is what one reader said, with what it cost.
type ModelAnswer struct {
	Model        string
	Parsed       json.RawMessage
	InputTokens  int // input, cache read and cache write together
	OutputTokens int
	CostUSD      float64
}

// ModelCaller asks a model-store role one question about one picture.
type ModelCaller interface {
	Ask(ctx context.Context, role, systemPrompt, prompt string, schema json.RawMessage, images []msg.OneShotImage, maxTokens int) (ModelAnswer, error)
}

// OneShotModelCaller asks through the bridge's one-shot router, in this
// process, which picks the role's model and instance and falls back down the
// role's list, and prices the answer at model-store's list price for the model
// that answered.
type OneShotModelCaller struct {
	// RunRole is the router's call for a request naming a model role.
	RunRole func(ctx context.Context, request msg.OneShotRequest) (msg.OneShotResponse, error)
	// ListPrice answers a model's list price, dollars per million input and
	// output tokens; a model model-store does not know is an error, since a
	// reading with a made-up cost would read as measured.
	ListPrice func(modelID string) (ModelPrice, error)
}

// ModelPrice is a model's list price, dollars per million tokens.
type ModelPrice struct {
	InputCost  float64
	OutputCost float64
}

// Ask makes one call.
func (caller OneShotModelCaller) Ask(ctx context.Context, role, systemPrompt, prompt string, schema json.RawMessage, images []msg.OneShotImage, maxTokens int) (ModelAnswer, error) {
	reply, err := caller.RunRole(ctx, msg.OneShotRequest{Prompt: prompt, SystemPrompt: systemPrompt, ModelRole: role, Caller: OneShotCallerName,
		Schema: schema, MaxTokens: maxTokens, Images: images})
	if err != nil {
		return ModelAnswer{}, fmt.Errorf("one-shot call (role %s): %w", role, err)
	}
	if len(reply.Parsed) == 0 {
		return ModelAnswer{}, fmt.Errorf("%s (role %s) answered without the structured output: %q", reply.Model, role, reply.Text)
	}
	price, err := caller.ListPrice(reply.Model)
	if err != nil {
		return ModelAnswer{}, err
	}
	inputTokens := reply.Usage.InputTokens + reply.Usage.CacheReadTokens + reply.Usage.CacheWriteTokens
	return ModelAnswer{
		Model:        reply.Model,
		Parsed:       reply.Parsed,
		InputTokens:  inputTokens,
		OutputTokens: reply.Usage.OutputTokens,
		CostUSD:      (float64(inputTokens)*price.InputCost + float64(reply.Usage.OutputTokens)*price.OutputCost) / 1e6,
	}, nil
}

// OneShotCallerName is the Caller on every model call the store makes, which
// the router's logs name.
const OneShotCallerName = "possession-store"
