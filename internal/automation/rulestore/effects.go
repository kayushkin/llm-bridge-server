package rulestore

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// EffectCaller makes one planned effect's call to its owner.
type EffectCaller interface {
	Call(ctx context.Context, effect PlannedEffect) EffectResult
}

// OwnerCalls calls llm-bridge-server and kanban-store as the internal
// service, with the same tokens rule-store reads them with.
type OwnerCalls struct {
	Owners map[string]OwnerEndpoint // by PlannedEffect.Service
	Client *http.Client
}

// OwnerEndpoint is where a service is and the header that authenticates a
// call to it.
type OwnerEndpoint struct {
	BaseURL     string
	TokenHeader string
	Token       string
}

// NewOwnerCalls builds the caller from the bridge and kanban readers' settings.
func NewOwnerCalls(bridge *LLMBridge, kanban *KanbanStore) *OwnerCalls {
	return &OwnerCalls{
		Owners: map[string]OwnerEndpoint{
			"llm-bridge-server": {BaseURL: bridge.BaseURL, TokenHeader: "X-LLM-Bridge-Service-Token", Token: bridge.ServiceToken},
			"kanban-store":      {BaseURL: kanban.BaseURL, TokenHeader: "X-Kanban-Store-Service-Token", Token: kanban.ServiceToken},
		},
		Client: &http.Client{Timeout: 30 * time.Second},
	}
}

// effectResponseLimit bounds the answer kept on a run. The owners answer
// these calls with a row or nothing; a body past this is cut and says so.
const effectResponseLimit = 16 << 10

// Call makes the call and returns the owner's answer as it came.
func (o *OwnerCalls) Call(ctx context.Context, effect PlannedEffect) EffectResult {
	result := EffectResult{CalledAt: time.Now().Unix()}
	owner, known := o.Owners[effect.Service]
	if !known {
		result.Error = fmt.Sprintf("no endpoint for service %q", effect.Service)
		return result
	}
	var body io.Reader
	if effect.Body != nil {
		encoded, err := json.Marshal(effect.Body)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, effect.Method, owner.BaseURL+effect.Path, body)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	request.Header.Set(owner.TokenHeader, owner.Token)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := o.Client.Do(request)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, effectResponseLimit+1))
	result.Status = response.StatusCode
	if err != nil {
		result.Error = "read the answer: " + err.Error()
	}
	if len(answer) > effectResponseLimit {
		answer = append(answer[:effectResponseLimit], []byte(" …[cut at 16 KB]")...)
	}
	result.Body = string(answer)
	return result
}

// accepted says whether an owner's answer means the call did what it asked.
func (r EffectResult) accepted() bool {
	return r.Error == "" && r.Status >= 200 && r.Status < 300
}
