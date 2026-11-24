package referencestore

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// The grant vocabulary, as grant-store serves it.
const (
	RelationCanSeeReferences  = "can_see_references"
	ResourceTypeReferenceType = "reference_type"
	ResourceTypeReference     = "reference"
	allResources              = "*"
)

// AccessDirectory is what the access check reads about a principal:
// principal-store's standing and grant-store's grants. In the server this
// store is mounted in, both stores run in the same process and answer it
// directly.
type AccessDirectory interface {
	// Standing is the principal's administrator flag and whether it is
	// disabled; an error when principal-store cannot say, or has no such
	// principal.
	Standing(ctx context.Context, principalID string) (PrincipalStanding, error)
	// Grants are the grants the principal holds, its groups' included, as
	// grant-store's effective grants answer them.
	Grants(ctx context.Context, principalID string) ([]HeldGrant, error)
}

// PrincipalStanding is the part of a principal the check reads.
type PrincipalStanding struct {
	IsAdministrator bool
	Disabled        bool
}

// HeldGrant is the part of a grant the check reads.
type HeldGrant struct {
	Relation     string
	ResourceType string
	ResourceID   string
	Revoked      bool
}

// AccessChecker decides whether a principal may see a record of a type whose
// access is AccessByGrant. An administrator sees everything. Anyone else needs
// can_see_references on reference_type <type>, on reference_type *, or on
// reference <id>; holding none of those sees none.
type AccessChecker struct {
	Directory AccessDirectory
	TTL       time.Duration
	now       func() time.Time

	mu       sync.Mutex
	accesses map[string]cachedAccess
}

// PrincipalAccess is what one principal may see.
type PrincipalAccess struct {
	Administrator bool
	AllTypes      bool
	Types         map[string]bool
	Records       map[string]bool
}

type cachedAccess struct {
	access    PrincipalAccess
	expiresAt time.Time
}

func (c *AccessChecker) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// MaySee answers for one record.
func (c *AccessChecker) MaySee(ctx context.Context, principalID, typeName, id string) (bool, error) {
	access, err := c.Of(ctx, principalID)
	if err != nil {
		return false, err
	}
	return access.Administrator || access.AllTypes || access.Types[typeName] || access.Records[id], nil
}

// MaySeeType answers for every record of a type, as a search needs.
func (c *AccessChecker) MaySeeType(ctx context.Context, principalID, typeName string) (bool, error) {
	access, err := c.Of(ctx, principalID)
	if err != nil {
		return false, err
	}
	return access.Administrator || access.AllTypes || access.Types[typeName], nil
}

// Of reads, or reuses, what a principal may see. A store that cannot answer is
// an error, never an empty answer: an empty one would read as "sees nothing"
// and hide every chip without saying why.
func (c *AccessChecker) Of(ctx context.Context, principalID string) (PrincipalAccess, error) {
	if principalID == "" {
		return PrincipalAccess{}, fmt.Errorf("no principal to check")
	}
	now := c.clock()
	c.mu.Lock()
	if cached, ok := c.accesses[principalID]; ok && now.Before(cached.expiresAt) {
		c.mu.Unlock()
		return cached.access, nil
	}
	c.mu.Unlock()

	standing, err := c.Directory.Standing(ctx, principalID)
	if err != nil {
		return PrincipalAccess{}, fmt.Errorf("principal-store: %w", err)
	}
	access := PrincipalAccess{Types: map[string]bool{}, Records: map[string]bool{}}
	if standing.Disabled {
		// A disabled principal sees nothing, whatever it holds.
	} else if standing.IsAdministrator {
		access.Administrator = true
	} else {
		grants, err := c.Directory.Grants(ctx, principalID)
		if err != nil {
			return PrincipalAccess{}, fmt.Errorf("grant-store: %w", err)
		}
		for _, grant := range grants {
			if grant.Relation != RelationCanSeeReferences || grant.Revoked {
				continue
			}
			switch grant.ResourceType {
			case ResourceTypeReferenceType:
				if grant.ResourceID == allResources {
					access.AllTypes = true
				} else {
					access.Types[grant.ResourceID] = true
				}
			case ResourceTypeReference:
				access.Records[grant.ResourceID] = true
			}
		}
	}
	c.mu.Lock()
	if c.accesses == nil {
		c.accesses = map[string]cachedAccess{}
	}
	c.accesses[principalID] = cachedAccess{access: access, expiresAt: now.Add(c.TTL)}
	c.mu.Unlock()
	return access, nil
}
