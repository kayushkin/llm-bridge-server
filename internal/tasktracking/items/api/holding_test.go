package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/issues/issueitems"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/items/api"
	"github.com/kayushkin/llm-bridge-server/internal/tasktracking/notes"
	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// notes keeps notes, ranked list entries and workspaces, and issues keeps
// todos; each refuses the other's types by name, so a caller that still
// writes a todo to notes learns where todos went instead of losing it.
func TestEachModuleKeepsOnlyItsOwnTypesAndSaysWhereTheOthersGo(t *testing.T) {
	module, err := notes.Module()
	if err != nil {
		t.Fatal(err)
	}
	database := postgrestest.NewDatabase(t, module)
	pool := database.Open(module)
	// The issues module's items tables are the same shape; one schema
	// holding them is enough to test the two holdings apart.
	notesRoutes := api.New(notes.New(pool)).Handler()
	issueRoutes := api.New(issueitems.New(pool)).Handler()

	call := func(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, path, strings.NewReader(body))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	cases := []struct {
		name        string
		handler     http.Handler
		method      string
		path        string
		body        string
		wantStatus  int
		wantInError string
	}{
		{"notes refuses a todo", notesRoutes, "POST", "/items", `{"type":"todo","title":"x"}`, 400, "/issues/items"},
		{"notes lists no todos", notesRoutes, "GET", "/items?type=todo", "", 400, "/issues/items"},
		{"notes searches no todos", notesRoutes, "GET", "/search?q=x&type=todo", "", 400, "/issues/items"},
		{"notes keeps a note", notesRoutes, "POST", "/items", `{"type":"note","title":"x"}`, 201, ""},
		{"notes keeps a ranked list entry", notesRoutes, "POST", "/items", `{"type":"rank","title":"x","list_id":"songs"}`, 201, ""},
		{"notes needs a type", notesRoutes, "POST", "/items", `{"title":"x"}`, 400, "type is required"},
		{"issues takes an item with no type as a todo", issueRoutes, "POST", "/items", `{"title":"x"}`, 201, ""},
		{"issues refuses a note", issueRoutes, "POST", "/items", `{"type":"note","title":"x"}`, 400, "/notes/items"},
		{"issues refuses a list", issueRoutes, "POST", "/items", `{"type":"todo","title":"x","list_id":"songs"}`, 400, "rank entry"},
		{"issues lists no workspaces", issueRoutes, "GET", "/items?type=workspace", "", 400, "/notes/items"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			answer := call(test.handler, test.method, test.path, test.body)
			if answer.Code != test.wantStatus || !strings.Contains(answer.Body.String(), test.wantInError) {
				t.Errorf("%s %s = %d %s, want %d naming %q", test.method, test.path, answer.Code, answer.Body.String(), test.wantStatus, test.wantInError)
			}
			if test.wantStatus == 201 && test.handler == issueRoutes && !strings.Contains(answer.Body.String(), `"type":"todo"`) {
				t.Errorf("an issue reads %s, want type todo", answer.Body.String())
			}
		})
	}
}
