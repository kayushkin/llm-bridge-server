package mail

import (
	"errors"
	"net/http"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/mail/accounts"
	"github.com/kayushkin/llm-bridge-server/internal/mail/localmail"
	"github.com/kayushkin/llm-bridge-server/internal/mail/mailmodule/mailmoduletest"
)

// In these tests a request names its caller the way llm-bridge-server does in
// production, in the request context; testCallerHandler puts it there from
// two test-only headers, so each test reads as the request it sends.
const (
	// testPrincipalHeader names the principal a request is from. Absent, the
	// request is the internal service's.
	testPrincipalHeader = "X-Test-Principal-Id"
	// testNoCallerHeader set to anything sends the request with no caller at
	// all, which the mounting server never does.
	testNoCallerHeader = "X-Test-No-Caller"
)

// testCallerHandler is s's handler as the mounting server serves it.
func testCallerHandler(s *Server) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(testNoCallerHeader) != "" {
			s.Handler().ServeHTTP(w, r)
			return
		}
		caller := Caller{InternalService: true}
		if principalID := r.Header.Get(testPrincipalHeader); principalID != "" {
			caller = Caller{PrincipalID: principalID}
		}
		s.Handler().ServeHTTP(w, r.WithContext(ContextWithCaller(r.Context(), caller)))
	})
}

// errTestsReachNoProvider is every token the test token source is asked for:
// no test may reach Gmail or Outlook.
var errTestsReachNoProvider = errors.New("tests reach no mail provider")

// refusingTokenSource answers every token request with an error and counts
// them.
type refusingTokenSource struct{ asked int }

func (source *refusingTokenSource) Token(provider, account string) (string, error) {
	source.asked++
	return "", errTestsReachNoProvider
}

func (source *refusingTokenSource) Invalidate(provider, account string) {}

// newTestStores is both halves of a mail module of the test's own.
func newTestStores(t *testing.T) (*accounts.Store, *localmail.Store) {
	t.Helper()
	db := mailmoduletest.NewDB(t)
	return accounts.New(db), localmail.New(db)
}

// newTestServer builds a server on store and a local mailbox in the same
// module. Provider connections are on, with a token source that refuses, so a
// test of a Microsoft account reaches the Graph backend's own refusals with
// no network and no credential.
func newTestServer(t *testing.T, store *accounts.Store, local *localmail.Store, parts ServerParts) *Server {
	t.Helper()
	parts.Accounts, parts.LocalMail = store, local
	if parts.Tokens == nil {
		parts.Tokens = &refusingTokenSource{}
		parts.ProviderConnectionsEnabled = true
	}
	s := NewServer(parts)
	t.Cleanup(func() { s.Shutdown(nil) })
	return s
}
