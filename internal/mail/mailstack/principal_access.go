package mailstack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"time"
)

// ─── Per-person access ──────────────────────────────────────────────────
//
// mailstack has two kinds of caller. A service presents the bearer token and
// nothing else, and is unrestricted, as every caller was before. A person
// reaches mailstack through llm-bridge-server's /mailstack/ proxy, which
// verifies their login, presents the bearer for them and names them in
// X-Principal-Id. Such a request is checked on every call, live, against the
// stores that own the answer, so a revoked grant stops the next read:
//
//   - principal-store says whether the principal is an active human, and
//     whether it is an administrator, which passes every check;
//   - grant-store holds can_read_mail and can_import_mail on mail_account
//     resources, the account ids here; can_import_mail includes can_read_mail;
//   - kanban-store says which messages the person sees through a ticket: a
//     card they may view that links the message as an email entity whose ref
//     is the message's locator, "<account_id>:<id>".
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
//
// X-Principal-Id is trusted only beside the bearer token, which only services
// hold; a token holder can already read everything, so naming a principal can
// only narrow what it reads.

// PrincipalIDHeader names the person a gateway forwards a request for.
const PrincipalIDHeader = "X-Principal-Id"

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

// PrincipalAccessStores says where the three stores answer and with what
// token mailstack calls them.
type PrincipalAccessStores struct {
	PrincipalStoreURL       string
	GrantStoreURL           string
	GrantStoreServiceToken  string
	KanbanStoreURL          string
	KanbanStoreServiceToken string
	HTTP                    *http.Client
}

// WithPrincipalAccess lets mailstack serve people. Without it, a request that
// names a principal is refused with 503: it cannot be checked, and serving it
// unchecked would serve everything.
func WithPrincipalAccess(stores PrincipalAccessStores) ServerOption {
	return func(s *Server) {
		if stores.HTTP == nil {
			stores.HTTP = &http.Client{Timeout: 5 * time.Second}
		}
		s.principalStores = &stores
	}
}

// accessOfPrincipal reads what principalID may do, live.
func (s *Server) accessOfPrincipal(ctx context.Context, principalID string) (*principalAccess, *accessRefusal) {
	if !principalIDPattern.MatchString(principalID) {
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "%s %q is not a principal id", PrincipalIDHeader, principalID)
	}
	stores := s.principalStores
	if stores == nil {
		return nil, refusal(http.StatusServiceUnavailable, errorCodePrincipalAccessDown,
			"this mailstack is not configured to check people (principal-store, grant-store and kanban-store settings), so it serves only services")
	}

	var principal struct {
		Kind            string `json:"kind"`
		DisabledAt      int64  `json:"disabled_at"`
		IsAdministrator bool   `json:"is_administrator"`
	}
	status, err := stores.getJSON(ctx, stores.PrincipalStoreURL+"/principals/"+url.PathEscape(principalID), nil, &principal)
	switch {
	case err != nil:
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "principal-store: %v", err)
	case status == http.StatusNotFound:
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "principal-store has no principal %s", principalID)
	case status != http.StatusOK:
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "principal-store answered %d for %s", status, principalID)
	case principal.Kind != "human":
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "principal %s is a %s, and only a person reads mail", principalID, principal.Kind)
	case principal.DisabledAt != 0:
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "principal %s is disabled", principalID)
	}
	access := &principalAccess{principalID: principalID, administrator: principal.IsAdministrator,
		readAccounts: map[string]bool{}, importAccount: map[string]bool{}}
	if access.administrator {
		return access, nil
	}

	var grants []struct {
		Relation     string `json:"relation"`
		ResourceType string `json:"resource_type"`
		ResourceID   string `json:"resource_id"`
	}
	status, err = stores.getJSON(ctx,
		stores.GrantStoreURL+"/principals/"+url.PathEscape(principalID)+"/effective?resource_type="+resourceTypeMail,
		map[string]string{"X-Grant-Store-Service-Token": stores.GrantStoreServiceToken}, &grants)
	switch {
	case err != nil:
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "grant-store: %v", err)
	case status == http.StatusNotFound:
		return nil, refusal(http.StatusForbidden, errorCodePrincipalRefused, "grant-store does not know principal %s", principalID)
	case status != http.StatusOK:
		// Never read a failure as "holds nothing": grant-store's contract
		// answers 502 rather than [] for exactly this reason.
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "grant-store answered %d for %s", status, principalID)
	}
	for _, g := range grants {
		if g.ResourceType != resourceTypeMail {
			continue
		}
		switch g.Relation {
		case relationCanReadMail:
			access.readAccounts[g.ResourceID] = true
		case relationCanImportMail:
			access.importAccount[g.ResourceID] = true
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
	stores := s.principalStores
	type entity struct {
		EntityType string `json:"entity_type"`
		EntityRef  string `json:"entity_ref"`
	}
	request := struct {
		PrincipalID string   `json:"principal_id"`
		Entities    []entity `json:"entities"`
	}{PrincipalID: access.principalID}
	for _, locator := range locators {
		request.Entities = append(request.Entities, entity{EntityType: entityTypeEmail, EntityRef: locator})
	}
	var answer struct {
		Results []struct {
			EntityRef string `json:"entity_ref"`
			Visible   bool   `json:"visible"`
		} `json:"results"`
	}
	status, err := stores.postJSON(ctx, stores.KanbanStoreURL+"/api/entities/visible-to-principal",
		map[string]string{"X-Kanban-Store-Service-Token": stores.KanbanStoreServiceToken}, request, &answer)
	switch {
	case err != nil:
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "kanban-store: %v", err)
	case status != http.StatusOK:
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "kanban-store answered %d asking which messages %s sees through tickets", status, access.principalID)
	case len(answer.Results) != len(locators):
		return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "kanban-store answered %d results for %d messages", len(answer.Results), len(locators))
	}
	for i, result := range answer.Results {
		if result.EntityRef != locators[i] {
			return nil, refusal(http.StatusBadGateway, errorCodePrincipalAccessDown, "kanban-store answered result %d for %q, asked %q", i, result.EntityRef, locators[i])
		}
		visible[locators[i]] = result.Visible
	}
	return visible, nil
}

func (stores *PrincipalAccessStores) getJSON(ctx context.Context, target string, headers map[string]string, out any) (int, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	return stores.do(request, headers, out)
}

func (stores *PrincipalAccessStores) postJSON(ctx context.Context, target string, headers map[string]string, body, out any) (int, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return 0, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Content-Type", "application/json")
	return stores.do(request, headers, out)
}

// do sends one request and decodes a 200's body into out. Any other status
// is returned for the caller to judge, with the body dropped.
func (stores *PrincipalAccessStores) do(request *http.Request, headers map[string]string, out any) (int, error) {
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := stores.HTTP.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return 0, fmt.Errorf("read answer: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return response.StatusCode, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return 0, fmt.Errorf("answer does not parse: %w", err)
	}
	return response.StatusCode, nil
}

// routeFor wraps a handler in the bearer check and the person check its rule
// names.
func (s *Server) routeFor(rule accessRule, account accountOfRoute, next http.HandlerFunc) http.Handler {
	if rule == 0 {
		panic("mailstack: a route must declare who may use it")
	}
	if (rule == ruleReadAccount || rule == ruleImportAccount || rule == ruleReadMessage) && account == 0 {
		panic("mailstack: a route checked against an account must say where it names the account")
	}
	return s.auth(func(w http.ResponseWriter, r *http.Request) {
		principalID := r.Header.Get(PrincipalIDHeader)
		if principalID == "" {
			next(w, r)
			return
		}
		access, refused := s.accessOfPrincipal(r.Context(), principalID)
		if refused != nil {
			writeCodedError(w, refused.status, refused.code, refused)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), principalAccessKey{}, access))
		if access.administrator || rule == ruleHandlerFilters {
			next(w, r)
			return
		}

		accountID := r.URL.Query().Get("account")
		if account == accountInPath {
			accountID = r.PathValue("id")
		}
		switch rule {
		case ruleServicesOnly:
			writeCodedError(w, http.StatusForbidden, errorCodeRouteNotOpenToPrincipal,
				fmt.Errorf("%s %s serves services and administrators, not %s", r.Method, r.Pattern, principalID))
			return
		case ruleReadAccount:
			if !access.mayReadAccount(accountID) {
				writeCodedError(w, http.StatusForbidden, errorCodeMailAccountNotGranted,
					fmt.Errorf("%s holds no %s on mail account %q", principalID, relationCanReadMail, accountID))
				return
			}
		case ruleImportAccount:
			if !access.mayImportAccount(accountID) {
				writeCodedError(w, http.StatusForbidden, errorCodeMailAccountNotGranted,
					fmt.Errorf("%s holds no %s on mail account %q", principalID, relationCanImportMail, accountID))
				return
			}
		case ruleReadMessage:
			if !access.mayReadAccount(accountID) {
				locator := accountID + ":" + r.PathValue("id")
				visible, refused := s.visibleThroughTickets(r.Context(), access, []string{locator})
				if refused != nil {
					writeCodedError(w, refused.status, refused.code, refused)
					return
				}
				if !visible[locator] {
					// The same answer as a message that does not exist, so a
					// person cannot learn which ids are real.
					writeCodedError(w, http.StatusNotFound, errorCodeMessageNotFound,
						fmt.Errorf("no message %s that %s may read", locator, principalID))
					return
				}
			}
		default:
			panic(fmt.Sprintf("mailstack: unknown access rule %d", rule))
		}
		next(w, r)
	})
}
