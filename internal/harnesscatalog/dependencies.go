package harnesscatalog

import (
	"github.com/kayushkin/llm-bridge-server/internal/agents"
	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	AgentStore func() *agents.Store
	CfState    *ConformanceState
	Cfg        *config.Config
	Store      *store.Store
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
