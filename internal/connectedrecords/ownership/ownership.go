// Package ownership is who owns a connected record, and whose records a
// request may reach.
//
// The connected_records stores (people, calendar, journal and the rest) keep
// one person's private life each, and are meant for several people on one
// server. So every record carries the principal that owns it: an
// owner_principal_id, principal-store's id. That owner is the person who keeps
// the record, which is a different thing from any principal the record names:
// a person record's principal_id is the login of the person it describes, and
// says nothing about whose address book it is in.
//
// A request reaches a store with a Caller in its context, set by the server
// from the credential it verified, never from anything the client sent. A
// caller who does not see every owner reaches only the records it owns; one
// that does (an administrator, or this server's own service acting as
// itself) reaches every owner's, or one owner's when the request names
// owner_principal_id. A record outside the caller's reach answers as if it
// did not exist.
package ownership

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// OwnerParameter is the query parameter that narrows a read to one owner's
// records.
const OwnerParameter = "owner_principal_id"

// principalIDPattern is principal-store's id shape.
var principalIDPattern = regexp.MustCompile(`^principal_\d{6,}$`)

// CheckPrincipalID refuses a value that is not principal-store's id shape.
func CheckPrincipalID(field, id string) error {
	if !principalIDPattern.MatchString(id) {
		return fmt.Errorf("%s %q is not a principal-store id (principal_000001)", field, id)
	}
	return nil
}

// Caller is who a request acts as.
type Caller struct {
	// PrincipalID is the principal the request acts as. It is empty only for
	// this server's own service acting as itself, which names nobody.
	PrincipalID string
	// SeesEveryOwner is set for an administrator and for this server's own
	// service: their reads are not narrowed to their own records.
	SeesEveryOwner bool
}

type callerContextKey struct{}

// ContextWithCaller is ctx carrying caller.
func ContextWithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, caller)
}

// CallerFromContext is the caller ctx carries, and false when it carries
// none.
func CallerFromContext(ctx context.Context) (Caller, bool) {
	caller, ok := ctx.Value(callerContextKey{}).(Caller)
	return caller, ok
}

// Scope is whose records a call may reach: one owner's, or every owner's. The
// zero value reaches nothing and is refused by Check, so a scope that was
// never set cannot silently mean "everyone".
type Scope struct {
	ownerPrincipalID string
	everyOwner       bool
}

// EveryOwner reaches every owner's records.
func EveryOwner() Scope { return Scope{everyOwner: true} }

// OwnedBy reaches the records ownerPrincipalID owns.
func OwnedBy(ownerPrincipalID string) Scope { return Scope{ownerPrincipalID: ownerPrincipalID} }

// Check refuses the zero scope and an owner that is not a principal id.
func (scope Scope) Check() error {
	if scope.everyOwner {
		return nil
	}
	if scope.ownerPrincipalID == "" {
		return fmt.Errorf("ownership: a scope with no owner reaches nothing; use EveryOwner or OwnedBy")
	}
	return CheckPrincipalID("owner", scope.ownerPrincipalID)
}

// IsEveryOwner reports whether the scope reaches every owner's records.
func (scope Scope) IsEveryOwner() bool { return scope.everyOwner }

// OwnerPrincipalID is the one owner the scope reaches, and "" when it reaches
// every owner.
func (scope Scope) OwnerPrincipalID() string { return scope.ownerPrincipalID }

// Includes reports whether a record owned by ownerPrincipalID is in reach.
func (scope Scope) Includes(ownerPrincipalID string) bool {
	return scope.everyOwner || (scope.ownerPrincipalID != "" && scope.ownerPrincipalID == ownerPrincipalID)
}

// Condition is a SQL condition on column that holds the scope, with its
// argument numbered placeholder ($placeholder): "TRUE" and no argument for
// every owner. Callers append the arguments to their own.
func (scope Scope) Condition(column string, placeholder int) (string, []any) {
	if scope.everyOwner {
		return "TRUE", nil
	}
	return column + " = $" + strconv.Itoa(placeholder), []any{scope.ownerPrincipalID}
}

// String names the scope in an error message.
func (scope Scope) String() string {
	if scope.everyOwner {
		return "every owner"
	}
	return "owner " + scope.ownerPrincipalID
}

// RequestError is a request this package refuses, with the status to answer.
type RequestError struct {
	Status  int
	Message string
}

func (e *RequestError) Error() string { return e.Message }

// CallerOfRequest is the caller the server put on r. A request with none
// never comes from the server's own routes, so it is refused, not guessed.
func CallerOfRequest(r *http.Request) (Caller, *RequestError) {
	caller, ok := CallerFromContext(r.Context())
	if !ok {
		return Caller{}, &RequestError{http.StatusUnauthorized, "this store answers only a caller the server verified, and the request carries none"}
	}
	return caller, nil
}

// ReadScope is the scope of a request: the caller's own records, or, for a
// caller that sees every owner, every owner's, or the one owner the request
// names in owner_principal_id. A caller that does not see every owner may name
// only itself there.
func ReadScope(r *http.Request) (Scope, *RequestError) {
	caller, refusal := CallerOfRequest(r)
	if refusal != nil {
		return Scope{}, refusal
	}
	named, present := r.URL.Query()[OwnerParameter]
	owner := ""
	if present {
		if len(named) != 1 {
			return Scope{}, &RequestError{http.StatusBadRequest, OwnerParameter + " is given more than once"}
		}
		owner = strings.TrimSpace(named[0])
		if err := CheckPrincipalID(OwnerParameter, owner); err != nil {
			return Scope{}, &RequestError{http.StatusBadRequest, err.Error()}
		}
	}
	if caller.SeesEveryOwner {
		if owner != "" {
			return OwnedBy(owner), nil
		}
		return EveryOwner(), nil
	}
	if caller.PrincipalID == "" {
		return Scope{}, &RequestError{http.StatusUnauthorized, "this store answers a principal, and the request names none"}
	}
	if owner != "" && owner != caller.PrincipalID {
		return Scope{}, &RequestError{http.StatusForbidden, fmt.Sprintf("%s names %s, and a caller who is not an administrator reaches only its own records", OwnerParameter, owner)}
	}
	return OwnedBy(caller.PrincipalID), nil
}

// OwnerOfNewRecord is the owner a record the request creates gets: the
// caller's principal. This server's own service acting as itself names
// nobody, so it cannot create a record without naming the person it acts for.
func OwnerOfNewRecord(r *http.Request) (string, *RequestError) {
	caller, refusal := CallerOfRequest(r)
	if refusal != nil {
		return "", refusal
	}
	if caller.PrincipalID == "" {
		return "", &RequestError{http.StatusForbidden, "a new record is owned by the principal that makes it, and this request acts as no principal; send it as the person it is for"}
	}
	if err := CheckPrincipalID("the caller", caller.PrincipalID); err != nil {
		return "", &RequestError{http.StatusBadRequest, err.Error()}
	}
	return caller.PrincipalID, nil
}

// OwnerActedFor is the one owner a request that works through a whole
// owner's records acts for: a pass that groups, matches, segments or fills in
// what is already there. A caller that sees every owner acts for the owner the
// request names in owner_principal_id, or for itself when it names none; any
// other caller acts for itself and may name only itself. Like OwnerOfNewRecord,
// it refuses a request that acts as no principal and names no owner, because
// such a pass must never run over every owner's records at once.
func OwnerActedFor(r *http.Request) (string, *RequestError) {
	scope, refusal := ReadScope(r)
	if refusal != nil {
		return "", refusal
	}
	if !scope.IsEveryOwner() {
		return scope.OwnerPrincipalID(), nil
	}
	return OwnerOfNewRecord(r)
}
