package principalstore

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestAPeerActsHoldsGrantsAndIsRestrictedToThem pins what the other services
// read off a peer's record: it acts as a caller, holds grants, and is refused
// by a relation it holds no grant of — while a human keeps the lenient rule.
func TestAPeerActsHoldsGrantsAndIsRestrictedToThem(t *testing.T) {
	s := newTestStore(t)
	peer := mustCreate(t, s, KindPeer, "bridge on laptop", "")
	readBack, err := s.Get(peer.ID, false)
	if err != nil {
		t.Fatalf("get peer: %v", err)
	}
	if !readBack.ActsAsCaller || !readBack.ActsInThisDeployment || !readBack.RestrictedToItsGrants {
		t.Fatalf("a peer must act, hold grants and be restricted to them: %+v", readBack)
	}
	if readBack.Groups == nil {
		t.Fatal("a peer carries its groups, [] when none, as a human does")
	}
	human := mustCreate(t, s, KindHuman, "Slava Kayushkin", "")
	if human.RestrictedToItsGrants || !human.ActsAsCaller {
		t.Fatalf("a human acts and keeps the lenient rule: %+v", human)
	}
	group := mustCreate(t, s, KindGroup, "Operators", "")
	if group.ActsAsCaller || !group.ActsInThisDeployment {
		t.Fatalf("a group holds grants and does not act: %+v", group)
	}
	contact := mustCreate(t, s, KindContact, "Customer", "c@example.com")
	if contact.ActsAsCaller || contact.ActsInThisDeployment {
		t.Fatalf("a contact neither acts nor holds grants: %+v", contact)
	}
}

// TestAPeerCanBeAnAdministratorAndAGroupMember covers the two ways a peer is
// given access beyond single grants: administrator for everything, a group
// for a role.
func TestAPeerCanBeAnAdministratorAndAGroupMember(t *testing.T) {
	s := newTestStore(t)
	peer := mustCreate(t, s, KindPeer, "bridge on laptop", "")
	yes := true
	promoted, err := s.Patch(peer.ID, Patch{IsAdministrator: &yes})
	if err != nil || !promoted.IsAdministrator {
		t.Fatalf("promote a peer: %+v %v", promoted, err)
	}
	role := mustCreate(t, s, KindGroup, "Dispatch only", "")
	if _, err := s.AddMember(role.ID, peer.ID); err != nil {
		t.Fatalf("a peer joins a group: %v", err)
	}
	withGroups, err := s.Get(peer.ID, false)
	if err != nil || len(withGroups.Groups) != 1 || withGroups.Groups[0].ID != role.ID {
		t.Fatalf("the peer's groups: %+v %v", withGroups, err)
	}
	other := mustCreate(t, s, KindGroup, "Nested", "")
	if _, err := s.AddMember(role.ID, other.ID); !errors.Is(err, ErrInvalidMembership) {
		t.Fatalf("a group in the member slot is still refused, got %v", err)
	}
	contact := mustCreate(t, s, KindContact, "Customer", "c@example.com")
	if _, err := s.Patch(contact.ID, Patch{IsAdministrator: &yes}); !errors.Is(err, ErrInvalidPrincipal) {
		t.Fatalf("a contact is refused administrator, got %v", err)
	}
}

func TestAPeerHasNoAvailability(t *testing.T) {
	s := newTestStore(t)
	peer := mustCreate(t, s, KindPeer, "bridge on laptop", "")
	week := json.RawMessage(`{"tzid":"America/Los_Angeles","days":["MO"],"start":"09:00","end":"17:00"}`)
	if _, err := s.Patch(peer.ID, Patch{Availability: week}); !errors.Is(err, ErrInvalidAvailability) {
		t.Fatalf("a peer is refused a working week, got %v", err)
	} else if _, err := s.Patch(peer.ID, Patch{Availability: week}); !strings.Contains(err.Error(), "is a peer") {
		t.Fatalf("the refusal should be for being a peer, not for the week's shape: %v", err)
	}
}

func TestKindDefinitionsRoute(t *testing.T) {
	srv, _ := newTestServer(t)
	status, body := do(t, srv, "GET", "/kind-definitions", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /kind-definitions = %d: %s", status, body)
	}
	var definitions []KindDefinition
	if err := json.Unmarshal(body, &definitions); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if len(definitions) != len(Kinds) {
		t.Fatalf("one definition per kind: %s", body)
	}
	for _, definition := range definitions {
		if definition.Name == KindPeer && !definition.RestrictedToItsGrants {
			t.Fatalf("the peer kind is restricted to its grants: %+v", definition)
		}
	}
}
