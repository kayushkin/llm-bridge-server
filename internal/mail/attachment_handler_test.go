package mail

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /api/messages/{id}/attachments/{attachment_id} goes through the same
// account lookup and caller check as every other message route. The Microsoft
// account in testServer answers errNotImplemented for attachments, which makes
// the backend-refused path reachable with no network.
func TestTheAttachmentRouteChecksTheAccountAndTheCallerAndMapsARefusal(t *testing.T) {
	cases := []struct {
		name, header, target string
		want                 int
	}{
		{"no account", "", "/api/messages/m1/attachments/1", http.StatusBadRequest},
		{"unknown account", "", "/api/messages/m1/attachments/1?account=nope", http.StatusBadRequest},
		{"backend refuses", "", "/api/messages/m1/attachments/1?account=acct-ms", http.StatusBadGateway},
		{"no caller", testNoCallerHeader, "/api/messages/m1/attachments/1?account=acct-ms", http.StatusUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := testServer(t)
			r := httptest.NewRequest(http.MethodGet, c.target, nil)
			if c.header != "" {
				r.Header.Set(c.header, "1")
			}
			w := httptest.NewRecorder()
			testCallerHandler(s).ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("want %d, got %d (%s)", c.want, w.Code, w.Body.String())
			}
		})
	}
}
