package api_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kayushkin/kanban-store/internal/referencestore"
)

// fakeReferenceStoreTypes is a few rows in reference-store's shape: enough for
// the ticket source check and for GET /api/entity-types to pass through.
const fakeReferenceStoreTypes = `{"types":[
	{"type":"session","service":"llm-bridge-server","get":"/sessions/{id}","id_patterns":["br_\\d{16,19}"],"access":"owner"},
	{"type":"principal","service":"principal-store","search":"/principals?q=","get":"/principals/{id}","id_patterns":["principal_\\d{6,}"],"label_field":"display_name","access":"grant"},
	{"type":"email","service":"mailstack","access":"grant"}
]}`

// fakeReferenceStore answers GET /types, and counts the asks.
func fakeReferenceStore(t *testing.T) *referencestore.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/types" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(fakeReferenceStoreTypes))
	}))
	t.Cleanup(server.Close)
	return referencestore.New(server.URL)
}
