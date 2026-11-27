package server

// The connected_records stores moved in from the standalone people-store,
// calendar-store and journal-store: the same routes under /people/,
// /calendar/ and /journal/, answered from this process.
//
// Every record in them has an owner, and each store narrows every read and
// write to the records the caller owns, or every owner's for an
// administrator. The caller reaches the store in the request context
// (ownership.Caller), set here from the credential request authorization
// verified, never from a header a client sent. This server's own service
// acting as itself sees every owner's records in people-store and
// calendar-store, as the standalone stores' loopback callers did; journal-store
// answers only a principal, the owner's or an administrator's, so the service
// token reaches it only beside an X-Principal-Id.
//
// The bridge's own readers of people-store — chat links, sign-in and
// calendar-store's attendee lookups — call the module in process.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	"github.com/kayushkin/llm-bridge-server/internal/access/signin"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/calendar"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/journal"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/people"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	itemsapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/api"
	itemsmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/model"
)

// Where each store is mounted.
const (
	peopleMountPrefix   = "/people"
	calendarMountPrefix = "/calendar"
	journalMountPrefix  = "/journal"
)

// connectedRecordsStores is the three stores as this server serves them.
type connectedRecordsStores struct {
	people          *people.Store
	peopleHandler   http.Handler
	calendarHandler http.Handler
	journal         *journal.Store
	journalHandler  http.Handler
}

// ConnectedRecordsModules is each store's Postgres pool.
type ConnectedRecordsModules struct {
	People   *sql.DB
	Calendar *sql.DB
	Journal  *sql.DB
}

// EnableConnectedRecords serves people-store, calendar-store and journal-store
// from their modules. main calls it once, after EnableAccess (people-store
// checks a principal_id with principal-store), EnableIssues (journal-store's
// promote-todo makes its todo an issue) and EnableReferences (journal-store
// types its mentions from reference-store's list), and before
// the server listens. Until then their routes answer 503, and chat links and
// sign-in cannot read who owns an account.
func (s *Server) EnableConnectedRecords(ctx context.Context, modules ConnectedRecordsModules) error {
	if s.principalStore == nil {
		return errors.New("connected records: EnableAccess must run first, since people-store checks every principal_id with principal-store")
	}
	resolver := s.referenceResolver.Load()
	if resolver == nil {
		return errors.New("connected records: EnableReferences must run first, since journal-store types its mentions from reference-store's list")
	}
	issueItems := s.issueItems.Load()
	if issueItems == nil {
		return errors.New("connected records: EnableIssues must run first, since journal-store's promote-todo makes its todo an issue")
	}

	peopleStore := people.New(modules.People, peoplePrincipalChecker{principals: s.principalStore})
	peopleMux := http.NewServeMux()
	people.RegisterHandlers(peopleMux, peopleStore)

	calendarStore := calendar.New(modules.Calendar, calendarPeople{people: peopleStore})
	s.referenceReadable.calendar.Store(calendarStore)
	calendarMux := http.NewServeMux()
	calendar.RegisterHandlers(calendarMux, calendarStore)

	// reference-store's list is compiled into this binary, so it cannot
	// change while the process runs: one read at start types every mention.
	registry := journal.NewEntityTypeRegistry(func(context.Context) ([]journal.EntityTypeRow, error) {
		rows := make([]journal.EntityTypeRow, 0, len(resolver.Types))
		for _, row := range resolver.Types {
			rows = append(rows, journal.EntityTypeRow{Type: row.Type, Service: row.Service, Get: row.Get, IDPatterns: row.IDPatterns})
		}
		return rows, nil
	})
	if err := registry.Refresh(ctx); err != nil {
		return fmt.Errorf("journal-store: reading reference-store's list of types: %w", err)
	}
	journalStore := journal.New(modules.Journal, registry, journalIssues{issues: issueItems})
	typed, err := journalStore.BackfillMentionTypes(ctx)
	if err != nil {
		return fmt.Errorf("journal-store: typing the mentions stored untyped: %w", err)
	}
	if typed > 0 {
		log.Printf("journal-store: typed %d mentions stored before the list of types could classify them", typed)
	}
	journalMux := http.NewServeMux()
	journal.RegisterHandlers(journalMux, journalStore)

	s.connectedRecords.Store(&connectedRecordsStores{
		people: peopleStore, peopleHandler: peopleMux,
		calendarHandler: calendarMux,
		journal:         journalStore, journalHandler: journalMux,
	})
	return nil
}

// errConnectedRecordsOff is a use of people-store on a server that never
// enabled it.
var errConnectedRecordsOff = errors.New("people-store is off: EnableConnectedRecords was never called on this server")

// connectedRecordsRoute answers one store's route as the verified caller.
// requirePrincipal refuses this server's own service acting as itself, which
// names nobody.
func (s *Server) connectedRecordsRoute(mountPrefix, storeName, offCode string, handlerOf func(*connectedRecordsStores) http.Handler, requirePrincipal bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stores := s.connectedRecords.Load()
		if stores == nil {
			writeJSONError(w, http.StatusServiceUnavailable, offCode, storeName+" is not open on this server: EnableConnectedRecords was never called")
			return
		}
		var caller ownership.Caller
		if requestCaller, ok := callerOfRequest(r); ok && requestCaller.principalID != "" {
			caller = ownership.Caller{PrincipalID: requestCaller.principalID, SeesEveryOwner: requestCaller.isAdministrator}
		} else if s.requestIsTheInternalServiceActingAsItself(r) {
			if requirePrincipal {
				writeJSONError(w, http.StatusForbidden, strings.ReplaceAll(storeName, "-", "_")+"_requires_a_principal", fmt.Sprintf(
					"%s answers only its owner's or an administrator's principal; send the service token with %s naming the person it is for", storeName, principalIdentityHeader))
				return
			}
			caller = ownership.Caller{SeesEveryOwner: true}
		} else {
			// Request authorization lets nothing else through to these routes.
			writeJSONError(w, http.StatusUnauthorized, strings.ReplaceAll(storeName, "-", "_")+"_requires_a_caller",
				mountPrefix+"/ answers a principal or this server's service token")
			return
		}
		served := r.Clone(ownership.ContextWithCaller(r.Context(), caller))
		// The store reads none of these, and none is its business: the caller
		// is in the context.
		for key := range served.Header {
			for _, removed := range requestHeadersNeverForwardedToStores {
				if strings.EqualFold(key, removed) {
					delete(served.Header, key)
				}
			}
		}
		removeCookieFromRequestHeader(served.Header, loginSessionCookieName)
		http.StripPrefix(mountPrefix, handlerOf(stores)).ServeHTTP(w, served)
	}
}

func (s *Server) peopleStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(peopleMountPrefix, "people-store", "people_store_off",
		func(stores *connectedRecordsStores) http.Handler { return stores.peopleHandler }, false)
}

func (s *Server) calendarStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(calendarMountPrefix, "calendar-store", "calendar_store_off",
		func(stores *connectedRecordsStores) http.Handler { return stores.calendarHandler }, false)
}

func (s *Server) journalStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(journalMountPrefix, "journal-store", "journal_store_off",
		func(stores *connectedRecordsStores) http.Handler { return stores.journalHandler }, true)
}

// peoplePrincipalChecker is principal-store, in this process, as people-store
// checks a person's principal_id.
type peoplePrincipalChecker struct {
	principals principalDirectory
}

func (checker peoplePrincipalChecker) CheckPrincipal(ctx context.Context, id string) error {
	_, err := checker.principals.Get(ctx, id, false)
	if errors.Is(err, principals.ErrNotFound) {
		return fmt.Errorf("%w: principal-store holds no principal %s", people.ErrInvalid, id)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", people.ErrPrincipalStoreUnavailable, err)
	}
	return nil
}

// calendarPeople is people-store as calendar-store reads it: one owner's
// records, the owner of the event being read or written.
type calendarPeople struct {
	people *people.Store
}

func (lookup calendarPeople) PersonByEmail(ctx context.Context, ownerPrincipalID, email string) (string, error) {
	resolved, err := lookup.people.ResolveIdentity(ctx, ownership.OwnedBy(ownerPrincipalID), "email", email)
	if errors.Is(err, people.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("people-store: %w", err)
	}
	return resolved.Person.ID, nil
}

func (lookup calendarPeople) PersonExists(ctx context.Context, ownerPrincipalID, personID string) (bool, error) {
	person, err := lookup.people.GetPerson(ctx, ownership.OwnedBy(ownerPrincipalID), personID)
	if errors.Is(err, people.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("people-store: %w", err)
	}
	return person.DeletedAt == 0, nil
}

// chatLinkSendersInPeopleStore is people-store as chat links read it.
type chatLinkSendersInPeopleStore struct {
	server *Server
}

func (senders chatLinkSendersInPeopleStore) SenderOfAccount(ctx context.Context, ownerPrincipalID, kind, value string) (store.ChatLinkMessageSender, error) {
	stores := senders.server.connectedRecords.Load()
	if stores == nil {
		return store.ChatLinkMessageSender{}, errConnectedRecordsOff
	}
	resolved, err := stores.people.ResolveIdentity(ctx, ownership.OwnedBy(ownerPrincipalID), kind, value)
	if errors.Is(err, people.ErrNotFound) {
		return store.ChatLinkMessageSender{}, nil
	}
	if err != nil {
		return store.ChatLinkMessageSender{}, err
	}
	return store.ChatLinkMessageSender{PersonID: resolved.Person.ID, PrincipalID: resolved.Person.PrincipalID, PersonDisplayName: resolved.Person.DisplayName}, nil
}

// signInPeople is people-store as sign-in reads it. Sign-in has no caller:
// the person typing a Discord name or a phone number is not signed in yet,
// and what people-store answers decides whose sign-in link is sent where. So
// it trusts only records an administrator owns. A record anyone else keeps
// could otherwise name an administrator's principal beside the keeper's own
// Discord account, and the keeper would be sent a link that signs them in as
// that administrator. Today every record is the operator's, so this reads
// what the standalone store answered.
type signInPeople struct {
	server *Server
}

func (people signInPeople) stores() (*connectedRecordsStores, error) {
	stores := people.server.connectedRecords.Load()
	if stores == nil {
		return nil, errConnectedRecordsOff
	}
	return stores, nil
}

// ownerIsAnAdministrator asks principal-store whether the owner of a record
// is an administrator now.
func (people signInPeople) ownerIsAnAdministrator(ctx context.Context, ownerPrincipalID string) (bool, error) {
	principal, err := people.server.principalStore.Get(ctx, ownerPrincipalID, false)
	if errors.Is(err, principals.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("principal-store, about the owner %s: %w", ownerPrincipalID, err)
	}
	return principal.IsAdministrator && principal.DisabledAt == 0, nil
}

// trustedPerson reads one person whole, as sign-in needs it.
func (people signInPeople) trustedPerson(ctx context.Context, stores *connectedRecordsStores, ownerPrincipalID, personID string) (*signin.Person, error) {
	detail, err := stores.people.GetPerson(ctx, ownership.OwnedBy(ownerPrincipalID), personID)
	if err != nil {
		return nil, err
	}
	person := &signin.Person{ID: detail.ID, PrincipalID: detail.PrincipalID}
	for _, identity := range detail.Identities {
		person.Identities = append(person.Identities, signin.PersonIdentity{Kind: identity.Kind, Value: identity.Value})
	}
	return person, nil
}

func (directory signInPeople) PersonOwningHandle(ctx context.Context, kind, value string) (*signin.Person, error) {
	stores, err := directory.stores()
	if err != nil {
		return nil, err
	}
	matches, err := stores.people.ResolveIdentityInEveryOwner(ctx, kind, value)
	if err != nil {
		return nil, err
	}
	var trusted []people.IdentityResolution
	for _, match := range matches {
		administrator, err := directory.ownerIsAnAdministrator(ctx, match.Person.OwnerPrincipalID)
		if err != nil {
			return nil, err
		}
		if administrator {
			trusted = append(trusted, match)
		}
	}
	switch len(trusted) {
	case 0:
		return nil, nil
	case 1:
		return directory.trustedPerson(ctx, stores, trusted[0].Person.OwnerPrincipalID, trusted[0].Person.ID)
	}
	return nil, fmt.Errorf("the records of %d administrators hold this %s handle, so sign-in cannot tell whose it is", len(trusted), kind)
}

func (directory signInPeople) PersonPointingAtPrincipal(ctx context.Context, principalID string) (*signin.Person, error) {
	stores, err := directory.stores()
	if err != nil {
		return nil, err
	}
	listed, err := stores.people.ListPersons(ctx, ownership.EveryOwner(), people.PersonFilter{PrincipalID: principalID})
	if err != nil {
		return nil, err
	}
	var trusted []people.Person
	for _, person := range listed {
		administrator, err := directory.ownerIsAnAdministrator(ctx, person.OwnerPrincipalID)
		if err != nil {
			return nil, err
		}
		if administrator {
			trusted = append(trusted, person)
		}
	}
	switch len(trusted) {
	case 0:
		return nil, nil
	case 1:
		return directory.trustedPerson(ctx, stores, trusted[0].OwnerPrincipalID, trusted[0].ID)
	}
	return nil, fmt.Errorf("%d people in administrators' records point at %s", len(trusted), principalID)
}

// journalIssues is the issues module, in this process, as journal-store's
// promote-todo makes a todo: through the issue items API's own Create, so the
// issue is checked as one made at POST /issues/items.
type journalIssues struct {
	issues *itemsapi.API
}

func (issues journalIssues) CreateTodo(_ context.Context, title, body string, tags, links []string) (string, error) {
	item, err := issues.issues.Create(&itemsmodel.CreateItemRequest{Type: itemsmodel.TypeTodo, Title: title, Body: &body, Tags: tags, Links: links})
	var refusal *itemsapi.RequestRefusal
	if errors.As(err, &refusal) {
		return "", fmt.Errorf("%w: issues answered 400: %v", journal.ErrUpstream, refusal)
	}
	if err != nil {
		return "", fmt.Errorf("%w: issues: %v", journal.ErrUpstream, err)
	}
	return item.ID, nil
}

// registerConnectedRecordsRoutes mounts the three stores.
func (s *Server) registerConnectedRecordsRoutes() {
	people := s.peopleStoreRoute()
	s.mux.HandleFunc("GET /people/health", people)
	s.mux.HandleFunc("GET /people/identity-kinds", people)
	s.mux.HandleFunc("GET /people/relationship-kinds", people)
	s.mux.HandleFunc("GET /people/link-sources", people)
	s.mux.HandleFunc("GET /people/persons", people)
	s.mux.HandleFunc("POST /people/persons", people)
	s.mux.HandleFunc("GET /people/persons/{id}", people)
	s.mux.HandleFunc("PATCH /people/persons/{id}", people)
	s.mux.HandleFunc("DELETE /people/persons/{id}", people)
	s.mux.HandleFunc("POST /people/persons/{id}/restore", people)
	s.mux.HandleFunc("GET /people/persons/{id}/revisions", people)
	s.mux.HandleFunc("POST /people/persons/{id}/identities", people)
	s.mux.HandleFunc("POST /people/persons/{id}/relationships", people)
	s.mux.HandleFunc("GET /people/identities", people)
	s.mux.HandleFunc("GET /people/identities/resolve", people)
	s.mux.HandleFunc("GET /people/refusals", people)
	s.mux.HandleFunc("DELETE /people/identities/{id}", people)
	s.mux.HandleFunc("DELETE /people/relationships/{id}", people)
	s.mux.HandleFunc("GET /people/suggestions", people)
	s.mux.HandleFunc("POST /people/suggestions", people)
	s.mux.HandleFunc("GET /people/suggestions/{id}", people)
	s.mux.HandleFunc("POST /people/suggestions/{id}/accept", people)
	s.mux.HandleFunc("POST /people/suggestions/{id}/reject", people)
	s.mux.HandleFunc("PUT /people/handle-activity", people)
	s.mux.HandleFunc("GET /people/reviews/pending", people)
	s.mux.HandleFunc("POST /people/reviews/claim", people)
	s.mux.HandleFunc("POST /people/reviews", people)

	calendar := s.calendarStoreRoute()
	s.mux.HandleFunc("GET /calendar/health", calendar)
	s.mux.HandleFunc("GET /calendar/vocabularies", calendar)
	s.mux.HandleFunc("GET /calendar/calendars", calendar)
	s.mux.HandleFunc("POST /calendar/calendars", calendar)
	s.mux.HandleFunc("POST /calendar/calendars/{id}/imports", calendar)
	s.mux.HandleFunc("POST /calendar/calendars/{id}/google-event-imports", calendar)
	s.mux.HandleFunc("GET /calendar/imports", calendar)
	s.mux.HandleFunc("GET /calendar/events", calendar)
	s.mux.HandleFunc("GET /calendar/events/{id}", calendar)
	s.mux.HandleFunc("GET /calendar/events/{id}/revisions", calendar)
	s.mux.HandleFunc("POST /calendar/events/{id}/people", calendar)
	s.mux.HandleFunc("DELETE /calendar/events/{id}/people/{person_id}", calendar)

	journal := s.journalStoreRoute()
	s.mux.HandleFunc("GET /journal/health", journal)
	s.mux.HandleFunc("GET /journal/entry-kinds", journal)
	s.mux.HandleFunc("GET /journal/visibilities", journal)
	s.mux.HandleFunc("GET /journal/entries", journal)
	s.mux.HandleFunc("POST /journal/entries", journal)
	s.mux.HandleFunc("GET /journal/entries/{id}", journal)
	s.mux.HandleFunc("PATCH /journal/entries/{id}", journal)
	s.mux.HandleFunc("DELETE /journal/entries/{id}", journal)
	s.mux.HandleFunc("POST /journal/entries/{id}/restore", journal)
	s.mux.HandleFunc("GET /journal/entries/{id}/revisions", journal)
	s.mux.HandleFunc("POST /journal/entries/{id}/promote-todo", journal)
	s.mux.HandleFunc("GET /journal/mentions", journal)
	s.mux.HandleFunc("GET /journal/published", journal)
	s.mux.HandleFunc("GET /journal/published/{slug}", journal)
	s.mux.HandleFunc("POST /journal/imports", journal)
	s.mux.HandleFunc("GET /journal/time-precisions", journal)
	s.mux.HandleFunc("GET /journal/timelines/{id}/items", journal)
	s.mux.HandleFunc("POST /journal/timelines/{id}/items", journal)
	s.mux.HandleFunc("GET /journal/timeline-items", journal)
	s.mux.HandleFunc("GET /journal/timeline-items/{id}", journal)
	s.mux.HandleFunc("PATCH /journal/timeline-items/{id}", journal)
	s.mux.HandleFunc("DELETE /journal/timeline-items/{id}", journal)
	s.mux.HandleFunc("POST /journal/timeline-items/{id}/restore", journal)
	s.mux.HandleFunc("GET /journal/timeline-items/{id}/revisions", journal)
}
