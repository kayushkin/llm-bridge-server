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
//
// A request a connected server makes for one of its users (phase 2 of design
// note d107154a) is decided for two principals at once: the local principal
// the user is mapped to and the peer principal the server acts as. The
// subject then carries both, and the answer is the intersection — a resource
// is allowed only when each principal's grants allow it by the rules above,
// a side principal-store marks an administrator restricting nothing.

import (
	"context"
	"fmt"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/grantclient"
)

// grantSubject is whose grants decide: a principal, and, when a connected
// server acts for one of its users, the peer principal that server acts as.
type grantSubject struct {
	principalID string
	// throughPeerPrincipalID is the peer principal, empty for a principal
	// acting on its own.
	throughPeerPrincipalID string
}

func (subject grantSubject) String() string {
	if subject.throughPeerPrincipalID == "" {
		return subject.principalID
	}
	return subject.principalID + " through peer " + subject.throughPeerPrincipalID
}

// grantSubjectOfCaller is whose grants decide for an authorized request.
func grantSubjectOfCaller(caller requestCaller) grantSubject {
	subject := grantSubject{principalID: caller.principalID}
	if caller.peer.actsForRemoteUser() {
		subject.throughPeerPrincipalID = caller.peer.peerPrincipalID
	}
	return subject
}

// grantedResources is what one principal's effective grants of one relation
// say about one resource type, and, for a subject acting through a peer,
// what the peer's say too (alsoLimitedBy).
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
	// administratorInIntersection is set on a side of an intersection that
	// principal-store marks an administrator: that side restricts nothing.
	administratorInIntersection bool
	// alsoLimitedBy is the peer's grants when the subject acts through a
	// peer: the answer is allowed only where both allow it.
	alsoLimitedBy *grantedResources
}

// restrictsOnItsOwn reports whether this one principal's grants limit it.
func (granted grantedResources) restrictsOnItsOwn() bool {
	if granted.administratorInIntersection {
		return false
	}
	return len(granted.resourceIDs) > 0 || granted.restrictedToItsGrants
}

// namesOnItsOwn reports whether one of this principal's grants names the
// resource, directly or as "*".
func (granted grantedResources) namesOnItsOwn(resourceID string) bool {
	for _, grantedID := range granted.resourceIDs {
		if grantedID == grantclient.AllResources || grantedID == resourceID {
			return true
		}
	}
	return false
}

// allowsOnItsOwn reports whether this one principal's grants allow the
// resource.
func (granted grantedResources) allowsOnItsOwn(resourceID string) bool {
	return !granted.restrictsOnItsOwn() || granted.namesOnItsOwn(resourceID)
}

// restricts reports whether the relation limits the subject at all.
func (granted grantedResources) restricts() bool {
	return granted.restrictsOnItsOwn() || (granted.alsoLimitedBy != nil && granted.alsoLimitedBy.restrictsOnItsOwn())
}

// names reports whether the subject's grants name this resource, directly or
// as "*": every side that restricts names it, and at least one restricts.
func (granted grantedResources) names(resourceID string) bool {
	if !granted.restricts() {
		return false
	}
	return granted.allowsOnItsOwn(resourceID) && (granted.alsoLimitedBy == nil || granted.alsoLimitedBy.allowsOnItsOwn(resourceID))
}

// requiresAGrantAndNames reports whether every side names the resource,
// for a relation where holding no grant allows nothing whatever the kind,
// such as can_observe_sessions_on. A side principal-store marks an
// administrator needs no grant.
func (granted grantedResources) requiresAGrantAndNames(resourceID string) bool {
	side := func(one grantedResources) bool {
		return one.administratorInIntersection || one.namesOnItsOwn(resourceID)
	}
	return side(granted) && (granted.alsoLimitedBy == nil || side(*granted.alsoLimitedBy))
}

// permits reports whether the subject may use this resource by this
// relation.
func (granted grantedResources) permits(resourceID string) bool {
	return granted.allowsOnItsOwn(resourceID) && (granted.alsoLimitedBy == nil || granted.alsoLimitedBy.allowsOnItsOwn(resourceID))
}

// namesAll reports whether a grant names every resource of the type.
func (granted grantedResources) namesAll() bool {
	return granted.names(grantclient.AllResources)
}

// describe says what the grants are, for a refusal or a note.
func (granted grantedResources) describe() string {
	if granted.alsoLimitedBy != nil {
		return granted.describeOnItsOwn() + "; and, acting through that peer, " + granted.alsoLimitedBy.describeOnItsOwn()
	}
	return granted.describeOnItsOwn()
}

func (granted grantedResources) describeOnItsOwn() string {
	switch {
	case granted.administratorInIntersection:
		return fmt.Sprintf("%s is an administrator, so its own grants restrict nothing here", granted.principalID)
	case len(granted.resourceIDs) > 0:
		return fmt.Sprintf("%s's %s grants name %ss %v", granted.principalID, granted.relation, granted.resourceType, granted.resourceIDs)
	case granted.restrictedToItsGrants:
		return fmt.Sprintf("%s holds no %s grant on a %s, and principal-store marks its kind restricted to its grants, so it may use none", granted.principalID, granted.relation, granted.resourceType)
	default:
		return fmt.Sprintf("%s holds no %s grant on a %s, so any is allowed (lenient)", granted.principalID, granted.relation, granted.resourceType)
	}
}

// grantedResourcesOf reads a subject's effective grants of one relation: its
// principal's, and the peer's when it acts through one, each with whether
// principal-store marks it restricted to its grants. Any store failing is an
// error: an unread grant set is never treated as an empty one.
func (s *Server) grantedResourcesOf(ctx context.Context, subject grantSubject, relation, resourceType string) (grantedResources, error) {
	own, err := s.grantedResourcesOfOnePrincipal(ctx, subject.principalID, relation, resourceType, subject.throughPeerPrincipalID != "")
	if err != nil {
		return grantedResources{}, err
	}
	if subject.throughPeerPrincipalID == "" {
		return own, nil
	}
	peer, err := s.grantedResourcesOfOnePrincipal(ctx, subject.throughPeerPrincipalID, relation, resourceType, true)
	if err != nil {
		return grantedResources{}, err
	}
	own.alsoLimitedBy = &peer
	return own, nil
}

func (s *Server) grantedResourcesOfOnePrincipal(ctx context.Context, principalID, relation, resourceType string, partOfIntersection bool) (grantedResources, error) {
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
		principalID:                 principalID,
		relation:                    relation,
		resourceType:                resourceType,
		resourceIDs:                 resourceIDs,
		restrictedToItsGrants:       principal.RestrictedToItsGrants,
		administratorInIntersection: partOfIntersection && principal.IsAdministrator,
	}, nil
}

// dispatchGrants is a principal's can_dispatch_on grants on instances and on
// machines, which together say where its sessions may start.
type dispatchGrants struct {
	instances grantedResources
	machines  grantedResources
}

func (s *Server) dispatchGrantsOf(ctx context.Context, subject grantSubject) (dispatchGrants, error) {
	instances, err := s.grantedResourcesOf(ctx, subject, "can_dispatch_on", "instance")
	if err != nil {
		return dispatchGrants{}, err
	}
	machines, err := s.grantedResourcesOf(ctx, subject, "can_dispatch_on", "machine")
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

// permits reports whether a session may start on this instance: for each
// principal of the subject, its grants on the instance or on its machine
// allow it.
func (granted dispatchGrants) permits(instanceID, machineID string) bool {
	if !granted.allowsOnItsOwn(granted.instances, granted.machines, instanceID, machineID) {
		return false
	}
	if granted.instances.alsoLimitedBy == nil {
		return true
	}
	return granted.allowsOnItsOwn(*granted.instances.alsoLimitedBy, *granted.machines.alsoLimitedBy, instanceID, machineID)
}

// allowsOnItsOwn applies one principal's instance and machine grants, which
// are one relation: holding either restricts, and either naming allows.
func (dispatchGrants) allowsOnItsOwn(instances, machines grantedResources, instanceID, machineID string) bool {
	if !instances.restrictsOnItsOwn() && !machines.restrictsOnItsOwn() {
		return true
	}
	return instances.namesOnItsOwn(instanceID) || machines.namesOnItsOwn(machineID)
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
