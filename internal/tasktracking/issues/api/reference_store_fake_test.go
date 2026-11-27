package api_test

import (
	"encoding/json"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/referencestore"
)

// fakeReferenceStoreTypes is a few rows in reference-store's shape: enough for
// the ticket source check and for GET /api/entity-types to pass through.
const fakeReferenceStoreTypes = `{"types":[
	{"type":"session","service":"llm-bridge-server","get":"/sessions/{id}","id_patterns":["br_\\d{16,19}"],"access":"owner"},
	{"type":"principal","service":"principal-store","search":"/principals?q=","get":"/principals/{id}","id_patterns":["principal_\\d{6,}"],"label_field":"display_name","access":"grant"},
	{"type":"email","service":"mailstack","access":"grant"}
]}`

// fakeReferenceStore is reference-store's list as kanban-store is given it.
func fakeReferenceStore(t *testing.T) referencestore.Types {
	t.Helper()
	var answer struct {
		Types []json.RawMessage `json:"types"`
	}
	if err := json.Unmarshal([]byte(fakeReferenceStoreTypes), &answer); err != nil {
		t.Fatal(err)
	}
	list, err := referencestore.NewList(answer.Types)
	if err != nil {
		t.Fatal(err)
	}
	return list
}
