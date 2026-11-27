// Package grantcheck decides what a principal's grants allow, in one place.
//
// Every check this server makes against grant-store — where a session may
// start, as which agent, which tools it is offered, which operations a
// principal may start, which instances it is shown — asks the same question:
// does this principal's effective grants of one relation allow this resource?
// The answer has three rules, and they live here so no caller restates them:
//
//   - A grant naming resource_id "*" (grants.AllResources) names every
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

package grantcheck

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/kayushkin/llm-bridge-server/internal/access/requestcaller"

	"github.com/kayushkin/llm-bridge-server/internal/access/grants"
)

// GrantReader is grant-store as this server reads and writes it:
// *grants.Store in production, a fake in tests. Effective answers
// grants.ErrNotFound for a principal principal-store does not have, never an
// empty list, so a spawn can tell "nothing granted" from "no such principal".
type GrantReader interface {
	Effective(ctx context.Context, directory grants.PrincipalDirectory, principalID, relation, resourceType string) ([]*grants.Grant, error)
	Create(ctx context.Context, directory grants.PrincipalDirectory, checker grants.ResourceChecker, request grants.GrantRequest) (*grants.Grant, bool, error)
}

// Subject is whose grants decide: a principal, and, when a connected
// server acts for one of its users, the peer principal that server acts as.
type Subject struct {
	PrincipalID string
	// ThroughPeerPrincipalID is the peer principal, empty for a principal
	// acting on its own.
	ThroughPeerPrincipalID string
}

func (subject Subject) String() string {
	if subject.ThroughPeerPrincipalID == "" {
		return subject.PrincipalID
	}
	return subject.PrincipalID + " through peer " + subject.ThroughPeerPrincipalID
}

// SubjectOfCaller is whose grants decide for an authorized request.
func SubjectOfCaller(caller requestcaller.Caller) Subject {
	subject := Subject{PrincipalID: caller.PrincipalID}
	if caller.Peer.ActsForRemoteUser() {
		subject.ThroughPeerPrincipalID = caller.Peer.PeerPrincipalID
	}
	return subject
}

// Resources is what one principal's effective grants of one relation
// say about one resource type, and, for a subject acting through a peer,
// what the peer's say too (alsoLimitedBy).
type Resources struct {
	principalID  string
	relation     string
	resourceType string
	// resourceIDs is every id the grants name, "*" included, as grant-store
	// answered them.
	resourceIDs []string
	// RestrictedToItsGrants is principal-store's answer for the principal's
	// kind: holding no grant of a relation refuses it.
	RestrictedToItsGrants bool
	// administratorInIntersection is set on a side of an intersection that
	// principal-store marks an administrator: that side restricts nothing.
	administratorInIntersection bool
	// alsoLimitedBy is the peer's grants when the subject acts through a
	// peer: the answer is allowed only where both allow it.
	alsoLimitedBy *Resources
}

// restrictsOnItsOwn reports whether this one principal's grants limit it.
func (granted Resources) restrictsOnItsOwn() bool {
	if granted.administratorInIntersection {
		return false
	}
	return len(granted.resourceIDs) > 0 || granted.RestrictedToItsGrants
}

// namesOnItsOwn reports whether one of this principal's grants names the
// resource, directly or as "*".
func (granted Resources) namesOnItsOwn(resourceID string) bool {
	for _, grantedID := range granted.resourceIDs {
		if grantedID == grants.AllResources || grantedID == resourceID {
			return true
		}
	}
	return false
}

// allowsOnItsOwn reports whether this one principal's grants allow the
// resource.
func (granted Resources) allowsOnItsOwn(resourceID string) bool {
	return !granted.restrictsOnItsOwn() || granted.namesOnItsOwn(resourceID)
}

// Restricts reports whether the relation limits the subject at all.
func (granted Resources) Restricts() bool {
	return granted.restrictsOnItsOwn() || (granted.alsoLimitedBy != nil && granted.alsoLimitedBy.restrictsOnItsOwn())
}

// names reports whether the subject's grants name this resource, directly or
// as "*": every side that restricts names it, and at least one restricts.
func (granted Resources) names(resourceID string) bool {
	if !granted.Restricts() {
		return false
	}
	return granted.allowsOnItsOwn(resourceID) && (granted.alsoLimitedBy == nil || granted.alsoLimitedBy.allowsOnItsOwn(resourceID))
}

// RequiresAGrantAndNames reports whether every side names the resource,
// for a relation where holding no grant allows nothing whatever the kind,
// such as can_observe_sessions_on. A side principal-store marks an
// administrator needs no grant.
func (granted Resources) RequiresAGrantAndNames(resourceID string) bool {
	side := func(one Resources) bool {
		return one.administratorInIntersection || one.namesOnItsOwn(resourceID)
	}
	return side(granted) && (granted.alsoLimitedBy == nil || side(*granted.alsoLimitedBy))
}

// Permits reports whether the subject may use this resource by this
// relation.
func (granted Resources) Permits(resourceID string) bool {
	return granted.allowsOnItsOwn(resourceID) && (granted.alsoLimitedBy == nil || granted.alsoLimitedBy.allowsOnItsOwn(resourceID))
}

// NamesAll reports whether a grant names every resource of the type.
func (granted Resources) NamesAll() bool {
	return granted.names(grants.AllResources)
}

// Describe says what the grants are, for a refusal or a note.
func (granted Resources) Describe() string {
	if granted.alsoLimitedBy != nil {
		return granted.describeOnItsOwn() + "; and, acting through that peer, " + granted.alsoLimitedBy.describeOnItsOwn()
	}
	return granted.describeOnItsOwn()
}

func (granted Resources) describeOnItsOwn() string {
	switch {
	case granted.administratorInIntersection:
		return fmt.Sprintf("%s is an administrator, so its own grants restrict nothing here", granted.principalID)
	case len(granted.resourceIDs) > 0:
		return fmt.Sprintf("%s's %s grants name %ss %v", granted.principalID, granted.relation, granted.resourceType, granted.resourceIDs)
	case granted.RestrictedToItsGrants:
		return fmt.Sprintf("%s holds no %s grant on a %s, and principal-store marks its kind restricted to its grants, so it may use none", granted.principalID, granted.relation, granted.resourceType)
	default:
		return fmt.Sprintf("%s holds no %s grant on a %s, so any is allowed (lenient)", granted.principalID, granted.relation, granted.resourceType)
	}
}

// GrantedResourcesOf reads a subject's effective grants of one relation: its
// principal's, and the peer's when it acts through one, each with whether
// principal-store marks it restricted to its grants. Any store failing is an
// error: an unread grant set is never treated as an empty one.
func (s *Checker) GrantedResourcesOf(ctx context.Context, subject Subject, relation, resourceType string) (Resources, error) {
	own, err := s.grantedResourcesOfOnePrincipal(ctx, subject.PrincipalID, relation, resourceType, subject.ThroughPeerPrincipalID != "")
	if err != nil {
		return Resources{}, err
	}
	if subject.ThroughPeerPrincipalID == "" {
		return own, nil
	}
	peer, err := s.grantedResourcesOfOnePrincipal(ctx, subject.ThroughPeerPrincipalID, relation, resourceType, true)
	if err != nil {
		return Resources{}, err
	}
	own.alsoLimitedBy = &peer
	return own, nil
}

func (s *Checker) grantedResourcesOfOnePrincipal(ctx context.Context, principalID, relation, resourceType string, partOfIntersection bool) (Resources, error) {
	if s.GrantStore() == nil {
		return Resources{}, fmt.Errorf("%s's %s grants are read from grant-store, and this server has none (LLMBRIDGE_GRANT_STORE_URL)", principalID, relation)
	}
	resourceIDs, err := s.EffectiveGrantResourceIDs(ctx, principalID, relation, resourceType)
	if err != nil {
		return Resources{}, fmt.Errorf("could not read %s's %s grants: %w", principalID, relation, err)
	}
	if s.PrincipalLookupCache() == nil {
		return Resources{}, fmt.Errorf("%s's %s grants depend on what principal-store says its kind is, and this server has no principal lookup", principalID, relation)
	}
	principal, err := s.PrincipalLookupCache().LookUp(ctx, principalID)
	if err != nil {
		return Resources{}, fmt.Errorf("could not read principal %s from principal-store to know whether its grants restrict it: %w", principalID, err)
	}
	return Resources{
		principalID:                 principalID,
		relation:                    relation,
		resourceType:                resourceType,
		resourceIDs:                 resourceIDs,
		RestrictedToItsGrants:       principal.RestrictedToItsGrants,
		administratorInIntersection: partOfIntersection && principal.IsAdministrator,
	}, nil
}

// DispatchGrants is a principal's can_dispatch_on grants on instances and on
// machines, which together say where its sessions may start.
type DispatchGrants struct {
	instances Resources
	machines  Resources
}

func (s *Checker) DispatchGrantsOf(ctx context.Context, subject Subject) (DispatchGrants, error) {
	instances, err := s.GrantedResourcesOf(ctx, subject, "can_dispatch_on", "instance")
	if err != nil {
		return DispatchGrants{}, err
	}
	machines, err := s.GrantedResourcesOf(ctx, subject, "can_dispatch_on", "machine")
	if err != nil {
		return DispatchGrants{}, err
	}
	return DispatchGrants{instances: instances, machines: machines}, nil
}

// Restricts reports whether the grants limit where sessions start. Grants on
// instances and on machines are one relation, so holding either restricts.
func (granted DispatchGrants) Restricts() bool {
	return granted.instances.Restricts() || granted.machines.Restricts()
}

// Permits reports whether a session may start on this instance: for each
// principal of the subject, its grants on the instance or on its machine
// allow it.
func (granted DispatchGrants) Permits(instanceID, machineID string) bool {
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
func (DispatchGrants) allowsOnItsOwn(instances, machines Resources, instanceID, machineID string) bool {
	if !instances.restrictsOnItsOwn() && !machines.restrictsOnItsOwn() {
		return true
	}
	return instances.namesOnItsOwn(instanceID) || machines.namesOnItsOwn(machineID)
}

func (granted DispatchGrants) Describe() string {
	if !granted.Restricts() {
		return granted.instances.Describe()
	}
	return fmt.Sprintf("%s's can_dispatch_on grants name instances %v and machines %v (restricted to its grants: %t)",
		granted.instances.principalID, granted.instances.resourceIDs, granted.machines.resourceIDs, granted.instances.RestrictedToItsGrants)
}

// PermitsTool reports whether a can_use grant set allows a tool-store id.
func (granted Resources) PermitsTool(toolID int64) bool {
	return granted.Permits(strconv.FormatInt(toolID, 10))
}

// NamesTool reports whether a can_use grant names a tool-store id, directly
// or as "*".
func (granted Resources) NamesTool(toolID int64) bool {
	return granted.names(strconv.FormatInt(toolID, 10))
}

// EffectiveGrantResourceIDs returns the resource ids the principal's effective
// grants of one relation name, for one resource type: its own active grants
// and every active group's, with the groups read from principal-store on each
// call, so a membership change is visible at the next spawn. An empty answer
// means the principal and every group it is in hold no such grant; a
// principal principal-store does not have is an error wrapping
// grants.ErrNotFound.
func (s *Checker) EffectiveGrantResourceIDs(ctx context.Context, principalID, relation, resourceType string) ([]string, error) {
	effective, err := s.GrantStore().Effective(ctx, s.GrantsPrincipalDirectory(), principalID, relation, resourceType)
	if errors.Is(err, grants.ErrNotFound) {
		return nil, fmt.Errorf("principal unknown: %w", err)
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
