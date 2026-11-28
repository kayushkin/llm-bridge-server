package requestcaller

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"
)

// CredentialError is a refusal decided while establishing who a
// request comes from.
type CredentialError struct {
	Status  int
	Code    string
	Message string
}

func (e *CredentialError) Write(w http.ResponseWriter) {
	httpjson.WriteJSONError(w, e.Status, e.Code, e.Message)
}

// NewCredentialError is a refusal with its status, code and message.
func NewCredentialError(status int, code, message string) *CredentialError {
	return &CredentialError{Status: status, Code: code, Message: message}
}
