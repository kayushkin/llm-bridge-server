package peoplestore

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

// newStandaloneDatabase writes a SQLite file the way the standalone store
// kept one, with a few made-up rows and gaps in every numbering: two persons
// (one deleted) with a relationship, identities with and without a label, a
// suggestion accepted into a person and one still waiting whose handle is in
// the legacy table only, a refusal, handle activity, a review, a claim and a
// revision.
func newStandaloneDatabase(t *testing.T) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "standalone_schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "people-store.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("standalone schema: %v", err)
	}
	statements := []string{
		`INSERT INTO persons VALUES ('person_000001', 1, 'Alice Smith', '--05-14', 'book club', '["neighbors"]', 'principal_000001', 1800000000, 1800000100, 0)`,
		`INSERT INTO persons VALUES ('person_000003', 3, 'Bob', '', '', '[]', '', 1800000200, 1800000300, 1800000300)`,
		`INSERT INTO identities VALUES ('identity_000002', 2, 'person_000001', 'email', 'alice@example.com', 'work', 1800000010, 1800000010, 'import', 'linked before provenance', 1700000000)`,
		`INSERT INTO identities VALUES ('identity_000005', 5, 'person_000001', 'matrix_user', '@telegram_42:chat.example.com', '', 1800000020, 1800000020, 'operator', '', 1800000020)`,
		`INSERT INTO relationships VALUES ('relationship_000004', 4, 'person_000003', 'person_000001', 'sibling', 'older', 1800000250)`,
		`INSERT INTO suggestions VALUES ('suggestion_000002', 2, 'multichat', '{"display_name":"Alice","identities":[],"matched_person_ids":[],"source_detail":{"z":1,"a":2}}', 'accepted', 'person_000001', 1800000005, 1800000020)`,
		`INSERT INTO suggestions VALUES ('suggestion_000006', 6, 'multichat', '{"display_name":"Carol","identities":[{"kind":"email","value":"carol@example.com","label":""}],"matched_person_ids":[]}', 'proposed', '', 1800000400, 0)`,
		`INSERT INTO suggestion_handles VALUES ('suggestion_000002', 'matrix_user', '@telegram_42:chat.example.com')`,
		`INSERT INTO suggestion_identities VALUES ('suggestion_000002', 'matrix_user', '@telegram_42:chat.example.com')`,
		`INSERT INTO suggestion_identities VALUES ('suggestion_000006', 'email', 'carol@example.com')`,
		`INSERT INTO identity_refusals VALUES ('refusal_000007', 7, 'email', 'bob@example.com', 'person_000003', 'operator', 'not him', 1800000350)`,
		`INSERT INTO handle_activity VALUES ('email', 'carol@example.com', 'multichat', 1800000500, 9, 1, 1800000600)`,
		`INSERT INTO handle_activity VALUES ('matrix_user', '@telegram_42:chat.example.com', 'multichat', 1800000450, 2, 0, 1800000600)`,
		`INSERT INTO handle_reviews VALUES ('matrix_user', '@telegram_42:chat.example.com', 'agent', 1800000700)`,
		`INSERT INTO handle_review_claims VALUES ('email', 'carol@example.com', 'agent', 1700000650)`,
		`INSERT INTO revisions VALUES ('revision_000008', 8, 'person_000001', 'update', '{"id":"person_000001","display_name":"Alice","birthday":"","how_met":"","tags":[],"principal_id":"","created_at":1800000000,"updated_at":1800000000,"deleted_at":0,"identities":[]}', 1800000100)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

func TestImportCopiesEveryRowAsItWasAndNumbersOnAfterIt(t *testing.T) {
	store, _ := openTestStore(t)
	counts, err := ImportStandaloneDatabase(testContext, store.db, newStandaloneDatabase(t), testOwner)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := ImportCounts{Persons: 2, Identities: 2, Relationships: 1, Suggestions: 2, SuggestionHandles: 1, LegacySuggestionIdentities: 2,
		IdentityRefusals: 1, HandleActivity: 2, HandleReviews: 1, HandleReviewClaims: 1, Revisions: 1}
	if counts != want {
		t.Fatalf("counts = %+v, want %+v", counts, want)
	}
	if summary := counts.Summary(); !strings.HasPrefix(summary, "persons=2 identities=2 relationships=1 suggestions=2 ") {
		t.Errorf("summary = %q", summary)
	}

	alice, err := store.GetPerson(testContext, testScope, "person_000001")
	if err != nil {
		t.Fatal(err)
	}
	if alice.OwnerPrincipalID != testOwner || alice.DisplayName != "Alice Smith" || alice.Birthday != "--05-14" || alice.PrincipalID != "principal_000001" ||
		strings.Join(alice.Tags, ",") != "neighbors" || alice.CreatedAt != 1800000000 || alice.UpdatedAt != 1800000100 {
		t.Errorf("person_000001 = %+v", alice.Person)
	}
	if len(alice.Identities) != 2 || alice.Identities[0].ID != "identity_000002" || alice.Identities[0].LinkedAt != 1700000000 ||
		alice.Identities[1].ID != "identity_000005" || alice.Identities[1].OwnerPrincipalID != testOwner {
		t.Errorf("identities = %+v", alice.Identities)
	}
	if len(alice.Relationships) != 1 || alice.Relationships[0].ID != "relationship_000004" || alice.Relationships[0].PersonID != "person_000003" || alice.Relationships[0].Kind != "sibling" {
		t.Errorf("relationships = %+v", alice.Relationships)
	}
	if bob, err := store.GetPerson(testContext, testScope, "person_000003"); err != nil || bob.DeletedAt != 1800000300 {
		t.Errorf("person_000003 = %+v, %v", bob, err)
	}
	if _, err := store.GetPerson(testContext, otherScope, "person_000001"); !errors.Is(err, ErrNotFound) {
		t.Errorf("another owner reads an imported person: %v", err)
	}

	// The search was rebuilt from the copied identities.
	for _, query := range []string{"smi", "telegram_42", "work", "neighb"} {
		found, err := store.ListPersons(testContext, testScope, PersonFilter{Query: query})
		if err != nil || len(found) != 1 || found[0].ID != "person_000001" {
			t.Errorf("q=%q after import found %+v, %v", query, found, err)
		}
	}

	accepted, err := store.GetSuggestion(testContext, testScope, "suggestion_000002")
	if err != nil || accepted.PersonID != "person_000001" || accepted.Status != "accepted" || string(accepted.Payload.SourceDetail) != `{"z":1,"a":2}` {
		t.Errorf("suggestion_000002 = %+v, %v", accepted, err)
	}
	waiting, err := store.GetSuggestion(testContext, testScope, "suggestion_000006")
	if err != nil || waiting.PersonID != "" || waiting.Activity != (SuggestionActivity{LastActivityAt: 1800000500, MessageCount: 9, DirectChat: true}) {
		t.Errorf("suggestion_000006 = %+v, %v", waiting, err)
	}
	// The legacy table's handle counts: an importer does not propose it again.
	if again, err := store.ProposeSuggestion(testContext, testOwner, SuggestionInput{Source: "multichat", DisplayName: "Carol", Identities: []IdentityInput{{Kind: "email", Value: "carol@example.com"}}}); err != nil || again.Created {
		t.Errorf("a handle from the legacy table was proposed again: %+v, %v", again, err)
	}
	refusals, err := store.ListRefusals(testContext, testScope, RefusalFilter{PersonID: "person_000003"})
	if err != nil || len(refusals) != 1 || refusals[0].ID != "refusal_000007" || refusals[0].Reason != "not him" {
		t.Errorf("refusals = %+v, %v", refusals, err)
	}
	// The telegram handle was reviewed, so it stays out of the queue; Carol's
	// claim is long expired, so her handle waits again.
	if queue, err := store.PendingReviews(testContext, testScope, 0); err != nil || queue.Total != 1 || queue.ActiveClaims != 0 || queue.Pending[0].Value != "carol@example.com" {
		t.Errorf("queue = %+v, %v", queue, err)
	}
	revisions, err := store.Revisions(testContext, testScope, "person_000001")
	if err != nil || len(revisions) != 1 || revisions[0].ID != "revision_000008" || revisions[0].Snapshot.DisplayName != "Alice" || revisions[0].Snapshot.OwnerPrincipalID != testOwner {
		t.Errorf("revisions = %+v, %v", revisions, err)
	}

	// Each table numbers on after the highest number copied.
	next := mustCreate(t, store, "Dana")
	if next.ID != "person_000004" {
		t.Errorf("the next person is %s, want person_000004", next.ID)
	}
	identity, _, err := store.AddIdentity(testContext, testScope, next.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "dana@example.com"})
	if err != nil || identity.ID != "identity_000006" {
		t.Errorf("the next identity is %+v, %v, want identity_000006", identity, err)
	}
	relationship, err := store.AddRelationship(testContext, testScope, next.ID, RelationshipInput{PersonID: "person_000001", Kind: "friend"})
	if err != nil || relationship.ID != "relationship_000005" {
		t.Errorf("the next relationship is %+v, %v", relationship, err)
	}
	proposal, err := store.ProposeSuggestion(testContext, testOwner, SuggestionInput{Source: "multichat", DisplayName: "Erin", Identities: []IdentityInput{{Kind: "email", Value: "erin@example.com"}}})
	if err != nil || proposal.Suggestion.ID != "suggestion_000007" {
		t.Errorf("the next suggestion is %+v, %v", proposal, err)
	}
	if revisions, _ := store.Revisions(testContext, testScope, next.ID); len(revisions) != 1 || revisions[0].ID != "revision_000009" {
		t.Errorf("the next revision is %+v", revisions)
	}
	if err := store.RemoveIdentity(testContext, testScope, identity.ID); err != nil {
		t.Fatal(err)
	}
	if refusals, _ := store.ListRefusals(testContext, testScope, RefusalFilter{PersonID: next.ID}); len(refusals) != 1 || refusals[0].ID != "refusal_000008" {
		t.Errorf("the next refusal is %+v", refusals)
	}
}

func TestImportRefusesAnOwnerAFullModuleAndAFileItCannotCopyWhole(t *testing.T) {
	store, _ := openTestStore(t)
	path := newStandaloneDatabase(t)
	for _, owner := range []string{"", "alice", "principal_1"} {
		if _, err := ImportStandaloneDatabase(testContext, store.db, path, owner); err == nil {
			t.Errorf("owner %q was accepted", owner)
		}
	}

	extra := newStandaloneDatabase(t)
	db, err := sql.Open("sqlite", extra)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE persons ADD COLUMN nickname TEXT NOT NULL DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(testContext, store.db, extra, testOwner); err == nil || !strings.Contains(err.Error(), "nickname") {
		t.Errorf("a file with a column the import does not know = %v", err)
	}

	renumbered := newStandaloneDatabase(t)
	db, err = sql.Open("sqlite", renumbered)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE relationships SET seq = 9`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := ImportStandaloneDatabase(testContext, store.db, renumbered, testOwner); err == nil || !strings.Contains(err.Error(), "will not change one") {
		t.Errorf("an id its number does not make = %v", err)
	}
	if counts, _ := store.Counts(testContext, ownership.EveryOwner()); counts != (Counts{}) {
		t.Errorf("a refused import left rows: %+v", counts)
	}

	if _, err := ImportStandaloneDatabase(testContext, store.db, path, testOwner); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, path, otherOwner); err == nil || !strings.Contains(err.Error(), "empty module only") {
		t.Errorf("a second import = %v, want a refusal", err)
	}
	if _, err := ImportStandaloneDatabase(testContext, store.db, filepath.Join(t.TempDir(), "missing.db"), testOwner); err == nil {
		t.Error("a missing file was accepted")
	}
}
