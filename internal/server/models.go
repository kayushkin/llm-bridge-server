package server

import (
	"net/http"

	"github.com/kayushkin/aiauth"
	modelstore "github.com/kayushkin/model-store"
)

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.modelStore.AllModelsWithStatus()
	if err != nil {
		http.Error(w, `{"error":"failed to query models"}`, http.StatusInternalServerError)
		return
	}

	// Load credentials from aiauth, index by provider
	authStore := aiauth.DefaultStore()
	profiles := authStore.Profiles()

	credsByProvider := make(map[string][]credResponse)
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
		credsByProvider[c.Provider] = append(credsByProvider[c.Provider], cr)
		priority++
	}

	type modelWithCreds struct {
		modelstore.ModelStatus
		Credentials []credResponse `json:"credentials"`
	}

	var result []modelWithCreds
	for _, ms := range statuses {
		mc := modelWithCreds{ModelStatus: ms}
		mc.Credentials = credsByProvider[ms.Provider]
		if mc.Credentials == nil {
			mc.Credentials = []credResponse{}
		}
		result = append(result, mc)
	}

	writeJSON(w, result)
}
