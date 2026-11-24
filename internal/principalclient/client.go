// Package principalclient is principal-store as this server reads and writes
// it: whether a principal exists, what its kind may do, which groups it is in,
// which principal an outside identity signs in as, and the few writes peer
// provisioning makes.
//
// principal-store runs in this process (internal/access/principalstore), so
// every call here is a call into its store, not a request over HTTP. The
// types below are the part of its records this server reads.
//
// principal-store answers a disabled principal like any other — a departed
// human's grants are dormant, not invalid — so "exists" here includes
// disabled.
package principalclient

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/principalstore"
)

// ErrNotFound is principal-store answering that it has no such principal.
var ErrNotFound = errors.New("principal not found")

// ErrIdentityNotProvisioned is principal-store answering that no active
// binding links the identity to a principal: the person has not been
// provisioned, and may not sign in.
var ErrIdentityNotProvisioned = errors.New("identity is not bound to any principal")

// KindPeer is principal-store's kind for another llm-bridge-server linked to
// this one. Spelled here because a peer invite may be made only for one: what
// a peer may do is read from the answers below, never from this word.
const KindPeer = "peer"

// KindGroup is principal-store's kind for a group, which a level's
// provisioning makes. Spelled here because creating one must name it.
const KindGroup = "group"

// Principal is the part of principal-store's principal record this server
// reads. DisabledAt is 0 for an active principal and the epoch second it was
// disabled otherwise.
//
// IsAdministrator is principal-store's own answer to "may this person do
// anything?", and it is read, never derived: this server does not keep a list
// of administrators and must not infer one from a name, an email or a group.
//
// ActsAsCaller, RestrictedToItsGrants and HoldsServicePrincipalTokens are
// principal-store's answers for the principal's kind: may a request act as it
// (a human, a peer or a service), does holding no grant of a relation refuse it
// (a peer, a service) rather than leave it unrestricted (a human), and may it
// be issued a service principal token (a service). This server reads them
// instead of comparing Kind to a word.
type Principal struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// DisplayName is for showing only; never look a principal up by it.
	DisplayName                 string `json:"display_name"`
	DisabledAt                  int64  `json:"disabled_at"`
	IsAdministrator             bool   `json:"is_administrator"`
	ActsAsCaller                bool   `json:"acts_as_caller"`
	RestrictedToItsGrants       bool   `json:"restricted_to_its_grants"`
	HoldsServicePrincipalTokens bool   `json:"holds_service_principal_tokens"`
}

// IdentityBinding is principal-store's record linking an outside identity —
// for Entra ID the tenant id and object id — to one human principal.
type IdentityBinding struct {
	ID           string `json:"id"`
	Seq          int64  `json:"seq"`
	PrincipalID  string `json:"principal_id"`
	Provider     string `json:"provider"`
	TenantID     string `json:"tenant_id"`
	Subject      string `json:"subject"`
	LinkedAt     int64  `json:"linked_at"`
	LinkedBy     string `json:"linked_by"`
	LinkNote     string `json:"link_note"`
	UnlinkedAt   int64  `json:"unlinked_at"`
	UnlinkedBy   string `json:"unlinked_by"`
	UnlinkReason string `json:"unlink_reason"`
}

// IdentityResolution is principal-store's answer to "who does this identity
// sign in as": the active binding and the principal it names, which comes back
// even when disabled so the caller can say why it refuses.
type IdentityResolution struct {
	Binding   IdentityBinding `json:"binding"`
	Principal Principal       `json:"principal"`
}

// Client is principal-store in this process.
type Client struct {
	store *principalstore.Store
}

// New reads and writes principals through store.
func New(store *principalstore.Store) *Client {
	return &Client{store: store}
}

// principalOf is the part of a store record this server reads.
func principalOf(record *principalstore.Principal) Principal {
	return Principal{ID: record.ID, Kind: record.Kind, DisplayName: record.DisplayName, DisabledAt: record.DisabledAt,
		IsAdministrator: record.IsAdministrator, ActsAsCaller: record.ActsAsCaller, RestrictedToItsGrants: record.RestrictedToItsGrants,
		HoldsServicePrincipalTokens: record.HoldsServicePrincipalTokens}
}

// CheckExists returns nil when principal-store has the principal, an error
// wrapping ErrNotFound when it does not, and any other error when it could not
// answer.
func (c *Client) CheckExists(ctx context.Context, principalID string) error {
	_, err := c.Get(ctx, principalID)
	return err
}

// Get returns principal-store's record for principalID, with the same error
// contract as CheckExists.
func (c *Client) Get(ctx context.Context, principalID string) (Principal, error) {
	record, err := c.store.Get(ctx, principalID, false)
	if errors.Is(err, principalstore.ErrNotFound) {
		return Principal{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if err != nil {
		return Principal{}, fmt.Errorf("principal-store: reading %s: %w", principalID, err)
	}
	return principalOf(record), nil
}

// GroupIDsOf returns the ids of the active groups principalID is a direct
// member of. principal-store answers only for a kind that joins groups;
// asking about any other is its error, passed on.
func (c *Client) GroupIDsOf(ctx context.Context, principalID string) ([]string, error) {
	groups, err := c.store.ListGroups(ctx, principalID, false)
	if err != nil {
		return nil, fmt.Errorf("principal-store: the groups of %s: %w", principalID, err)
	}
	ids := make([]string, 0, len(groups))
	for _, group := range groups {
		ids = append(ids, group.ID)
	}
	return ids, nil
}

// ResolveIdentityBinding asks principal-store which principal an outside
// identity is bound to. It returns an error wrapping ErrIdentityNotProvisioned
// when no active binding names the identity, and any other error when the
// store could not answer. An answer whose binding does not name the identity
// asked about is an error, not a match.
func (c *Client) ResolveIdentityBinding(ctx context.Context, provider, tenantID, subject string) (IdentityResolution, error) {
	resolution, err := c.store.ResolveIdentity(ctx, provider, tenantID, subject)
	if errors.Is(err, principalstore.ErrNotFound) {
		return IdentityResolution{}, fmt.Errorf("%w: %v", ErrIdentityNotProvisioned, err)
	}
	if err != nil {
		return IdentityResolution{}, fmt.Errorf("principal-store: resolving a %s identity: %w", provider, err)
	}
	if resolution.Binding == nil || resolution.Principal == nil {
		return IdentityResolution{}, fmt.Errorf("principal-store resolved a %s identity to no binding or no principal", provider)
	}
	stored := resolution.Binding
	binding := IdentityBinding{ID: stored.ID, Seq: stored.Seq, PrincipalID: stored.PrincipalID, Provider: stored.Provider,
		TenantID: stored.TenantID, Subject: stored.Subject, LinkedAt: stored.LinkedAt, LinkedBy: stored.LinkedBy, LinkNote: stored.LinkNote,
		UnlinkedAt: stored.UnlinkedAt, UnlinkedBy: stored.UnlinkedBy, UnlinkReason: stored.UnlinkReason}
	principal := principalOf(resolution.Principal)
	if binding.Provider != provider || !strings.EqualFold(binding.TenantID, tenantID) || !strings.EqualFold(binding.Subject, subject) ||
		binding.UnlinkedAt != 0 || principal.ID != binding.PrincipalID {
		return IdentityResolution{}, fmt.Errorf("principal-store resolved a %s identity to binding %s of principal %s, which is not an active binding of the identity asked about",
			provider, binding.ID, principal.ID)
	}
	return IdentityResolution{Binding: binding, Principal: principal}, nil
}

// The writes below are made for one purpose: granting another
// llm-bridge-server access (internal/server/peers.go). This server makes the
// peer principal it grants to, sets its administrator flag, and puts it in or
// takes it out of the level's group. It writes nothing else there.

// Create makes a principal of the given kind and returns its id.
func (c *Client) Create(ctx context.Context, kind, displayName string) (string, error) {
	created, err := c.store.Create(ctx, &principalstore.Principal{Kind: kind, DisplayName: displayName})
	if err != nil {
		return "", fmt.Errorf("principal-store: creating a %s principal: %w", kind, err)
	}
	return created.ID, nil
}

// SetAdministrator sets principal-store's is_administrator on a principal.
func (c *Client) SetAdministrator(ctx context.Context, principalID string, isAdministrator bool) error {
	if _, err := c.store.Patch(ctx, principalID, principalstore.Patch{IsAdministrator: &isAdministrator}); err != nil {
		return fmt.Errorf("principal-store: setting is_administrator on %s: %w", principalID, err)
	}
	return nil
}

// AddMember puts a principal in a group. Already a member is not an error.
func (c *Client) AddMember(ctx context.Context, groupID, memberID string) error {
	if _, err := c.store.AddMember(ctx, groupID, memberID); err != nil {
		return fmt.Errorf("principal-store: adding %s to group %s: %w", memberID, groupID, err)
	}
	return nil
}

// RemoveMember takes a principal out of a group.
func (c *Client) RemoveMember(ctx context.Context, groupID, memberID string) error {
	if err := c.store.RemoveMember(ctx, groupID, memberID); err != nil {
		return fmt.Errorf("principal-store: removing %s from group %s: %w", memberID, groupID, err)
	}
	return nil
}
