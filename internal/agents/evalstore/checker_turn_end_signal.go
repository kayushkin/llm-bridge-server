package evalstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// turnEndSignalChecker checks llm-bridge-server's turn-end classifier: given
// the final message of a finished turn, it answers kind question,
// notification or neither, and for a question one entry per separate
// decision the assistant is waiting on, each with the options it offered.
//
// The answer shape is the classifier's output schema:
//
//	{"kind":"question","questions":[{"question":…,"context":…,"options":[{"label":…,"description":…}]}]}
//
// The case's expected_answer says what a right answer holds:
//
//	{"kind":"question","questions":[{"options":3},{"options":null}]}
//	{"kind":"question","minimum_questions":2,"maximum_questions":3}
//	{"kind":"neither"}
//
// questions, when given, fixes how many questions there are and, in the
// assistant's order, how many options each has (null: any number). When the
// number of questions is a judgment call, minimum_questions and
// maximum_questions bound it instead.
type turnEndSignalChecker struct{}

// Turn-end signal check names.
const (
	CheckKindMatchesExpected          = "kind_matches_expected"
	CheckQuestionCountMatchesExpected = "question_count_matches_expected"
	CheckOptionCountsMatchExpected    = "option_counts_match_expected"
	CheckOptionLabelsNameAChoice      = "option_labels_name_a_choice"
)

var turnEndSignalKinds = []string{"question", "notification", "neither"}

type turnEndSignalAnswer struct {
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Questions []struct {
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}

type turnEndSignalExpectation struct {
	Kind      string `json:"kind"`
	Questions []struct {
		Options *int `json:"options"`
	} `json:"questions"`
	MinimumQuestions int `json:"minimum_questions"`
	MaximumQuestions int `json:"maximum_questions"`
}

func readTurnEndSignalExpectation(raw json.RawMessage) (*turnEndSignalExpectation, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var expected turnEndSignalExpectation
	if err := decoder.Decode(&expected); err != nil {
		return nil, fmt.Errorf("not a turn-end signal expectation: %w", err)
	}
	if !containsString(turnEndSignalKinds, expected.Kind) {
		return nil, fmt.Errorf("kind %q is not one of %v", expected.Kind, turnEndSignalKinds)
	}
	bounded := expected.MinimumQuestions != 0 || expected.MaximumQuestions != 0
	if expected.Kind != "question" {
		if len(expected.Questions) > 0 || bounded {
			return nil, fmt.Errorf("a %s expectation names no questions", expected.Kind)
		}
		return &expected, nil
	}
	switch {
	case len(expected.Questions) > 0 && bounded:
		return nil, errors.New("give questions or minimum_questions and maximum_questions, not both")
	case len(expected.Questions) == 0 && !bounded:
		return nil, errors.New("a question expectation needs questions, or minimum_questions and maximum_questions")
	case bounded && (expected.MinimumQuestions < 1 || expected.MaximumQuestions < expected.MinimumQuestions):
		return nil, fmt.Errorf("minimum_questions %d and maximum_questions %d are not a range starting at 1 or more", expected.MinimumQuestions, expected.MaximumQuestions)
	}
	for i, question := range expected.Questions {
		if question.Options != nil && *question.Options < 0 {
			return nil, fmt.Errorf("question %d expects %d options", i+1, *question.Options)
		}
	}
	return &expected, nil
}

func (turnEndSignalChecker) Description() string {
	return "turn-end signal: the expected kind, one question per separate decision, the expected options for each, and option labels that name a choice"
}

// ValidateExpectedAnswer refuses an expectation the checker cannot read. An
// empty one is allowed: the option-label check needs none.
func (turnEndSignalChecker) ValidateExpectedAnswer(expected json.RawMessage) error {
	_, err := readTurnEndSignalExpectation(expected)
	return err
}

func (turnEndSignalChecker) Features(c *Case) map[string]float64 {
	expected, err := readTurnEndSignalExpectation(c.ExpectedAnswer)
	if err != nil || expected == nil {
		return map[string]float64{}
	}
	return map[string]float64{"expected_questions": float64(len(expected.Questions))}
}

// placeholderOptionLabel matches a label a person cannot click on its own:
// a bare letter or number ("c", "(2)", "Option b"), the same after a topic
// ("PRs: c", "Next steps (a)"), or an instruction to pick ("Choose a, b, or
// c"). The 2026-10-20 failure had two such labels.
var placeholderOptionLabel = regexp.MustCompile(`(?i)^\s*(?:[^:()]{0,40}[:(]\s*)?(?:option\s+)?[a-z0-9]\s*[).:]?\s*$|\b(?:choose|pick|select)\b`)

func (turnEndSignalChecker) Check(c *Case, raw json.RawMessage) []CheckResult {
	var answer turnEndSignalAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return []CheckResult{{CheckName: CheckKindMatchesExpected, Passed: false, Count: 1, Detail: "answer is not a turn-end signal: " + err.Error()}}
	}

	var placeholders []string
	for _, question := range answer.Questions {
		for _, option := range question.Options {
			if placeholderOptionLabel.MatchString(option.Label) {
				placeholders = append(placeholders, option.Label)
			}
		}
	}
	results := []CheckResult{{CheckName: CheckOptionLabelsNameAChoice, Passed: len(placeholders) == 0, Count: len(placeholders),
		Detail: listDetail("option labels that name no choice", placeholders)}}

	expected, err := readTurnEndSignalExpectation(c.ExpectedAnswer)
	if err != nil {
		// Refused when the case was added; reaching here means the row was
		// written some other way, and the comparison cannot be made.
		return append(results, CheckResult{CheckName: CheckKindMatchesExpected, Passed: false, Count: 1, Detail: "case expected_answer unreadable: " + err.Error()})
	}
	if expected == nil {
		return results
	}
	kind := CheckResult{CheckName: CheckKindMatchesExpected, Passed: answer.Kind == expected.Kind}
	if !kind.Passed {
		kind.Count, kind.Detail = 1, fmt.Sprintf("expected %s, answered %s", expected.Kind, answer.Kind)
	}
	results = append(results, kind)
	if expected.Kind != "question" || answer.Kind != "question" {
		return results
	}

	got := len(answer.Questions)
	count := CheckResult{CheckName: CheckQuestionCountMatchesExpected, Passed: true}
	if len(expected.Questions) > 0 {
		if got != len(expected.Questions) {
			count.Passed, count.Count = false, abs(got-len(expected.Questions))
			count.Detail = fmt.Sprintf("expected %d questions, answered %d", len(expected.Questions), got)
		}
	} else if got < expected.MinimumQuestions || got > expected.MaximumQuestions {
		count.Passed, count.Count = false, 1
		count.Detail = fmt.Sprintf("expected %d to %d questions, answered %d", expected.MinimumQuestions, expected.MaximumQuestions, got)
	}
	results = append(results, count)
	if len(expected.Questions) == 0 || !count.Passed {
		return results
	}

	var mismatches []string
	for i, question := range expected.Questions {
		if question.Options == nil {
			continue
		}
		if offered := len(answer.Questions[i].Options); offered != *question.Options {
			mismatches = append(mismatches, fmt.Sprintf("question %d: expected %d options, answered %d", i+1, *question.Options, offered))
		}
	}
	return append(results, CheckResult{CheckName: CheckOptionCountsMatchExpected, Passed: len(mismatches) == 0, Count: len(mismatches),
		Detail: listDetail("questions with the wrong number of options", mismatches)})
}

// Choices compares models on the shape of the verdict: its kind and, for a
// question, how many questions it split the turn into.
func (turnEndSignalChecker) Choices(_ *Case, raw json.RawMessage) map[string]string {
	var answer turnEndSignalAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return nil
	}
	choice := answer.Kind
	if answer.Kind == "question" {
		choice = fmt.Sprintf("question×%d", len(answer.Questions))
	}
	return map[string]string{"verdict": choice}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}
