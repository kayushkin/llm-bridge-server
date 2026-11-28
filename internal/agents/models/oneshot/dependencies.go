package oneshot

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models/modelrouting"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations/operationroutes"
	"github.com/kayushkin/llm-bridge-server/internal/machines"
	"github.com/kayushkin/llm-bridge-server/internal/sessions/sessionfiles"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	FileStore       func() sessionfiles.SessionFileStore
	HarnessStore    func() *machines.Store
	IssueReads      func() *issuereads.Reader
	ModelStore      func() *models.Store
	OneShotRouter   func() *modelrouting.Router
	OperationRoutes *operationroutes.Service
	SessionFiles    *sessionfiles.Service
	Settings        *servicesettings.Registry
	Store           *store.Store
	Server          Host
}

// Host is what this package calls on the server that composes it.
type Host interface {
	CheckSettingNamesAnEnabledInstance(instanceID string) error
	ReadUsageStoreLimits(context.Context) (map[string]modelrouting.UsageProviderLimits, error)
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
