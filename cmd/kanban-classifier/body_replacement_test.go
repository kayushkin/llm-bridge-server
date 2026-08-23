package main

import (
	"encoding/json"
	"fmt"
	"github.com/slava/scheduler/internal/degradations"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/slava/scheduler/internal/kanbanclient"
)

// This file states what a classifier state update does to a card body. It
// decides nothing.
//
// Card adfff493-8b12-433e-8b29-b38c92bd9238 reserves the decision — whether a
// classifier-owned body should keep replacing, start appending, own a marked
// region, or patch only when it changed. All four repairs move the behaviour
// pinned below, and before this file every one of them could have landed, or
// the behaviour could have drifted the other way, with the suite green.
//
// A kanban card id IS a noteboard item id, so the row an update overwrites is a
// type:todo row in the user's single source of truth for work.
//
// ⚠️ The card's mechanism sentence — "it never reads the body it replaces" — is
// wrong, and wrong in the direction that makes the defect subtler. The old body
// IS fetched (ListCardsForEntity puts it in EntityCardEntry.Item["body"]) and IS
// shown to the model, but only the first 400 characters of it, and under budget
// pressure none of it at all. The overwrite is not blind. It is partly sighted,
// which is worse: the model can be asked to carry text forward, and it cannot
// see past character 400, and when the section overflows it sees nothing.
// TestTheModelIsShownAtMost400CharactersOfTheBodyItWillReplace and
// TestUnderBudgetPressureTheModelIsShownNoneOfTheBodyAtAll pin that.

// bodyReplacementStub records every request the classifier makes, not just the
// bodies it writes. The claim "nothing reads the card before replacing it" is
// about requests that were never sent, so the request log is the subject and
// has to be captured rather than inferred.
type bodyReplacementStub struct {
	requests      []string          // "METHOD /path", in the order they arrived
	patchedBodies map[string]string // card id -> body sent by PatchCardBody
	patchedFields map[string][]string
	patchCount    map[string]int // card id -> how many PATCHes it received
	failMoves     bool
}

func newBodyReplacementStub(t *testing.T) (*kanbanclient.Client, *bodyReplacementStub) {
	t.Helper()
	rec := &bodyReplacementStub{
		patchedBodies: map[string]string{},
		patchedFields: map[string][]string{},
		patchCount:    map[string]int{},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var payload map[string]any
		_ = json.Unmarshal(raw, &payload)
		path := r.URL.Path
		rec.requests = append(rec.requests, r.Method+" "+path)

		switch {
		case r.Method == http.MethodPatch && strings.HasPrefix(path, "/api/cards/"):
			cardID := strings.TrimPrefix(path, "/api/cards/")
			body, _ := payload["body"].(string)
			rec.patchedBodies[cardID] = body
			rec.patchCount[cardID]++
			fields := make([]string, 0, len(payload))
			for k := range payload {
				fields = append(fields, k)
			}
			rec.patchedFields[cardID] = fields
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		case r.Method == http.MethodPost && strings.HasSuffix(path, "/move"):
			if rec.failMoves {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"column is gone"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))

		default:
			t.Errorf("stub kanban got an unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	kc := kanbanclient.New(srv.URL, "")
	kc.BoardID = "board-1"
	kc.ColumnsByName = map[string]string{
		"Backlog": "col-backlog", "In Progress": "col-inprogress",
		"Blocked": "col-blocked", "Review": "col-review", "Done": "col-done",
		"Failed": "col-failed",
	}
	return kc, rec
}

func updateSession() session {
	return session{
		SessionID: "br_1784567616625538966",
		Harness:   "claudecode",
		Purpose:   "autoworker",
		State:     "idle",
	}
}

// ==================== What the update path writes ====================

// The whole finding, stated as an equality rather than as an absence. The body
// that goes out is built from scratch out of two things the classifier minted
// this tick — the model's summary and the trailer — so every byte is accounted
// for and there is nowhere for a previous body to have survived.
//
// A "does not contain the old text" assertion would be weaker: it passes for a
// body that kept nine tenths of the old text and dropped the tenth.
func TestAnUpdateSendsABodyMadeOnlyOfThisTicksSummaryAndTrailer(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)
	sess := updateSession()

	applyExisting(kc, sess, &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "in_progress", Summary: "still working on the resolver"},
	}}, degradations.New("classifier"))

	got := rec.patchedBodies["card-1"]
	rest := strings.TrimPrefix(got, "still working on the resolver\n\n")
	if rest == got {
		t.Fatalf("body does not open with this tick's summary: %q", got)
	}
	rest = strings.TrimSuffix(rest, "\n")
	if !strings.HasPrefix(rest, "_updated by classifier @ ") || !strings.HasSuffix(rest, "_") {
		t.Fatalf("what follows the summary is not the trailer alone: %q", rest)
	}
	if strings.Contains(rest, "\n") {
		t.Fatalf("the trailer is not the last thing in the body; something else follows it: %q", got)
	}
}

// The PATCH carries the body and nothing else, so an update cannot restore a
// title, a tag or a status that a previous body's card had. Named because a
// repair that starts sending more fields changes the blast radius of an update
// and should have to say so.
func TestAnUpdatePatchesTheBodyFieldAndNoOtherField(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)

	applyExisting(kc, updateSession(), &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "done", Summary: "finished"},
	}}, degradations.New("classifier"))

	fields := rec.patchedFields["card-1"]
	if len(fields) != 1 || fields[0] != "body" {
		t.Fatalf("an update patches exactly one field, body; got %v", fields)
	}
}

// The read-before-write option the card lists as (4), stated as the absence it
// currently is. Nothing between the move and the patch asks kanban-store what
// the card says now, so the classifier cannot compare and cannot decline.
func TestNothingReadsTheCardBetweenDecidingToUpdateItAndReplacingIt(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)

	applyExisting(kc, updateSession(), &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "in_progress", Summary: "still going"},
	}}, degradations.New("classifier"))

	want := []string{
		"POST /api/cards/card-1/move",
		"PATCH /api/cards/card-1",
	}
	if !reflect.DeepEqual(rec.requests, want) {
		t.Fatalf("the update path's requests changed.\n got: %v\nwant: %v", rec.requests, want)
	}
	for _, r := range rec.requests {
		if strings.HasPrefix(r, "GET ") {
			t.Fatalf("something now reads the card before replacing it: %v", rec.requests)
		}
	}
}

// applyExisting cannot preserve the old body because it is never handed one.
// Its only inputs are the client, the session, the model's report and the
// shortfall recorder, and the report's per-card record has four fields, none of
// them the body. Any repair under options 2, 3 or 4 has to change this shape or
// add a fetch — so this is
// the pin that makes the change visible.
func TestTheRecordDrivingAnUpdateCarriesNoPriorBody(t *testing.T) {
	rt := reflect.TypeOf(ExistingRec{})
	got := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		got = append(got, rt.Field(i).Name)
	}
	want := []string{"CardID", "State", "Summary", "Blocker"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExistingRec's shape changed.\n got: %v\nwant: %v", got, want)
	}

	ft := reflect.TypeOf(applyExisting)
	if ft.NumIn() != 4 {
		t.Fatalf("applyExisting takes %d arguments, want 4", ft.NumIn())
	}
	if in := ft.In(2).String(); in != "*main.Report" {
		t.Fatalf("applyExisting's third argument is %s; the report is the only carrier of what a body becomes", in)
	}
	if in := ft.In(3).String(); in != "*degradations.Recorder" {
		t.Fatalf("applyExisting's fourth argument is %s, want the shortfall recorder", in)
	}
}

// The maximal case, and nothing guards it. A model that returns an empty
// summary for a card — which the schema permits, since it requires the field
// and not its content — leaves that card holding a trailer and no text.
func TestAnEmptySummaryStillReplacesTheBodyLeavingOnlyTheTrailer(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)

	applyExisting(kc, updateSession(), &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "in_progress", Summary: ""},
	}}, degradations.New("classifier"))

	got, ok := rec.patchedBodies["card-1"]
	if !ok {
		t.Fatalf("an empty summary produced no patch at all; got %v", rec.patchedBodies)
	}
	if !strings.HasPrefix(got, "\n\n_updated by classifier @ ") {
		t.Fatalf("an empty summary should leave the trailer and nothing before it; got %q", got)
	}
}

// Option (4) again, from the other side: sameness is not a reason to skip. Two
// ticks that agree word for word still send two writes, and noteboard snapshots
// a revision for each, so an unchanged card still accumulates history.
func TestAnUnchangedSummaryStillSendsASecondWrite(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)
	sess := updateSession()
	report := &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "in_progress", Summary: "still working on the resolver"},
	}}

	applyExisting(kc, sess, report, degradations.New("classifier"))
	applyExisting(kc, sess, report, degradations.New("classifier"))

	if rec.patchCount["card-1"] != 2 {
		t.Fatalf("two identical ticks sent %d writes, want 2 — a skip would mean read-before-write landed",
			rec.patchCount["card-1"])
	}
}

// ==================== The blast radius, as it stands ====================

// Two guards already narrow which cards get replaced, and a repair must not
// lose them: an unknown column means the card is never touched at all.
func TestACardWhoseColumnIsUnknownIsNeverWrittenTo(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)
	delete(kc.ColumnsByName, "Blocked")

	applyExisting(kc, updateSession(), &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "blocked", Summary: "would have replaced the body"},
		{CardID: "card-2", State: "done", Summary: "this one is fine"},
	}}, degradations.New("classifier"))

	if _, patched := rec.patchedBodies["card-1"]; patched {
		t.Fatalf("a card with no column was written to anyway: %q", rec.patchedBodies["card-1"])
	}
	if _, patched := rec.patchedBodies["card-2"]; !patched {
		t.Fatalf("the guard swallowed the whole loop; card-2 should still have been updated")
	}
}

// The second guard: a move that fails skips the patch, so a card that could not
// be put in the right column keeps the body it had.
func TestACardWhoseMoveFailsKeepsTheBodyItHad(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)
	rec.failMoves = true

	applyExisting(kc, updateSession(), &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "in_progress", Summary: "would have replaced the body"},
	}}, degradations.New("classifier"))

	if _, patched := rec.patchedBodies["card-1"]; patched {
		t.Fatalf("a card whose move failed was written to anyway: %q", rec.patchedBodies["card-1"])
	}
	if len(rec.requests) != 1 || !strings.HasSuffix(rec.requests[0], "/move") {
		t.Fatalf("want the move alone, got %v", rec.requests)
	}
}

// Instrument guard. Two tests above pass by observing that nothing was written.
// They would pass just as well against an applyExisting that writes nothing
// ever, which is the shape of the vacuous green this floor exists to prevent.
func TestTheAbsenceAssertionsAboveAreNotVacuous(t *testing.T) {
	kc, rec := newBodyReplacementStub(t)

	applyExisting(kc, updateSession(), &Report{ExistingCards: []ExistingRec{
		{CardID: "card-1", State: "in_progress", Summary: "an ordinary tick"},
	}}, degradations.New("classifier"))

	if len(rec.patchedBodies) != 1 {
		t.Fatalf("the ordinary path writes exactly one body; got %v — the absence assertions above prove nothing",
			rec.patchedBodies)
	}
}

// ==================== What the model is shown of what it replaces ====================

// The card says the classifier never reads the body it replaces. It does read
// it. It shows the model 400 characters of it and drops the rest, so a model
// asked to carry text forward could only ever carry the head of it.
func TestTheModelIsShownAtMost400CharactersOfTheBodyItWillReplace(t *testing.T) {
	const head = "HEAD-OF-THE-OLD-BODY "
	tail := "TAIL-OF-THE-OLD-BODY"
	old := head + strings.Repeat("x", 400-len(head)) + tail

	section := buildExistingCardsSection([]kanbanclient.EntityCardEntry{{
		CardID: "card-1",
		Item:   map[string]any{"title": "Port the resolver", "status": "open", "body": old},
	}}, 8000)

	if !strings.Contains(section, head) {
		t.Fatalf("the head of the old body did not reach the model:\n%s", section)
	}
	if strings.Contains(section, tail) {
		t.Fatalf("the digest now carries more than 400 characters of the body:\n%s", section)
	}
	if !strings.Contains(section, "…") {
		t.Fatalf("the body was cut with no mark saying so:\n%s", section)
	}
}

// And when the section overflows its share of the prompt, the bodies go first —
// so on a busy session the model is shown none of what it is about to replace,
// while the system prompt still promises it "their current state and body".
// The existing digest test pins the shedding; this one names what is being shed
// and states that the cards themselves survive it.
func TestUnderBudgetPressureTheModelIsShownNoneOfTheBodyAtAll(t *testing.T) {
	var cards []kanbanclient.EntityCardEntry
	for i := 0; i < 30; i++ {
		cards = append(cards, kanbanclient.EntityCardEntry{
			CardID: fmt.Sprintf("card-%d", i),
			Item: map[string]any{
				"title":  fmt.Sprintf("Task %d", i),
				"status": "open",
				"body":   "A-BODY-A-HUMAN-WROTE " + strings.Repeat("y", 300),
			},
		})
	}

	budget := int(float64(digestCharBudget) * existingCardsBudgetShare)
	section := buildExistingCardsSection(cards, budget)

	if strings.Contains(section, "A-BODY-A-HUMAN-WROTE") {
		t.Fatalf("bodies still reached the model at the shipped budget; the shed threshold moved:\n%s", section)
	}
	if !strings.Contains(section, "card_id=card-29") {
		t.Fatalf("card identity was dropped along with the bodies:\n%s", section)
	}
}

// Where the shedding starts, as a number, because "under pressure" is not a
// measurement. At the shipped budget the bodies survive 19 cards on a session
// and are gone at 20.
func TestBodiesSurviveNineteenCardsOnASessionAndAreGoneAtTwenty(t *testing.T) {
	budget := int(float64(digestCharBudget) * existingCardsBudgetShare)
	body := "A-BODY-A-HUMAN-WROTE " + strings.Repeat("y", 379) // 400 chars, the most the digest carries

	build := func(n int) string {
		var cards []kanbanclient.EntityCardEntry
		for i := 0; i < n; i++ {
			cards = append(cards, kanbanclient.EntityCardEntry{
				CardID: fmt.Sprintf("card-%d", i),
				Item:   map[string]any{"title": fmt.Sprintf("Task %d", i), "status": "open", "body": body},
			})
		}
		return buildExistingCardsSection(cards, budget)
	}

	if !strings.Contains(build(19), "A-BODY-A-HUMAN-WROTE") {
		t.Fatalf("bodies are already shed at 19 cards; the threshold moved down")
	}
	if strings.Contains(build(20), "A-BODY-A-HUMAN-WROTE") {
		t.Fatalf("bodies still survive at 20 cards; the threshold moved up")
	}
}

// ==================== What the model is allowed to say ====================

// The replacement is not the model's choice. summary is required on every
// existing-card record, so a model with nothing to add must still mint one, and
// that minted sentence becomes the card's whole body. The system prompt tells
// it to repeat the current state when it has no opinion; there is no such
// instruction for the summary because there is no way to express it.
func TestAModelWithNoOpinionMustStillMintASummaryThatBecomesTheWholeBody(t *testing.T) {
	schema := reportSchema()
	props := schema["properties"].(map[string]any)
	items := props["existing_cards"].(map[string]any)["items"].(map[string]any)

	required := map[string]bool{}
	for _, r := range items["required"].([]string) {
		required[r] = true
	}
	if !required["summary"] {
		t.Fatalf("summary is no longer required on an existing-card record; the model can now decline to replace")
	}

	fields := items["properties"].(map[string]any)
	for _, name := range []string{"keep_body", "leave_body", "body", "no_change", "unchanged"} {
		if _, ok := fields[name]; ok {
			t.Fatalf("the schema grew %q — a model can now say something about the body it replaces", name)
		}
	}

	if !strings.Contains(systemPrompt, "If you have no opinion, repeat its current state.") {
		t.Fatalf("the no-opinion instruction changed; it covers state only, and that asymmetry is the point")
	}
}

// The system prompt promises the model the body, unconditionally. The digest
// keeps that promise for the first 400 characters, and drops it entirely past
// 19 cards. Pinned together so a repair to either half cannot leave the two
// disagreeing in silence.
func TestThePromptPromisesTheBodyItIsNotAlwaysGiven(t *testing.T) {
	if !strings.Contains(systemPrompt, "with their current state and body") {
		t.Fatalf("the prompt no longer promises the model the card bodies; the digest's shedding is now unremarked")
	}
}
