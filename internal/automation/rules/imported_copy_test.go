package rules

import (
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

var (
	standaloneRuleStoreCopy = flag.String("standalone-rule-store-copy", "", "a copy of a standalone rule-store's SQLite file to import and read back")
	ruleStoreAnswersOutput  = flag.String("rule-store-answers-output", "", "where to write what the imported copy answers each GET, as JSON")
)

// TestImportedStandaloneRuleStoreAnswers imports a copy of a standalone
// rule-store and writes what its read routes answer from it, for comparing
// with what the standalone service answers the same paths. It runs only when
// given a copy:
//
//	go test ./internal/automation/rules -run TestImportedStandaloneRuleStoreAnswers \
//	  -args -standalone-rule-store-copy <copy.db> -rule-store-answers-output <answers.json>
func TestImportedStandaloneRuleStoreAnswers(t *testing.T) {
	if *standaloneRuleStoreCopy == "" || *ruleStoreAnswersOutput == "" {
		t.Skip("give -standalone-rule-store-copy and -rule-store-answers-output to read an imported copy back")
	}
	module, err := Module()
	if err != nil {
		t.Fatal(err)
	}
	store := New(postgrestest.NewDatabase(t, module).Open(module))
	counts, err := ImportStandaloneDatabase(context.Background(), store.db, *standaloneRuleStoreCopy)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("imported %+v", counts)
	bridge, kanban := &LLMBridge{}, &KanbanStore{}
	reactor := NewReactor(store, bridge, kanban, NewOwnerCalls(bridge, kanban), 0)
	reactor.SwitchOff("reading an imported copy")
	routes := http.NewServeMux()
	RegisterHandlers(context.Background(), routes, store, reactor, kanban)

	rules, err := store.ListRules("")
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{"/vocabulary", "/rules", "/rules?state=active", "/rules?state=retired", "/runs", "/runs?limit=1000", "/rules/rule_999999", "/subjects/session/br_not_held"}
	for _, rule := range rules {
		paths = append(paths, "/rules/"+rule.ID, "/runs?limit=1000&rule_id="+url.QueryEscape(rule.ID))
	}
	runs, err := store.ListRuns(RunFilter{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, run := range runs {
		if seen[run.SubjectID] {
			continue
		}
		seen[run.SubjectID] = true
		paths = append(paths, "/subjects/"+run.SubjectType+"/"+url.PathEscape(run.SubjectID), "/runs?subject_id="+url.QueryEscape(run.SubjectID))
	}
	answers := map[string]json.RawMessage{}
	for _, path := range paths {
		recorder := httptest.NewRecorder()
		routes.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		encoded, err := json.Marshal(map[string]any{"status": recorder.Code, "body": json.RawMessage(recorder.Body.Bytes())})
		if err != nil {
			t.Fatal(err)
		}
		answers[path] = encoded
	}
	encoded, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(*ruleStoreAnswersOutput, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %d answers to %s", len(answers), *ruleStoreAnswersOutput)
}
