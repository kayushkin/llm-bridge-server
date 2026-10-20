package server

// What a principal's grants allow, decided in one place.
//
// Every check this server makes against grant-store — where a session may
// start, as which agent, which tools it is offered, which operations a
// principal may start, which instances it is shown — asks the same question:
// does this principal's effective grants of one relation allow this resource?
// The answer has three rules, and they live here so no caller restates them:
//
//   - A grant naming resource_id "*" (grantclient.AllResources) names every
//     resource of its type.
//   - A principal holding no grant of the relation is not restricted by it —
//     the lenient rule the operator chose on 2026-09-11 — unless
//     principal-store marks its kind restricted_to_its_grants, as it marks a
//     peer (another llm-bridge-server). Then holding none allows nothing, so a
//     link nobody has granted anything gives nothing.
//   - Otherwise the principal may use exactly what its grants name.
//
// An administrator never reaches these checks; the callers skip them.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
)

// grantedResources is what one principal's effective grants of one relation
// say about one resource type.
type grantedResources struct {
	principalID  string
	relation     string
	resourceType string
	// resourceIDs is every id the grants name, "*" included, as grant-store
	// answered them.
	resourceIDs []string
	// restrictedToItsGrants is principal-store's answer for the principal's
	// kind: holding no grant of a relation refuses it.
	restrictedToItsGrants bool
}

// restricts reports whether the relation limits the principal at all.
func (granted grantedResources) restricts() bool {
	return len(granted.resourceIDs) > 0 || granted.restrictedToItsGrants
}

// names reports whether a grant names this resource, directly or as "*".
func (granted grantedResources) names(resourceID string) bool {
	for _, grantedID := range granted.resourceIDs {
		if grantedID == grantclient.AllResources || grantedID == resourceID {
			return true
		}
	}
	return false
}

// permits reports whether the principal may use this resource by this
// relation.
func (granted grantedResources) permits(resourceID string) bool {
	return !granted.restricts() || granted.names(resourceID)
}

// namesAll reports whether a grant names every resource of the type.
func (granted grantedResources) namesAll() bool {
	return granted.names(grantclient.AllResources)
}

// describe says what the grants are, for a refusal or a note.
func (granted grantedResources) describe() string {
	switch {
	case len(granted.resourceIDs) > 0:
		return fmt.Sprintf("%s's %s grants name %ss %v", granted.principalID, granted.relation, granted.resourceType, granted.resourceIDs)
	case granted.restrictedToItsGrants:
		return fmt.Sprintf("%s holds no %s grant on a %s, and principal-store marks its kind restricted to its grants, so it may use none", granted.principalID, granted.relation, granted.resourceType)
	default:
		return fmt.Sprintf("%s holds no %s grant on a %s, so any is allowed (lenient)", granted.principalID, granted.relation, granted.resourceType)
	}
}

// grantedResourcesOf reads one principal's effective grants of one relation
// and whether principal-store marks it restricted to its grants. Either store
// failing is an error: an unread grant set is never treated as an empty one.
func (s *Server) grantedResourcesOf(ctx context.Context, principalID, relation, resourceType string) (grantedResources, error) {
	if s.grantClient == nil {
		return grantedResources{}, fmt.Errorf("%s's %s grants are read from grant-store, and this server has none (LLMBRIDGE_GRANT_STORE_URL)", principalID, relation)
	}
	resourceIDs, err := s.grantClient.EffectiveResourceIDs(ctx, principalID, relation, resourceType)
	if err != nil {
		return grantedResources{}, fmt.Errorf("could not read %s's %s grants: %w", principalID, relation, err)
	}
	if s.principalLookupCache == nil {
		return grantedResources{}, fmt.Errorf("%s's %s grants depend on what principal-store says its kind is, and this server has no principal lookup", principalID, relation)
	}
	principal, err := s.principalLookupCache.lookUp(ctx, principalID)
	if err != nil {
		return grantedResources{}, fmt.Errorf("could not read principal %s from principal-store to know whether its grants restrict it: %w", principalID, err)
	}
	return grantedResources{
		principalID:           principalID,
		relation:              relation,
		resourceType:          resourceType,
		resourceIDs:           resourceIDs,
		restrictedToItsGrants: principal.RestrictedToItsGrants,
	}, nil
}

// dispatchGrants is a principal's can_dispatch_on grants on instances and on
// machines, which together say where its sessions may start.
type dispatchGrants struct {
	instances grantedResources
	machines  grantedResources
}

func (s *Server) dispatchGrantsOf(ctx context.Context, principalID string) (dispatchGrants, error) {
	instances, err := s.grantedResourcesOf(ctx, principalID, "can_dispatch_on", "instance")
	if err != nil {
		return dispatchGrants{}, err
	}
	machines, err := s.grantedResourcesOf(ctx, principalID, "can_dispatch_on", "machine")
	if err != nil {
		return dispatchGrants{}, err
	}
	return dispatchGrants{instances: instances, machines: machines}, nil
}

// restricts reports whether the grants limit where sessions start. Grants on
// instances and on machines are one relation, so holding either restricts.
func (granted dispatchGrants) restricts() bool {
	return granted.instances.restricts() || granted.machines.restricts()
}

// permits reports whether a session may start on this instance.
func (granted dispatchGrants) permits(instanceID, machineID string) bool {
	return !granted.restricts() || granted.instances.names(instanceID) || granted.machines.names(machineID)
}

func (granted dispatchGrants) describe() string {
	if !granted.restricts() {
		return granted.instances.describe()
	}
	return fmt.Sprintf("%s's can_dispatch_on grants name instances %v and machines %v (restricted to its grants: %t)",
		granted.instances.principalID, granted.instances.resourceIDs, granted.machines.resourceIDs, granted.instances.restrictedToItsGrants)
}

// permitsTool reports whether a can_use grant set allows a tool-store id.
func (granted grantedResources) permitsTool(toolID int64) bool {
	return granted.permits(strconv.FormatInt(toolID, 10))
}

// namesTool reports whether a can_use grant names a tool-store id, directly
// or as "*".
func (granted grantedResources) namesTool(toolID int64) bool {
	return granted.names(strconv.FormatInt(toolID, 10))
}
