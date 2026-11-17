package principalclient

import (
	"encoding/json"
	"testing"
)

// The name principal-store sends survives the strict decoder: the chat labels
// messages and session starters with it.
func TestPrincipalDecodingKeepsTheDisplayName(t *testing.T) {
	var principal Principal
	if err := json.Unmarshal([]byte(`{"id":"principal_000002","kind":"human","display_name":"Priya Raman","acts_as_caller":true,"restricted_to_its_grants":false}`), &principal); err != nil {
		t.Fatal(err)
	}
	if principal.DisplayName != "Priya Raman" {
		t.Fatalf("display name = %q, want Priya Raman", principal.DisplayName)
	}
}
