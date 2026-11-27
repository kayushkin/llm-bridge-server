package bundles

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// A session names its bundle by id, as a string, and asks two things about
// it: does the id exist (at session creation, so a session is never created
// with an id nobody hands out) and what does it resolve to (at spawn, so the
// session is provisioned exactly the tools the bundle names). A missing or
// malformed id, and a bundle that cannot be applied because it is disabled,
// are ErrSessionBundleNotFound: the caller's id is wrong. Any other error is
// the store failing to answer.

// ErrSessionBundleNotFound is a session's bundle id that names no bundle, is
// not an id, or names one that cannot be applied (disabled).
var ErrSessionBundleNotFound = errors.New("bundle not found")

// ParseSessionBundleID turns a session's string bundle id into the store's
// integer id. A name ("docker") is refused here, before any call: the store
// keys by id and names are renameable.
func ParseSessionBundleID(bundleID string) (int64, error) {
	id, err := strconv.ParseInt(bundleID, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("bundle_id %q is not a bundle-store id (a positive integer such as \"6\"); a bundle's name is not accepted, because it is renameable", bundleID)
	}
	return id, nil
}

// CheckSessionBundleExists returns nil when the store has the bundle (enabled
// or not), an error wrapping ErrSessionBundleNotFound when it does not, and
// any other error when the store could not answer.
func (s *Store) CheckSessionBundleExists(ctx context.Context, bundleID string) error {
	id, err := ParseSessionBundleID(bundleID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSessionBundleNotFound, err)
	}
	if _, err := s.GetBundle(ctx, id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%w: bundle-store has no bundle %d", ErrSessionBundleNotFound, id)
		}
		return fmt.Errorf("bundle-store could not read bundle %d: %w", id, err)
	}
	return nil
}

// ResolveSessionBundle composes one bundle: the resolution of that id alone,
// which applies the bundle's extends chain plus base. A disabled or missing
// bundle is ErrSessionBundleNotFound: the store refuses rather than composing
// something smaller than what was asked.
func (s *Store) ResolveSessionBundle(ctx context.Context, bundleID string) (*ResolveResponse, error) {
	id, err := ParseSessionBundleID(bundleID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSessionBundleNotFound, err)
	}
	resolution, err := Resolve(ctx, s, ResolveRequest{BundleIDs: []int64{id}})
	if err != nil {
		if errors.Is(err, ErrBundleNotApplicable) {
			return nil, fmt.Errorf("%w: %v", ErrSessionBundleNotFound, err)
		}
		return nil, fmt.Errorf("bundle-store could not resolve bundle %d: %w", id, err)
	}
	return resolution, nil
}

// ToolIDs is the tool-store ids the resolution names, in bundle order.
func (r *ResolveResponse) ToolIDs() []int64 {
	ids := make([]int64, 0, len(r.Tools))
	for _, tool := range r.Tools {
		ids = append(ids, tool.ID)
	}
	return ids
}

// DeniedToolIDs is the tool-store ids the resolution denies. A deny anywhere
// in the resolved set wins, so Resolve has already taken these out of Tools; a
// harness tool among them is one the session's harness must not offer.
func (r *ResolveResponse) DeniedToolIDs() []int64 {
	ids := make([]int64, 0, len(r.DeniedTools))
	for _, tool := range r.DeniedTools {
		ids = append(ids, tool.ID)
	}
	return ids
}
