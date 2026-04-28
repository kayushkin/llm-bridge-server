package server

import (
	"context"
	"net/http"
	"time"

	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
	modelstore "github.com/kayushkin/model-store"
)

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.modelStore.AllModelsWithStatus()
	if err != nil {
		http.Error(w, `{"error":"failed to query models"}`, http.StatusInternalServerError)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	creds, err := s.authClient.List(ctx, authstoreclient.ListFilter{})
	if err != nil {
		http.Error(w, `{"error":"`+err.Error()+`"}`, http.StatusBadGateway)
		return
	}

	credsByProvider := make(map[string][]credResponse)
	for i := range creds {
		c := &creds[i]
		credsByProvider[c.Provider] = append(credsByProvider[c.Provider], toCredResponse(c))
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
