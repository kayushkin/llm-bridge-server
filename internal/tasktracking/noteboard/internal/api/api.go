package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/kayushkin/llm-bridge/servicesettings"
	"github.com/kayushkin/noteboard/internal/boundedtext"
	"github.com/kayushkin/noteboard/internal/db"
	"github.com/kayushkin/noteboard/model"
)

type API struct {
	store    *db.Store
	settings *servicesettings.Registry
}

// New builds the API over store. settings is what GET /settings describes; a
// nil one panics here, at boot, rather than at the first read of the route.
func New(store *db.Store, settings *servicesettings.Registry) *API {
	if settings == nil {
		panic("noteboard: api.New needs the settings registry that GET /settings serves")
	}
	return &API{store: store, settings: settings}
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", a.health)
	mux.HandleFunc("/api/items/rerank", a.rerank)
	mux.HandleFunc("/api/items/query", a.queryItems)
	mux.HandleFunc("/api/items/batch", a.batchItems)
	mux.HandleFunc("/api/items/query-options", a.itemsQueryOptions)
	mux.HandleFunc("/api/items/", a.itemByID)
	mux.HandleFunc("/api/items", a.items)
	mux.HandleFunc("/api/lists", a.lists)
	mux.HandleFunc("/api/tags", a.tags)
	mux.HandleFunc("/api/search", a.search)
	// Read-only, and as open as every other route. PUT /settings/{key} is not
	// mounted: no setting is Editable, so there is nothing a write could change.
	mux.Handle("GET /settings", servicesettings.Handler(a.settings, "/settings"))
	return cors(mux)
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// positiveIntParam reads an optional non-negative integer query parameter.
// Absent means 0, which every caller of this reads as "unset". Anything that is
// present but not a non-negative integer is the caller's mistake and is returned
// as an error rather than quietly becoming 0 — the two mean opposite things on a
// limit, and the quiet one is unbounded.
func positiveIntParam(r *http.Request, name string) (int, error) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", name, raw)
	}
	if v < 0 {
		return 0, fmt.Errorf("%s must not be negative, got %d", name, v)
	}
	return v, nil
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	// Report a broken database as unhealthy. Swallowing this error answered
	// {"status":"ok","items":0} when the store could not be read at all, which
	// is the one answer a health check must never give.
	count, err := a.store.ItemCount()
	if err != nil {
		writeJSON(w, 500, map[string]interface{}{"status": "error", "error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"status": "ok", "items": count})
}

func (a *API) items(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		items, err := a.store.ListItems(db.ListParams{
			Type:           q.Get("type"),
			Tag:            q.Get("tag"),
			ExcludeTags:    q["exclude_tag"],
			Status:         q.Get("status"),
			ListID:         q.Get("list_id"),
			CreatedBy:      q.Get("created_by"),
			ParentID:       q.Get("parent_id"),
			IncludeDeleted: q.Get("include_deleted") == "true",
			IncludeHeld:    q.Get("include_held") == "true",
			Limit:          limit,
			Offset:         offset,
			Sort:           q.Get("sort"),
		})
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, items)
	case "POST":
		var req model.CreateItemRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		item, err := a.store.CreateItem(&req)
		if err != nil {
			// A parent_id naming no item is the caller's mistake, not the
			// store's failure, and it has to read as one: a 500 says "try
			// again" about a request that will never succeed as written, and
			// the caller keeps the id it got wrong.
			if errors.Is(err, db.ErrUnknownParent) {
				writeError(w, 400, err.Error())
				return
			}
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 201, item)
	default:
		writeError(w, 405, "method not allowed")
	}
}

// itemChangedResponse is the body of a 412 on PATCH /api/items/{id}.
type itemChangedResponse struct {
	Error   string      `json:"error"`
	Current *model.Item `json:"current"`
}

// itemVersionTag is an item's version as an ETag: its updated_at, exactly as
// the item's JSON carries it, in quotes. A caller that has the item already
// has the version and need not have kept the header.
func itemVersionTag(item *model.Item) string {
	return `"` + item.UpdatedAt.Format(time.RFC3339Nano) + `"`
}

// expectedUpdatedAtFromIfMatch reads the version a PATCH was made against. An
// absent header, or "*", asks for no check. Anything else must be one
// updated_at, quoted or bare; a value that cannot be read is refused rather
// than ignored, because ignoring it would apply an update its sender believed
// was conditional.
func expectedUpdatedAtFromIfMatch(header string) (*time.Time, error) {
	value := strings.TrimSpace(header)
	if value == "" || value == "*" {
		return nil, nil
	}
	value = strings.Trim(value, `"`)
	expected, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("If-Match must be the item's updated_at exactly as the item carries it (for example \"2026-09-18T18:59:29.430695045Z\"), got %q", header)
	}
	return &expected, nil
}

// itemsQueryOptions serves what a caller of POST /api/items/query must not
// hardcode.
func (a *API) itemsQueryOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method not allowed")
		return
	}
	writeJSON(w, 200, model.ItemsQueryOptions{Sorts: model.ItemsQuerySorts, MaxIDs: model.MaxItemsQueryIDs, MaxLimit: model.MaxItemsQueryLimit})
}

// queryItems is POST /api/items/query: of these ids, which match, in what
// order. It is a POST because the ids are the question and a board's worth of
// them does not fit in a URL. It changes nothing.
func (a *API) queryItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	var query model.ItemsQuery
	decoder := json.NewDecoder(r.Body)
	// A misspelled filter that was dropped would answer every item, which reads
	// as "all of these match".
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&query); err != nil {
		writeError(w, 400, "invalid JSON: "+err.Error())
		return
	}
	if err := query.Validate(); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	result, err := a.store.QueryItems(&query)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, result)
}

func (a *API) itemByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/items/")
	if id == "" {
		writeError(w, 400, "missing id")
		return
	}

	// Sub-resources: /api/items/{id}/revisions, /api/items/{id}/restore.
	if base, action, found := strings.Cut(id, "/"); found {
		a.itemAction(w, r, base, action)
		return
	}

	switch r.Method {
	case "GET":
		// A deleted item is not found unless asked for by name — the caller has
		// to say it wants a tombstone.
		var item *model.Item
		var err error
		if r.URL.Query().Get("include_deleted") == "true" {
			item, err = a.store.GetItemIncludingDeleted(id)
		} else {
			item, err = a.store.GetItem(id)
		}
		if err != nil {
			writeError(w, 404, "not found")
			return
		}
		w.Header().Set("ETag", itemVersionTag(item))
		writeJSON(w, 200, item)
	case "PATCH":
		var req model.UpdateItemRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, 400, "invalid JSON")
			return
		}
		if err := req.Validate(); err != nil {
			writeError(w, 400, err.Error())
			return
		}
		expectedUpdatedAt, err := expectedUpdatedAtFromIfMatch(r.Header.Get("If-Match"))
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		req.ExpectedUpdatedAt = expectedUpdatedAt
		item, err := a.store.UpdateItem(id, &req)
		var changed *db.ItemChangedError
		if errors.As(err, &changed) {
			// 412 carries the item as it is now, so the caller can show what
			// the other writer did instead of fetching it again.
			w.Header().Set("ETag", itemVersionTag(changed.Current))
			writeJSON(w, http.StatusPreconditionFailed, itemChangedResponse{Error: changed.Error(), Current: changed.Current})
			return
		}
		if err != nil {
			// A rejected schedule is a bad request, not a missing item. Collapsing
			// every store error to 404 would report "not found" for an item that is
			// plainly there, and hide the one message that says what is wrong with
			// the rule.
			if _, missing := a.store.GetItem(id); missing != nil {
				writeError(w, 404, "not found")
				return
			}
			writeError(w, 400, err.Error())
			return
		}
		w.Header().Set("ETag", itemVersionTag(item))
		writeJSON(w, 200, item)
	case "DELETE":
		// Default is reversible: deleted_at is stamped and the row stays.
		// ?hard=true purges, and even that snapshots into item_revisions first.
		hard := r.URL.Query().Get("hard") == "true"
		if err := a.store.DeleteItem(id, hard); err != nil {
			// An id that names no row is the caller's mistake, and it reads as
			// one here for the same reason it does on create: a 500 says "try
			// again" about a request that will never succeed as written. Every
			// other missing-item route on this service already answers 404, so
			// delete reporting 500 also made the store look like it fails on
			// one verb and not the others. Anything that is not a missing row
			// is still a server fault and still says so.
			if errors.Is(err, db.ErrItemNotFound) {
				writeError(w, 404, "not found")
				return
			}
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	default:
		writeError(w, 405, "method not allowed")
	}
}

// itemAction serves the item sub-resources: the change log, the undo, and the
// recurrence preview.
func (a *API) itemAction(w http.ResponseWriter, r *http.Request, id, action string) {
	switch {
	case action == "revisions" && r.Method == "GET":
		// Unparseable and negative values are refused rather than defaulted.
		// Every revision carries the whole body it replaced, so "no limit" here
		// is the most expensive answer this service gives — 246 MB for the
		// nightly signpost todo, measured — and it is the one a silently
		// swallowed ?limit=abc would hand back. A caller that asked for a page
		// and got the entire history instead has no way to tell.
		limit, err := positiveIntParam(r, "limit")
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		offset, err := positiveIntParam(r, "offset")
		if err != nil {
			writeError(w, 400, err.Error())
			return
		}
		revisions, err := a.store.ListRevisions(id, limit, offset)
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, revisions)
	case action == "restore" && r.Method == "POST":
		item, err := a.store.RestoreItem(id)
		if err != nil {
			writeError(w, 404, "not found")
			return
		}
		writeJSON(w, 200, item)
	case action == "hold" && r.Method == "POST":
		// Body is optional: holding without a stated reason is allowed, because a
		// gate that is annoying to close is a gate that stays open.
		var req struct {
			Reason string `json:"reason"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		item, err := a.store.HoldItem(id, req.Reason)
		if err != nil {
			writeError(w, 404, "not found")
			return
		}
		writeJSON(w, 200, item)
	case action == "unhold" && r.Method == "POST":
		item, err := a.store.UnholdItem(id)
		if err != nil {
			writeError(w, 404, "not found")
			return
		}
		writeJSON(w, 200, item)
	case action == "occurrences" && r.Method == "GET":
		a.occurrences(w, r, id)
	default:
		writeError(w, 404, "no such item action: "+boundedtext.Value(action))
	}
}

// occurrences expands an item's rule and answers the only question that matters
// when you write one: "so when does this actually fire?"
//
// A recurrence rule is uniquely bad at failing visibly — BYDAY=3TU and
// BYDAY=TU;BYSETPOS=3 both look plausible, and a wrong one does not error, it
// just quietly reminds you on the wrong Tuesday, which you find out about a month
// later. This endpoint makes the rule's behavior checkable at the moment you
// write it rather than at the moment it fails.
//
// Both due occurrences and nag firings are returned, because they are separately
// wrong in different ways.
func (a *API) occurrences(w http.ResponseWriter, r *http.Request, id string) {
	item, err := a.store.GetItem(id)
	if err != nil {
		writeError(w, 404, "not found")
		return
	}
	if item.Schedule == nil {
		writeError(w, 400, "item has no schedule")
		return
	}

	q := r.URL.Query()
	from := time.Now()
	if v := q.Get("from"); v != "" {
		from, err = time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, 400, "from must be RFC3339: "+boundedtext.ErrorMessage(err))
			return
		}
	}
	to := from.AddDate(1, 0, 0)
	if v := q.Get("to"); v != "" {
		to, err = time.Parse(time.RFC3339, v)
		if err != nil {
			writeError(w, 400, "to must be RFC3339: "+boundedtext.ErrorMessage(err))
			return
		}
	}

	due, err := item.Schedule.DueOccurrences(item.DueAt, from, to)
	if err != nil {
		writeError(w, 400, "expand due rule: "+err.Error())
		return
	}
	nag, err := item.Schedule.NagOccurrences(item.DueAt, from, to)
	if err != nil {
		writeError(w, 400, "expand nag rule: "+err.Error())
		return
	}

	// Capped, and the cap is REPORTED. A silently truncated preview is worse than
	// no preview: it reads as "this is when it fires" while being a lie of
	// omission, and a cadence bug hiding past item 500 would never be seen.
	const maxOccurrences = 500
	truncated := false
	if len(due) > maxOccurrences {
		due, truncated = due[:maxOccurrences], true
	}
	if len(nag) > maxOccurrences {
		nag, truncated = nag[:maxOccurrences], true
	}

	writeJSON(w, 200, map[string]any{
		"item_id":   item.ID,
		"tzid":      item.Schedule.TZID,
		"mode":      item.Schedule.EffectiveMode(),
		"from":      from.Format(time.RFC3339),
		"to":        to.Format(time.RFC3339),
		"due":       formatTimes(due),
		"nag":       formatTimes(nag),
		"truncated": truncated,
	})
}

// formatTimes renders occurrences in the rule's own zone, so a reader checking
// "is that 9am local?" can see the answer instead of doing offset arithmetic on
// a UTC timestamp.
func formatTimes(times []time.Time) []string {
	out := make([]string, 0, len(times))
	for _, t := range times {
		out = append(out, t.Format(time.RFC3339))
	}
	return out
}

func (a *API) rerank(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		writeError(w, 405, "method not allowed")
		return
	}
	var req model.RerankRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, 400, "invalid JSON")
		return
	}
	if err := a.store.Rerank(req.Items); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (a *API) lists(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		lists, err := a.store.ListLists()
		if err != nil {
			writeError(w, 500, err.Error())
			return
		}
		writeJSON(w, 200, lists)
	// There is deliberately no POST. A list is not a stored row — ListLists
	// derives the set by grouping items on list_id, so a list exists exactly
	// when an item claims it and cannot be created ahead of one. The POST that
	// used to sit here answered 201 "created" and wrote nothing, so a caller was
	// told its list existed and then could not find it in GET /api/lists. Assign
	// list_id on an item instead.
	default:
		writeError(w, 405, "method not allowed")
	}
}

func (a *API) tags(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	tags, err := a.store.ListTags()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, tags)
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		writeError(w, 405, "method not allowed")
		return
	}
	q := r.URL.Query()
	query := q.Get("q")
	if query == "" {
		writeError(w, 400, "q parameter is required")
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	items, err := a.store.Search(db.SearchParams{
		Query:       query,
		Type:        q.Get("type"),
		Tag:         q.Get("tag"),
		Status:      q.Get("status"),
		Limit:       limit,
		IncludeHeld: q.Get("include_held") == "true",
	})
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, items)
}
