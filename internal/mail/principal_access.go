package mail

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// ─── Per-person access ──────────────────────────────────────────────────
//
// mailstack runs inside llm-bridge-server, which verifies every request's
// credential and says who the request is from in its context
// (ContextWithCaller). Nothing a client sends — no header, no body field —
// names the caller. There are two kinds:
//
//   - The internal service: the server itself, or a caller that presented its
//     service token. It is unrestricted, as a holder of the standalone
//     service's bearer token was.
//   - A principal, checked on every call, live, against the stores that own
//     the answer, so a revoked grant stops the next read:
//   - principal-store says whether the principal is an active human, and
//     whether it is an administrator, which passes every check;
//   - grant-store holds can_read_mail and can_import_mail on mail_account
//     resources, the account ids here; can_import_mail includes can_read_mail;
//   - kanban-store says which messages the person sees through a ticket: a
//     card they may view that links the message as an email entity whose ref
//     is the message's locator, "<account_id>:<id>".
//
// A request with neither is 401.
//
// A person may read a message with can_read_mail on its account or through a
// ticket. Everything wider than one message — a folder, a thread, a search —
// needs the account grant, because a ticket grants its own messages and not
// their neighbors. A message the person may not read is 404, as if it did not
// exist; an account route they hold no grant for is 403.
//
// The rule is strict: a person with no mail grant reads only what tickets
// show them. That differs from grant-store's lenient default on purpose —
// mail is the one store here where "not restricted" would mean "reads
// everyone's mail".

// Caller is who a request comes from, as the mounting server verified it.
type Caller struct {
	// InternalService is the server itself, or a caller holding its service
	// token: past every check.
	InternalService bool
	// PrincipalID is the principal a request acts as; empty for the internal
	// service.
	PrincipalID string
}

type callerContextKey struct{}

// ContextWithCaller returns ctx carrying caller, for routeFor to read.
func ContextWithCaller(ctx context.Context, caller Caller) context.Context {
	return context.WithValue(ctx, callerContextKey{}, caller)
}

// CallerIsInternalService reports whether ctx names the internal service as
// its caller.
func CallerIsInternalService(ctx context.Context) bool {
	caller, named := ctx.Value(callerContextKey{}).(Caller)
	return named && caller.InternalService
}

// callerOf is the request's caller; false when the mounting server named none.
func callerOf(r *http.Request) (Caller, bool) {
	caller, named := r.Context().Value(callerContextKey{}).(Caller)
	return caller, named
}

// PrincipalFacts is what principal-store says about one principal.
type PrincipalFacts struct {
	Kind            string
	Disabled        bool
	IsAdministrator bool
}

// MailGrant is one grant-store grant on a mail account.
type MailGrant struct {
	Relation  string
	AccountID string
}

// ErrPrincipalUnknown is a principal the directory has no record of.
var ErrPrincipalUnknown = errors.New("no such principal")

// PeopleDirectory is what a person's access is read from: principal-store,
// grant-store and kanban-store, in the server mailstack runs in. Every method
// answers ErrPrincipalUnknown for a principal that does not exist, and any
// other error when it could not answer; an error is never "holds nothing".
type PeopleDirectory interface {
	Principal(ctx context.Context, principalID string) (PrincipalFacts, error)
	// MailGrants is every live grant the principal holds on mail_account
	// resources, directly or through a group.
	MailGrants(ctx context.Context, principalID string) ([]MailGrant, error)
	// MessagesVisibleThroughTickets answers, for each locator in order,
	// whether the principal may view a card that links it as an email entity.
	MessagesVisibleThroughTickets(ctx context.Context, principalID string, locators []string) ([]bool, error)
}

// Relations mailstack enforces, as grant-store serves them.
const (
	relationCanReadMail   = "can_read_mail"
	relationCanImportMail = "can_import_mail"
	resourceTypeMail      = "mail_account"
	entityTypeEmail       = "email"
)

// Error codes of a refused person.
const (
	errorCodePrincipalRefused        = "principal_refused"
	errorCodeMailAccountNotGranted   = "mail_account_not_granted"
	errorCodeRouteNotOpenToPrincipal = "route_not_open_to_principal"
	errorCodePrincipalAccessDown     = "principal_access_unavailable"
	errorCodeNoCaller                = "no_caller"
)

var principalIDPattern = regexp.MustCompile(`^principal_[0-9]{6,}$`)

// accessRule says what a person needs to use a route. Every route declares
// one; routeFor refuses to register a route without it.
type accessRule int

const (
	// ruleServicesOnly: services and administrators. The default for
	// anything that is not about one account or one message.
	ruleServicesOnly accessRule = iota + 1
	// ruleReadAccount: can_read_mail on the account the route names.
	ruleReadAccount
	// ruleImportAccount: can_import_mail on the account the route names.
	ruleImportAccount
	// ruleReadMessage: can_read_mail on ?account=, or a ticket linking
	// "<account>:{id}".
	ruleReadMessage
	// ruleHandlerFilters: the handler applies the person's access itself,
	// because the answer is a list filtered to what they may see.
	ruleHandlerFilters
)

// accountOfRoute says where a route names its account.
type accountOfRoute int

const (
	accountInQuery accountOfRoute = iota + 1 // ?account=
	accountInPath                            // {id} under /api/accounts/
)

// principalAccess is what one person may do, read for one request.
type principalAccess struct {
	principalID   string
	administrator bool
	readAccounts  map[string]bool
	importAccount map[string]bool
}

func (a *principalAccess) mayReadAccount(accountID string) bool {
	return a.administrator || a.readAccounts[accountID] || a.importAccount[accountID]
}

func (a *principalAccess) mayImportAccount(accountID string) bool {
	return a.administrator || a.importAccount[accountID]
}

type principalAccessKey struct{}

// principalAccessOfRequest is the person a request is for, or nil for a
// service.
func principalAccessOfRequest(r *http.Request) *principalAccess {
	access, _ := r.Context().Value(principalAccessKey{}).(*principalAccess)
	return access
}

// accessRefusal is a check that said no, or could not be made.
type accessRefusal struct {
	status int
	code   string
	err    error
}

func (e *accessRefusal) Error() string { return e.err.Error() }

func refusal(status int, code string, format string, args ...any) *accessRefusal {
	return &accessRefusal{status: status, code: code, err: fmt.Errorf(format, args...)}
}

// accessOfPrincipal reads what principalID may do, live.
func (s *Server) accessOfPrincipal(ctx context.Context, principalID string) (*principalAccess, *accessRefusal) {
	if !principalIDPattern.MatchString(principalID) {
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "the caller %q is not a principal id", principalID)
	}
	people := s.people
	if people == nil {
		return nil, refusal(http.StatusServiceUnavailable, errorCodePrincipalAccessDown,
			"this mailstack was given no directory of people (principal-store, grant-store and kanban-store), so it serves only the internal service")
	}
	principal, err := people.Principal(ctx, principalID)
	switch {
	case errors.Is(err, ErrPrincipalUnknown):
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "principal-store has no principal %s", principalID)
	case err != nil:
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "principal-store: %v", err)
	case principal.Kind != "human":
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "principal %s is a %s, and only a person reads mail", principalID, principal.Kind)
	case principal.Disabled:
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "principal %s is disabled", principalID)
	}
	access := &principalAccess{principalID: principalID, administrator: principal.IsAdministrator,
		readAccounts: map[string]bool{}, importAccount: map[string]bool{}}
	if access.administrator {
		return access, nil
	}
	grants, err := people.MailGrants(ctx, principalID)
	switch {
	case errors.Is(err, ErrPrincipalUnknown):
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "grant-store does not know principal %s", principalID)
	case err != nil:
		// Never read a failure as "holds nothing".
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "grant-store: %v", err)
	}
	for _, g := range grants {
		switch g.Relation {
		case relationCanReadMail:
			access.readAccounts[g.AccountID] = true
		case relationCanImportMail:
			access.importAccount[g.AccountID] = true
		}
	}
	return access, nil
}

// visibleThroughTickets asks kanban-store which of locators the person sees
// through a card they may view, in one call.
func (s *Server) visibleThroughTickets(ctx context.Context, access *principalAccess, locators []string) (map[string]bool, *accessRefusal) {
	visible := map[string]bool{}
	if len(locators) == 0 {
		return visible, nil
	}
	answers, err := s.people.MessagesVisibleThroughTickets(ctx, access.principalID, locators)
	switch {
	case err != nil:
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "kanban-store: %v", err)
	case len(answers) != len(locators):
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "kanban-store answered %d results for %d messages", len(answers), len(locators))
	}
	for i, locator := range locators {
		visible[locator] = answers[i]
	}
	return visible, nil
}

// routeFor wraps a handler in the caller check and the person check its rule
// names.
// admitPrincipal is what routeFor decides for a principal: what it may do,
// or the refusal the route answers. messageID is the message a
// ruleReadMessage route names; route names the route in a refusal.
func (s *Server) admitPrincipal(ctx context.Context, principalID string, rule accessRule, accountID, messageID, route string) (*principalAccess, *accessRefusal) {
	access, refused := s.accessOfPrincipal(ctx, principalID)
	if refused != nil {
		return nil, refused
	}
	if access.administrator || rule == ruleHandlerFilters {
		return access, nil
	}
	switch rule {
	case ruleServicesOnly:
		return nil, refusal(http.StatusForbidden, errorCodeRouteNotOpenToPrincipal, "%s serves services and administrators, not %s", route, principalID)
	case ruleReadAccount:
		if !access.mayReadAccount(accountID) {
			return nil, refusal(http.StatusForbidden, errorCodeMailAccountNotGranted, "%s holds no %s on mail account %q", principalID, relationCanReadMail, accountID)
		}
	case ruleImportAccount:
		if !access.mayImportAccount(accountID) {
			return nil, refusal(http.StatusForbidden, errorCodeMailAccountNotGranted, "%s holds no %s on mail account %q", principalID, relationCanImportMail, accountID)
		}
	case ruleReadMessage:
		if !access.mayReadAccount(accountID) {
			locator := accountID + ":" + messageID
			visible, refused := s.visibleThroughTickets(ctx, access, []string{locator})
			if refused != nil {
				return nil, refused
			}
			if !visible[locator] {
				// The same answer as a message that does not exist, so a
				// person cannot learn which ids are real.
				return nil, refusal(http.StatusNotFound, errorCodeMessageNotFound, "no message %s that %s may read", locator, principalID)
			}
		}
	default:
		panic(fmt.Sprintf("mailstack: unknown access rule %d", rule))
	}
	return access, nil
}

func (s *Server) routeFor(rule accessRule, account accountOfRoute, next http.HandlerFunc) http.Handler {
	if rule == 0 {
		panic("mailstack: a route must declare who may use it")
	}
	if (rule == ruleReadAccount || rule == ruleImportAccount || rule == ruleReadMessage) && account == 0 {
		panic("mailstack: a route checked against an account must say where it names the account")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller, named := callerOf(r)
		if !named || (!caller.InternalService && caller.PrincipalID == "") {
			// The mounting server lets nothing through without verifying who
			// it is from, so this is a wiring mistake; serving it would serve
			// everything.
			writeCodedError(w, http.StatusUnauthorized, errorCodeNoCaller,
				errors.New("no caller: mailstack answers a principal or the internal service, as the server it runs in verified them"))
			return
		}
		if caller.InternalService {
			next(w, r)
			return
		}
		accountID := r.URL.Query().Get("account")
		if account == accountInPath {
			accountID = r.PathValue("id")
		}
		access, refused := s.admitPrincipal(r.Context(), caller.PrincipalID, rule, accountID, r.PathValue("id"), r.Method+" "+r.Pattern)
		if refused != nil {
			writeCodedError(w, refused.status, refused.code, refused)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalAccessKey{}, access))
		next(w, r)
	})
}
