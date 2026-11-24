package ownership

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func requestAs(caller *Caller, query string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/records"+query, nil)
	if caller != nil {
		r = r.WithContext(ContextWithCaller(r.Context(), *caller))
	}
	return r
}

func TestReadScopeNarrowsAPrincipalToItsOwnRecords(t *testing.T) {
	scope, refusal := ReadScope(requestAs(&Caller{PrincipalID: "principal_000002"}, ""))
	if refusal != nil || scope.IsEveryOwner() || scope.OwnerPrincipalID() != "principal_000002" {
		t.Fatalf("scope %v, refusal %v", scope, refusal)
	}
	if _, refusal := ReadScope(requestAs(&Caller{PrincipalID: "principal_000002"}, "?owner_principal_id=principal_000003")); refusal == nil || refusal.Status != http.StatusForbidden {
		t.Fatalf("naming another owner: %v", refusal)
	}
	if scope, refusal := ReadScope(requestAs(&Caller{PrincipalID: "principal_000002"}, "?owner_principal_id=principal_000002")); refusal != nil || scope.OwnerPrincipalID() != "principal_000002" {
		t.Fatalf("naming itself: %v %v", scope, refusal)
	}
}

func TestReadScopeOfACallerThatSeesEveryOwner(t *testing.T) {
	administrator := &Caller{PrincipalID: "principal_000001", SeesEveryOwner: true}
	if scope, refusal := ReadScope(requestAs(administrator, "")); refusal != nil || !scope.IsEveryOwner() {
		t.Fatalf("scope %v, refusal %v", scope, refusal)
	}
	if scope, refusal := ReadScope(requestAs(administrator, "?owner_principal_id=principal_000004")); refusal != nil || scope.OwnerPrincipalID() != "principal_000004" {
		t.Fatalf("scope %v, refusal %v", scope, refusal)
	}
	if _, refusal := ReadScope(requestAs(administrator, "?owner_principal_id=alice")); refusal == nil || refusal.Status != http.StatusBadRequest {
		t.Fatalf("malformed owner: %v", refusal)
	}
}

func TestARequestWithNoCallerIsRefused(t *testing.T) {
	if _, refusal := ReadScope(requestAs(nil, "")); refusal == nil || refusal.Status != http.StatusUnauthorized {
		t.Fatalf("refusal %v", refusal)
	}
	if _, refusal := OwnerOfNewRecord(requestAs(&Caller{SeesEveryOwner: true}, "")); refusal == nil || refusal.Status != http.StatusForbidden {
		t.Fatalf("the internal service making a record: %v", refusal)
	}
}

func TestTheZeroScopeReachesNothing(t *testing.T) {
	var scope Scope
	if scope.Check() == nil || scope.Includes("principal_000001") || scope.Includes("") {
		t.Fatal("the zero scope must reach nothing")
	}
	if condition, arguments := OwnedBy("principal_000001").Condition("owner_principal_id", 3); condition != "owner_principal_id = $3" || len(arguments) != 1 {
		t.Fatalf("%s %v", condition, arguments)
	}
	if condition, arguments := EveryOwner().Condition("owner_principal_id", 1); condition != "TRUE" || arguments != nil {
		t.Fatalf("%s %v", condition, arguments)
	}
}
