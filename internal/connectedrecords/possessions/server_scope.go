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

// requestScope is r's scope, and r with the caller every store asked on the
// way acts as.
func requestScope(r *http.Request) (ownership.Scope, *http.Request, *ownership.RequestError) {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		readScope, refusal := ownership.ReadScope(r)
		if refusal != nil {
			return ownership.Scope{}, nil, refusal
		}
		return readScope, r, nil
	}
	owner, refusal := ownership.OwnerActedFor(r)
	if refusal != nil {
		return ownership.Scope{}, nil, refusal
	}
	return ownership.OwnedBy(owner), r.WithContext(ownership.ContextWithCaller(r.Context(), ownership.Caller{PrincipalID: owner})), nil
}

// handlerOn answers a function that registers a route on mux whose handler
// build makes from narrow's answer for each request's scope: a server whose
// stores reach that scope's records only.
func handlerOn[Narrowed any](mux *http.ServeMux, narrow func(ctx context.Context, scope ownership.Scope) (Narrowed, error)) func(pattern string, build func(Narrowed) http.HandlerFunc) {
	return func(pattern string, build func(Narrowed) http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			scope, served, refusal := requestScope(r)
			if refusal != nil {
				writeJSON(w, refusal.Status, map[string]string{"error": refusal.Message})
				return
			}
			narrowed, err := narrow(served.Context(), scope)
			if err != nil {
				writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
				return
			}
			build(narrowed)(w, served)
		})
	}
}

func (server *Server) narrowedTo(ctx context.Context, scope ownership.Scope) (*Server, error) {
	store, err := server.Store.Narrowed(ctx, scope)
	if err != nil {
		return nil, err
	}
	narrowed := *server
	narrowed.Store = store
	return &narrowed, nil
}
