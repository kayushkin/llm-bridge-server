package access

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/access/signin"
)

// The standalone grant-store and sign-in-store asked principal-store over
// HTTP. Here they ask its code: these two adapters answer each store's own
// directory interface from principal-store's records.

// GrantsPrincipalDirectory is principal-store as grant-store reads it.
func GrantsPrincipalDirectory(principals *principals.Store) grants.PrincipalDirectory {
	return grantsPrincipalDirectory{principals: principals}
}

type grantsPrincipalDirectory struct {
	principals *principals.Store
}

// LookupPrincipal answers what the standalone store read from
// GET /principals/{id}: the record, disabled or not, with the active groups it
// is in.
func (directory grantsPrincipalDirectory) LookupPrincipal(ctx context.Context, principalID string) (*grants.PrincipalSummary, error) {
	principal, err := directory.principals.Get(ctx, principalID, false)
	if errors.Is(err, principals.ErrNotFound) {
		return nil, fmt.Errorf("%w: %v", grants.ErrPrincipalNotFound, err)
	}
	if err != nil {
		return nil, fmt.Errorf("principal-store: reading %s: %w", principalID, err)
	}
	summary := &grants.PrincipalSummary{
		ID: principal.ID, Kind: principal.Kind, DisabledAt: principal.DisabledAt, IsAdministrator: principal.IsAdministrator,
		ActsInThisDeployment: principal.ActsInThisDeployment, ActsAsCaller: principal.ActsAsCaller,
	}
	for _, group := range principal.Groups {
		summary.GroupIDs = append(summary.GroupIDs, group.ID)
	}
	return summary, nil
}

// SignInPrincipalDirectory is principal-store as sign-in-store reads and
// writes it.
func SignInPrincipalDirectory(principals *principals.Store) signin.PrincipalDirectory {
	return signInPrincipalDirectory{principals: principals}
}

type signInPrincipalDirectory struct {
	principals *principals.Store
}

func signInPrincipalOf(principal *principals.Principal) signin.Principal {
	answer := signin.Principal{ID: principal.ID, Kind: principal.Kind, DisplayName: principal.DisplayName,
		DisabledAt: principal.DisabledAt, IsAdministrator: principal.IsAdministrator}
	for _, group := range principal.Groups {
		answer.Groups = append(answer.Groups, struct {
			ID string `json:"id"`
		}{ID: group.ID})
	}
	return answer
}

// Principal is the record, disabled or not, with its active groups.
func (directory signInPrincipalDirectory) Principal(ctx context.Context, principalID string) (signin.Principal, error) {
	principal, err := directory.principals.Get(ctx, principalID, false)
	if errors.Is(err, principals.ErrNotFound) {
		return signin.Principal{}, fmt.Errorf("%w: %v", signin.ErrNotFound, err)
	}
	if err != nil {
		return signin.Principal{}, fmt.Errorf("principal-store: reading %s: %w", principalID, err)
	}
	return signInPrincipalOf(principal), nil
}

// PrincipalOfIdentity is the principal an outside identity's active binding
// names. The standalone store read only the principal out of the resolution;
// so does this, with its groups, as GET /principals/{id} gave them.
func (directory signInPrincipalDirectory) PrincipalOfIdentity(ctx context.Context, provider, tenantID, subject string) (signin.Principal, error) {
	resolution, err := directory.principals.ResolveIdentity(ctx, provider, tenantID, subject)
	if errors.Is(err, principals.ErrNotFound) {
		return signin.Principal{}, fmt.Errorf("%w: no %s identity %s is bound", signin.ErrNotFound, provider, subject)
	}
	if err != nil {
		return signin.Principal{}, fmt.Errorf("principal-store: resolving a %s identity: %w", provider, err)
	}
	if resolution.Principal == nil || resolution.Principal.ID == "" {
		return signin.Principal{}, fmt.Errorf("principal-store resolved %s identity %s to no principal", provider, subject)
	}
	return signInPrincipalOf(resolution.Principal), nil
}

// LinkIdentity binds an outside identity to a principal.
func (directory signInPrincipalDirectory) LinkIdentity(ctx context.Context, principalID, provider, tenantID, subject, linkedBy, note string) error {
	_, err := directory.principals.LinkIdentity(ctx, principalID, provider, tenantID, subject, linkedBy, note)
	if errors.Is(err, principals.ErrIdentityAlreadyBound) {
		return fmt.Errorf("%w: %v", signin.ErrIdentityAlreadyBound, err)
	}
	if err != nil {
		return fmt.Errorf("principal-store: linking a %s identity to %s: %w", provider, principalID, err)
	}
	return nil
}

// IdentityBindingRoutesAtTheirOwnPaths serves principal-store's identity
// binding routes at the paths the standalone store had, for the bridge's
// administration routes, which pass a request through to the store and its
// answer back unchanged.
func IdentityBindingRoutesAtTheirOwnPaths(principals *principals.Handlers) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /identity-bindings", principals.ListIdentityBindings)
	mux.HandleFunc("POST /identity-bindings", principals.LinkIdentity)
	mux.HandleFunc("POST /identity-bindings/{id}/unlink", principals.UnlinkIdentity)
	return mux
}
