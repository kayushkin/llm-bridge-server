package operationroutes

import (
	"context"

	"github.com/kayushkin/llm-bridge-server/internal/access/grantcheck"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/agents/models"
	"github.com/kayushkin/llm-bridge-server/internal/automation/operations"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issuereads"
	"github.com/kayushkin/llm-bridge/servicesettings"
)

// Dependencies is what this package's code reads from the server that composes it.
type Dependencies struct {
	GrantChecker   *grantcheck.Checker
	IssueReads     func() *issuereads.Reader
	ModelStore     func() *models.Store
	Operations     func() *operations.Coordinator
	PrincipalStore func() PrincipalStore
	Settings       *servicesettings.Registry
}

// PrincipalStore is what this package calls on the server's principalStore.
type PrincipalStore interface {
	Get(ctx context.Context, id string, includeDisabledMemberships bool) (*principals.Principal, error)
	ListGroups(ctx context.Context, memberID string, includeDisabled bool) ([]*principals.Principal, error)
}

// Service is the package's state and what it was built with.
type Service struct {
	Dependencies
}
