package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kayushkin/aiauth"
	"github.com/kayushkin/llm-bridge/msg"
)

// credResponse is the canonical credential type from msg.
type credResponse = msg.Credential

func (s *Server) handleCredentialsList(w http.ResponseWriter, _ *http.Request) {
	store := aiauth.DefaultStore()
	profiles := store.Profiles()

	var response []credResponse
	priority := 0
	for name, c := range profiles {
		cr := credResponse{
			ID:        name,
			Provider:  c.Provider,
			Label:     name,
			AuthType:  c.Type,
			Priority:  priority,
			Enabled:   true,
			ExpiresAt: c.Expires,
		}
		if c.Key != "" {
			cr.APIKeyMasked = maskKey(c.Key)
		}
		if c.Token != "" {
			cr.TokenMasked = maskKey(c.Token)
		}
		if c.Access != "" {
			cr.TokenMasked = maskKey(c.Access)
		}
		response = append(response, cr)
		priority++
	}

	if response == nil {
		response = []credResponse{}
	}
	writeJSON(w, response)
}

func (s *Server) handleCredentialCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		Provider string `json:"provider"`
		Type     string `json:"type"`
		Key      string `json:"key,omitempty"`
		Token    string `json:"token,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}
	if req.Name == "" || req.Provider == "" {
		http.Error(w, `{"error":"name and provider required"}`, http.StatusBadRequest)
		return
	}
	if req.Type == "" {
		req.Type = "api_key"
	}

	cred := &aiauth.Credential{
		Type:     req.Type,
		Provider: req.Provider,
	}
	switch req.Type {
	case "api_key":
		if req.Key == "" {
			http.Error(w, `{"error":"key required for api_key type"}`, http.StatusBadRequest)
			return
		}
		cred.Key = req.Key
	case "token":
		if req.Token == "" {
			http.Error(w, `{"error":"token required for token type"}`, http.StatusBadRequest)
			return
		}
		cred.Token = req.Token
	default:
		http.Error(w, `{"error":"unsupported type, use api_key or token"}`, http.StatusBadRequest)
		return
	}

	store := aiauth.DefaultStore()
	if err := store.SetProfile(req.Name, cred); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": req.Name, "status": "created"})
}

func (s *Server) handleCredentialDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, `{"error":"credential id required"}`, http.StatusBadRequest)
		return
	}

	store := aiauth.DefaultStore()
	if err := store.DeleteProfile(id); err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusInternalServerError)
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
