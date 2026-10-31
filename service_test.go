package principalstore

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestAServiceActsHoldsGrantsIsRestrictedToThemAndHoldsTokens pins what the
// other services read off a service's record. It is the only kind that may be
// issued a service principal token.
func TestAServiceActsHoldsGrantsIsRestrictedToThemAndHoldsTokens(t *testing.T) {
	s := newTestStore(t)
	service := mustCreate(t, s, KindService, "orchestratord", "")
	readBack, err := s.Get(service.ID, false)
	if err != nil {
		t.Fatalf("get service: %v", err)
	}
	if !readBack.ActsAsCaller || !readBack.ActsInThisDeployment || !readBack.RestrictedToItsGrants || !readBack.HoldsServicePrincipalTokens {
		t.Fatalf("a service must act, hold grants, be restricted to them and hold tokens: %+v", readBack)
	}
	if readBack.Groups == nil {
		t.Fatal("a service carries its groups, [] when none, as a human does")
	}
	role := mustCreate(t, s, KindGroup, "Session readers", "")
	if _, err := s.AddMember(role.ID, service.ID); err != nil {
		t.Fatalf("a service joins a group: %v", err)
	}
	for _, kind := range []string{KindHuman, KindPeer, KindGroup} {
		other := mustCreate(t, s, kind, "not a service", "")
		if other.HoldsServicePrincipalTokens {
			t.Errorf("a %s must not hold service principal tokens: %+v", kind, other)
		}
	}
}

func TestAServiceHasNoAvailability(t *testing.T) {
	s := newTestStore(t)
	service := mustCreate(t, s, KindService, "orchestratord", "")
	week := json.RawMessage(`{"tzid":"America/Los_Angeles","days":["MO"],"start":"09:00","end":"17:00"}`)
	_, err := s.Patch(service.ID, Patch{Availability: week})
	if !errors.Is(err, ErrInvalidAvailability) || !strings.Contains(err.Error(), "is a service") {
		t.Fatalf("a service is refused a working week for being a service, got %v", err)
	}
}

func TestKindDefinitionsServeTheServiceKind(t *testing.T) {
	srv, _ := newTestServer(t)
	status, body := do(t, srv, "GET", "/kind-definitions", nil)
	if status != http.StatusOK {
		t.Fatalf("GET /kind-definitions = %d: %s", status, body)
	}
	var definitions []KindDefinition
	if err := json.Unmarshal(body, &definitions); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	for _, definition := range definitions {
		if definition.Name == KindService {
			if !definition.HoldsServicePrincipalTokens || !definition.RestrictedToItsGrants {
				t.Fatalf("the service kind: %+v", definition)
			}
			return
		}
	}
	t.Fatalf("no service kind in %s", body)
}
