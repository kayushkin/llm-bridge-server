package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kayushkin/kanban-store/internal/api"
	"github.com/kayushkin/kanban-store/internal/model"
)

func putTagRules(t *testing.T, h http.Handler, boardID string, rules []model.BoardTagRuleInput) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "PUT", "/api/boards/"+boardID+"/tag-rules", model.SetBoardTagRulesRequest{Rules: rules})
}

func effectiveFor(t *testing.T, h http.Handler, boardID string, tags ...string) model.EffectiveDefaults {
	t.Helper()
	query := url.Values{}
	for _, tag := range tags {
		query.Add("tag", tag)
	}
	w := do(t, h, "GET", "/api/boards/"+boardID+"/effective-defaults?"+query.Encode(), nil)
	if w.Code != 200 {
		t.Fatalf("effective defaults: %d %s", w.Code, w.Body.String())
	}
	var resolved model.EffectiveDefaults
	decodeSuccessfulResponse(t, w, &resolved)
	return resolved
}

func TestTagRulesRoundTripInOrderAndKeepIDs(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Rules")

	w := putTagRules(t, h, boardID, []model.BoardTagRuleInput{
		{Tags: []string{"cat:product", "urgency:high"}, DefaultInstanceID: knownInstanceID},
		{Tags: []string{"cat:product"}, DefaultBundleID: knownBundleID, DefaultAgentID: knownAgentID},
	})
	if w.Code != 200 {
		t.Fatalf("put: %d %s", w.Code, w.Body.String())
	}
	var first model.BoardTagRules
	decodeSuccessfulResponse(t, w, &first)
	if len(first.Rules) != 2 || first.Rules[0].Position != 0 || first.Rules[1].Tags[0] != "cat:product" || first.Rules[0].ID == "" {
		t.Fatalf("unexpected rules after put: %+v", first.Rules)
	}

	// Reorder, keeping both ids by sending them back.
	w = putTagRules(t, h, boardID, []model.BoardTagRuleInput{
		{ID: first.Rules[1].ID, Tags: first.Rules[1].Tags, DefaultBundleID: knownBundleID, DefaultAgentID: knownAgentID},
		{ID: first.Rules[0].ID, Tags: first.Rules[0].Tags, DefaultInstanceID: knownInstanceID},
	})
	if w.Code != 200 {
		t.Fatalf("reorder: %d %s", w.Code, w.Body.String())
	}
	got := do(t, h, "GET", "/api/boards/"+boardID+"/tag-rules", nil)
	var second model.BoardTagRules
	decodeSuccessfulResponse(t, got, &second)
	if second.Rules[0].ID != first.Rules[1].ID || second.Rules[1].ID != first.Rules[0].ID {
		t.Fatalf("reorder did not keep ids in the new order: %+v", second.Rules)
	}
	if !second.Rules[0].CreatedAt.Equal(first.Rules[1].CreatedAt) {
		t.Errorf("a kept rule's created_at moved: %v → %v", first.Rules[1].CreatedAt, second.Rules[0].CreatedAt)
	}

	// An empty list removes every rule.
	if w := putTagRules(t, h, boardID, []model.BoardTagRuleInput{}); w.Code != 200 {
		t.Fatalf("clear: %d %s", w.Code, w.Body.String())
	}
	got = do(t, h, "GET", "/api/boards/"+boardID+"/tag-rules", nil)
	if !strings.Contains(got.Body.String(), `"rules":[]`) {
		t.Fatalf("cleared rules should read back as an empty list: %s", got.Body.String())
	}
}

func TestTagRulesRefusals(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Rules")
	otherBoardID := mkBoard(t, h, "Other")
	w := putTagRules(t, h, otherBoardID, []model.BoardTagRuleInput{{Tags: []string{"a"}, DefaultInstanceID: knownInstanceID}})
	var other model.BoardTagRules
	decodeSuccessfulResponse(t, w, &other)

	for name, check := range map[string]struct {
		rules []model.BoardTagRuleInput
		want  string
	}{
		"unknown instance":         {[]model.BoardTagRuleInput{{Tags: []string{"a"}}, {Tags: []string{"b"}, DefaultInstanceID: unknownInstanceID}}, ""},
		"bundle name not id":       {[]model.BoardTagRuleInput{{Tags: []string{"a"}, DefaultBundleID: knownBundleName}}, "numeric id"},
		"disabled principal":       {[]model.BoardTagRuleInput{{Tags: []string{"a"}, DefaultPrincipalID: disabledPrincipal}}, "disabled"},
		"rule id from other board": {[]model.BoardTagRuleInput{{ID: other.Rules[0].ID, Tags: []string{"a"}, DefaultInstanceID: knownInstanceID}}, "not one of board"},
		"no defaults":              {[]model.BoardTagRuleInput{{Tags: []string{"a"}}}, "sets no default"},
	} {
		w := putTagRules(t, h, boardID, check.rules)
		if w.Code != 400 {
			t.Errorf("%s: want 400, got %d %s", name, w.Code, w.Body.String())
			continue
		}
		if check.want != "" && !strings.Contains(w.Body.String(), check.want) {
			t.Errorf("%s: refusal %s does not say %q", name, w.Body.String(), check.want)
		}
	}
	// The owner refusal names the rule it came from.
	w = putTagRules(t, h, boardID, []model.BoardTagRuleInput{{Tags: []string{"a"}, DefaultInstanceID: knownInstanceID}, {Tags: []string{"cat:x", "b"}, DefaultInstanceID: unknownInstanceID}})
	if w.Code != 400 || !strings.Contains(w.Body.String(), "rules[1] (tags cat:x + b)") {
		t.Errorf("owner refusal should name rules[1] and its tags: %d %s", w.Code, w.Body.String())
	}
	// A misspelled field is refused, not dropped.
	raw := httptest.NewRequest("PUT", "/api/boards/"+boardID+"/tag-rules", strings.NewReader(`{"rules":[{"tags":["a"],"instance_id":"inst-test"}]}`))
	raw.Header.Set(api.ServiceTokenHeader, testServiceToken)
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, raw)
	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "instance_id") {
		t.Errorf("unknown field: want 400 naming it, got %d %s", recorder.Code, recorder.Body.String())
	}
	// Nothing was written by any refusal.
	got := do(t, h, "GET", "/api/boards/"+boardID+"/tag-rules", nil)
	if !strings.Contains(got.Body.String(), `"rules":[]`) {
		t.Errorf("a refused put wrote rules: %s", got.Body.String())
	}
}

func TestTagRulesWithAnOwnerDownAre502AndWriteNothing(t *testing.T) {
	principals := httptest.NewServer(newFakePrincipalStore().handler())
	defer principals.Close()
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	h, _, _, cleanup := setupWithOwners(t, principals.URL, closed.URL, closed.URL)
	defer cleanup()
	boardID := mkBoard(t, h, "Down")
	if w := putTagRules(t, h, boardID, []model.BoardTagRuleInput{{Tags: []string{"a"}, DefaultInstanceID: knownInstanceID}}); w.Code != 502 {
		t.Fatalf("want 502, got %d %s", w.Code, w.Body.String())
	}
	got := do(t, h, "GET", "/api/boards/"+boardID+"/tag-rules", nil)
	if !strings.Contains(got.Body.String(), `"rules":[]`) {
		t.Errorf("a 502 put wrote rules: %s", got.Body.String())
	}
}

// The precedence the operator chose: ordered rules, each matching a card that
// carries ALL of its tags; per field, the first matching rule that sets it
// wins, the rest fall through, and past every rule the board's own default.
func TestEffectiveDefaultsPrecedence(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Precedence")
	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultInstanceID: str(knownInstanceID), DefaultAgentID: str("12")}); w.Code != 200 {
		t.Fatalf("board defaults: %d %s", w.Code, w.Body.String())
	}
	w := putTagRules(t, h, boardID, []model.BoardTagRuleInput{
		{Tags: []string{"cat:product", "urgency:high"}, DefaultInstanceID: otherKnownInstanceID},
		{Tags: []string{"cat:product"}, DefaultAgentID: knownAgentID, DefaultBundleID: knownBundleID},
	})
	if w.Code != 200 {
		t.Fatalf("rules: %d %s", w.Code, w.Body.String())
	}
	var rules model.BoardTagRules
	decodeSuccessfulResponse(t, w, &rules)
	narrow, broad := rules.Rules[0], rules.Rules[1]

	both := effectiveFor(t, h, boardID, "urgency:high", "cat:product")
	for field, want := range map[model.DefaultField]struct{ value, ruleID string }{
		model.DefaultFieldInstance: {otherKnownInstanceID, narrow.ID},
		model.DefaultFieldAgent:    {knownAgentID, broad.ID},
		model.DefaultFieldBundle:   {knownBundleID, broad.ID},
	} {
		got := both.Defaults[field]
		if got.Value != want.value || got.Source.Kind != model.DefaultSourceTagRule || got.Source.RuleID != want.ruleID {
			t.Errorf("both tags, %s = %+v, want %s from rule %s", field, got, want.value, want.ruleID)
		}
	}

	oneTag := effectiveFor(t, h, boardID, "cat:product")
	if got := oneTag.Defaults[model.DefaultFieldInstance]; got.Value != knownInstanceID || got.Source.Kind != model.DefaultSourceBoard {
		t.Errorf("cat:product alone does not satisfy the two-tag rule, so instance = %+v should be the board's", got)
	}
	if got := oneTag.Defaults[model.DefaultFieldAgent]; got.Value != knownAgentID {
		t.Errorf("cat:product alone, agent = %+v, want the broad rule's", got)
	}

	none := effectiveFor(t, h, boardID)
	if got := none.Defaults[model.DefaultFieldAgent]; got.Value != "12" || got.Source.Kind != model.DefaultSourceBoard {
		t.Errorf("no tags, agent = %+v, want the board's 12", got)
	}
	if _, set := none.Defaults[model.DefaultFieldBundle]; set {
		t.Errorf("no tags, bundle resolved though only a rule sets it: %+v", none.Defaults)
	}

	// Order decides: put the broad rule first and it wins the instance too… it
	// sets none, so the narrow rule still does; give it one and it wins.
	w = putTagRules(t, h, boardID, []model.BoardTagRuleInput{
		{ID: broad.ID, Tags: broad.Tags, DefaultAgentID: knownAgentID, DefaultBundleID: knownBundleID, DefaultInstanceID: knownInstanceID},
		{ID: narrow.ID, Tags: narrow.Tags, DefaultInstanceID: otherKnownInstanceID},
	})
	if w.Code != 200 {
		t.Fatalf("reorder: %d %s", w.Code, w.Body.String())
	}
	if got := effectiveFor(t, h, boardID, "urgency:high", "cat:product").Defaults[model.DefaultFieldInstance]; got.Value != knownInstanceID || got.Source.RuleID != broad.ID {
		t.Errorf("after moving the broad rule first, instance = %+v, want its %s", got, knownInstanceID)
	}
}

func TestCardEffectiveDefaultsReadTheCardsTagsFromNoteboard(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Card")
	columnID := mkColumn(t, h, boardID, "Todo", "")
	if w := putTagRules(t, h, boardID, []model.BoardTagRuleInput{{Tags: []string{"cat:product"}, DefaultBundleID: knownBundleID}}); w.Code != 200 {
		t.Fatalf("rules: %d %s", w.Code, w.Body.String())
	}
	w := do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "tagged", ColumnID: columnID, Tags: []string{"cat:product", "email"}})
	var card model.CardView
	decodeSuccessfulResponse(t, w, &card)

	w = do(t, h, "GET", "/api/boards/"+boardID+"/cards/"+card.Placement.CardID+"/effective-defaults", nil)
	if w.Code != 200 {
		t.Fatalf("card effective defaults: %d %s", w.Code, w.Body.String())
	}
	var resolved model.EffectiveDefaults
	decodeSuccessfulResponse(t, w, &resolved)
	if resolved.CardID != card.Placement.CardID || len(resolved.Tags) != 2 {
		t.Fatalf("resolution did not carry the card and its tags: %+v", resolved)
	}
	if got := resolved.Defaults[model.DefaultFieldBundle]; got.Value != knownBundleID || got.Source.Kind != model.DefaultSourceTagRule {
		t.Errorf("bundle = %+v, want the rule's", got)
	}

	if w := do(t, h, "GET", "/api/boards/"+boardID+"/cards/nb-missing/effective-defaults", nil); w.Code != 404 {
		t.Errorf("missing card: want 404, got %d %s", w.Code, w.Body.String())
	}
}

// Decision 3: a tag rule's principal applies only when a card arrives with no
// assignee, exactly as the board's default does — and it outranks the board's.
func TestATagRulesPrincipalIsAppliedOnArrival(t *testing.T) {
	h, _, cleanup := setup(t)
	defer cleanup()
	boardID := mkBoard(t, h, "Arrival")
	columnID := mkColumn(t, h, boardID, "Inbox", "")
	if w := patchBoard(t, h, boardID, model.UpdateBoardRequest{DefaultPrincipalID: str(activePrincipal)}); w.Code != 200 {
		t.Fatalf("board default: %d %s", w.Code, w.Body.String())
	}
	w := putTagRules(t, h, boardID, []model.BoardTagRuleInput{{Tags: []string{"cat:product"}, DefaultPrincipalID: otherActivePrincipal}})
	if w.Code != 200 {
		t.Fatalf("rules: %d %s", w.Code, w.Body.String())
	}
	var rules model.BoardTagRules
	decodeSuccessfulResponse(t, w, &rules)

	w = do(t, h, "POST", "/api/boards/"+boardID+"/cards?actor=email-classifier", model.CreateCardRequest{Title: "product", ColumnID: columnID, Tags: []string{"cat:product"}})
	var tagged model.CardView
	decodeSuccessfulResponse(t, w, &tagged)
	if len(tagged.Assignments) != 1 || tagged.Assignments[0].PrincipalID != otherActivePrincipal {
		t.Fatalf("tagged card: assignments %+v, want the rule's principal", tagged.Assignments)
	}
	events := eventsOfKind(t, h, tagged.Placement.CardID, model.EventAssigned)
	var detail map[string]any
	if err := json.Unmarshal(events[0].Detail, &detail); err != nil {
		t.Fatal(err)
	}
	if detail["source"] != "tag_rule" || detail["rule_id"] != rules.Rules[0].ID {
		t.Errorf("assigned event should name the rule it came from: %s", events[0].Detail)
	}

	w = do(t, h, "POST", "/api/boards/"+boardID+"/cards", model.CreateCardRequest{Title: "plain", ColumnID: columnID})
	var plain model.CardView
	decodeSuccessfulResponse(t, w, &plain)
	if len(plain.Assignments) != 1 || plain.Assignments[0].PrincipalID != activePrincipal {
		t.Fatalf("untagged card: assignments %+v, want the board's principal", plain.Assignments)
	}

	// Attaching an existing tagged card reads its tags from noteboard.
	sourceBoard := mkBoard(t, h, "Source")
	sourceColumn := mkColumn(t, h, sourceBoard, "Todo", "")
	w = do(t, h, "POST", "/api/boards/"+sourceBoard+"/cards", model.CreateCardRequest{Title: "moving", ColumnID: sourceColumn, Tags: []string{"cat:product"}})
	var moving model.CardView
	decodeSuccessfulResponse(t, w, &moving)
	if w := do(t, h, "PUT", "/api/boards/"+boardID+"/cards/"+moving.Placement.CardID, model.AttachCardRequest{ColumnID: columnID}); w.Code != 201 {
		t.Fatalf("attach: %d %s", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/api/cards/"+moving.Placement.CardID+"/assignments", nil)
	var assignments []model.CardAssignment
	decodeSuccessfulResponse(t, w, &assignments)
	if len(assignments) != 1 || assignments[0].PrincipalID != otherActivePrincipal {
		t.Fatalf("attached tagged card: assignments %+v, want the rule's principal", assignments)
	}
}
