package evalstore

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"
)

// Checker reads a suite's cases and answers without a model: it computes a
// case's features at import, checks an answer, and says what an answer chose
// for each unit (an email, a message) so two models' choices can be compared.
type Checker interface {
	Description() string
	// Features are numbers about the case that cost projection and reports
	// use, e.g. how many cards and emails a filing prompt holds.
	Features(c *Case) map[string]float64
	// Check checks one answer to the case. It is called only for an answer
	// that exists; a missing answer is the runner's to record.
	Check(c *Case, answer json.RawMessage) []CheckResult
	// Choices maps each unit of the case to what the answer chose for it,
	// or nil when the task has no per-unit choice to compare.
	Choices(c *Case, answer json.RawMessage) map[string]string
}

// ExpectedAnswerReader is a checker that compares an answer with what the
// case says a right answer must hold. ValidateExpectedAnswer runs when a case
// is added, so a malformed expectation is refused then rather than failing
// every answer later; an empty one is allowed when the checker can still
// check something without it.
type ExpectedAnswerReader interface {
	ValidateExpectedAnswer(expected json.RawMessage) error
}

// checkers is every checker a suite may name.
var checkers = map[string]Checker{
	"email-filing":    emailFilingChecker{},
	"demo-mail":       demoMailChecker{},
	"turn-end-signal": turnEndSignalChecker{},
}

// CheckerNames lists the registered checkers.
func CheckerNames() []string {
	names := make([]string, 0, len(checkers))
	for name := range checkers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// CheckAnswered is the check every answer gets: did the model produce one.
const CheckAnswered = "answered"

func runChecker(checker Checker, c *Case, a *Answer) []CheckResult {
	if !a.HasAnswer() {
		detail := a.Error
		if detail == "" {
			detail = "no parsed answer"
		}
		return []CheckResult{{CheckName: CheckAnswered, Passed: false, Count: 1, Detail: detail}}
	}
	return append([]CheckResult{{CheckName: CheckAnswered, Passed: true}}, checker.Check(c, a.Parsed)...)
}

// emailFilingChecker checks scheduler's email-classifier filing answers.
// The prompt lists cards as card_id=<uuid> and emails as lines starting
// "--- email_ref=<ref>"; the answer files each email to cards.
type emailFilingChecker struct{}

var (
	emailFilingCardPattern  = regexp.MustCompile(`card_id=([0-9a-f-]{36})`)
	emailFilingEmailPattern = regexp.MustCompile(`(?m)^--- email_ref=(\S+)`)
)

// Email-filing check names.
const (
	CheckEveryEmailFiledOnce  = "every_email_filed_once"
	CheckNoInventedCardID     = "no_invented_card_id"
	CheckEveryFilingHasCard   = "every_filing_has_a_card"
	CheckLinkLabelWithinLimit = "link_label_within_50_characters"
	CheckNewCards             = "new_cards"
	CheckCardUpdates          = "card_updates"
)

const emailFilingLinkLabelLimit = 50

type emailFilingAnswer struct {
	Filings []struct {
		EmailRef  string   `json:"email_ref"`
		CardIDs   []string `json:"card_ids"`
		LinkLabel string   `json:"link_label"`
		NewCard   *struct {
			Title string `json:"title"`
		} `json:"new_card"`
	} `json:"filings"`
	CardUpdates []struct {
		CardID string `json:"card_id"`
	} `json:"card_updates"`
}

func (emailFilingChecker) Description() string {
	return "email filing: every email filed exactly once, no card id absent from the prompt, every filing names a card, " +
		"labels within 50 characters; counts new cards and card updates; compares the cards chosen per email"
}

func (emailFilingChecker) prompt(c *Case) (cards map[string]bool, emailRefs []string) {
	cards = map[string]bool{}
	for _, match := range emailFilingCardPattern.FindAllStringSubmatch(c.UserPrompt, -1) {
		cards[match[1]] = true
	}
	for _, match := range emailFilingEmailPattern.FindAllStringSubmatch(c.UserPrompt, -1) {
		emailRefs = append(emailRefs, match[1])
	}
	return cards, emailRefs
}

func (checker emailFilingChecker) Features(c *Case) map[string]float64 {
	cards, emailRefs := checker.prompt(c)
	return map[string]float64{"cards": float64(len(cards)), "emails": float64(len(emailRefs))}
}

func (checker emailFilingChecker) Check(c *Case, raw json.RawMessage) []CheckResult {
	var answer emailFilingAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return []CheckResult{{CheckName: CheckEveryEmailFiledOnce, Passed: false, Count: 1, Detail: "answer is not a filing: " + err.Error()}}
	}
	cards, emailRefs := checker.prompt(c)

	filed := make([]string, len(answer.Filings))
	for i, f := range answer.Filings {
		filed[i] = f.EmailRef
	}
	coverage := CheckResult{CheckName: CheckEveryEmailFiledOnce, Passed: true}
	if !sameSet(filed, emailRefs) || len(filed) != len(emailRefs) {
		missing, extra := difference(emailRefs, filed), difference(filed, emailRefs)
		coverage.Passed, coverage.Count = false, len(missing)+len(extra)+len(filed)-len(uniqueStrings(filed))
		coverage.Detail = fmt.Sprintf("prompt has %d emails, answer files %d; missing %v, not in prompt %v", len(emailRefs), len(filed), missing, extra)
	}

	var invented []string
	for _, f := range answer.Filings {
		for _, id := range f.CardIDs {
			if !cards[id] {
				invented = append(invented, id)
			}
		}
	}
	for _, update := range answer.CardUpdates {
		if !cards[update.CardID] {
			invented = append(invented, update.CardID)
		}
	}
	inventedResult := CheckResult{CheckName: CheckNoInventedCardID, Passed: len(invented) == 0, Count: len(invented)}
	if len(invented) > 0 {
		inventedResult.Detail = fmt.Sprintf("card ids not in the prompt: %v", invented)
	}

	var withoutCard, longLabels []string
	newCards := 0
	for _, f := range answer.Filings {
		if len(f.CardIDs) == 0 && f.NewCard == nil {
			withoutCard = append(withoutCard, f.EmailRef)
		}
		if utf8.RuneCountInString(f.LinkLabel) > emailFilingLinkLabelLimit {
			longLabels = append(longLabels, f.EmailRef)
		}
		if f.NewCard != nil {
			newCards++
		}
	}
	return []CheckResult{
		coverage,
		inventedResult,
		{CheckName: CheckEveryFilingHasCard, Passed: len(withoutCard) == 0, Count: len(withoutCard), Detail: listDetail("filings with no card", withoutCard)},
		{CheckName: CheckLinkLabelWithinLimit, Passed: len(longLabels) == 0, Count: len(longLabels), Detail: listDetail("labels over 50 characters on", longLabels)},
		{CheckName: CheckNewCards, Passed: true, Count: newCards},
		{CheckName: CheckCardUpdates, Passed: true, Count: len(answer.CardUpdates)},
	}
}

// Choices maps each filed email to the sorted cards it went to and the title
// of any new card, so two answers agree on an email only when both match.
func (emailFilingChecker) Choices(c *Case, raw json.RawMessage) map[string]string {
	var answer emailFilingAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return map[string]string{}
	}
	choices := make(map[string]string, len(answer.Filings))
	for _, f := range answer.Filings {
		cardIDs := slices.Sorted(slices.Values(f.CardIDs))
		newCard := "no new card"
		if f.NewCard != nil {
			newCard = "new card titled " + f.NewCard.Title
		}
		choices[f.EmailRef] = strings.Join(cardIDs, ",") + " | " + newCard
	}
	return choices
}

// demoMailChecker checks scheduler's demo-mail generator answers. The
// prompt writes each message's brief as a block starting "--- key: <key>"
// with a "from: Name <address>" line; the answer has one message per key.
type demoMailChecker struct{}

var (
	demoMailKeyPattern   = regexp.MustCompile(`(?m)^--- key: (\S+)\s*$`)
	demoMailFromPattern  = regexp.MustCompile(`(?m)^from: ([^<\n]+?)\s*<`)
	demoMailGreetingWord = regexp.MustCompile(`(?i)^\W*(hi|hey|hello|dear|greetings|morning|afternoon|evening|good (morning|afternoon|evening))\b`)
)

// Demo-mail check names.
const (
	CheckMessageKeysMatchBrief  = "message_keys_match_brief"
	CheckSenderDoesNotGreetSelf = "sender_does_not_greet_self"
)

type demoMailBrief struct {
	key        string
	senderName string
}

type demoMailAnswer struct {
	Messages []struct {
		Key  string `json:"key"`
		Body string `json:"body"`
	} `json:"messages"`
}

func (demoMailChecker) Description() string {
	return "demo mail: one message per key in the brief, and no sender greeting themselves by name"
}

func (demoMailChecker) briefs(c *Case) []demoMailBrief {
	locations := demoMailKeyPattern.FindAllStringSubmatchIndex(c.UserPrompt, -1)
	briefs := make([]demoMailBrief, 0, len(locations))
	for i, location := range locations {
		end := len(c.UserPrompt)
		if i+1 < len(locations) {
			end = locations[i+1][0]
		}
		block := c.UserPrompt[location[1]:end]
		brief := demoMailBrief{key: c.UserPrompt[location[2]:location[3]]}
		if from := demoMailFromPattern.FindStringSubmatch(block); from != nil {
			brief.senderName = strings.TrimSpace(from[1])
		}
		briefs = append(briefs, brief)
	}
	return briefs
}

func (checker demoMailChecker) Features(c *Case) map[string]float64 {
	return map[string]float64{"messages": float64(len(checker.briefs(c)))}
}

func (checker demoMailChecker) Check(c *Case, raw json.RawMessage) []CheckResult {
	var answer demoMailAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return []CheckResult{{CheckName: CheckMessageKeysMatchBrief, Passed: false, Count: 1, Detail: "answer is not a set of messages: " + err.Error()}}
	}
	briefs := checker.briefs(c)
	briefKeys := make([]string, len(briefs))
	senderByKey := map[string]string{}
	for i, b := range briefs {
		briefKeys[i] = b.key
		senderByKey[b.key] = b.senderName
	}
	answerKeys := make([]string, len(answer.Messages))
	for i, m := range answer.Messages {
		answerKeys[i] = m.Key
	}
	keys := CheckResult{CheckName: CheckMessageKeysMatchBrief, Passed: true}
	if !sameSet(answerKeys, briefKeys) || len(answerKeys) != len(briefKeys) {
		missing, extra := difference(briefKeys, answerKeys), difference(answerKeys, briefKeys)
		keys.Passed, keys.Count = false, len(missing)+len(extra)+len(answerKeys)-len(uniqueStrings(answerKeys))
		keys.Detail = fmt.Sprintf("brief has keys %v, answer has %v", briefKeys, answerKeys)
	}
	var selfGreeting []string
	for _, m := range answer.Messages {
		if greetsByName(m.Body, firstName(senderByKey[m.Key])) {
			selfGreeting = append(selfGreeting, m.Key)
		}
	}
	return []CheckResult{
		keys,
		{CheckName: CheckSenderDoesNotGreetSelf, Passed: len(selfGreeting) == 0, Count: len(selfGreeting),
			Detail: listDetail("messages whose greeting names their own sender", selfGreeting)},
	}
}

func (demoMailChecker) Choices(*Case, json.RawMessage) map[string]string { return nil }

func firstName(fullName string) string {
	fields := strings.Fields(fullName)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// greetsByName says whether the body's first line greets name: a greeting
// word followed by the name ("Hi Ada,"), or the name alone as the opening
// ("Ada —"). A sign-off with the sender's name is normal and not looked at.
func greetsByName(body, name string) bool {
	if name == "" {
		return false
	}
	var opening string
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) != "" {
			opening = strings.TrimSpace(line)
			break
		}
	}
	namePattern := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\b`)
	if demoMailGreetingWord.MatchString(opening) {
		return namePattern.MatchString(opening)
	}
	return regexp.MustCompile(`(?i)^` + regexp.QuoteMeta(name) + `\s*[,!:—–-]`).MatchString(opening)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var unique []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			unique = append(unique, value)
		}
	}
	return unique
}

func sameSet(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(uniqueStrings(a))), slices.Sorted(slices.Values(uniqueStrings(b))))
}

// difference is the values of a that are not in b.
func difference(a, b []string) []string {
	inB := map[string]bool{}
	for _, value := range b {
		inB[value] = true
	}
	var out []string
	for _, value := range uniqueStrings(a) {
		if !inB[value] {
			out = append(out, value)
		}
	}
	return out
}

func listDetail(what string, items []string) string {
	if len(items) == 0 {
		return ""
	}
	return fmt.Sprintf("%s: %v", what, items)
}
