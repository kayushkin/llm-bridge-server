package server

import (
	"net/http"

	"github.com/kayushkin/llm-bridge-server/internal/httpjson"

	"github.com/kayushkin/llm-bridge-server/internal/config"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items"
	itemsapi "github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/api"
)

// The operator's notes, ranked list entries and workspaces (the notes module,
// noteboard until its todos became issues), under /notes/, and the content of
// issues, the same routes under /issues/items. Both answer from their
// Postgres modules through the items package, so a note and an issue take
// the same calls, holds, revisions, schedules and search.

// EnableNotes serves notes' routes from store. main calls it once the module
// is provisioned, migrated and open; until then, and on a server with no
// Postgres URL, the routes answer 503.
func (s *Server) EnableNotes(store *items.Store) {
	s.notes.Store(itemsapi.New(store))
	s.referenceReadable.notes.Store(store)
}

// EnableIssueItems serves /issues/items, the content of issues, from store,
// the issues module's items. EnableIssues calls it with the boards; a test
// that needs issues and no boards calls it alone. It answers the API it
// serves, which the boards call in Go.
func (s *Server) EnableIssueItems(store *items.Store) *itemsapi.API {
	issueItems := itemsapi.New(store)
	s.issueItems.Store(issueItems)
	s.referenceReadable.issueItems.Store(store)
	return issueItems
}

// itemsRoute answers one route with the handler of the store in api, or 503
// naming the module while it is not open.
func itemsRoute(api interface{ Load() *itemsapi.API }, offCode, module string, route func(*itemsapi.API, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handlers := api.Load()
		if handlers == nil {
			httpjson.WriteJSONError(w, http.StatusServiceUnavailable, offCode,
				config.PostgresURLEnvironmentVariable+" is not set, or the "+module+" module is not open, so this server keeps no "+module)
			return
		}
		route(handlers, w, r)
	}
}

func (s *Server) notesRoute(route func(*itemsapi.API, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return itemsRoute(&s.notes, "notes_off", "notes", route)
}

func (s *Server) issueItemsRoute(route func(*itemsapi.API, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return itemsRoute(&s.issueItems, "issues_off", "issues", route)
}

// registerNotesRoutes mounts notes. The patterns are written out, because the
// route classification test reads them from string literals.
func (s *Server) registerNotesRoutes() {
	s.mux.HandleFunc("GET /notes/health", s.notesRoute((*itemsapi.API).Health))
	s.mux.HandleFunc("GET /notes/items", s.notesRoute((*itemsapi.API).ListItems))
	s.mux.HandleFunc("POST /notes/items", s.notesRoute((*itemsapi.API).CreateItem))
	s.mux.HandleFunc("POST /notes/items/rerank", s.notesRoute((*itemsapi.API).Rerank))
	s.mux.HandleFunc("POST /notes/items/query", s.notesRoute((*itemsapi.API).QueryItems))
	s.mux.HandleFunc("POST /notes/items/batch", s.notesRoute((*itemsapi.API).BatchItems))
	s.mux.HandleFunc("GET /notes/items/query-options", s.notesRoute((*itemsapi.API).ItemsQueryOptions))
	s.mux.HandleFunc("GET /notes/items/{id}", s.notesRoute((*itemsapi.API).GetItem))
	s.mux.HandleFunc("PATCH /notes/items/{id}", s.notesRoute((*itemsapi.API).PatchItem))
	s.mux.HandleFunc("DELETE /notes/items/{id}", s.notesRoute((*itemsapi.API).DeleteItem))
	s.mux.HandleFunc("GET /notes/items/{id}/revisions", s.notesRoute((*itemsapi.API).ListRevisions))
	s.mux.HandleFunc("POST /notes/items/{id}/restore", s.notesRoute((*itemsapi.API).RestoreItem))
	s.mux.HandleFunc("POST /notes/items/{id}/hold", s.notesRoute((*itemsapi.API).HoldItem))
	s.mux.HandleFunc("POST /notes/items/{id}/unhold", s.notesRoute((*itemsapi.API).UnholdItem))
	s.mux.HandleFunc("GET /notes/items/{id}/occurrences", s.notesRoute((*itemsapi.API).Occurrences))
	s.mux.HandleFunc("GET /notes/lists", s.notesRoute((*itemsapi.API).Lists))
	s.mux.HandleFunc("GET /notes/tags", s.notesRoute((*itemsapi.API).Tags))
	s.mux.HandleFunc("GET /notes/search", s.notesRoute((*itemsapi.API).Search))
}

// registerIssueItemsRoutes mounts the content of issues under /issues/items:
// every issue, on a board or not, as the operator's. The rest of /issues/ is
// the boards and cards (kanban_store_routes.go), which answer each principal
// only the cards on boards it may view. Ranked lists are notes, so there is
// no /issues/items/lists; an issue's tags and search sit under /items because
// /issues/tags and /issues/search are the boards' own.
func (s *Server) registerIssueItemsRoutes() {
	s.mux.HandleFunc("GET /issues/items", s.issueItemsRoute((*itemsapi.API).ListItems))
	s.mux.HandleFunc("POST /issues/items", s.issueItemsRoute((*itemsapi.API).CreateItem))
	s.mux.HandleFunc("POST /issues/items/rerank", s.issueItemsRoute((*itemsapi.API).Rerank))
	s.mux.HandleFunc("POST /issues/items/query", s.issueItemsRoute((*itemsapi.API).QueryItems))
	s.mux.HandleFunc("POST /issues/items/batch", s.issueItemsRoute((*itemsapi.API).BatchItems))
	s.mux.HandleFunc("GET /issues/items/query-options", s.issueItemsRoute((*itemsapi.API).ItemsQueryOptions))
	s.mux.HandleFunc("GET /issues/items/health", s.issueItemsRoute((*itemsapi.API).Health))
	s.mux.HandleFunc("GET /issues/items/tags", s.issueItemsRoute((*itemsapi.API).Tags))
	s.mux.HandleFunc("GET /issues/items/search", s.issueItemsRoute((*itemsapi.API).Search))
	s.mux.HandleFunc("GET /issues/items/{id}", s.issueItemsRoute((*itemsapi.API).GetItem))
	s.mux.HandleFunc("PATCH /issues/items/{id}", s.issueItemsRoute((*itemsapi.API).PatchItem))
	s.mux.HandleFunc("DELETE /issues/items/{id}", s.issueItemsRoute((*itemsapi.API).DeleteItem))
	s.mux.HandleFunc("GET /issues/items/{id}/revisions", s.issueItemsRoute((*itemsapi.API).ListRevisions))
	s.mux.HandleFunc("POST /issues/items/{id}/restore", s.issueItemsRoute((*itemsapi.API).RestoreItem))
	s.mux.HandleFunc("POST /issues/items/{id}/hold", s.issueItemsRoute((*itemsapi.API).HoldItem))
	s.mux.HandleFunc("POST /issues/items/{id}/unhold", s.issueItemsRoute((*itemsapi.API).UnholdItem))
	s.mux.HandleFunc("GET /issues/items/{id}/occurrences", s.issueItemsRoute((*itemsapi.API).Occurrences))
}
