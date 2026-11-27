package possessions

import (
	"context"
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// Every request reaches the records of one scope, chosen from the caller the
// server put in the request context (ownership.CallerOfRequest), never from
// anything the client sent:
//
//   - A read (GET or HEAD) reaches the caller's own records, or, for an
//     administrator and this server's own service, every owner's or the one
//     owner_principal_id names (ownership.ReadScope).
//   - Anything else acts for one owner (ownership.OwnerActedFor): the
//     caller, or the owner an administrator names. It reads and writes only
//     that owner's records, and every store it asks on the way — photo-store,
//     location-store, mailstack — is asked as that owner, so a pass never
//     mixes two owners' records and a record outside the scope answers 404 as
//     a missing one.

// narrowedFor is the server answering r's scope, and r with the caller every
// store asked on the way acts as.
func (server *Server) narrowedFor(r *http.Request) (*Server, *http.Request, *ownership.RequestError) {
	var scope ownership.Scope
	served := r
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		readScope, refusal := ownership.ReadScope(r)
		if refusal != nil {
			return nil, nil, refusal
		}
		scope = readScope
	} else {
		owner, refusal := ownership.OwnerActedFor(r)
		if refusal != nil {
			return nil, nil, refusal
		}
		scope = ownership.OwnedBy(owner)
		served = r.WithContext(ownership.ContextWithCaller(r.Context(), ownership.Caller{PrincipalID: owner}))
	}
	narrowed, err := server.narrowedTo(served.Context(), scope)
	if err != nil {
		return nil, nil, &ownership.RequestError{Status: http.StatusServiceUnavailable, Message: err.Error()}
	}
	return narrowed, served, nil
}

func (server *Server) narrowedTo(ctx context.Context, scope ownership.Scope) (*Server, error) {
	store, err := server.Store.Narrowed(ctx, scope)
	if err != nil {
		return nil, err
	}
	narrowed := *server
	narrowed.Store = store
	narrowed.Reviewer = server.Reviewer.Narrowed(store)
	return &narrowed, nil
}

// handlerOn answers a function that registers a route on mux whose handler
// build makes from the server narrowed to each request's scope.
func (server *Server) handlerOn(mux *http.ServeMux) func(pattern string, build func(*Server) http.HandlerFunc) {
	return func(pattern string, build func(*Server) http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			narrowed, served, refusal := server.narrowedFor(r)
			if refusal != nil {
				writeJSON(w, refusal.Status, map[string]string{"error": refusal.Message})
				return
			}
			build(narrowed)(w, served)
		})
	}
}
