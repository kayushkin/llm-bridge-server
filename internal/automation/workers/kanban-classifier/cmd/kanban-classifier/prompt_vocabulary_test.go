package main

// The classifier writes its state vocabulary down four times and its title bound
// three times. Nothing held them to each other, so drift between them was silent —
// and silent in the expensive direction.
//
// The two kinds of drift are not symmetric:
//
//   - A word the SCHEMA rejects is a refused tool call. Loud, and the model retries.
//   - A word the PROMPT omits is one the model simply never reaches for. A whole
//     state stops being reported, every card that should have moved to that column
//     stays where it was, and nothing anywhere fails.
//
// The second is why these tests exist. The prompt is prose inside a const, so no
// walk of the schema reaches it and no compiler check touches it.
//
// Every expectation below is DERIVED — parsed out of the prompt, walked out of
// reportSchema(), or measured through the applier. None of the four vocabularies
// is restated here, because a test that restates a constant is a fifth authoring of
// it and drifts in exactly the same way.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/degradations"
	"github.com/kayushkin/llm-bridge-server/internal/automation/workers/shared/kanbanclient"
)

// ============================ Reading the four authorings ============================

// promptStateVocabulary returns the states the system prompt tells the model it may
// report. It fails the test rather than returning empty: a prompt that has stopped
// naming its states is the defect under test, not a reason to skip.
func promptStateVocabulary(t *testing.T) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^-\s*state must be one of:\s*([^.\n]+)`)
	m := re.FindStringSubmatch(systemPrompt)
	if m == nil {
		t.Fatal("systemPrompt no longer contains a `- state must be one of: ...` line. " +
			"The model is now told nothing about which states it may report, and every " +
			"state it stops reporting is silent. If this line moved, update this parser; " +
			"if it was deleted, that is the bug.")
	}
	var states []string
	for _, part := range strings.Split(m[1], ",") {
		if s := strings.TrimSpace(part); s != "" {
			states = append(states, s)
		}
	}
	if len(states) == 0 {
		t.Fatalf("the prompt's state line parsed to no states at all: %q", m[1])
	}
	return states
}

// promptTitleCharacterAdvice returns the title length the system prompt advises.
func promptTitleCharacterAdvice(t *testing.T) int {
	t.Helper()
	re := regexp.MustCompile(`≤\s*(\d+)\s*chars`)
	m := re.FindStringSubmatch(systemPrompt)
	if m == nil {
		t.Fatal("systemPrompt no longer advises a title length (`≤N chars`). " +
			"If the advice was deliberately dropped, drop this test with it.")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parsing the prompt's title advice %q: %v", m[1], err)
	}
	return n
}

// schemaArrayItemProperty walks reportSchema() down to one property of one
// array-valued top-level property, e.g. ("new_tasks", "title"). Each step fails
// loudly, because a silent zero value here would make every assertion below pass
// against nothing.
func schemaArrayItemProperty(t *testing.T, arrayName, propertyName string) map[string]any {
	t.Helper()
	step := func(m map[string]any, key, path string) map[string]any {
		v, ok := m[key]
		if !ok {
			t.Fatalf("reportSchema(): %s has no %q", path, key)
		}
		sub, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("reportSchema(): %s.%s is %T, want map[string]any", path, key, v)
		}
		return sub
	}
	schema := reportSchema()
	props := step(schema, "properties", "root")
	arr := step(props, arrayName, "properties")
	items := step(arr, "items", arrayName)
	itemProps := step(items, "properties", arrayName+".items")
	return step(itemProps, propertyName, arrayName+".items.properties")
}

// schemaStateEnum returns the state enum the tool schema advertises for one of its
// two array properties.
func schemaStateEnum(t *testing.T, arrayName string) []string {
	t.Helper()
	prop := schemaArrayItemProperty(t, arrayName, "state")
	raw, ok := prop["enum"]
	if !ok {
		t.Fatalf("reportSchema(): %s.items.properties.state has no enum — the model "+
			"is no longer constrained to a state vocabulary at all", arrayName)
	}
	enum, ok := raw.([]string)
	if !ok {
		t.Fatalf("reportSchema(): %s state enum is %T, want []string", arrayName, raw)
	}
	if len(enum) == 0 {
		t.Fatalf("reportSchema(): %s state enum is empty", arrayName)
	}
	return enum
}

// schemaTitleMaxLength returns the hard title bound the tool schema advertises.
func schemaTitleMaxLength(t *testing.T) int {
	t.Helper()
	prop := schemaArrayItemProperty(t, "new_tasks", "title")
	raw, ok := prop["maxLength"]
	if !ok {
		t.Fatal("reportSchema(): new_tasks.items.properties.title has no maxLength")
	}
	n, ok := raw.(int)
	if !ok {
		t.Fatalf("reportSchema(): new_tasks title maxLength is %T, want int", raw)
	}
	return n
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// ============================ The state vocabulary ============================

// The schema names the state vocabulary twice, once per array. They are two
// authorings of one list and nothing but this test connects them.
func TestBothSchemaArraysOfferOneStateVocabularyAndNotTwo(t *testing.T) {
	existing := sorted(schemaStateEnum(t, "existing_cards"))
	newTasks := sorted(schemaStateEnum(t, "new_tasks"))
	if strings.Join(existing, ",") != strings.Join(newTasks, ",") {
		t.Errorf("the two schema state enums disagree:\n  existing_cards: %v\n  new_tasks:      %v\n"+
			"A state accepted for one and refused for the other means the model can report a "+
			"card's state on creation and be refused when it later updates the same card.",
			existing, newTasks)
	}
}

// This is the loose authoring the census could not see: the prompt is prose, so no
// schema walk reaches it. Both directions matter, and they fail differently.
func TestThePromptNamesExactlyTheStatesTheSchemaAccepts(t *testing.T) {
	prompt := promptStateVocabulary(t)
	schema := schemaStateEnum(t, "new_tasks")

	inSchema := map[string]bool{}
	for _, s := range schema {
		inSchema[s] = true
	}
	inPrompt := map[string]bool{}
	for _, s := range prompt {
		inPrompt[s] = true
	}

	for _, s := range schema {
		if !inPrompt[s] {
			t.Errorf("the schema accepts state %q and the prompt never names it. "+
				"This is the quiet direction: the model is not told the word, so it never "+
				"reaches for it, that state is never reported, and nothing fails.", s)
		}
	}
	for _, s := range prompt {
		if !inSchema[s] {
			t.Errorf("the prompt tells the model it may report state %q and the schema "+
				"rejects it. Every session whose real state is %q costs a refused tool call.", s, s)
		}
	}
}

// A state the applier cannot place is a state that silently lands somewhere else:
// stateToColumn has a default, so a dropped case does not error — it returns the
// default column and the card moves to the wrong place.
//
// The expectation is derived, not restated: N distinct states must reach N distinct
// columns. Drop a case and it collapses onto the default's column, so the count falls.
func TestEveryStateTheModelMayReportReachesItsOwnColumn(t *testing.T) {
	states := schemaStateEnum(t, "new_tasks")

	columnForState := map[string]string{}
	stateForColumn := map[string]string{}
	for _, s := range states {
		col := stateToColumn(s)
		if col == "" {
			t.Errorf("state %q maps to an empty column name", s)
			continue
		}
		columnForState[s] = col
		if other, clash := stateForColumn[col]; clash {
			t.Errorf("states %q and %q both land in column %q. stateToColumn has a default, "+
				"so a case dropped from the switch does not error — it silently sends that "+
				"state to the default column.", other, s, col)
			continue
		}
		stateForColumn[col] = s
	}

	if len(stateForColumn) != len(states) {
		t.Errorf("%d states reach only %d distinct columns: %v", len(states), len(stateForColumn), columnForState)
	}
}

// ============================ The title bound ============================

// 60 and 80 are not one number badly copied. They are advice and a hard cut, and the
// only relationship that has to hold is that the advice fits inside the cut — see the
// comments at the three sites. A model told to aim past the bound would have its
// titles silently shortened by the applier.
func TestThePromptsTitleAdviceFitsInsideTheBoundTheSchemaAdvertises(t *testing.T) {
	advice := promptTitleCharacterAdvice(t)
	hard := schemaTitleMaxLength(t)
	if advice > hard {
		t.Errorf("the prompt advises titles of ≤%d chars but the schema's hard bound is %d. "+
			"The model is being aimed past the cut, so its titles arrive truncated.", advice, hard)
	}
}

// The third authoring of the bound is the applier's own cut, and it is the one that
// decides what the board actually stores. Measured THROUGH applyNew against a fake
// kanban-store rather than restated, so a change to the cut is caught even though
// nothing imports the number.
//
// The schema's maxLength counts CHARACTERS, so the cut has to as well. Two titles per
// script: one of exactly the bound, which obeys the promise and must be stored
// untouched, and one far over it, which must come back marked and no longer than the
// bound. A byte cut failed the first half for every non-ASCII script — an 80-character
// title survived as 41 characters in Latin-1 accents, 29 in Japanese, 21 in emoji —
// and landed mid-rune in the Japanese case. The shift slides the wide runes across
// the cut, because 80 is divisible by every rune width and a fixture aligned at zero
// hides a mid-rune cut.
func TestTheTitleBoundTheBoardEnforcesIsTheOneTheSchemaAdvertises(t *testing.T) {
	hard := schemaTitleMaxLength(t)

	var createdTitles []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/cards"):
			var payload struct {
				Title string `json:"title"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("fake kanban-store could not decode the create-card body: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			createdTitles = append(createdTitles, payload.Title)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"item":{"id":"card-1"}}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/links"):
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("fake kanban-store got an unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	kc := kanbanclient.New(srv.URL, "")
	kc.BoardID = "board-1"
	kc.ColumnsByName = map[string]string{}
	for _, s := range schemaStateEnum(t, "new_tasks") {
		kc.ColumnsByName[stateToColumn(s)] = "col-" + s
	}

	store := func(title string) string {
		t.Helper()
		createdTitles = nil
		applyNew(kc, session{SessionID: "sess-1"}, nil, &Report{
			NewTasks: []NewRec{{
				Title:   title,
				State:   schemaStateEnum(t, "new_tasks")[0],
				Summary: "summary",
			}},
		}, degradations.New("classifier"))
		if len(createdTitles) != 1 {
			t.Fatalf("applyNew created %d cards for one new task, want 1 — a test that reaches "+
				"no create call passes for the wrong reason", len(createdTitles))
		}
		return createdTitles[0]
	}

	for _, script := range []struct{ name, rune_ string }{
		{"ascii", "a"}, {"latin1-accent", "é"}, {"japanese", "日"}, {"emoji", "🟢"},
	} {
		for _, shift := range []int{0, 1, 2} {
			prefix := strings.Repeat("x", shift)

			atBound := prefix + strings.Repeat(script.rune_, hard-shift)
			if got := store(atBound); got != atBound {
				t.Errorf("%s shift=%d: a title of exactly the advertised %d characters was stored "+
					"as %d characters (%q). The model obeyed the bound and was cut anyway.",
					script.name, shift, hard, utf8.RuneCountInString(got), got)
			}

			overLong := prefix + strings.Repeat(script.rune_, hard*3)
			got := store(overLong)
			if !utf8.ValidString(got) || strings.ContainsRune(got, utf8.RuneError) {
				t.Errorf("%s shift=%d: the stored title holds a split rune: %q", script.name, shift, got)
			}
			if !strings.HasSuffix(got, "…") {
				t.Errorf("%s shift=%d: the applier stored a title with no truncation marker for an "+
					"over-long input — the bound is no longer being applied at all", script.name, shift)
			}
			if n := utf8.RuneCountInString(got); n != hard {
				t.Errorf("%s shift=%d: the schema advertises a %d-character title bound and the board "+
					"stored %d characters for an over-long title", script.name, shift, hard, n)
			}
		}
	}
}
