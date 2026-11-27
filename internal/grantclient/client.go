// Package grantclient is grant-store as this server reads and writes it: which
// resources a principal's effective grants name, and the grants peer
// provisioning makes.
//
// grant-store runs in this process (internal/access/grants), so every call
// here is a call into its store, not a request over HTTP. This server calls it
// as the internal service, the way it called the standalone store with its
// service token.
//
// The effective set is the principal's own active grants plus every active
// group's, with the groups read from principal-store on each call — so a
// membership change is visible at the next spawn without this server being
// told. A principal principal-store does not know is ErrPrincipalUnknown,
// never an empty list, so the spawn can tell "nothing granted" from "no such
// principal".
package grantclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
)

// ErrPrincipalUnknown is grant-store answering that principal-store has no
// such principal. A session carrying that id was created against a directory
// that has since lost the row, or never had it.
var ErrPrincipalUnknown = errors.New("principal unknown")

// AllResources is the resource_id grant-store stores for a grant on every
// resource of its type, now and later.
const AllResources = grants.AllResources

// Client is grant-store in this process.
type Client struct {
	store     *grants.Store
	directory grants.PrincipalDirectory
	checker   grants.ResourceChecker
}

// New reads and writes grants through store, asking directory about
// principals and checker about resources, as grant-store's own routes do.
func New(store *grants.Store, directory grants.PrincipalDirectory, checker grants.ResourceChecker) *Client {
	return &Client{store: store, directory: directory, checker: checker}
}

// EffectiveResourceIDs returns the resource ids the principal's effective
// grants of one relation name, for one resource type. An empty answer means
// the principal — and every group they are in — holds no such grant; it is
// not an error.
func (c *Client) EffectiveResourceIDs(ctx context.Context, principalID, relation, resourceType string) ([]string, error) {
	effective, err := c.store.Effective(ctx, c.directory, principalID, relation, resourceType)
	if errors.Is(err, grants.ErrNotFound) {
		return nil, fmt.Errorf("%w: %v", ErrPrincipalUnknown, err)
	}
	if err != nil {
		return nil, fmt.Errorf("grant-store: the effective %s grants of %s on %s: %w", relation, principalID, resourceType, err)
	}
	ids := make([]string, 0, len(effective))
	for _, grant := range effective {
		ids = append(ids, grant.ResourceID)
	}
	return ids, nil
}

// GrantInput is a grant to record.
type GrantInput struct {
	PrincipalID  string
	Relation     string
	ResourceType string
	ResourceID   string
	Note         string
}

// Create records a grant and returns its id. grant-store is idempotent on the
// active tuple, so a grant that already exists answers with its id too.
func (c *Client) Create(ctx context.Context, input GrantInput) (string, error) {
	grant, _, err := c.store.Create(ctx, c.directory, c.checker, grants.GrantRequest{
		PrincipalID: input.PrincipalID, Relation: input.Relation, ResourceType: input.ResourceType,
		ResourceID: input.ResourceID, Note: input.Note,
	})
	if err != nil {
		return "", fmt.Errorf("grant-store: granting %s %s on %s %s: %w", input.PrincipalID, input.Relation, input.ResourceType, input.ResourceID, err)
	}
	return grant.ID, nil
}
