package server

import (
	"context"
	"log"
	"net/http"
	"time"

	modelstore "github.com/kayushkin/llm-bridge-server/internal/agents/modelstore"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
)

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	statuses, err := s.modelStore.AllModelsWithStatus()
	if err != nil {
		http.Error(w, `{"error":"failed to query models"}`, http.StatusInternalServerError)
		return
	}

	// The credentials are joined in for the Auth page; the list itself is
	// model-store's. An auth-store that cannot be read must not take the list
	// away, because the chat's model picker reads it too: the rows are answered
	// with their credentials marked unknown, and the failure is logged and named
	// on each row rather than shown as "no credentials".
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	creds, credentialsErr := s.authClient.List(ctx, authstoreclient.ListFilter{})
	if credentialsErr != nil {
		log.Printf("[models] ERROR: reading credentials from auth-store; answering the models with their credentials unknown: %v", credentialsErr)
	}

	credsByProvider := make(map[string][]credResponse)
	for i := range creds {
		c := &creds[i]
		credsByProvider[c.Provider] = append(credsByProvider[c.Provider], toCredResponse(c))
	}

	result := make([]modelWithCredentials, 0, len(statuses))
	for _, ms := range statuses {
		mc := modelWithCredentials{ModelStatus: ms}
		if credentialsErr != nil {
			mc.CredentialsError = credentialsErr.Error()
		} else {
			mc.CredentialsKnown = true
			mc.Credentials = credsByProvider[ms.Provider]
			if mc.Credentials == nil {
				mc.Credentials = []credResponse{}
			}
		}
		result = append(result, mc)
	}

	writeJSON(w, result)
}

// modelWithCredentials is one row of GET /models: model-store's model and the
// auth-store credentials for its provider. When auth-store could not be read,
// CredentialsKnown is false, Credentials is null and CredentialsError says why;
// an empty list always means auth-store answered and holds none.
type modelWithCredentials struct {
	modelstore.ModelStatus
	Credentials      []credResponse `json:"credentials"`
	CredentialsKnown bool           `json:"credentials_known"`
	CredentialsError string         `json:"credentials_error,omitempty"`
}
