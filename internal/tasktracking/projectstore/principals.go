package projectstore

import (
	"context"
	"errors"

	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
)

// Principals is the principal-store a project link to a principal is checked
// with. It moved into the same server, so it is asked in process.
type Principals interface {
	// DisplayName is the principal's name for display; ErrNotFound when no
	// principal has the id.
	DisplayName(ctx context.Context, id string) (string, error)
}

// PrincipalStoreModule is Principals over the principal-store module.
type PrincipalStoreModule struct {
	Store *principals.Store
}

func (module PrincipalStoreModule) DisplayName(ctx context.Context, id string) (string, error) {
	principal, err := module.Store.Get(ctx, id, false)
	if errors.Is(err, principals.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return principal.DisplayName, nil
}
