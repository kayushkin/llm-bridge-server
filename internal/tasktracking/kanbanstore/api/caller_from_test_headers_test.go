package api_test

import (
	"crypto/subtle"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/kanbanstore/api"
)

// The store reads its caller from the request context, which the server it is
// mounted in sets from the credential it verified. These tests were written
// against the standalone store, which read the caller from two headers, so
// they keep sending them, and callerFromTestHeaders plays the mounting server:
// it turns the headers into the caller and removes them. It is test tooling
// only; in the server the headers name nobody.
const (
	testPrincipalIDHeader  = "X-Principal-Id"
	testServiceTokenHeader = "X-Kanban-Store-Service-Token"
	testServiceToken       = "service-token-that-is-at-least-32-characters"
)

func callerFromTestHeaders(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := r.Header.Get(testServiceTokenHeader)
		principalID := r.Header.Get(testPrincipalIDHeader)
		r = r.Clone(r.Context())
		r.Header.Del(testServiceTokenHeader)
		r.Header.Del(testPrincipalIDHeader)
		switch {
		case token != "":
			if subtle.ConstantTimeCompare([]byte(token), []byte(testServiceToken)) != 1 {
				http.Error(w, `{"error":"the stand-in server refuses a wrong service token"}`, http.StatusUnauthorized)
				return
			}
			r = r.WithContext(api.ContextWithCaller(r.Context(), api.Caller{InternalService: true}))
		case principalID != "":
			r = r.WithContext(api.ContextWithCaller(r.Context(), api.Caller{PrincipalID: principalID}))
		}
		handler.ServeHTTP(w, r)
	})
}
