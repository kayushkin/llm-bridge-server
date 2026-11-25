package grantstore

import (
	"fmt"
	"strings"
)

// The relation vocabulary is served (GET /relations) so no caller builds a
// picker out of whatever values happen to be in the rows. Each relation names
// the resource types it may pair with; a grant pairing a relation with a type
// outside that list is a 400 naming the allowed ones.
//
// can_view, can_edit and can_administer are board access, enforced by
// kanban-store when it runs with principal enforcement on. This store keeps
// them as three independent tuples; kanban-store decides that can_administer
// includes can_edit and can_edit includes can_view, because what a relation on
// a board lets someone do is the board owner's rule, not this store's.
//
// Enforced relations are the ones llm-bridge-server reads at session start:
// a session with a principal is offered only the tools and skills it can_use,
// may only run as an agent it can_run_as, and may only start on an instance
// or machine it can_dispatch_on. works_with is advisory — it is the list
// principal-store used to keep, moved here so there is one place to look —
// and nothing refuses anything because of it.
//
// A grant whose resource_id is AllResources ("*") names every resource of its
// type, including ones created after the grant. Only a relation whose
// enforcing service reads "*" accepts it: llm-bridge-server's five. kanban-store
// does not read it yet, so a board grant of "*" is refused here rather than
// stored and silently ignored there; works_with is advisory and a list of
// everything advises nothing.
//
// Nothing here is normalized: the relation arrives in a body and is stored as
// a join key, so "Can_Use" is a 400 naming the right spelling, not a rewrite.

const (
	RelationCanUse          = "can_use"
	RelationCanRunAs        = "can_run_as"
	RelationCanDispatchOn   = "can_dispatch_on"
	RelationWorksWith       = "works_with"
	RelationCanView         = "can_view"
	RelationCanEdit         = "can_edit"
	RelationCanAdminister   = "can_administer"
	RelationCanRunOperation = "can_run_operation"
	// RelationCanObserveSessionsOn lets a principal read sessions it does
	// not own on an instance or machine, and change nothing.
	RelationCanObserveSessionsOn = "can_observe_sessions_on"
	RelationCanStartSessionsAs   = "can_start_sessions_as"
	RelationCanCallThrough       = "can_call_through"
	// RelationCanSeeReferences lets a principal see records drawn as
	// reference chips: every record of a type, or one record.
	RelationCanSeeReferences = "can_see_references"
	// RelationCanReadMail lets a principal read the mail of one mailstack
	// account; RelationCanImportMail also lets it import into the account
	// and change its messages, and includes can_read_mail. mailstack reads
	// both.
	RelationCanReadMail   = "can_read_mail"
	RelationCanImportMail = "can_import_mail"
)

// AllResources is the resource_id that names every resource of the grant's
// type, now and later.
const AllResources = "*"

// RelationDefinition is everything a caller needs to offer one relation.
type RelationDefinition struct {
	Name          string   `json:"name"`
	ResourceTypes []string `json:"resource_types"`
	// Enforced says llm-bridge-server filters a session's offer by this
	// relation. False means the relation is a list someone reads, not a lock.
	Enforced bool `json:"enforced"`
	// AcceptsAllResources says a grant of this relation may name resource_id
	// "*", meaning every resource of its type.
	AcceptsAllResources bool   `json:"accepts_all_resources"`
	Description         string `json:"description"`
	// allResourcesRefusal is why a relation refuses "*", for the 400.
	allResourcesRefusal string
}

// Relations is every relation a grant can carry, in the order GET /relations
// serves them.
var Relations = []RelationDefinition{
	{
		Name:                RelationCanUse,
		AcceptsAllResources: true,
		ResourceTypes:       []string{ResourceTypeTool, ResourceTypeSkill},
		Enforced:            true,
		Description:         "may be offered this tool or skill in a session started as this principal",
	},
	{
		Name:                RelationCanRunAs,
		AcceptsAllResources: true,
		ResourceTypes:       []string{ResourceTypeAgent},
		Enforced:            true,
		Description:         "may start a session as this agent",
	},
	{
		Name:                RelationCanDispatchOn,
		AcceptsAllResources: true,
		ResourceTypes:       []string{ResourceTypeInstance, ResourceTypeMachine},
		Enforced:            true,
		Description:         "may start a session on this harness instance or machine",
	},
	{
		Name:                RelationCanView,
		allResourcesRefusal: "kanban-store does not read a board grant of \"*\" yet",
		ResourceTypes:       []string{ResourceTypeBoard},
		Enforced:            true,
		Description:         "may see this kanban board and its cards; kanban-store enforces it",
	},
	{
		Name:                RelationCanEdit,
		allResourcesRefusal: "kanban-store does not read a board grant of \"*\" yet",
		ResourceTypes:       []string{ResourceTypeBoard},
		Enforced:            true,
		Description:         "may create, move, assign and annotate cards on this kanban board, and includes can_view; kanban-store enforces it",
	},
	{
		Name:                RelationCanAdminister,
		allResourcesRefusal: "kanban-store does not read a board grant of \"*\" yet",
		ResourceTypes:       []string{ResourceTypeBoard},
		Enforced:            true,
		Description:         "may change this kanban board's settings, columns, rules and triggers, or delete it, and includes can_edit; kanban-store enforces it",
	},
	{
		Name:                RelationCanRunOperation,
		AcceptsAllResources: true,
		ResourceTypes:       []string{ResourceTypeOperationType},
		Enforced:            true,
		Description:         "may start operations of this type through llm-bridge-server's POST /operations; the bridge's operations.grant_enforcement says whether holding no such grant restricts",
	},
	{
		Name:                RelationCanObserveSessionsOn,
		AcceptsAllResources: true,
		ResourceTypes:       []string{ResourceTypeInstance, ResourceTypeMachine},
		Enforced:            true,
		Description:         "may read sessions it did not start on this harness instance or machine, and change none of them; llm-bridge-server reads it on its read-only session routes, and holding none allows nothing",
	},
	{
		Name:                RelationCanStartSessionsAs,
		allResourcesRefusal: "starting sessions as every principal is every principal's access at once; name each one",
		ResourceTypes:       []string{ResourceTypePrincipal},
		Enforced:            true,
		Description:         "may start a session on llm-bridge-server that runs as this principal, and drive the sessions it started that way; the session holds that principal's grants, not the starter's",
	},
	{
		Name:                RelationCanCallThrough,
		AcceptsAllResources: true,
		ResourceTypes:       []string{ResourceTypePeerConnection},
		Enforced:            true,
		Description:         "may see this llm-bridge-server peer connection and call the connected server through it; llm-bridge-server reads it on GET /peer-connections and its proxy for a principal restricted to its grants",
	},
	{
		Name:                RelationCanSeeReferences,
		AcceptsAllResources: true,
		ResourceTypes:       []string{ResourceTypeReferenceType, ResourceTypeReference},
		Enforced:            true,
		Description:         "may see records drawn as reference chips: every record of a reference type (or of every type, with *), or one record; reference-store reads it, an administrator needs none, and holding none shows none",
	},
	{
		Name:                RelationWorksWith,
		allResourcesRefusal: "works_with is advisory, and a list of everything advises nothing",
		ResourceTypes:       []string{ResourceTypeAgent, ResourceTypeInstance, ResourceTypeMachine, ResourceTypeSkill, ResourceTypeTool},
		Enforced:            false,
		Description:         "advisory: what this principal usually works with; a card's dispatch picker reads it to put an assignee's instances first, and nothing refuses anything because of it",
	},
	{
		Name:                RelationCanReadMail,
		allResourcesRefusal: "mailstack reads a mail grant for one account at a time, and every account's mail at once is not a grant it serves",
		ResourceTypes:       []string{ResourceTypeMailAccount},
		Enforced:            true,
		Description:         "may read the folders, threads and messages of this mailstack account; mailstack enforces it, an administrator needs none, and holding none reads only the messages a ticket the principal may view links",
	},
	{
		Name:                RelationCanImportMail,
		allResourcesRefusal: "mailstack reads a mail grant for one account at a time, and every account's mail at once is not a grant it serves",
		ResourceTypes:       []string{ResourceTypeMailAccount},
		Enforced:            true,
		Description:         "may import mail into this mailstack account and change its messages, and includes can_read_mail; mailstack enforces it",
	},
}

// RelationNames is the names alone, for error messages.
var RelationNames = func() []string {
	names := make([]string, 0, len(Relations))
	for _, definition := range Relations {
		names = append(names, definition.Name)
	}
	return names
}()

// lookupRelation resolves a caller's relation to its definition, or the 400
// that names the whole vocabulary.
func lookupRelation(raw string) (RelationDefinition, error) {
	for _, definition := range Relations {
		if raw == definition.Name {
			return definition, nil
		}
	}
	return RelationDefinition{}, fmt.Errorf("%w: unknown relation %q: use one of %s",
		ErrInvalidGrant, raw, strings.Join(RelationNames, ", "))
}

// allowsResourceType says whether a relation may pair with a resource type.
func (d RelationDefinition) allowsResourceType(resourceType string) bool {
	for _, allowed := range d.ResourceTypes {
		if allowed == resourceType {
			return true
		}
	}
	return false
}
