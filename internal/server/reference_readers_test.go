package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/references/referencestore"
	"github.com/kayushkin/llm-bridge-server/internal/repositories/repostore"
)

// referenceMountPrefixes is where each in-process service's routes are
// mounted on this server, for comparing a reader with its route.
var referenceMountPrefixes = map[string]string{
	"llm-bridge-server": "",
	"prediction-store":  "/prediction-store",
	"noteboard":         "/noteboard",
	"project-store":     "/project-store",
	"principal-store":   "/principal-store",
	"repo-store":        repoStoreMountPrefix,
	"work-graph-store":  workGraphStoreMountPrefix,
	"people-store":      peopleStoreMountPrefix,
	"journal-store":     journalStoreMountPrefix,
	"calendar-store":    calendarStoreMountPrefix,
	"message-store":     messageStoreMountPrefix,
	"activity-store":    activityStoreMountPrefix,
	"time-ledger-store": timeLedgerStoreMountPrefix,
	"budget-store":      budgetStoreMountPrefix,
	"scheduler":         schedulerMountPrefix,
	"photo-store":       photoStoreMountPrefix,
	"location-store":    locationStoreMountPrefix,
	"possession-store":  possessionStoreMountPrefix,
}

// createdID posts body and answers the id the store gave the record.
func createdID(t *testing.T, gated *gatedTestServer, principalID, path string, body map[string]any, idField string) string {
	t.Helper()
	var response = gated.requestAsServiceWithJSON(t, http.MethodPost, path, body)
	if principalID != "" {
		response = gated.requestAsPrincipal(t, principalID, http.MethodPost, path, body)
	}
	if response.Code != http.StatusCreated && response.Code != http.StatusOK {
		t.Fatalf("POST %s = %d %s", path, response.Code, response.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	switch id := created[idField].(type) {
	case string:
		return id
	case float64:
		return fmt.Sprintf("%06d", int64(id))
	}
	t.Fatalf("POST %s answered no %s: %s", path, idField, response.Body)
	return ""
}

func ownerDecides(row *referencestore.ReferenceType) bool {
	return row.Access == referencestore.AccessByOwner || row.Access == referencestore.AccessByGrantAndOwner
}

// Each in-process reader answers what its type's route answers: the same
// record for the internal service and for each principal, and the same
// refusal where the route refuses. The routes are the old path
// reference-store took through this server, so this holds the readers to it.
func TestEveryInProcessReaderAnswersWhatItsRouteAnswers(t *testing.T) {
	gated := newGatedTestServer(t, nil)
	enablePossessionsOnTestDatabase(t, gated)
	enableMessageActivityAndTimeLedgerModulesOnTestDatabase(t, gated)
	enablePredictionStoreOnTestDatabase(t, gated.server)
	enableSchedulerOnTestDatabase(t, gated.server)
	if _, err := gated.server.referenceReadable.repos.Load().UpsertRepo(&repostore.Repo{Name: "webapp", GitRemote: "git@github.com:example-owner/webapp.git"}); err != nil {
		t.Fatal(err)
	}

	session := gated.createSessionAs(t, gated.loginAs(t, firstTestPrincipalID), firstTestPrincipalID)
	asked := map[string][]string{
		"session":    {session.SessionID, "br_1794083694355010889"},
		"repo":       {"repo:webapp", "repo:nothing"},
		"note":       {createdID(t, gated, "", "/noteboard/api/items", map[string]any{"type": "note", "title": "Printer"}, "id"), "00000000-0000-0000-0000-000000000000"},
		"project":    {createdID(t, gated, "", "/project-store/projects", map[string]any{"name": "Inbox", "goal": "One inbox."}, "id"), "project_000099"},
		"prediction": {createdID(t, gated, "", "/prediction-store/predictions", map[string]any{"claim": "it rains", "resolution_criteria": "look outside", "category": "will-fix", "provenance": "inferred", "probability": 0.7}, "id"), "prediction_000099"},
		"scheduler_job": {"scheduler_job_" + createdID(t, gated, "", "/scheduler/api/jobs", map[string]any{"name": "disk check", "schedule": "*/5 * * * *", "command": "echo disk"}, "id"),
			"scheduler_job_000099"},
		"person":         {createdID(t, gated, firstTestPrincipalID, "/people-store/persons", map[string]any{"display_name": "Sam"}, "id"), "person_000099"},
		"entry":          {createdID(t, gated, firstTestPrincipalID, "/journal-store/entries", map[string]any{"kind": "note", "title": "Errands", "body": "post a letter"}, "id"), "entry_000099"},
		"place":          {createdID(t, gated, firstTestPrincipalID, "/location-store/places", map[string]any{"name": "Example home", "latitude": 10.0, "longitude": 20.0, "kind": "home"}, "id"), "place_000099"},
		"budget_account": {createdID(t, gated, firstTestPrincipalID, "/budget-store/accounts", map[string]any{"name": "Checking", "kind": "checking"}, "id"), "account_000099"},
		"time_entry": {createdID(t, gated, firstTestPrincipalID, "/time-ledger-store/time-entries", map[string]any{"title": "Lunch", "starts_at": 1710100800, "ends_at": 1710104400,
			"by": "test", "by_kind": "operator", "reason": "made up for a test", "time_zone": "Europe/Lisbon"}, "id"), "time_entry_000099"},
		"possession": {createdID(t, gated, firstTestPrincipalID, "/possession-store/possessions", map[string]any{"actor": firstTestPrincipalID, "name": "rug"}, "id"), "possession_000099"},
	}

	readers := gated.server.InProcessReferenceReaders()
	ctx := context.Background()
	answersByStatus := map[int]int{}
	for typeName, ids := range asked {
		row := referencestore.TypeNamed(typeName)
		if row == nil {
			t.Fatalf("no type %s", typeName)
		}
		askers := []string{""}
		if ownerDecides(row) {
			askers = append(askers, firstTestPrincipalID, secondTestPrincipalID, administratorTestPrincipalID, disabledTestPrincipalID)
		}
		for _, id := range ids {
			path := referenceMountPrefixes[row.Service] + strings.Replace(row.Get, "{id}", url.PathEscape(id), 1)
			for _, asker := range askers {
				route := gated.requestAsService(t, http.MethodGet, path)
				if asker != "" {
					route = gated.requestAsServiceAssertingPrincipal(t, asker, http.MethodGet, path)
				}
				data, err := readers.Readers[typeName].Record(ctx, id, referencestore.InProcessAsker{PrincipalID: asker})
				answersByStatus[route.Code]++
				switch route.Code {
				case http.StatusOK:
					var fromRoute, fromReader any
					if err != nil || json.Unmarshal(route.Body.Bytes(), &fromRoute) != nil || json.Unmarshal(data, &fromReader) != nil || !reflect.DeepEqual(fromRoute, fromReader) {
						t.Errorf("%s %s as %q: the route answered %s, the reader %s, %v", typeName, id, asker, route.Body, data, err)
					}
				case http.StatusNotFound:
					if !errors.Is(err, referencestore.ErrRecordNotFound) {
						t.Errorf("%s %s as %q: the route answered 404, the reader %s, %v", typeName, id, asker, data, err)
					}
				case http.StatusForbidden:
					if !errors.Is(err, referencestore.ErrRefusedByOwner) && !(asker == "" && err != nil) {
						t.Errorf("%s %s as %q: the route answered 403 %s, the reader %s, %v", typeName, id, asker, route.Body, data, err)
					}
				default:
					if err == nil || errors.Is(err, referencestore.ErrRecordNotFound) || errors.Is(err, referencestore.ErrRefusedByOwner) {
						t.Errorf("%s %s as %q: the route answered %d %s, the reader %s, %v", typeName, id, asker, route.Code, route.Body, data, err)
					}
				}
			}
		}
	}

	// Every kind of answer was compared: records, missing ones, a disabled
	// principal's refusal, and journal-store refusing the service acting as
	// itself.
	if answersByStatus[http.StatusOK] < 20 || answersByStatus[http.StatusNotFound] < 20 || answersByStatus[http.StatusForbidden] < 8 {
		t.Errorf("the routes answered %v; the comparison saw too few of some kind", answersByStatus)
	}
	t.Logf("compared answers by the route's status: %v", answersByStatus)

	// A search answers what the type's search route answers.
	for typeName, query := range map[string]string{"note": "Printer", "project": "Inbox", "prediction": "rains", "person": "Sam", "entry": "Errands"} {
		row := referencestore.TypeNamed(typeName)
		path := referenceMountPrefixes[row.Service] + row.Search + url.QueryEscape(query)
		askers := []string{""}
		if ownerDecides(row) {
			askers = []string{firstTestPrincipalID, secondTestPrincipalID, administratorTestPrincipalID}
		}
		for _, asker := range askers {
			route := gated.requestAsService(t, http.MethodGet, path)
			if asker != "" {
				route = gated.requestAsServiceAssertingPrincipal(t, asker, http.MethodGet, path)
			}
			data, err := readers.Readers[typeName].Search(ctx, query, referencestore.InProcessAsker{PrincipalID: asker})
			var fromRoute, fromReader any
			if route.Code != http.StatusOK || err != nil || json.Unmarshal(route.Body.Bytes(), &fromRoute) != nil || json.Unmarshal(data, &fromReader) != nil || !reflect.DeepEqual(fromRoute, fromReader) {
				t.Errorf("search %s %q as %q: the route answered %d %s, the reader %s, %v", typeName, query, asker, route.Code, route.Body, data, err)
			}
		}
	}
}

// principal-store's reader answers what its routes answer. The gated test
// server has no principal-store module, so this one has.
func TestThePrincipalReaderAnswersWhatItsRoutesAnswer(t *testing.T) {
	test := newAccessTestServer(t)
	readers := test.server.InProcessReferenceReaders()
	ctx := context.Background()
	for _, id := range []string{accessTestMemberID, "principal_000099"} {
		route := test.call(t, callerServiceTokenAlone, http.MethodGet, "/principal-store/principals/"+id, "")
		data, err := readers.Readers["principal"].Record(ctx, id, referencestore.InProcessAsker{})
		if route.Code == http.StatusNotFound {
			if !errors.Is(err, referencestore.ErrRecordNotFound) {
				t.Errorf("%s: the route answered 404, the reader %s, %v", id, data, err)
			}
			continue
		}
		var fromRoute, fromReader any
		if route.Code != http.StatusOK || err != nil || json.Unmarshal(route.Body.Bytes(), &fromRoute) != nil || json.Unmarshal(data, &fromReader) != nil || !reflect.DeepEqual(fromRoute, fromReader) {
			t.Errorf("%s: the route answered %d %s, the reader %s, %v", id, route.Code, route.Body, data, err)
		}
	}
	route := test.call(t, callerServiceTokenAlone, http.MethodGet, "/principal-store/principals?q=Member", "")
	data, err := readers.Readers["principal"].Search(ctx, "Member", referencestore.InProcessAsker{})
	var fromRoute, fromReader any
	if route.Code != http.StatusOK || err != nil || json.Unmarshal(route.Body.Bytes(), &fromRoute) != nil || json.Unmarshal(data, &fromReader) != nil || !reflect.DeepEqual(fromRoute, fromReader) {
		t.Errorf("search: the route answered %d %s, the reader %s, %v", route.Code, route.Body, data, err)
	}
}
