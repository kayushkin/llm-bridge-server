package grantstore

import "context"

// PrincipalDirectory is principal-store as this store needs it: does a
// principal exist, what kind is it, and which active groups is it in. Both
// writes (is the grantee real?) and the effective-set read (which groups'
// grants does this human inherit?) go through it.
//
// LookupPrincipal returns an error wrapping ErrPrincipalNotFound when
// principal-store has no such principal, and any other error when it could not
// be asked. The two are kept apart because they mean
// opposite things to a caller: a wrong id is theirs to fix, a directory that is
// down is not.
type PrincipalDirectory interface {
	LookupPrincipal(ctx context.Context, principalID string) (*PrincipalSummary, error)
}

// PrincipalSummary is the part of a principal-store row this store reads.
type PrincipalSummary struct {
	ID         string
	Kind       string // as principal-store spells it; shown in messages, never compared
	DisabledAt int64
	// ActsInThisDeployment and ActsAsCaller are principal-store's answers for
	// this principal's kind: may it hold grants, and may a request act as it.
	// This store reads them rather than comparing Kind to a word, so a kind
	// principal-store adds (a peer, say) is handled by what principal-store
	// says it may do.
	ActsInThisDeployment bool
	ActsAsCaller         bool
	// IsAdministrator is principal-store's one fact about a person that this
	// store acts on: an administrator may read and write every grant, whatever
	// it is on and whoever holds it.
	IsAdministrator bool
	// GroupIDs is the active groups a principal belongs to, as principal-store
	// expands them on a read of the principal. Empty for a group: groups do not nest.
	GroupIDs []string
}
