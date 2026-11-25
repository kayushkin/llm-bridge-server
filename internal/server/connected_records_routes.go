package server

// The connected_records stores moved in from the standalone people-store,
// calendar-store and journal-store: the same routes under /people-store/,
// /calendar-store/ and /journal-store/, answered from this process.
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

	"github.com/kayushkin/llm-bridge-server/internal/access/signinstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/calendarstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/journalstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/peoplestore"
	"github.com/kayushkin/llm-bridge-server/internal/principalclient"
	"github.com/kayushkin/llm-bridge-server/internal/store"
	noteboardapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/api"
	noteboardmodel "github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/model"
)

// Where each store is mounted.
const (
	peopleStoreMountPrefix   = "/people-store"
	calendarStoreMountPrefix = "/calendar-store"
	journalStoreMountPrefix  = "/journal-store"
)

// connectedRecordsStores is the three stores as this server serves them.
type connectedRecordsStores struct {
	people          *peoplestore.Store
	peopleHandler   http.Handler
	calendarHandler http.Handler
	journal         *journalstore.Store
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
// checks a principal_id with principal-store), EnableNoteboard (journal-store's
// promote-todo makes its todo there) and EnableReferenceStore (journal-store
// types its mentions from reference-store's list), and before
// the server listens. Until then their routes answer 503, and chat links and
// sign-in cannot read who owns an account.
func (s *Server) EnableConnectedRecords(ctx context.Context, modules ConnectedRecordsModules) error {
	if s.principalClient == nil {
		return errors.New("connected records: EnableAccess must run first, since people-store checks every principal_id with principal-store")
	}
	resolver := s.referenceResolver.Load()
	if resolver == nil {
		return errors.New("connected records: EnableReferenceStore must run first, since journal-store types its mentions from reference-store's list")
	}
	noteboard := s.noteboard.Load()
	if noteboard == nil {
		return errors.New("connected records: EnableNoteboard must run first, since journal-store's promote-todo makes its todo in noteboard")
	}

	people := peoplestore.New(modules.People, peoplePrincipalChecker{principals: s.principalClient})
	peopleMux := http.NewServeMux()
	peoplestore.RegisterHandlers(peopleMux, people)

	calendar := calendarstore.New(modules.Calendar, calendarPeople{people: people})
	calendarMux := http.NewServeMux()
	calendarstore.RegisterHandlers(calendarMux, calendar)

	// reference-store's list is compiled into this binary, so it cannot
	// change while the process runs: one read at start types every mention.
	registry := journalstore.NewEntityTypeRegistry(func(context.Context) ([]journalstore.EntityTypeRow, error) {
		rows := make([]journalstore.EntityTypeRow, 0, len(resolver.Types))
		for _, row := range resolver.Types {
			rows = append(rows, journalstore.EntityTypeRow{Type: row.Type, Service: row.Service, Get: row.Get, IDPatterns: row.IDPatterns})
		}
		return rows, nil
	})
	if err := registry.Refresh(ctx); err != nil {
		return fmt.Errorf("journal-store: reading reference-store's list of types: %w", err)
	}
	journal := journalstore.New(modules.Journal, registry, journalNoteboard{notes: noteboard})
	typed, err := journal.BackfillMentionTypes(ctx)
	if err != nil {
		return fmt.Errorf("journal-store: typing the mentions stored untyped: %w", err)
	}
	if typed > 0 {
		log.Printf("journal-store: typed %d mentions stored before the list of types could classify them", typed)
	}
	journalMux := http.NewServeMux()
	journalstore.RegisterHandlers(journalMux, journal)

	s.connectedRecords.Store(&connectedRecordsStores{
		people: people, peopleHandler: peopleMux,
		calendarHandler: calendarMux,
		journal:         journal, journalHandler: journalMux,
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
	return s.connectedRecordsRoute(peopleStoreMountPrefix, "people-store", "people_store_off",
		func(stores *connectedRecordsStores) http.Handler { return stores.peopleHandler }, false)
}

func (s *Server) calendarStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(calendarStoreMountPrefix, "calendar-store", "calendar_store_off",
		func(stores *connectedRecordsStores) http.Handler { return stores.calendarHandler }, false)
}

func (s *Server) journalStoreRoute() http.HandlerFunc {
	return s.connectedRecordsRoute(journalStoreMountPrefix, "journal-store", "journal_store_off",
		func(stores *connectedRecordsStores) http.Handler { return stores.journalHandler }, true)
}

// peoplePrincipalChecker is principal-store, in this process, as people-store
// checks a person's principal_id.
type peoplePrincipalChecker struct {
	principals principalDirectory
}

func (checker peoplePrincipalChecker) CheckPrincipal(ctx context.Context, id string) error {
	err := checker.principals.CheckExists(ctx, id)
	if errors.Is(err, principalclient.ErrNotFound) {
		return fmt.Errorf("%w: principal-store holds no principal %s", peoplestore.ErrInvalid, id)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", peoplestore.ErrPrincipalStoreUnavailable, err)
	}
	return nil
}

// calendarPeople is people-store as calendar-store reads it: one owner's
// records, the owner of the event being read or written.
type calendarPeople struct {
	people *peoplestore.Store
}

func (lookup calendarPeople) PersonByEmail(ctx context.Context, ownerPrincipalID, email string) (string, error) {
	resolved, err := lookup.people.ResolveIdentity(ctx, ownership.OwnedBy(ownerPrincipalID), "email", email)
	if errors.Is(err, peoplestore.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("people-store: %w", err)
	}
	return resolved.Person.ID, nil
}

func (lookup calendarPeople) PersonExists(ctx context.Context, ownerPrincipalID, personID string) (bool, error) {
	person, err := lookup.people.GetPerson(ctx, ownership.OwnedBy(ownerPrincipalID), personID)
	if errors.Is(err, peoplestore.ErrNotFound) {
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
	if errors.Is(err, peoplestore.ErrNotFound) {
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
	principal, err := people.server.principalClient.Get(ctx, ownerPrincipalID)
	if errors.Is(err, principalclient.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("principal-store, about the owner %s: %w", ownerPrincipalID, err)
	}
	return principal.IsAdministrator && principal.DisabledAt == 0, nil
}

// trustedPerson reads one person whole, as sign-in needs it.
func (people signInPeople) trustedPerson(ctx context.Context, stores *connectedRecordsStores, ownerPrincipalID, personID string) (*signinstore.Person, error) {
	detail, err := stores.people.GetPerson(ctx, ownership.OwnedBy(ownerPrincipalID), personID)
	if err != nil {
		return nil, err
	}
	person := &signinstore.Person{ID: detail.ID, PrincipalID: detail.PrincipalID}
	for _, identity := range detail.Identities {
		person.Identities = append(person.Identities, signinstore.PersonIdentity{Kind: identity.Kind, Value: identity.Value})
	}
	return person, nil
}

func (people signInPeople) PersonOwningHandle(ctx context.Context, kind, value string) (*signinstore.Person, error) {
	stores, err := people.stores()
	if err != nil {
		return nil, err
	}
	matches, err := stores.people.ResolveIdentityInEveryOwner(ctx, kind, value)
	if err != nil {
		return nil, err
	}
	var trusted []peoplestore.IdentityResolution
	for _, match := range matches {
		administrator, err := people.ownerIsAnAdministrator(ctx, match.Person.OwnerPrincipalID)
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
		return people.trustedPerson(ctx, stores, trusted[0].Person.OwnerPrincipalID, trusted[0].Person.ID)
	}
	return nil, fmt.Errorf("the records of %d administrators hold this %s handle, so sign-in cannot tell whose it is", len(trusted), kind)
}

func (people signInPeople) PersonPointingAtPrincipal(ctx context.Context, principalID string) (*signinstore.Person, error) {
	stores, err := people.stores()
	if err != nil {
		return nil, err
	}
	listed, err := stores.people.ListPersons(ctx, ownership.EveryOwner(), peoplestore.PersonFilter{PrincipalID: principalID})
	if err != nil {
		return nil, err
	}
	var trusted []peoplestore.Person
	for _, person := range listed {
		administrator, err := people.ownerIsAnAdministrator(ctx, person.OwnerPrincipalID)
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
		return people.trustedPerson(ctx, stores, trusted[0].OwnerPrincipalID, trusted[0].ID)
	}
	return nil, fmt.Errorf("%d people in administrators' records point at %s", len(trusted), principalID)
}

// journalNoteboard is noteboard, in this process, as journal-store's
// promote-todo makes a todo: through noteboard's own Create, so the todo is
// checked as any other.
type journalNoteboard struct {
	notes *noteboardapi.API
}

func (noteboard journalNoteboard) CreateTodo(_ context.Context, title, body string, tags, links []string) (string, error) {
	item, err := noteboard.notes.Create(&noteboardmodel.CreateItemRequest{Type: "todo", Title: title, Body: &body, Tags: tags, Links: links})
	var refusal *noteboardapi.RequestRefusal
	if errors.As(err, &refusal) {
		return "", fmt.Errorf("%w: noteboard answered 400: %v", journalstore.ErrUpstream, refusal)
	}
	if err != nil {
		return "", fmt.Errorf("%w: noteboard: %v", journalstore.ErrUpstream, err)
	}
	return item.ID, nil
}

// registerConnectedRecordsRoutes mounts the three stores.
func (s *Server) registerConnectedRecordsRoutes() {
	people := s.peopleStoreRoute()
	s.mux.HandleFunc("GET /people-store/health", people)
	s.mux.HandleFunc("GET /people-store/identity-kinds", people)
	s.mux.HandleFunc("GET /people-store/relationship-kinds", people)
	s.mux.HandleFunc("GET /people-store/link-sources", people)
	s.mux.HandleFunc("GET /people-store/persons", people)
	s.mux.HandleFunc("POST /people-store/persons", people)
	s.mux.HandleFunc("GET /people-store/persons/{id}", people)
	s.mux.HandleFunc("PATCH /people-store/persons/{id}", people)
	s.mux.HandleFunc("DELETE /people-store/persons/{id}", people)
	s.mux.HandleFunc("POST /people-store/persons/{id}/restore", people)
	s.mux.HandleFunc("GET /people-store/persons/{id}/revisions", people)
	s.mux.HandleFunc("POST /people-store/persons/{id}/identities", people)
	s.mux.HandleFunc("POST /people-store/persons/{id}/relationships", people)
	s.mux.HandleFunc("GET /people-store/identities", people)
	s.mux.HandleFunc("GET /people-store/identities/resolve", people)
	s.mux.HandleFunc("GET /people-store/refusals", people)
	s.mux.HandleFunc("DELETE /people-store/identities/{id}", people)
	s.mux.HandleFunc("DELETE /people-store/relationships/{id}", people)
	s.mux.HandleFunc("GET /people-store/suggestions", people)
	s.mux.HandleFunc("POST /people-store/suggestions", people)
	s.mux.HandleFunc("GET /people-store/suggestions/{id}", people)
	s.mux.HandleFunc("POST /people-store/suggestions/{id}/accept", people)
	s.mux.HandleFunc("POST /people-store/suggestions/{id}/reject", people)
	s.mux.HandleFunc("PUT /people-store/handle-activity", people)
	s.mux.HandleFunc("GET /people-store/reviews/pending", people)
	s.mux.HandleFunc("POST /people-store/reviews/claim", people)
	s.mux.HandleFunc("POST /people-store/reviews", people)

	calendar := s.calendarStoreRoute()
	s.mux.HandleFunc("GET /calendar-store/health", calendar)
	s.mux.HandleFunc("GET /calendar-store/vocabularies", calendar)
	s.mux.HandleFunc("GET /calendar-store/calendars", calendar)
	s.mux.HandleFunc("POST /calendar-store/calendars", calendar)
	s.mux.HandleFunc("POST /calendar-store/calendars/{id}/imports", calendar)
	s.mux.HandleFunc("POST /calendar-store/calendars/{id}/google-event-imports", calendar)
	s.mux.HandleFunc("GET /calendar-store/imports", calendar)
	s.mux.HandleFunc("GET /calendar-store/events", calendar)
	s.mux.HandleFunc("GET /calendar-store/events/{id}", calendar)
	s.mux.HandleFunc("GET /calendar-store/events/{id}/revisions", calendar)
	s.mux.HandleFunc("POST /calendar-store/events/{id}/people", calendar)
	s.mux.HandleFunc("DELETE /calendar-store/events/{id}/people/{person_id}", calendar)

	journal := s.journalStoreRoute()
	s.mux.HandleFunc("GET /journal-store/health", journal)
	s.mux.HandleFunc("GET /journal-store/entry-kinds", journal)
	s.mux.HandleFunc("GET /journal-store/visibilities", journal)
	s.mux.HandleFunc("GET /journal-store/entries", journal)
	s.mux.HandleFunc("POST /journal-store/entries", journal)
	s.mux.HandleFunc("GET /journal-store/entries/{id}", journal)
	s.mux.HandleFunc("PATCH /journal-store/entries/{id}", journal)
	s.mux.HandleFunc("DELETE /journal-store/entries/{id}", journal)
	s.mux.HandleFunc("POST /journal-store/entries/{id}/restore", journal)
	s.mux.HandleFunc("GET /journal-store/entries/{id}/revisions", journal)
	s.mux.HandleFunc("POST /journal-store/entries/{id}/promote-todo", journal)
	s.mux.HandleFunc("GET /journal-store/mentions", journal)
	s.mux.HandleFunc("GET /journal-store/published", journal)
	s.mux.HandleFunc("GET /journal-store/published/{slug}", journal)
	s.mux.HandleFunc("POST /journal-store/imports", journal)
	s.mux.HandleFunc("GET /journal-store/time-precisions", journal)
	s.mux.HandleFunc("GET /journal-store/timelines/{id}/items", journal)
	s.mux.HandleFunc("POST /journal-store/timelines/{id}/items", journal)
	s.mux.HandleFunc("GET /journal-store/timeline-items", journal)
	s.mux.HandleFunc("GET /journal-store/timeline-items/{id}", journal)
	s.mux.HandleFunc("PATCH /journal-store/timeline-items/{id}", journal)
	s.mux.HandleFunc("DELETE /journal-store/timeline-items/{id}", journal)
	s.mux.HandleFunc("POST /journal-store/timeline-items/{id}/restore", journal)
	s.mux.HandleFunc("GET /journal-store/timeline-items/{id}/revisions", journal)
}
