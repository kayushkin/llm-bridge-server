package evalstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every fixture here is synthetic: a made-up turn about a garden, never a
// real session's text.

const syntheticTwoQuestionExpectation = `{"kind":"question","questions":[{"options":3},{"options":null}]}`

func turnEndCase(expected string) *Case {
	c := &Case{UserPrompt: "Final message of the finished turn:\n\nShould the shed be red, blue or green? And where should it go?"}
	if expected != "" {
		c.ExpectedAnswer = json.RawMessage(expected)
	}
	return c
}

func TestTurnEndSignalCheckerPassesAnAnswerSplitIntoItsQuestions(t *testing.T) {
	answer := json.RawMessage(`{"kind":"question","questions":[
		{"question":"Which color should the shed be?","options":[{"label":"Red"},{"label":"Blue"},{"label":"Green"}]},
		{"question":"Where should the shed go?","options":[]}]}`)
	results := checkByName(turnEndSignalChecker{}.Check(turnEndCase(syntheticTwoQuestionExpectation), answer))
	for _, name := range []string{CheckKindMatchesExpected, CheckQuestionCountMatchesExpected, CheckOptionCountsMatchExpected, CheckOptionLabelsNameAChoice} {
		if r, ok := results[name]; !ok || !r.Passed {
			t.Errorf("%s: %+v (present %v)", name, r, ok)
		}
	}
}

// The defect this checker exists for: two questions merged into one, with
// option labels that only tell the person to choose.
func TestTurnEndSignalCheckerCatchesMergedQuestionsAndPlaceholderLabels(t *testing.T) {
	answer := json.RawMessage(`{"kind":"question","questions":[
		{"question":"Which option do you want for color and place?","options":[{"label":"Color: choose a, b, or c"},{"label":"Place: choose a or b"}]}]}`)
	results := checkByName(turnEndSignalChecker{}.Check(turnEndCase(syntheticTwoQuestionExpectation), answer))
	if r := results[CheckQuestionCountMatchesExpected]; r.Passed || !strings.Contains(r.Detail, "expected 2 questions, answered 1") {
		t.Errorf("question count: %+v", r)
	}
	if r := results[CheckOptionLabelsNameAChoice]; r.Passed || r.Count != 2 {
		t.Errorf("placeholder labels: %+v, want both flagged", r)
	}
	if _, ran := results[CheckOptionCountsMatchExpected]; ran {
		t.Error("option counts were compared although the questions did not line up")
	}
	if !results[CheckKindMatchesExpected].Passed {
		t.Errorf("kind: %+v", results[CheckKindMatchesExpected])
	}
}

func TestPlaceholderOptionLabels(t *testing.T) {
	placeholders := []string{"c", "C", "(2)", "Option b", "option 3", "PRs: c", "Next steps (a)", "a.", "Choose a, b, or c", "Pick one"}
	choices := []string{"Red", "Add pull requests to repo-store", "Both stored and generated", "Leave it", "Next steps (c) Both",
		"Option A: rename the directory too", "Query gh live", "4K", "Yes"}
	for _, label := range placeholders {
		if !placeholderOptionLabel.MatchString(label) {
			t.Errorf("%q names no choice but passed", label)
		}
	}
	for _, label := range choices {
		if placeholderOptionLabel.MatchString(label) {
			t.Errorf("%q names a choice but was flagged", label)
		}
	}
}

func TestTurnEndSignalCheckerComparesKindsAndBoundedQuestionCounts(t *testing.T) {
	checker := turnEndSignalChecker{}
	neither := turnEndCase(`{"kind":"neither"}`)
	r := checkByName(checker.Check(neither, json.RawMessage(`{"kind":"notification","title":"The shed is built"}`)))[CheckKindMatchesExpected]
	if r.Passed || r.Detail != "expected neither, answered notification" {
		t.Errorf("kind mismatch: %+v", r)
	}

	bounded := turnEndCase(`{"kind":"question","minimum_questions":2,"maximum_questions":3}`)
	three := json.RawMessage(`{"kind":"question","questions":[{"question":"a?"},{"question":"b?"},{"question":"c?"}]}`)
	if r := checkByName(checker.Check(bounded, three))[CheckQuestionCountMatchesExpected]; !r.Passed {
		t.Errorf("three questions within 2..3: %+v", r)
	}
	one := json.RawMessage(`{"kind":"question","questions":[{"question":"a?"}]}`)
	if r := checkByName(checker.Check(bounded, one))[CheckQuestionCountMatchesExpected]; r.Passed {
		t.Errorf("one question within 2..3 passed: %+v", r)
	}

	noExpectation := checkByName(checker.Check(turnEndCase(""), one))
	if len(noExpectation) != 1 || !noExpectation[CheckOptionLabelsNameAChoice].Passed {
		t.Errorf("with no expectation only the label check runs: %+v", noExpectation)
	}
	if got := checker.Choices(nil, three); got["verdict"] != "question×3" {
		t.Errorf("choices = %v", got)
	}
}

func TestTurnEndSignalExpectationsAreRefusedWhenUnreadable(t *testing.T) {
	refused := []string{
		`{"kind":"maybe"}`,
		`{"kind":"question"}`,
		`{"kind":"question","questions":[{"options":2}],"minimum_questions":1,"maximum_questions":2}`,
		`{"kind":"question","minimum_questions":3,"maximum_questions":2}`,
		`{"kind":"neither","questions":[{"options":1}]}`,
		`{"kind":"question","questions":[{"options":-1}]}`,
		`{"kind":"question","questions":[{"choices":2}]}`,
	}
	for _, expected := range refused {
		if err := (turnEndSignalChecker{}).ValidateExpectedAnswer(json.RawMessage(expected)); err == nil {
			t.Errorf("%s was accepted", expected)
		}
	}
	if err := (turnEndSignalChecker{}).ValidateExpectedAnswer(nil); err != nil {
		t.Errorf("no expectation refused: %v", err)
	}
}

func turnEndSuite(t *testing.T, store *Store) *Suite {
	t.Helper()
	suite, err := store.CreateSuite(&Suite{Name: "synthetic turn ends", TaskKind: "turn-end-signal", JudgeInstructions: "Judge the verdict.",
		JudgeFailureKinds: []string{"merged_questions", "no_answer", "none"}, OutputSchema: json.RawMessage(`{"type":"object"}`),
		MaxTokens: 1000, JudgeMaxTokens: 2000, CheckerName: "turn-end-signal", BaselineModelID: "model-original",
		WeeklyCallsSource: WeeklyCallsSource{Kind: WeeklyCallsSourceFixed, CallsPerWeek: 10, Note: "a synthetic figure"}})
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

func TestACaseKeepsItsExpectedAnswerAndABadOneIsRefused(t *testing.T) {
	store := openTestStore(t)
	suite := turnEndSuite(t, store)
	created, err := store.CreateCase(suite.ID, &Case{SourceKind: SourceKindClaudeCodeTranscript, SourceRef: "br_synthetic_1",
		SystemPrompt: "Classify.", UserPrompt: turnEndCase("").UserPrompt, ExpectedAnswer: json.RawMessage(syntheticTwoQuestionExpectation)})
	if err != nil {
		t.Fatal(err)
	}
	read, err := store.GetCase(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if string(read.ExpectedAnswer) != syntheticTwoQuestionExpectation {
		t.Errorf("expected_answer read back as %s", read.ExpectedAnswer)
	}
	if read.Features["expected_questions"] != 2 {
		t.Errorf("features = %v", read.Features)
	}

	_, err = store.CreateCase(suite.ID, &Case{SourceKind: SourceKindClaudeCodeTranscript, SourceRef: "br_synthetic_2",
		UserPrompt: "x", ExpectedAnswer: json.RawMessage(`{"kind":"question"}`)})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("unreadable expectation = %v, want invalid", err)
	}
	_, err = store.CreateCase(emailSuite(t, store).ID, &Case{SourceKind: SourceKindLLMBridgeOperation, SourceRef: "operation_x",
		UserPrompt: syntheticFilingPrompt("e1"), ExpectedAnswer: json.RawMessage(`{"kind":"neither"}`)})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("expectation on a suite whose checker reads none = %v, want invalid", err)
	}
}

// A database made before expected_answer existed gets the column on open,
// and its cases read back with none.
func TestOpenAddsExpectedAnswerToAnOlderDatabase(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(dataDir, databaseFileName)
	older, err := sql.Open("sqlite3", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := older.Exec(strings.Replace(schemaSQL, "  expected_answer              TEXT,\n", "", 1)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := older.QueryRow("SELECT COUNT(*) FROM pragma_table_info('cases') WHERE name = 'expected_answer'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("the older schema already has the column (%d, %v); the test no longer builds an older database", count, err)
	}
	older.Close()
	if err := os.Chmod(databasePath, 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c := addEmailCase(t, store, emailSuite(t, store), "old")
	if c.ExpectedAnswer != nil {
		t.Errorf("expected_answer = %s, want none", c.ExpectedAnswer)
	}
}

func TestPatchingACaseChangesOnlyItsExpectedAnswer(t *testing.T) {
	store := openTestStore(t)
	suite := turnEndSuite(t, store)
	created, err := store.CreateCase(suite.ID, &Case{SourceKind: SourceKindClaudeCodeTranscript, SourceRef: "br_synthetic_patch",
		SystemPrompt: "Classify.", UserPrompt: turnEndCase("").UserPrompt, ExpectedAnswer: json.RawMessage(syntheticTwoQuestionExpectation)})
	if err != nil {
		t.Fatal(err)
	}
	bounded := `{"kind":"question","minimum_questions":2,"maximum_questions":3}`
	patched, err := store.PatchCase(created.ID, CasePatch{ExpectedAnswer: json.RawMessage(bounded)})
	if err != nil {
		t.Fatal(err)
	}
	if string(patched.ExpectedAnswer) != bounded || patched.UserPrompt != created.UserPrompt || patched.SystemPrompt != created.SystemPrompt {
		t.Errorf("patched case = %+v", patched)
	}
	if patched.Features["expected_questions"] != 0 {
		t.Errorf("features not recomputed: %v", patched.Features)
	}
	for _, refused := range []string{``, `null`, `{"kind":"question"}`} {
		if _, err := store.PatchCase(created.ID, CasePatch{ExpectedAnswer: json.RawMessage(refused)}); !errors.Is(err, ErrInvalid) {
			t.Errorf("patch %q = %v, want invalid", refused, err)
		}
	}
	email := addEmailCase(t, store, emailSuite(t, store), "patch")
	if _, err := store.PatchCase(email.ID, CasePatch{ExpectedAnswer: json.RawMessage(`{"kind":"neither"}`)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("patching a case whose checker reads no expectation = %v, want invalid", err)
	}
}
