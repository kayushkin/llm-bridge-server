package grantstore

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const peerPrincipal = "principal_000020"

func directoryWithAPeer() *fakeDirectory {
	directory := newFakeDirectory()
	directory.principals[peerPrincipal] = &PrincipalSummary{ID: peerPrincipal, Kind: "peer", ActsInThisDeployment: true, ActsAsCaller: true}
	return directory
}

// TestAPeerHoldsGrants: principal-store says a peer acts in this deployment,
// and this store reads that answer rather than a list of kinds.
func TestAPeerHoldsGrants(t *testing.T) {
	s := newTestStore(t)
	grant, created, err := s.Create(context.Background(), directoryWithAPeer(), &fakeResourceChecker{},
		GrantRequest{PrincipalID: peerPrincipal, Relation: RelationCanDispatchOn, ResourceType: ResourceTypeInstance, ResourceID: "inst-cc-local"})
	if err != nil || !created || grant.PrincipalID != peerPrincipal {
		t.Fatalf("grant to a peer: %+v %v %v", grant, created, err)
	}
}

// TestAllResourcesIsAcceptedWhereItIsEnforcedAndNoOwnerIsAsked covers "*":
// it names no row, so no owner is asked; and only the relations
// llm-bridge-server enforces accept it.
func TestAllResourcesIsAcceptedWhereItIsEnforcedAndNoOwnerIsAsked(t *testing.T) {
	s := newTestStore(t)
	checker := &fakeResourceChecker{}
	accepted := []GrantRequest{
		{Relation: RelationCanDispatchOn, ResourceType: ResourceTypeInstance},
		{Relation: RelationCanDispatchOn, ResourceType: ResourceTypeMachine},
		{Relation: RelationCanRunAs, ResourceType: ResourceTypeAgent},
		{Relation: RelationCanUse, ResourceType: ResourceTypeTool},
		{Relation: RelationCanRunOperation, ResourceType: ResourceTypeOperationType},
		{Relation: RelationCanCallThrough, ResourceType: ResourceTypePeerConnection},
	}
	for _, request := range accepted {
		request.PrincipalID = "principal_000001"
		request.ResourceID = AllResources
		grant, created, err := s.Create(context.Background(), newFakeDirectory(), checker, request)
		if err != nil || !created || grant.ResourceID != AllResources {
			t.Fatalf("%s on every %s: %+v %v", request.Relation, request.ResourceType, grant, err)
		}
	}
	if checker.callCount() != 0 {
		t.Fatalf("\"*\" names no row, so no owner may be asked; asked %v", checker.calls)
	}
	refused := []GrantRequest{
		{Relation: RelationCanView, ResourceType: ResourceTypeBoard},
		{Relation: RelationCanAdminister, ResourceType: ResourceTypeBoard},
		{Relation: RelationWorksWith, ResourceType: ResourceTypeInstance},
		{Relation: RelationCanStartSessionsAs, ResourceType: ResourceTypePrincipal},
	}
	for _, request := range refused {
		request.PrincipalID = "principal_000001"
		request.ResourceID = AllResources
		_, _, err := s.Create(context.Background(), newFakeDirectory(), checker, request)
		if !errors.Is(err, ErrInvalidGrant) || !strings.Contains(err.Error(), "does not accept") {
			t.Fatalf("%s on every %s should be a 400 naming why, got %v", request.Relation, request.ResourceType, err)
		}
	}
}

// TestAPeerActsAsACaller: a request may act as a peer when principal-store
// says it acts as a caller.
func TestAPeerActsAsACaller(t *testing.T) {
	srv := newEnforcingTestServerWith(t, directoryWithAPeer())
	status, body := doWithHeaders(t, srv, asGrantStorePrincipal(peerPrincipal), "GET", "/principals/"+peerPrincipal+"/effective", nil)
	expectStatus(t, 200, status, body, "a peer reads its own effective grants")
}
