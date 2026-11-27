package machineroutes

import (
	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/harness"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/store"
)

// Dependencies is what the machine, instance and runner routes read from the
// server that composes them.
type Dependencies struct {
	GrantChecker *grantcheck.Checker
	Harness      *harness.Manager
	HarnessStore func() *machines.Store
	Store        *store.Store
}

// Service answers the machine, instance and runner routes.
type Service struct {
	Dependencies
}
