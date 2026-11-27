package people

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrPrincipalStoreUnavailable means principal-store could not say whether a
// principal exists. The write is refused with a 502 rather than stored
// unchecked.
var ErrPrincipalStoreUnavailable = errors.New("principal-store could not answer")

// PrincipalChecker says whether principal-store holds a principal. The server
// answers it from principal-store in its own process.
type PrincipalChecker interface {
	// CheckPrincipal returns nil when the principal exists, an error wrapping
	// ErrInvalid when principal-store says it does not, and one wrapping
	// ErrPrincipalStoreUnavailable when it cannot tell.
	CheckPrincipal(ctx context.Context, id string) error
}

// checkPrincipalID validates the shape of a person's principal_id and asks
// principal-store whether it exists. "" means none and is not checked.
func (s *Store) checkPrincipalID(ctx context.Context, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", nil
	}
	if !principalIDPattern.MatchString(id) {
		return "", fmt.Errorf("%w: principal_id %q is not a principal-store id (principal_000001)", ErrInvalid, id)
	}
	if s.principals == nil {
		return "", fmt.Errorf("%w: this store was built with no principal checker, so it cannot check principal_id %s", ErrPrincipalStoreUnavailable, id)
	}
	if err := s.principals.CheckPrincipal(ctx, id); err != nil {
		return "", err
	}
	return id, nil
}
