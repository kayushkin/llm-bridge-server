package server

// reference-store reads the records of the stores in this process by Go
// calls, never by a request through this server. Each reader calls the store
// method the type's own route calls and answers the JSON that route writes,
// so a chip shows what the route would have shown.
//
// Who a read is for follows the route rules it used to pass through:
//
//   - A read for nobody is this server's own service acting as itself: it
//     sees every owner's records, except in journal-store, which answers only
//     a principal.
//   - A read for a principal, made for a type whose owner decides, checks the
//     principal as an X-Principal-Id beside the service token is checked
//     (callerAssertedByInternalService): one principal-store does not have is
//     an error, a disabled one is refused. An administrator sees every
//     owner's records; anyone else only their own, and someone else's answers
//     as a missing one. A session is reached as GET /sessions/{id} reaches
//     it: the owner's, one the caller started and drives, or one its grants
//     let it observe.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/kayushkin/llm-bridge-server/internal/access/principals"
	schedulerapi "github.com/kayushkin/llm-bridge-server/internal/automation/scheduler/api"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/activitystore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/budgetstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/calendarstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/journalstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/locationstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/messagestore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/peoplestore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/photostore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/possessionstore"
	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/timeledgerstore"
	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/workgraphstore"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/noteboard/items"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/predictions"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/projects"
)

// InProcessReferenceServices are the services, by the name the type rows
// use, whose records reference-store reads in this process. Every other
// service a type names is asked over HTTP at reference_store.service_urls.
var InProcessReferenceServices = []string{
	"llm-bridge-server",
	"agent-store",
	"prediction-store",
	"noteboard",
	"project-store",
	"tool-store",
	"skill-store",
	"principal-store",
	"repo-store",
	"work-graph-store",
	"people-store",
	"journal-store",
	"calendar-store",
	"message-store",
	"activity-store",
	"time-ledger-store",
	"budget-store",
	"scheduler",
	"photo-store",
	"location-store",
	"possession-store",
}

// referenceReadableStores are the stores reference-store's readers call that
// this server otherwise holds only as handlers. Each is set by the Enable
// call that opens it; nil until then, and a read of it is an error naming it.
type referenceReadableStores struct {
	noteboard     atomic.Pointer[items.Store]
	predictions   atomic.Pointer[predictions.Store]
	principals    atomic.Pointer[principals.Store]
	repos         atomic.Pointer[repostore.Store]
	workGraph     atomic.Pointer[workgraphstore.Store]
	scheduler     atomic.Pointer[schedulerapi.Server]
	calendar      atomic.Pointer[calendarstore.Store]
	messages      atomic.Pointer[messagestore.Store]
	activities    atomic.Pointer[activitystore.Store]
	timeLedger    atomic.Pointer[timeledgerstore.Store]
	possessionsAt atomic.Pointer[possessionstore.Server]
}

// EnableSchedulerJobReferences lets reference-store read scheduler jobs by
// their reference ids from routes, the scheduler's routes in this process.
func (s *Server) EnableSchedulerJobReferences(routes *schedulerapi.Server) {
	s.referenceReadable.scheduler.Store(routes)
}

// loaded is a store's pointer, or an error naming the store when it is not
// open on this server.
func loaded[T any](pointer *atomic.Pointer[T], storeName string) (*T, error) {
	value := pointer.Load()
	if value == nil {
		return nil, fmt.Errorf("%s is not open on this server", storeName)
	}
	return value, nil
}

// referenceAnswer is a store's answer as the JSON its route writes, with the
// store's not-found error read as a record that is not there.
func referenceAnswer(value any, err error, notFound error) (json.RawMessage, error) {
	if errors.Is(err, notFound) {
		return nil, referencestore.ErrRecordNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// ownershipCallerOfReferenceAsker is who a connected-records read made for
// reference-store acts as, as the store proxy routes decide it for a request.
// requirePrincipal refuses this server's own service acting as itself, as
// journal-store's route does.
func (s *Server) ownershipCallerOfReferenceAsker(ctx context.Context, asker referencestore.InProcessAsker, storeName string, requirePrincipal bool) (ownership.Caller, error) {
	if asker.PrincipalID == "" {
		if requirePrincipal {
			return ownership.Caller{}, fmt.Errorf("%s answers only its owner's or an administrator's principal, and the read names none", storeName)
		}
		return ownership.Caller{SeesEveryOwner: true}, nil
	}
	caller, err := s.callerOfReferenceAsker(ctx, asker.PrincipalID)
	if err != nil {
		return ownership.Caller{}, err
	}
	return ownership.Caller{PrincipalID: caller.principalID, SeesEveryOwner: caller.isAdministrator}, nil
}

// callerOfReferenceAsker checks the principal a read is for as an
// X-Principal-Id beside the service token is checked. A refusal that route
// answered 403 (a disabled principal) is the owner refusing.
func (s *Server) callerOfReferenceAsker(ctx context.Context, principalID string) (requestCaller, error) {
	caller, credentialError := s.callerAssertedByInternalService(ctx, principalID)
	if credentialError == nil {
		return caller, nil
	}
	if credentialError.status == 403 {
		return requestCaller{}, fmt.Errorf("%w: %s", referencestore.ErrRefusedByOwner, credentialError.message)
	}
	return requestCaller{}, fmt.Errorf("%s: %s", credentialError.code, credentialError.message)
}

// ownedRecordReader is a reader of one connected-records type: read is the
// store's read of one record in scope, with the caller in ctx for a store
// that asks another on the way.
func (s *Server) ownedRecordReader(storeName string, requirePrincipal bool, notFound error, read func(ctx context.Context, scope ownership.Scope, id string) (any, error)) func(context.Context, string, referencestore.InProcessAsker) (json.RawMessage, error) {
	return func(ctx context.Context, id string, asker referencestore.InProcessAsker) (json.RawMessage, error) {
		caller, err := s.ownershipCallerOfReferenceAsker(ctx, asker, storeName, requirePrincipal)
		if err != nil {
			return nil, err
		}
		scope, refusal := ownership.ReadScopeOf(caller)
		if refusal != nil {
			return nil, errors.New(refusal.Message)
		}
		value, err := read(ownership.ContextWithCaller(ctx, caller), scope, id)
		return referenceAnswer(value, err, notFound)
	}
}

// ownedRecordSearch is a connected-records type's search, read as the asker.
func (s *Server) ownedRecordSearch(storeName string, requirePrincipal bool, search func(ctx context.Context, scope ownership.Scope, query string) (any, error)) func(context.Context, string, referencestore.InProcessAsker) (json.RawMessage, error) {
	return func(ctx context.Context, query string, asker referencestore.InProcessAsker) (json.RawMessage, error) {
		caller, err := s.ownershipCallerOfReferenceAsker(ctx, asker, storeName, requirePrincipal)
		if err != nil {
			return nil, err
		}
		scope, refusal := ownership.ReadScopeOf(caller)
		if refusal != nil {
			return nil, errors.New(refusal.Message)
		}
		value, err := search(ownership.ContextWithCaller(ctx, caller), scope, query)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	}
}

// ownedImageReader reads a picture for this server's own service acting as
// itself, which sees every owner's: reference-store has already decided the
// caller may see the record.
func ownedImageReader(read func(ctx context.Context, scope ownership.Scope, id string) ([]byte, error), notFound error) func(context.Context, string) (string, []byte, error) {
	return func(ctx context.Context, id string) (string, []byte, error) {
		ctx = ownership.ContextWithCaller(ctx, ownership.Caller{SeesEveryOwner: true})
		data, err := read(ctx, ownership.EveryOwner(), id)
		if errors.Is(err, notFound) {
			return "", nil, referencestore.ErrNoAttachment
		}
		if err != nil {
			return "", nil, err
		}
		return "image/jpeg", data, nil
	}
}

// InProcessReferenceReaders are reference-store's readers of every type of
// the services in this process.
func (s *Server) InProcessReferenceReaders() referencestore.InProcessServices {
	readers := map[string]referencestore.InProcessReader{
		"session":       {Record: s.readSessionForReference},
		"repo":          {Record: s.readRepoForReference},
		"commit":        {Record: s.readCommitForReference},
		"note":          {Record: s.readNoteForReference, Search: s.searchNotesForReference},
		"prediction":    {Record: s.readPredictionForReference, Search: s.searchPredictionsForReference},
		"project":       {Record: s.readProjectForReference, Search: s.searchProjectsForReference},
		"principal":     {Record: s.readPrincipalForReference, Search: s.searchPrincipalsForReference},
		"scheduler_job": {Record: s.readSchedulerJobForReference},
	}

	people := func() (*peoplestore.Store, error) {
		stores := s.connectedRecords.Load()
		if stores == nil {
			return nil, errConnectedRecordsOff
		}
		return stores.people, nil
	}
	journal := func() (*journalstore.Store, error) {
		stores := s.connectedRecords.Load()
		if stores == nil {
			return nil, errors.New("journal-store is not open on this server")
		}
		return stores.journal, nil
	}
	readers["person"] = referencestore.InProcessReader{
		Record: s.ownedRecordReader("people-store", false, peoplestore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
			store, err := people()
			if err != nil {
				return nil, err
			}
			return store.GetPerson(ctx, scope, id)
		}),
		Search: s.ownedRecordSearch("people-store", false, func(ctx context.Context, scope ownership.Scope, query string) (any, error) {
			store, err := people()
			if err != nil {
				return nil, err
			}
			return store.ListPersons(ctx, scope, peoplestore.PersonFilter{Query: query})
		}),
	}
	readers["entry"] = referencestore.InProcessReader{
		Record: s.ownedRecordReader("journal-store", true, journalstore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
			store, err := journal()
			if err != nil {
				return nil, err
			}
			return store.Get(ctx, scope, id, false)
		}),
		Search: s.ownedRecordSearch("journal-store", true, func(ctx context.Context, scope ownership.Scope, query string) (any, error) {
			store, err := journal()
			if err != nil {
				return nil, err
			}
			return store.List(ctx, scope, journalstore.Filter{Query: query})
		}),
	}
	readers["calendar_event"] = referencestore.InProcessReader{
		Record: s.ownedRecordReader("calendar-store", false, calendarstore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
			store, err := loaded(&s.referenceReadable.calendar, "calendar-store")
			if err != nil {
				return nil, err
			}
			return store.GetEvent(ctx, scope, id)
		}),
	}

	photos := func() (*photosAndLocationsStores, error) {
		stores := s.photosAndLocations.Load()
		if stores == nil {
			return nil, errors.New("photo-store and location-store are not open on this server")
		}
		return stores, nil
	}
	readers["photo"] = referencestore.InProcessReader{
		Record: s.ownedRecordReader("photo-store", false, photostore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
			stores, err := photos()
			if err != nil {
				return nil, err
			}
			return stores.photos.Get(ctx, scope, id, false)
		}),
		Attachment: ownedImageReader(func(ctx context.Context, scope ownership.Scope, id string) ([]byte, error) {
			stores, err := photos()
			if err != nil {
				return nil, err
			}
			content, err := stores.photos.OpenThumbnail(ctx, scope, id)
			if err != nil {
				return nil, err
			}
			defer content.Close()
			return io.ReadAll(io.LimitReader(content, referencestore.MaximumAttachmentBytes+1))
		}, photostore.ErrNotFound),
	}
	readers["face_group"] = referencestore.InProcessReader{
		Record: s.ownedRecordReader("photo-store", false, photostore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
			stores, err := photos()
			if err != nil {
				return nil, err
			}
			return stores.photos.FaceGroupByID(ctx, scope, id, true)
		}),
	}
	locationReader := func(read func(store *locationstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error)) referencestore.InProcessReader {
		return referencestore.InProcessReader{
			Record: s.ownedRecordReader("location-store", false, locationstore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
				stores, err := photos()
				if err != nil {
					return nil, err
				}
				return read(stores.locations, ctx, scope, id)
			}),
		}
	}
	readers["place"] = locationReader(func(store *locationstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.GetPlace(ctx, scope, id, false)
	})
	readers["visit"] = locationReader(func(store *locationstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.GetVisitWithCalendarEventLinks(ctx, scope, id, false)
	})
	readers["trip"] = locationReader(func(store *locationstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.GetTripWithCalendarEventLinks(ctx, scope, id, false)
	})

	messageReader := func(read func(store *messagestore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error)) referencestore.InProcessReader {
		return referencestore.InProcessReader{
			Record: s.ownedRecordReader("message-store", false, messagestore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
				store, err := loaded(&s.referenceReadable.messages, "message-store")
				if err != nil {
					return nil, err
				}
				return read(store, ctx, scope, id)
			}),
		}
	}
	// A retired message or a merged conversation answers its route with a
	// redirect to the record that replaced it, which reference-store never
	// followed: it is an error here as it was there.
	readers["conversation"] = messageReader(func(store *messagestore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.GetConversation(ctx, scope, id)
	})
	readers["chat_message"] = messageReader(func(store *messagestore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.GetMessage(ctx, scope, id)
	})
	readers["conversation_day"] = messageReader(func(store *messagestore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.GetConversationDay(ctx, scope, id)
	})
	readers["activity"] = referencestore.InProcessReader{
		Record: s.ownedRecordReader("activity-store", false, activitystore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
			store, err := loaded(&s.referenceReadable.activities, "activity-store")
			if err != nil {
				return nil, err
			}
			return store.GetActivity(ctx, scope, id, false)
		}),
	}
	timeLedgerReader := func(read func(store *timeledgerstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error)) referencestore.InProcessReader {
		return referencestore.InProcessReader{
			Record: s.ownedRecordReader("time-ledger-store", false, timeledgerstore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
				store, err := loaded(&s.referenceReadable.timeLedger, "time-ledger-store")
				if err != nil {
					return nil, err
				}
				return read(store, ctx, scope, id)
			}),
		}
	}
	readers["time_entry"] = timeLedgerReader(func(store *timeledgerstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.Entry(ctx, scope, id, true)
	})
	readers["time_entry_kind"] = timeLedgerReader(func(store *timeledgerstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.Kind(ctx, scope, id)
	})
	readers["time_entry_review"] = timeLedgerReader(func(store *timeledgerstore.Store, ctx context.Context, scope ownership.Scope, id string) (any, error) {
		return store.Review(ctx, scope, id)
	})

	// budget-store's and possession-store's types are each read by the path
	// their row's Get route names: /accounts/{id} is budget-store's
	// accounts, /garments/{id} possession-store's garments.
	for _, row := range referencestore.Types {
		route := firstPathSegment(row.Get)
		switch row.Service {
		case "budget-store":
			kind := budgetstore.RecordKind(route)
			readers[row.Type] = referencestore.InProcessReader{
				Record: s.ownedRecordReader("budget-store", false, budgetstore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
					module := s.budgetStore.Load()
					if module == nil {
						return nil, errors.New("budget-store is not open on this server")
					}
					return module.store.RecordInScope(ctx, scope, kind, id)
				}),
			}
		case "possession-store":
			readers[row.Type] = referencestore.InProcessReader{
				Record: s.ownedRecordReader("possession-store", false, possessionstore.ErrNotFound, func(ctx context.Context, scope ownership.Scope, id string) (any, error) {
					server, err := loaded(&s.referenceReadable.possessionsAt, "possession-store")
					if err != nil {
						return nil, err
					}
					return server.RecordInScope(ctx, scope, route, id)
				}),
				Attachment: ownedImageReader(func(ctx context.Context, scope ownership.Scope, id string) ([]byte, error) {
					server, err := loaded(&s.referenceReadable.possessionsAt, "possession-store")
					if err != nil {
						return nil, err
					}
					return server.ImageInScope(ctx, scope, route, id)
				}, possessionstore.ErrNotFound),
			}
		}
	}
	return referencestore.InProcessServices{Services: InProcessReferenceServices, Readers: readers}
}

// readSessionForReference is GET /sessions/{id}: the session record, for the
// internal service, or for a principal who reaches it as that route lets
// one.
func (s *Server) readSessionForReference(ctx context.Context, id string, asker referencestore.InProcessAsker) (json.RawMessage, error) {
	if asker.PrincipalID != "" {
		caller, err := s.callerOfReferenceAsker(ctx, asker.PrincipalID)
		if err != nil {
			return nil, err
		}
		if !caller.isAdministrator {
			reached, err := s.sessionReachedByCaller(ctx, caller, id, true)
			if err != nil {
				return nil, fmt.Errorf("grant_store_unavailable: %w", err)
			}
			if !reached {
				return nil, referencestore.ErrRecordNotFound
			}
		}
	}
	session, err := s.store.GetSession(id)
	if err != nil {
		// The route answers every failed read as a missing session.
		return nil, referencestore.ErrRecordNotFound
	}
	return json.Marshal(session)
}

func (s *Server) readRepoForReference(_ context.Context, mention string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	repos, err := loaded(&s.referenceReadable.repos, "repo-store")
	if err != nil {
		return nil, err
	}
	name, ok := repostore.RepoNameOfMention(mention)
	if !ok {
		return nil, fmt.Errorf("a repo mention is %s<name>, not %q", repostore.RepoMentionPrefix, mention)
	}
	repo, err := repos.GetRepoByName(name)
	return referenceAnswer(repo, err, repostore.ErrNotFound)
}

func (s *Server) readCommitForReference(ctx context.Context, mention string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	repos, err := loaded(&s.referenceReadable.repos, "repo-store")
	if err != nil {
		return nil, err
	}
	workGraph, err := loaded(&s.referenceReadable.workGraph, "work-graph-store")
	if err != nil {
		return nil, err
	}
	answer, err := workgraphstore.ResolveCommitMention(ctx, workGraph, workgraphstore.RepoStoreModule{Store: repos}, mention)
	return referenceAnswer(answer, err, workgraphstore.ErrNotFound)
}

func (s *Server) readNoteForReference(_ context.Context, id string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	notes, err := loaded(&s.referenceReadable.noteboard, "noteboard")
	if err != nil {
		return nil, err
	}
	item, err := notes.GetItem(id)
	return referenceAnswer(item, err, items.ErrItemNotFound)
}

func (s *Server) searchNotesForReference(_ context.Context, query string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	notes, err := loaded(&s.referenceReadable.noteboard, "noteboard")
	if err != nil {
		return nil, err
	}
	found, err := notes.Search(items.SearchParams{Query: query})
	if err != nil {
		return nil, err
	}
	return json.Marshal(found)
}

func (s *Server) readPredictionForReference(ctx context.Context, id string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	predictionStore, err := loaded(&s.referenceReadable.predictions, "prediction-store")
	if err != nil {
		return nil, err
	}
	prediction, err := predictionStore.Get(ctx, id)
	return referenceAnswer(prediction, err, predictions.ErrNotFound)
}

func (s *Server) searchPredictionsForReference(ctx context.Context, query string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	predictionStore, err := loaded(&s.referenceReadable.predictions, "prediction-store")
	if err != nil {
		return nil, err
	}
	list, err := predictionStore.ListWithTotal(ctx, predictions.Filter{Query: query})
	if err != nil {
		return nil, err
	}
	return json.Marshal(list)
}

func (s *Server) projectStoreForReference() (*projects.Store, error) {
	handlers := s.projectStore.Load()
	if handlers == nil {
		return nil, errors.New("project-store is not open on this server")
	}
	return handlers.Store, nil
}

func (s *Server) readProjectForReference(_ context.Context, id string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	projectStore, err := s.projectStoreForReference()
	if err != nil {
		return nil, err
	}
	project, err := projectStore.Project(id)
	return referenceAnswer(project, err, projects.ErrNotFound)
}

func (s *Server) searchProjectsForReference(_ context.Context, query string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	projectStore, err := s.projectStoreForReference()
	if err != nil {
		return nil, err
	}
	list, err := projectStore.ProjectList(projects.ProjectFilter{Query: query})
	if err != nil {
		return nil, err
	}
	return json.Marshal(list)
}

func (s *Server) readPrincipalForReference(ctx context.Context, id string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	directory, err := loaded(&s.referenceReadable.principals, "principal-store")
	if err != nil {
		return nil, err
	}
	principal, err := directory.Get(ctx, id, false)
	return referenceAnswer(principal, err, principals.ErrNotFound)
}

func (s *Server) searchPrincipalsForReference(ctx context.Context, query string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	directory, err := loaded(&s.referenceReadable.principals, "principal-store")
	if err != nil {
		return nil, err
	}
	found, err := directory.List(ctx, principals.Filter{Query: query})
	if err != nil {
		return nil, err
	}
	return json.Marshal(found)
}

func (s *Server) readSchedulerJobForReference(_ context.Context, reference string, _ referencestore.InProcessAsker) (json.RawMessage, error) {
	scheduler, err := loaded(&s.referenceReadable.scheduler, "the scheduler")
	if err != nil {
		return nil, err
	}
	job, err := scheduler.JobReferenceOf(reference)
	return referenceAnswer(job, err, schedulerapi.ErrJobNotFound)
}

// firstPathSegment is "accounts" for "/accounts/{id}".
func firstPathSegment(path string) string {
	segment, _, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	return segment
}
