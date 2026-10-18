package mailstack

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// GET /api/messages/{id}/attachments/{attachment_id} goes through the same
// account lookup and bearer check as every other message route. The Microsoft
// account in testServer answers errNotImplemented for attachments, which makes
// the backend-refused path reachable with no network.
func TestTheAttachmentRouteChecksTheAccountAndTheTokenAndMapsARefusal(t *testing.T) {
	cases := []struct {
		name, token, header, target string
		want                        int
	}{
		{"no account", "", "", "/api/messages/m1/attachments/1", http.StatusBadRequest},
		{"unknown account", "", "", "/api/messages/m1/attachments/1?account=nope", http.StatusBadRequest},
		{"backend refuses", "", "", "/api/messages/m1/attachments/1?account=acct-ms", http.StatusBadGateway},
		{"no token", "secret", "", "/api/messages/m1/attachments/1?account=acct-ms", http.StatusUnauthorized},
		{"right token", "secret", "Bearer secret", "/api/messages/m1/attachments/1?account=acct-ms", http.StatusBadGateway},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := testServer(t, c.token)
			r := httptest.NewRequest(http.MethodGet, c.target, nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			w := httptest.NewRecorder()
			s.mux.ServeHTTP(w, r)
			if w.Code != c.want {
				t.Errorf("want %d, got %d (%s)", c.want, w.Code, w.Body.String())
			}
		})
	}
}
