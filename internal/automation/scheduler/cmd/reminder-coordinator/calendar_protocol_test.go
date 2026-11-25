package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kayushkin/noteboard/model"
)

// The double in calendar_path_test.go answers 200 to any method on any path. That
// is right for what it pins — escaping is a property of the request line and needs
// no opinion from the server — but it means the mirror's ENTIRE error surface has
// never been executed by a test. This file is that surface, and every status and
// body below was measured against the running dash-server on :8101 and read out of
// dash/server/api_calendar.go on 2026-09-07, not guessed:
//
//	GET    /api/calendar/google      no Authorization      401 {"error":"unauthorized"}
//	PATCH  /api/calendar/google      valid Authorization   405 method not allowed   (PLAIN TEXT, not JSON)
//	DELETE /api/calendar/google/{id} valid Authorization   503 {"error":"auth-store error (404): ..."}
//	GET    /api/calendar/google      valid Authorization   503 {"error":"...","events":[]}
//
// The last two are what this box answers TODAY: auth-store holds no `google`
// credential, so dash cannot reach Google at all. That is a real outage and it is
// filed separately — what matters here is the SHAPE, because dash quotes
// auth-store's own status into its message, and that message is what the mirror
// used to classify on.

// dashCalendarStub routes through a real http.ServeMux built from dash's own
// route table, so a wrong verb and an unrouted path are refused HERE for the
// reason they are refused THERE. A stand-in that answers everything cannot notice
// a request aimed at the wrong place, which is the whole failure this file exists
// to close.
type dashCalendarStub struct {
	// eventStatus and eventBody are what the single-event routes answer.
	eventStatus int
	eventBody   string
	// requireBearer mirrors dash's auth middleware, which runs BEFORE the method
	// check — measured: an unauthenticated PATCH is 401, not 405.
	requireBearer string
	// lastMethod and lastBody record the last request that got past the auth check.
	lastMethod string
	lastBody   map[string]any
}

func (d *dashCalendarStub) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	authed := func(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if d.requireBearer != "" && r.Header.Get("Authorization") != "Bearer "+d.requireBearer {
				w.WriteHeader(http.StatusUnauthorized)
				w.Write([]byte(`{"error":"unauthorized"}`))
				return
			}
			d.lastMethod = r.Method
			d.lastBody = map[string]any{}
			json.NewDecoder(r.Body).Decode(&d.lastBody)
			next(w, r)
		}
	}

	mux.HandleFunc("/api/calendar/google", authed(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			w.Write([]byte(`{"events":[],"calendars":[]}`))
		case http.MethodPost:
			w.Write([]byte(`{"id":"6f8k2m4p1q9r3s5t7u0v2w4x6y"}`))
		default:
			// dash writes this with http.Error, so it is text/plain and not JSON.
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	mux.HandleFunc("/api/calendar/google/", authed(func(w http.ResponseWriter, r *http.Request) {
		if strings.TrimPrefix(r.URL.Path, "/api/calendar/google/") == "" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"missing event ID"}`))
			return
		}
		switch r.Method {
		case http.MethodGet, http.MethodPut, http.MethodPatch, http.MethodDelete:
			status, body := d.eventStatus, d.eventBody
			if status == 0 {
				status, body = http.StatusOK, `{"status":"deleted"}`
			}
			w.WriteHeader(status)
			w.Write([]byte(body))
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func probeMirrorItem() *model.Item {
	due := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
	return &model.Item{ID: "item-1", Title: "Water the plants", DueAt: &due}
}

// ---------------------------------------------------------------- the repair

// An ordinary Google event id is base32hex — the alphabet is a-v and 0-9 — so an
// id spelling "404" or "410" somewhere inside it is an ordinary id, not a
// contrived one. Classifying an HTTP failure by looking for those digits in a
// message that also carries the id makes every failure on THAT event read as
// "already gone".
const (
	idContaining404 = "6f8k404m2p1q9r3s5t"
	idContaining410 = "6f8k410m2p1q9r3s5t"
	ordinaryID      = "6f8k2m4p1q9r3s5t7u"
)

func TestDeleteEventReportsAServerErrorEvenWhenTheEventIDSpells404(t *testing.T) {
	for _, eventID := range []string{idContaining404, idContaining410, ordinaryID} {
		t.Run(eventID, func(t *testing.T) {
			stub := &dashCalendarStub{
				eventStatus: http.StatusInternalServerError,
				eventBody:   `{"error":"google is unreachable"}`,
			}
			mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

			err := mirror.DeleteEvent("token", eventID)
			if err == nil {
				t.Fatalf("a 500 was reported as a successful delete: the event is still on the "+
					"calendar and the coordinator has dropped its link. event id %q", eventID)
			}
			if !strings.Contains(err.Error(), "500") {
				t.Errorf("the status did not reach the message: %v", err)
			}
		})
	}
}

// dash quotes the far service's own status INTO its message. This is what :8101
// answers on this box today, and under a substring match the "(404)" inside the
// body decides the outcome — a 503 read as "the event is already gone".
func TestDeleteEventReportsA503WhoseBodyQuotesAnotherServices404(t *testing.T) {
	stub := &dashCalendarStub{
		eventStatus: http.StatusServiceUnavailable,
		eventBody: `{"error":"auth-store error (404): {\n  \"error\": ` +
			`\"no credentials for provider google account=\\\"default\\\"\"\n}\n"}`,
	}
	mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

	if err := mirror.DeleteEvent("token", ordinaryID); err == nil {
		t.Fatal("a 503 was reported as a successful delete because another service's " +
			"404 was quoted in the body — the calendar mirror goes silent exactly when " +
			"the credential it needs has gone away")
	}
}

// The two statuses that really do mean the event is gone must still be swallowed,
// or the repair above would turn a re-delete into a permanent error. Without this
// pair, refusing to classify anything at all would satisfy the tests above.
func TestDeleteEventStillTreatsAGoneEventAsDone(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"410 — Google's tombstone for an event deleted twice", http.StatusGone,
			`{"error":{"code":410,"message":"deleted"}}`},
		{"404 — an event Google has never heard of", http.StatusNotFound,
			`{"error":{"code":404,"message":"Not Found"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &dashCalendarStub{eventStatus: tc.status, eventBody: tc.body}
			mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

			if err := mirror.DeleteEvent("token", ordinaryID); err != nil {
				t.Errorf("an event that is already gone should be the state we wanted: %v", err)
			}
		})
	}
}

// UpdateEvent must NOT inherit the swallow. Only DeleteEvent has a state where
// "gone" is success; an update that 404s means the mirror is writing to an event
// that no longer exists and the caller has to hear about it.
func TestUpdateEventReportsAMissingEventRatherThanSwallowingIt(t *testing.T) {
	stub := &dashCalendarStub{
		eventStatus: http.StatusNotFound,
		eventBody:   `{"error":{"code":404,"message":"Not Found"}}`,
	}
	mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

	if err := mirror.UpdateEvent("token", ordinaryID, probeMirrorItem()); err == nil {
		t.Fatal("UpdateEvent swallowed a 404: the coordinator would keep the calendar link " +
			"for an event that is not there and never create a replacement")
	}
}

// ---------------------------------------------------------------- the refusals

// dash's auth middleware runs before its method switch, so an unauthenticated
// request is 401 whatever verb it carries. Measured on :8101.
func TestTheMirrorReportsAnUnauthorizedCallRatherThanReadingItsBody(t *testing.T) {
	stub := &dashCalendarStub{requireBearer: "the-real-token"}
	mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

	_, err := mirror.ListEvents("a-stale-token", time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatal("a 401 read as a day with no events")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("the status did not reach the message, so a rotated DASH_TOKEN is "+
			"indistinguishable from a malformed body: %v", err)
	}
}

// dash answers a wrong verb with http.Error, which is text/plain. A client that
// assumed every refusal is JSON would fail one step later, in the decoder,
// reporting a refused call as a malformed one.
func TestAMethodRefusalReachesTheCallerAsItsStatusAndNotAsADecodeError(t *testing.T) {
	stub := &dashCalendarStub{}
	server := stub.server(t)
	mirror := &GoogleCalendarMirror{DashURL: server.URL}

	// PUT on the COLLECTION route is the shape: dash routes /api/calendar/google
	// to GET and POST only.
	err := mirror.do("token", http.MethodPut, "/api/calendar/google", map[string]any{}, nil)
	if err == nil {
		t.Fatal("a 405 was read as success")
	}
	if !strings.Contains(err.Error(), "405") {
		t.Errorf("the status did not reach the message: %v", err)
	}
	if strings.Contains(err.Error(), "invalid character") {
		t.Errorf("a plain-text refusal was reported as a JSON decode failure: %v", err)
	}
}

// ---------------------------------------------------------------- the agreements

// CreateEvent reads one field out of dash's answer. dash copies Google's event
// resource through unchanged, and that resource carries a top-level `id`.
func TestCreateEventReadsTheEventIDDashPassesThrough(t *testing.T) {
	stub := &dashCalendarStub{}
	mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

	id, err := mirror.CreateEvent("token", probeMirrorItem())
	if err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if id != "6f8k2m4p1q9r3s5t7u0v2w4x6y" {
		t.Errorf("event id round-tripped as %q", id)
	}
}

// A 200 carrying no id is the one shape that would leave the coordinator storing
// an empty `gcal:` link and re-creating the event on every tick.
func TestCreateEventRefusesAnAnswerWithNoEventID(t *testing.T) {
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"summary":"created but unnamed"}`))
	}))
	t.Cleanup(empty.Close)

	mirror := &GoogleCalendarMirror{DashURL: empty.URL}
	if _, err := mirror.CreateEvent("token", probeMirrorItem()); err == nil {
		t.Fatal("a create that returned no event id was reported as success")
	}
}

// dash's multi-calendar branch answers 200 with {events, calendars, error} when
// SOME calendars could not be read — its own comment names reminder-coordinator as
// the reader of that field. A partial answer must not read as an empty day.
func TestListEventsTreatsA200CarryingAnErrorFieldAsAFailure(t *testing.T) {
	partial := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"events": []any{},
			"error":  "calendars that could not be read — Work: Google API error (503)",
		})
	}))
	t.Cleanup(partial.Close)

	mirror := &GoogleCalendarMirror{DashURL: partial.URL}
	events, err := mirror.ListEvents("token", time.Now(), time.Now().Add(time.Hour))
	if err == nil {
		t.Fatalf("a partial calendar read was reported as a day with no events (%d events)", len(events))
	}
}

// ---------------------------------------------------------------- what the mirror sends

// dash's event route takes PUT to change an event and refuses POST with 405.
func TestUpdateEventPutsToTheEventPath(t *testing.T) {
	stub := &dashCalendarStub{}
	mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

	if err := mirror.UpdateEvent("token", ordinaryID, probeMirrorItem()); err != nil {
		t.Fatalf("UpdateEvent: %v", err)
	}
	if stub.lastMethod != http.MethodPut {
		t.Errorf("UpdateEvent sent %s, want PUT", stub.lastMethod)
	}
}

// The 📋 prefix is the only thing on the user's calendar that tells a mirrored
// todo from an event they made themselves.
func TestCreateEventMarksTheSummaryAsAMirroredTodo(t *testing.T) {
	stub := &dashCalendarStub{}
	mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

	if _, err := mirror.CreateEvent("token", probeMirrorItem()); err != nil {
		t.Fatalf("CreateEvent: %v", err)
	}
	if summary := stub.lastBody["summary"]; summary != "📋 Water the plants" {
		t.Errorf("summary was %q, want %q", summary, "📋 Water the plants")
	}
}

// The refusal test above sends the wrong token. This one sends the right token and
// expects it to be accepted, so a malformed Authorization header fails here.
func TestTheMirrorsBearerHeaderIsAcceptedWithTheRightToken(t *testing.T) {
	stub := &dashCalendarStub{requireBearer: "the-real-token"}
	mirror := &GoogleCalendarMirror{DashURL: stub.server(t).URL}

	if _, err := mirror.ListEvents("the-real-token", time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("dash refused the right token: %v", err)
	}
}
