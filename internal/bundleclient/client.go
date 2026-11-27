// Package bundleclient asks the bundle-store module two things about a session
// bundle: does the id exist (at session creation, so a session is never
// created with an id nobody hands out) and what does it resolve to (at spawn,
// so the session is provisioned exactly the tools the bundle names).
//
// The module runs in this process (internal/agents/bundles), so both
// questions are calls into it, not HTTP. A missing or malformed id, and a
// bundle that cannot be applied because it is disabled, are ErrNotFound: the
// caller's id is wrong. Any other error is the store failing to answer.
package bundleclient

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/agents/bundles"
)

// ErrNotFound is bundle-store answering that it has no such bundle, or that
// the bundle cannot be applied (disabled).
var ErrNotFound = errors.New("bundle not found")

// Client asks one bundle-store module.
type Client struct {
	store *bundles.Store
}

// New builds a client on the module's store.
func New(store *bundles.Store) *Client {
	return &Client{store: store}
}

// Resolution is what a bundle composes to: the tool-store and skill-store ids
// to provision, and the bundles that contributed, for the log line.
type Resolution struct {
	Bundles []bundles.BundleRef
	Skills  []bundles.MemberRef
	Tools   []bundles.MemberRef
	// DeniedTools is every tool a bundle in the resolved set denies. A deny
	// anywhere wins, so bundle-store has already taken these out of Tools; a
	// harness tool among them is one the session's harness must not offer.
	DeniedTools []bundles.MemberRef
	// DeniedReadPaths is the paths the session may not read: absolute
	// ("/proc/**") or home-relative ("~/.config/**"), never a harness rule.
	DeniedReadPaths []string
	Model           string
	Effort          string
}

// DeniedToolIDs is the tool-store ids the resolution denies.
func (r *Resolution) DeniedToolIDs() []int64 {
	ids := make([]int64, 0, len(r.DeniedTools))
	for _, t := range r.DeniedTools {
		ids = append(ids, t.ID)
	}
	return ids
}

// ToolIDs is the tool-store ids the resolution names, in bundle order.
func (r *Resolution) ToolIDs() []int64 {
	ids := make([]int64, 0, len(r.Tools))
	for _, t := range r.Tools {
		ids = append(ids, t.ID)
	}
	return ids
}

// ParseID turns the session's string bundle id into bundle-store's integer
// id. A name ("docker") is refused here, before any call: bundle-store keys
// by id and names are renameable.
func ParseID(bundleID string) (int64, error) {
	id, err := strconv.ParseInt(bundleID, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("bundle_id %q is not a bundle-store id (a positive integer such as \"6\"); a bundle's name is not accepted, because it is renameable", bundleID)
	}
	return id, nil
}

// CheckExists returns nil when bundle-store has the bundle (enabled or not),
// an error wrapping ErrNotFound when it does not, and any other error when the
// store could not answer.
func (c *Client) CheckExists(ctx context.Context, bundleID string) error {
	id, err := ParseID(bundleID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	if _, err := c.store.GetBundle(ctx, id); err != nil {
		if errors.Is(err, bundles.ErrNotFound) {
			return fmt.Errorf("%w: bundle-store has no bundle %d", ErrNotFound, id)
		}
		return fmt.Errorf("bundle-store could not read bundle %d: %w", id, err)
	}
	return nil
}

// Resolve composes one bundle: bundle-store's resolution of that id alone,
// which applies the bundle's extends chain plus base. A disabled or missing
// bundle is ErrNotFound — bundle-store refuses rather than composing something
// smaller than what was asked.
func (c *Client) Resolve(ctx context.Context, bundleID string) (*Resolution, error) {
	id, err := ParseID(bundleID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	answer, err := bundles.Resolve(ctx, c.store, bundles.ResolveRequest{BundleIDs: []int64{id}})
	if err != nil {
		if errors.Is(err, bundles.ErrBundleNotApplicable) {
			return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
		}
		return nil, fmt.Errorf("bundle-store could not resolve bundle %d: %w", id, err)
	}
	return &Resolution{
		Bundles:         answer.Bundles,
		Skills:          answer.Skills,
		Tools:           answer.Tools,
		DeniedTools:     answer.DeniedTools,
		DeniedReadPaths: answer.DeniedReadPaths,
		Model:           answer.Model,
		Effort:          answer.Effort,
	}, nil
}
