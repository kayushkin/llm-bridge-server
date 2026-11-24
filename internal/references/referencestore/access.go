package referencestore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
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

// AccessChecker decides whether a principal may see a record of a type whose
// access is AccessByGrant. An administrator sees everything. Anyone else needs
// can_see_references on reference_type <type>, on reference_type *, or on
// reference <id>; holding none of those sees none.
type AccessChecker struct {
	PrincipalStoreURL string
	GrantStoreURL     string
	HTTPClient        *http.Client
	TTL               time.Duration
	now               func() time.Time

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

	var principal struct {
		IsAdministrator bool  `json:"is_administrator"`
		DisabledAt      int64 `json:"disabled_at"`
	}
	if err := c.getJSON(ctx, strings.TrimRight(c.PrincipalStoreURL, "/")+"/principals/"+url.PathEscape(principalID), principalID, &principal); err != nil {
		return PrincipalAccess{}, fmt.Errorf("principal-store: %w", err)
	}
	access := PrincipalAccess{Types: map[string]bool{}, Records: map[string]bool{}}
	if principal.DisabledAt != 0 {
		// A disabled principal sees nothing, whatever it holds.
	} else if principal.IsAdministrator {
		access.Administrator = true
	} else {
		var grants []struct {
			Relation     string `json:"relation"`
			ResourceType string `json:"resource_type"`
			ResourceID   string `json:"resource_id"`
			RevokedAt    int64  `json:"revoked_at"`
		}
		if err := c.getJSON(ctx, strings.TrimRight(c.GrantStoreURL, "/")+"/principals/"+url.PathEscape(principalID)+"/effective", principalID, &grants); err != nil {
			return PrincipalAccess{}, fmt.Errorf("grant-store: %w", err)
		}
		for _, grant := range grants {
			if grant.Relation != RelationCanSeeReferences || grant.RevokedAt != 0 {
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

// getJSON asks as the principal: grant-store lets a principal read its own
// effective grants, and principal-store answers anyone.
func (c *AccessChecker) getJSON(ctx context.Context, target, principalID string, into any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set(PrincipalIDHeader, principalID)
	response, err := c.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d: %s", target, response.StatusCode, firstLine(body))
	}
	return json.Unmarshal(body, into)
}
