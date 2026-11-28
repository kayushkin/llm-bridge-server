package modelcredentials

import (
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/authstoreclient"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	AuthClient func() *authstoreclient.Client
	ModelStore func() *models.Store
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
