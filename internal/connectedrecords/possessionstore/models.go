package possessionstore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

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

// BridgeModelCaller asks through llm-bridge-server's POST /oneshot, which
// picks the role's model and instance, and prices the answer from
// model-store's list prices.
type BridgeModelCaller struct {
	BridgeURL     string
	ServiceToken  string
	ModelStoreURL string
	HTTPClient    *http.Client

	pricesMutex sync.Mutex
	prices      map[string]modelPrice
	pricesRead  time.Time
}

type modelPrice struct {
	InputCost  float64 `json:"input_cost"`  // dollars per million tokens
	OutputCost float64 `json:"output_cost"` // dollars per million tokens
}

// Ask makes one call.
func (caller *BridgeModelCaller) Ask(ctx context.Context, role, systemPrompt, prompt string, schema json.RawMessage, images []msg.OneShotImage, maxTokens int) (ModelAnswer, error) {
	if caller.ServiceToken == "" {
		return ModelAnswer{}, fmt.Errorf("LLMBRIDGE_SERVICE_TOKEN is not set, so the photo readers cannot be asked")
	}
	body, err := json.Marshal(msg.OneShotRequest{Prompt: prompt, SystemPrompt: systemPrompt, ModelRole: role, Caller: ServiceName,
		Schema: schema, MaxTokens: maxTokens, Images: images})
	if err != nil {
		return ModelAnswer{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, caller.BridgeURL+"/oneshot", bytes.NewReader(body))
	if err != nil {
		return ModelAnswer{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-LLM-Bridge-Service-Token", caller.ServiceToken)
	response, err := caller.HTTPClient.Do(request)
	if err != nil {
		return ModelAnswer{}, fmt.Errorf("POST /oneshot (role %s): %w", role, err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(response.Body)
	if err != nil {
		return ModelAnswer{}, err
	}
	if response.StatusCode != http.StatusOK {
		return ModelAnswer{}, fmt.Errorf("POST /oneshot (role %s): %d %s", role, response.StatusCode, bytes.TrimSpace(answer))
	}
	var reply msg.OneShotResponse
	if err := json.Unmarshal(answer, &reply); err != nil {
		return ModelAnswer{}, fmt.Errorf("POST /oneshot (role %s) answered something that is not a OneShotResponse: %w", role, err)
	}
	if len(reply.Parsed) == 0 {
		return ModelAnswer{}, fmt.Errorf("%s (role %s) answered without the structured output: %q", reply.Model, role, reply.Text)
	}
	price, err := caller.price(ctx, reply.Model)
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

// price reads model-store's list price for a model, refreshing the table at
// most every ten minutes. A model model-store does not know is an error: a
// reading with a made-up cost would read as measured.
func (caller *BridgeModelCaller) price(ctx context.Context, modelID string) (modelPrice, error) {
	caller.pricesMutex.Lock()
	defer caller.pricesMutex.Unlock()
	if caller.prices == nil || time.Since(caller.pricesRead) > 10*time.Minute {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, caller.ModelStoreURL+"/api/models", nil)
		if err != nil {
			return modelPrice{}, err
		}
		response, err := caller.HTTPClient.Do(request)
		if err != nil {
			return modelPrice{}, fmt.Errorf("read model-store prices: %w", err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return modelPrice{}, fmt.Errorf("read model-store prices: status %d", response.StatusCode)
		}
		var models []struct {
			ID string `json:"id"`
			modelPrice
		}
		if err := json.NewDecoder(response.Body).Decode(&models); err != nil {
			return modelPrice{}, fmt.Errorf("read model-store prices: %w", err)
		}
		caller.prices = map[string]modelPrice{}
		for _, model := range models {
			caller.prices[model.ID] = model.modelPrice
		}
		caller.pricesRead = time.Now()
	}
	price, known := caller.prices[modelID]
	if !known {
		return modelPrice{}, fmt.Errorf("model-store has no price for %s", modelID)
	}
	return price, nil
}
