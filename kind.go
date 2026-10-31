package principalstore

import (
	"fmt"
	"strings"
)

// The kind vocabulary is served (GET /kinds, GET /kind-definitions) rather
// than inferred from the rows, so no caller ever builds a filter out of
// whatever values happen to exist. Same move as job-store's role families and
// prediction-store's categories.
//
// What a kind may do is served too, both at GET /kind-definitions and on every
// principal record, so grant-store, kanban-store and llm-bridge-server read
// "may this principal make a request?" from here instead of each comparing the
// kind to the word "human".

const (
	KindHuman = "human"
	KindGroup = "group"
	// KindContact is someone outside this deployment who a ticket is *from*:
	// the requester who sent the mail. A contact never logs in, never holds a
	// grant, never joins a group and never administers anything — it exists so
	// a requester has an id, and twenty tickets from one person join on one
	// row instead of on a string that changes when they mail from their phone.
	KindContact = "contact"
	// KindPeer is another llm-bridge-server that this deployment has linked to
	// and given access to. It acts here through a peer token that this
	// deployment's llm-bridge-server issued it, never through a login. It may
	// hold grants, join groups (a group is how a peer gets a role) and be an
	// administrator. Unlike a human, a peer holding no grant of a relation is
	// refused by that relation: a link that has just been made gives the other
	// server nothing until someone grants it something.
	KindPeer = "peer"
	// KindService is a program on this host that calls other services as
	// itself, such as a daemon that reads and starts sessions. It never logs
	// in: it presents a service principal token that llm-bridge-server issued
	// for it, which an administrator can revoke. Like a peer, it has only
	// what it is granted, and holding no grant of a relation refuses it.
	KindService = "service"
)

// KindDefinition is one kind and what a principal of it may do. The booleans
// are the questions other services ask; they read the answers and never
// restate them.
type KindDefinition struct {
	Name string `json:"name"`
	// ActsInThisDeployment says a principal of this kind may hold grants. A
	// contact may not: it is the outside party on a ticket.
	ActsInThisDeployment bool `json:"acts_in_this_deployment"`
	// ActsAsCaller says a request may act as a principal of this kind, so it
	// may be a group's member and may be an administrator. A group may not: it
	// is a set of principals, not someone who acts.
	ActsAsCaller bool `json:"acts_as_caller"`
	// RestrictedToItsGrants says that a principal of this kind holding no
	// grant of a relation is refused by that relation. Kinds without it keep
	// the lenient rule the operator chose on 2026-09-11: holding no grant of a
	// relation means that relation does not restrict them.
	RestrictedToItsGrants bool `json:"restricted_to_its_grants"`
	// HoldsServicePrincipalTokens says a principal of this kind may be issued
	// a token of its own, which a service accepts in place of a login. Only a
	// service may: a person signs in, and a peer presents a peer token.
	HoldsServicePrincipalTokens bool   `json:"holds_service_principal_tokens"`
	Description                 string `json:"description"`
}

// KindDefinitions is every kind a principal can be, in the order GET /kinds
// serves them. An agent is not modelled yet — add it here when it is, and the
// 400 below starts naming it.
var KindDefinitions = []KindDefinition{
	{
		Name: KindHuman, ActsInThisDeployment: true, ActsAsCaller: true,
		Description: "a person of this deployment: signs in, is assigned work, holds grants",
	},
	{
		Name: KindGroup, ActsInThisDeployment: true,
		Description: "a named set of humans and peers; its grants are every member's, which is how a role is made",
	},
	{
		Name:        KindContact,
		Description: "an outside person work arrives from, such as a ticket's requester; holds nothing",
	},
	{
		Name: KindPeer, ActsInThisDeployment: true, ActsAsCaller: true, RestrictedToItsGrants: true,
		Description: "another llm-bridge-server linked to this one; acts through a peer token, and has only what it is granted",
	},
	{
		Name: KindService, ActsInThisDeployment: true, ActsAsCaller: true, RestrictedToItsGrants: true, HoldsServicePrincipalTokens: true,
		Description: "a program on this host that calls other services as itself; acts through a service principal token, never a login, and has only what it is granted",
	},
}

// Kinds is the names alone, as GET /kinds serves them.
var Kinds = func() []string {
	names := make([]string, 0, len(KindDefinitions))
	for _, definition := range KindDefinitions {
		names = append(names, definition.Name)
	}
	return names
}()

// lookupKindDefinition returns the definition of a canonical kind.
func lookupKindDefinition(kind string) (KindDefinition, bool) {
	for _, definition := range KindDefinitions {
		if definition.Name == kind {
			return definition, true
		}
	}
	return KindDefinition{}, false
}

// NormalizeKind resolves a caller's word to a canonical kind.
func NormalizeKind(raw string) (string, bool) {
	key := strings.ToLower(strings.TrimSpace(raw))
	if _, ok := lookupKindDefinition(key); ok {
		return key, true
	}
	return "", false
}

// ErrUnknownKind names the whole vocabulary, because a caller who guessed
// wrong cannot guess right from a bare rejection.
func ErrUnknownKind(raw string) error {
	return fmt.Errorf("%w: unknown kind %q: use one of %s",
		ErrInvalidPrincipal, raw, strings.Join(Kinds, ", "))
}

// ActsInThisDeployment says whether a principal of this kind can be given
// access to anything. grant-store reads the same answer from the principal
// record before writing a grant, and the login path before issuing a session.
func ActsInThisDeployment(kind string) bool {
	definition, _ := lookupKindDefinition(kind)
	return definition.ActsInThisDeployment
}

// ActsAsCaller says whether a request may act as a principal of this kind.
func ActsAsCaller(kind string) bool {
	definition, _ := lookupKindDefinition(kind)
	return definition.ActsAsCaller
}

// fillKindAnswers copies what the principal's kind may do onto the record, so
// a reader of one principal needs no second call.
func (p *Principal) fillKindAnswers() {
	definition, _ := lookupKindDefinition(p.Kind)
	p.ActsInThisDeployment = definition.ActsInThisDeployment
	p.ActsAsCaller = definition.ActsAsCaller
	p.RestrictedToItsGrants = definition.RestrictedToItsGrants
	p.HoldsServicePrincipalTokens = definition.HoldsServicePrincipalTokens
}
