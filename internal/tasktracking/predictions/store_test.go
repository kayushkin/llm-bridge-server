package predictions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// testContext is the context every store call in these tests runs under.
var testContext = context.Background()

// newTestStore gives the test a store on a Postgres database of its own, made
// from a template with the module migrated (postgresstorage/postgrestest).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	database := postgrestest.NewDatabase(t, module)
	return New(database.Open(module))
}

// ageDueDate moves a stored deadline into the past by writing it straight to
// the row, which is what the passage of time looks like from the row's point of
// view. Tests that need an OVERDUE prediction have to go this way now: Create
// and Patch both refuse a deadline that has already passed, so a row can no
// longer be born late, only become late.
func ageDueDate(t *testing.T, s *Store, id string, dueAt int64) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE predictions SET due_at = $1 WHERE seq = $2`, dueAt, mustNumberOf(t, id)); err != nil {
		t.Fatalf("age due date: %v", err)
	}
}

func mustNumberOf(t *testing.T, id string) int64 {
	t.Helper()
	seq, err := predictionNumberOf(id)
	if err != nil {
		t.Fatalf("number of %s: %v", id, err)
	}
	return seq
}

func mustCreate(t *testing.T, s *Store, p *Prediction) *Prediction {
	t.Helper()
	created, err := s.Create(testContext, p)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return created
}

func samplePrediction() *Prediction {
	return &Prediction{
		Claim:              "PATCH with an unknown field returns 400",
		ResolutionCriteria: "curl a PATCH with a misspelled key against a throwaway job; read the status",
		Category:           "behavior",
		Provenance:         "inferred",
		Author:             "claude-code",
		Probability:        0.85,
		Tags:               []string{"scheduler", "api"},
	}
}

func TestCreateAssignsAPrefixedID(t *testing.T) {
	s := newTestStore(t)
	first := mustCreate(t, s, samplePrediction())
	if first.ID != "prediction_000001" {
		t.Fatalf("first id = %q, want prediction_000001", first.ID)
	}
	second := mustCreate(t, s, samplePrediction())
	if second.ID != "prediction_000002" {
		t.Fatalf("second id = %q, want prediction_000002", second.ID)
	}
	// The prefix is what keeps this store out of every bare-uuid resolution in
	// chat, and keeps '/' out of a ref kanban-store splits on.
	if strings.Contains(first.ID, "/") {
		t.Fatalf("id %q contains a slash", first.ID)
	}
}

func TestResolutionCriteriaIsRequired(t *testing.T) {
	s := newTestStore(t)
	p := samplePrediction()
	p.ResolutionCriteria = "   "
	_, err := s.Create(testContext, p)
	if !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
	if !strings.Contains(err.Error(), "OF WHAT") {
		t.Fatalf("error should say what is missing and why, got: %v", err)
	}
}

func TestOffLadderProbabilityIsRefusedAndNamesTheRungs(t *testing.T) {
	s := newTestStore(t)
	p := samplePrediction()
	p.Probability = 0.73
	_, err := s.Create(testContext, p)
	if !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
	if !strings.Contains(err.Error(), "0.7") || !strings.Contains(err.Error(), "0.75") {
		t.Fatalf("error should name the two nearest rungs, got: %v", err)
	}
}

func TestCertaintyIsNotAProbability(t *testing.T) {
	s := newTestStore(t)
	for _, p := range []float64{0, 1, -0.5, 1.5} {
		input := samplePrediction()
		input.Probability = p
		if _, err := s.Create(testContext, input); !errors.Is(err, ErrInvalidPrediction) {
			t.Fatalf("probability %v was accepted", p)
		}
	}
}

func TestUnknownCategoryNamesTheVocabulary(t *testing.T) {
	s := newTestStore(t)
	p := samplePrediction()
	p.Category = "vibes"
	_, err := s.Create(testContext, p)
	if !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
	for _, c := range Categories {
		if !strings.Contains(err.Error(), c) {
			t.Fatalf("error should list %q, got: %v", c, err)
		}
	}
}

func TestCategorySynonymsResolve(t *testing.T) {
	s := newTestStore(t)
	p := samplePrediction()
	p.Category = "Diagnosis"
	created := mustCreate(t, s, p)
	if created.Category != "root-cause" {
		t.Fatalf("category = %q, want root-cause", created.Category)
	}
}

func TestUnknownProvenanceIsRefused(t *testing.T) {
	s := newTestStore(t)
	p := samplePrediction()
	p.Provenance = "hunch"
	if _, err := s.Create(testContext, p); !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
}

func TestTheOpeningOddsAreThemselvesAnEstimate(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	if len(created.Estimates) != 1 {
		t.Fatalf("estimates = %d, want 1 — the trail should start full", len(created.Estimates))
	}
	if created.Estimates[0].Probability != 0.85 {
		t.Fatalf("opening estimate = %v, want 0.85", created.Estimates[0].Probability)
	}
}

func TestRevisingKeepsTheOldNumberAndFreezesTheFirst(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	updated, err := s.AddEstimate(testContext, created.ID, &Estimate{
		Probability: 0.3,
		Rationale:   "read the row back; the field was dropped",
		Provenance:  "measured",
	})
	if err != nil {
		t.Fatalf("add estimate: %v", err)
	}
	if updated.Probability != 0.3 {
		t.Fatalf("current probability = %v, want 0.3", updated.Probability)
	}
	if updated.InitialProbability != 0.85 {
		t.Fatalf("initial probability = %v, want 0.85 — the opening number must not move", updated.InitialProbability)
	}
	if len(updated.Estimates) != 2 {
		t.Fatalf("estimates = %d, want 2 — the old number must survive", len(updated.Estimates))
	}
	if updated.Estimates[0].Probability != 0.85 {
		t.Fatalf("first estimate = %v, want 0.85", updated.Estimates[0].Probability)
	}
	if updated.Provenance != "measured" {
		t.Fatalf("provenance = %q, want measured — a revision that measured something upgrades the provenance", updated.Provenance)
	}
}

func TestRevisingAfterTheAnswerIsKnownIsRefused(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	if _, err := s.Resolve(testContext, created.ID, "true", "measured live"); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	_, err := s.AddEstimate(testContext, created.ID, &Estimate{Probability: 0.99, Provenance: "measured"})
	if !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("err = %v, want ErrAlreadyResolved", err)
	}
}

func TestResolvingTwiceIsRefused(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	if _, err := s.Resolve(testContext, created.ID, "true", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := s.Resolve(testContext, created.ID, "false", ""); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("err = %v, want ErrAlreadyResolved", err)
	}
}

func TestVoidIsNotAMiss(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	voided, err := s.Resolve(testContext, created.ID, "void", "the work was canceled")
	if err != nil {
		t.Fatalf("resolve void: %v", err)
	}
	if voided.Status != "void" || voided.Outcome != "" {
		t.Fatalf("status/outcome = %q/%q, want void/empty", voided.Status, voided.Outcome)
	}
	report, err := s.Calibration(testContext, Filter{}, "")
	if err != nil {
		t.Fatalf("calibration: %v", err)
	}
	if report.Overall.N != 0 {
		t.Fatalf("scored %d predictions, want 0 — a void claim is not a wrong one", report.Overall.N)
	}
	if report.Overall.Void != 1 {
		t.Fatalf("void count = %d, want 1", report.Overall.Void)
	}
}

func TestPatchCannotMoveTheNumberOrTheOutcome(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	newClaim := "PATCH with an unknown field returns 400, measured on a throwaway job"
	updated, err := s.Patch(testContext, created.ID, Patch{Claim: &newClaim})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.Claim != newClaim {
		t.Fatalf("claim did not change")
	}
	if updated.Probability != 0.85 || updated.Status != "open" {
		t.Fatalf("patch moved the number or the status: %v %q", updated.Probability, updated.Status)
	}
	// Patch has nowhere to put a probability or an outcome; the HTTP layer
	// turns an attempt into a 400 naming the right route (see server_test.go).
}

func TestTagFilterMatchesWholeTagsOnly(t *testing.T) {
	s := newTestStore(t)
	p := samplePrediction()
	p.Tags = []string{"scheduler-api"}
	mustCreate(t, s, p)
	got, err := s.List(testContext, Filter{Tag: "scheduler"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("tag %q matched %q", "scheduler", "scheduler-api")
	}
	got, err = s.List(testContext, Filter{Tag: "scheduler-api"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("whole-tag match returned %d rows, want 1", len(got))
	}
}

func TestSearchFindsClaimAndCriteria(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, samplePrediction())
	got, err := s.List(testContext, Filter{Query: "throwaway"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("search over resolution_criteria returned %d rows, want 1", len(got))
	}
}

func TestBadSearchQueryIsTheCallersFault(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, samplePrediction())
	if _, err := s.List(testContext, Filter{Query: `"unclosed`}); !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
}

func TestSoftDeleteHidesAndRestoreBringsBack(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	if err := s.SoftDelete(testContext, created.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	got, _ := s.List(testContext, Filter{})
	if len(got) != 0 {
		t.Fatalf("deleted prediction still listed")
	}
	if _, err := s.Restore(testContext, created.ID); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ = s.List(testContext, Filter{})
	if len(got) != 1 {
		t.Fatalf("restore did not bring it back")
	}
}

func TestLinkingIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	link := &Link{EntityType: "note", EntityRef: "f1db12e8-6857-46d9-80ae-9bc4e0d7c200", Label: "the todo"}
	if _, err := s.AddLink(testContext, created.ID, link); err != nil {
		t.Fatalf("add link: %v", err)
	}
	if _, err := s.AddLink(testContext, created.ID, link); err != nil {
		t.Fatalf("re-link: %v", err)
	}
	links, err := s.ListLinks(testContext, created.ID)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1 — re-linking must be idempotent", len(links))
	}
}

func TestLinkNeedsBothTypeAndRef(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	if _, err := s.AddLink(testContext, created.ID, &Link{EntityType: "note"}); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("err = %v, want ErrInvalidLink", err)
	}
}

func TestOverdueFindsOnlyUnresolvedPastDeadlines(t *testing.T) {
	s := newTestStore(t)
	past := samplePrediction()
	past.DueAt = now() + 86400
	overdue := mustCreate(t, s, past)
	ageDueDate(t, s, overdue.ID, 1)

	future := samplePrediction()
	future.DueAt = now() + 86400
	mustCreate(t, s, future)

	noDeadline := samplePrediction()
	mustCreate(t, s, noDeadline)

	got, err := s.List(testContext, Filter{Overdue: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ID != overdue.ID {
		t.Fatalf("overdue returned %d rows, want just %s", len(got), overdue.ID)
	}
	if _, err := s.Resolve(testContext, overdue.ID, "true", ""); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, _ = s.List(testContext, Filter{Overdue: true})
	if len(got) != 0 {
		t.Fatalf("a resolved prediction is still reported overdue")
	}
}

// The note is prose written in a hurry at the moment of resolving, so a wrong
// one is a wrong record and worth correcting. What must stay put is the outcome
// — and Patch cannot reach it, which is what makes this safe.
func TestTheResolutionNoteCanBeCorrectedAndTheOutcomeCannot(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	if _, err := s.Resolve(testContext, created.ID, "true", "measured live; the field was kept"); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	corrected := "measured live on a throwaway job, not the real one — same result"
	updated, err := s.Patch(testContext, created.ID, Patch{ResolutionNote: &corrected})
	if err != nil {
		t.Fatalf("patch note: %v", err)
	}
	if updated.ResolutionNote != corrected {
		t.Fatalf("note = %q, want %q", updated.ResolutionNote, corrected)
	}
	if updated.Outcome != "true" || updated.Status != "resolved" {
		t.Fatalf("patching the note moved the outcome: status=%q outcome=%q", updated.Status, updated.Outcome)
	}
	if updated.ResolvedAt != created.ResolvedAt && updated.ResolvedAt == 0 {
		t.Fatalf("patching the note cleared resolved_at")
	}
}

// An open row has no resolution, so a note about one is a claim that the row
// settled while it still scores nothing. Refusing it is the whole point.
func TestAnOpenPredictionHasNoResolutionToAnnotate(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())

	note := "it went fine"
	_, err := s.Patch(testContext, created.ID, Patch{ResolutionNote: &note})
	if !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
	if !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("error should name the route that settles it, got %v", err)
	}
	after, err := s.Get(testContext, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.ResolutionNote != "" {
		t.Fatalf("refused patch still wrote the note: %q", after.ResolutionNote)
	}
}

// The mistake this refuses is not hypothetical: three of the live store's
// twenty-nine rows were written with a due date a year BEFORE their own
// created_at, and every read path downstream reported them as merely overdue.
func TestADeadlineThatHasAlreadyPassedIsRefused(t *testing.T) {
	s := newTestStore(t)
	stale := samplePrediction()
	stale.DueAt = now() - 86400

	_, err := s.Create(testContext, stale)
	if !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
	// The message has to name the date it refused, or the caller cannot see that
	// the year is what is wrong with it.
	if !strings.Contains(err.Error(), "due_at") {
		t.Fatalf("error should name the field it refused, got %v", err)
	}

	got, err := s.Count(testContext, Filter{})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != 0 {
		t.Fatalf("refused create still stored %d row(s)", got)
	}
}

// A claim with no deadline is a normal thing to log, and 0 is how it is said.
func TestOmittingTheDeadlineIsStillAllowed(t *testing.T) {
	s := newTestStore(t)
	noDeadline := samplePrediction()
	noDeadline.DueAt = 0
	created := mustCreate(t, s, noDeadline)
	if created.DueAt != 0 {
		t.Fatalf("due_at = %d, want 0", created.DueAt)
	}
}

func TestPatchRefusesToMoveADeadlineIntoThePast(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())

	stale := now() - 86400
	if _, err := s.Patch(testContext, created.ID, Patch{DueAt: &stale}); !errors.Is(err, ErrInvalidPrediction) {
		t.Fatalf("err = %v, want ErrInvalidPrediction", err)
	}
	after, err := s.Get(testContext, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if after.DueAt == stale {
		t.Fatalf("refused patch still moved the deadline to %d", after.DueAt)
	}
}

// A row goes overdue by the clock advancing, not by anyone writing to it. A
// read-modify-write caller that resends the deadline it just read must still be
// able to correct the claim — otherwise the check would freeze every late row.
func TestAnAlreadyLateRowCanStillBeEditedWithItsOwnDeadlineResent(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	ageDueDate(t, s, created.ID, now()-86400)

	current, err := s.Get(testContext, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	corrected := "the same claim, with the typo in it fixed"
	updated, err := s.Patch(testContext, created.ID, Patch{Claim: &corrected, DueAt: &current.DueAt})
	if err != nil {
		t.Fatalf("patching a late row with its own deadline resent: %v", err)
	}
	if updated.Claim != corrected {
		t.Fatalf("claim = %q, want %q", updated.Claim, corrected)
	}
	if updated.DueAt != current.DueAt {
		t.Fatalf("due_at moved from %d to %d", current.DueAt, updated.DueAt)
	}
}

// Correcting the year IS the repair the live rows need, so moving a bad
// deadline forward has to keep working.
func TestADeadlineCanBeCorrectedForwards(t *testing.T) {
	s := newTestStore(t)
	created := mustCreate(t, s, samplePrediction())
	ageDueDate(t, s, created.ID, now()-86400)

	fixed := now() + 30*86400
	updated, err := s.Patch(testContext, created.ID, Patch{DueAt: &fixed})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if updated.DueAt != fixed {
		t.Fatalf("due_at = %d, want %d", updated.DueAt, fixed)
	}
}

// q reads as a web search over claim, criteria, tags and note: every word,
// "a phrase" as a phrase, `or` for either, -word for not. Postgres reads any
// text that way, so the store itself refuses the two queries that cannot
// mean anything.
func TestSearchReadsTheQueryAsAWebSearch(t *testing.T) {
	s := newTestStore(t)
	first := samplePrediction()
	first.Claim = "the nightly backup finishes before six, see ~/backups/nightly-run.sh"
	first.Tags = []string{"backups"}
	backup := mustCreate(t, s, first)
	second := samplePrediction()
	second.Claim = "the scheduler restarts cleanly"
	scheduler := mustCreate(t, s, second)
	resolved := mustCreate(t, s, samplePrediction())
	if _, err := s.Resolve(testContext, resolved.ID, "true", "measured on a canary host"); err != nil {
		t.Fatal(err)
	}

	for query, want := range map[string][]string{
		"nightly backup":      {backup.ID},
		`"backup finishes"`:   {backup.ID},
		`"finishes backup"`:   {},
		"nightly or restarts": {scheduler.ID, backup.ID},
		"throwaway -nightly":  {resolved.ID, scheduler.ID},
		"backups":             {backup.ID},
		"canary":              {resolved.ID},
		"NIGHTLY":             {backup.ID},
		"the":                 {resolved.ID, scheduler.ID, backup.ID},
	} {
		found, err := s.List(testContext, Filter{Query: query})
		if err != nil {
			t.Fatalf("q=%s: %v", query, err)
		}
		var ids []string
		for _, p := range found {
			ids = append(ids, p.ID)
		}
		if strings.Join(ids, ",") != strings.Join(want, ",") {
			t.Errorf("q=%s found %v, want %v", query, ids, want)
		}
	}
	for _, query := range []string{`"unclosed`, `!!!`, `-`} {
		if _, err := s.List(testContext, Filter{Query: query}); !errors.Is(err, ErrInvalidPrediction) {
			t.Errorf("q=%s: err = %v, want ErrInvalidPrediction", query, err)
		}
	}
}

// An id names a prediction only in exactly the form the store writes it, as
// when the id was a stored string.
func TestOnlyTheExactIdFormNamesAPrediction(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, samplePrediction())
	for _, id := range []string{"prediction_0000001", "prediction_1", "prediction_00000a", "note_000001", "", "prediction_000002"} {
		if _, err := s.Get(testContext, id); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
	if _, err := s.Get(testContext, "prediction_000001"); err != nil {
		t.Fatal(err)
	}
}

// A purged number is never handed out again, so an id quoted in a chat or a
// link can never come to name another claim.
func TestAPurgedNumberIsNotReused(t *testing.T) {
	s := newTestStore(t)
	mustCreate(t, s, samplePrediction())
	second := mustCreate(t, s, samplePrediction())
	if err := s.Purge(testContext, second.ID); err != nil {
		t.Fatal(err)
	}
	third := mustCreate(t, s, samplePrediction())
	if third.ID != "prediction_000003" {
		t.Fatalf("after purging %s the next id is %s, want prediction_000003", second.ID, third.ID)
	}
}

// A create refused for a bad link stores nothing and uses up no number.
func TestABadLinkRefusesTheWholeCreate(t *testing.T) {
	s := newTestStore(t)
	input := samplePrediction()
	input.Links = []*Link{{EntityType: "note"}}
	if _, err := s.Create(testContext, input); !errors.Is(err, ErrInvalidLink) {
		t.Fatalf("err = %v, want ErrInvalidLink", err)
	}
	if created := mustCreate(t, s, samplePrediction()); created.ID != "prediction_000001" {
		t.Fatalf("first stored id = %s, want prediction_000001", created.ID)
	}
}

func TestTagsAreCountedMostUsedFirst(t *testing.T) {
	s := newTestStore(t)
	for _, tags := range [][]string{{"b", "a"}, {"a"}, {"B", " "}} {
		p := samplePrediction()
		p.Tags = tags
		mustCreate(t, s, p)
	}
	counted, err := s.ListTags(testContext)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tag := range counted {
		got = append(got, fmt.Sprintf("%s=%d", tag.Tag, tag.Count))
	}
	if strings.Join(got, ",") != "a=2,B=1,b=1" {
		t.Fatalf("tags = %v, want a=2,B=1,b=1 (ties in byte order, blank tags dropped)", got)
	}
}
