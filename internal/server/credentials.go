package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	"github.com/kayushkin/llm-bridge/msg"
)

// credResponse is the canonical credential type — defined in llm-bridge/msg/server.go.
// DO NOT define new request/response types here. Add them to msg/ instead,
// then run generate-ts.sh so the TypeScript frontend stays in sync.
type credResponse = msg.Credential

func toCredResponses(creds []authstoreclient.Credential) []credResponse {
	out := make([]credResponse, 0, len(creds))
	for i := range creds {
		out = append(out, toCredResponse(&creds[i]))
	}
	return out
}

func toCredResponse(c *authstoreclient.Credential) credResponse {
	cr := credResponse{
		ID:        c.ID,
		Provider:  c.Provider,
		Label:     c.Label,
		AuthType:  c.AuthType,
		Priority:  c.Priority,
		Enabled:   c.Enabled,
		ExpiresAt: c.ExpiresAt,
	}
	if c.APIKey != "" {
		cr.APIKeyMasked = c.APIKey // already masked by auth-store
	}
	if c.Token != "" {
		cr.TokenMasked = c.Token
	}
	return cr
}

func (s *Server) handleCredentialsList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	creds, err := s.authClient.List(ctx, authstoreclient.ListFilter{})
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}
	writeJSON(w, toCredResponses(creds))
}

func (s *Server) handleCredentialCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Owner    string `json:"owner"`
		Type     string `json:"type"`
		Key      string `json:"key,omitempty"`
		Token    string `json:"token,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	if req.Provider == "" {
		http.Error(w, `{"error":"provider required"}`, http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		req.Type = "api_key"
	}

	in := authstoreclient.CredentialInput{
		Provider:    req.Provider,
		Owner:       req.Owner,
		Label:       req.Name,
		AuthType:    req.Type,
		IntendedApp: "llm-bridge-server",
	}
	switch req.Type {
	case "api_key":
		if req.Key == "" {
			http.Error(w, `{"error":"key required for api_key type"}`, http.StatusBadRequest)
			return
		}
		in.APIKey = req.Key
		in.RefreshMode = "none"
	case "token":
		if req.Token == "" {
			http.Error(w, `{"error":"token required for token type"}`, http.StatusBadRequest)
			return
		}
		in.Token = req.Token
		in.RefreshMode = "none"
	default:
		http.Error(w, `{"error":"unsupported type, use api_key or token"}`, http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	c, err := s.authClient.Create(ctx, in)
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": c.ID, "status": "created"})
}

func (s *Server) handleCredentialDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, `{"error":"credential id required"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	if err := s.authClient.Delete(ctx, id); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func maskKey(key string) string {
	if len(key) <= 16 {
		return strings.Repeat("*", len(key))
	}
	return key[:8] + "..." + key[len(key)-4:]
}
