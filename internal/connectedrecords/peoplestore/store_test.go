package peoplestore

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// fakePrincipals answers CheckPrincipal from a set, or with an outage.
type fakePrincipals struct {
	known map[string]bool
	down  bool
}

func (fake *fakePrincipals) CheckPrincipal(id string) error {
	if fake.down {
		return fmt.Errorf("%w: connection refused", ErrPrincipalStoreUnavailable)
	}
	if !fake.known[id] {
		return fmt.Errorf("%w: principal-store holds no principal %s", ErrInvalid, id)
	}
	return nil
}

func openTestStore(t *testing.T) (*Store, *fakePrincipals) {
	t.Helper()
	principals := &fakePrincipals{known: map[string]bool{"principal_000001": true}}
	store, err := Open(t.TempDir(), principals)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, principals
}

func mustCreate(t *testing.T, store *Store, name string) *PersonDetail {
	t.Helper()
	person, err := store.CreatePerson(PersonInput{DisplayName: name})
	if err != nil {
		t.Fatal(err)
	}
	return person
}

func TestIdsAreZeroPaddedSequencesWithTheirPrefix(t *testing.T) {
	store, _ := openTestStore(t)
	first := mustCreate(t, store, "Alice")
	second := mustCreate(t, store, "Bob")
	if first.ID != "person_000001" || second.ID != "person_000002" {
		t.Errorf("ids = %s, %s", first.ID, second.ID)
	}
	identity, _, err := store.AddIdentity(first.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := store.AddRelationship(first.ID, RelationshipInput{PersonID: second.ID, Kind: "friend"})
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := store.Revisions(first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if identity.ID != "identity_000001" || relationship.ID != "relationship_000001" || len(revisions) != 1 || revisions[0].ID != "revision_000001" {
		t.Errorf("identity=%s relationship=%s revisions=%+v", identity.ID, relationship.ID, revisions)
	}
	pattern := regexp.MustCompile(`^person_\d{6,}$`)
	if !pattern.MatchString(first.ID) {
		t.Errorf("%s does not match the pattern registered in kanban-store", first.ID)
	}
}

func TestIdentitiesAreNormalizedAndBelongToOnePerson(t *testing.T) {
	store, _ := openTestStore(t)
	alice := mustCreate(t, store, "Alice")
	bob := mustCreate(t, store, "Bob")

	email, created, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: " Email ", Value: "  Alice@Example.COM ", Label: " work "})
	if err != nil || !created {
		t.Fatalf("add = %v created=%v", err, created)
	}
	if email.Kind != "email" || email.Value != "alice@example.com" || email.Label != "work" {
		t.Errorf("stored %+v", email)
	}
	phone, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "phone", Value: "+1 (202) 555-0100"})
	if err != nil || phone.Value != "+12025550100" {
		t.Fatalf("phone = %+v, %v", phone, err)
	}
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "phone", Value: "202-555-0100"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a phone with no country code = %v, want a 400", err)
	}
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "fax", Value: "1"}); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "matrix_user") {
		t.Errorf("an unknown kind = %v, want a 400 naming the kinds", err)
	}

	// The same handle again on the same person is not a second row.
	again, created, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "ALICE@example.com"})
	if err != nil || created || again.ID != email.ID {
		t.Errorf("re-adding = %+v created=%v err=%v", again, created, err)
	}
	// Another spelling of the handle on another person is a conflict naming the owner.
	_, _, err = store.AddIdentity(bob.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "alice@EXAMPLE.com"})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), alice.ID) || !strings.Contains(err.Error(), "Alice") {
		t.Errorf("conflicting add = %v, want a 409 naming %s", err, alice.ID)
	}

	resolved, err := store.ResolveIdentity("email", " ALICE@example.com")
	if err != nil || resolved.Person.ID != alice.ID || resolved.Identity.ID != email.ID {
		t.Errorf("resolve = %+v, %v", resolved, err)
	}
	if _, err := store.ResolveIdentity("email", "nobody@example.com"); !errors.Is(err, ErrNotFound) {
		t.Errorf("resolving an unknown handle = %v, want a 404", err)
	}

	if err := store.RemoveIdentity(email.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddIdentity(bob.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "alice@example.com"}); err != nil {
		t.Errorf("a removed handle could not move to another person: %v", err)
	}
}

func TestDeleteIsSoftAndRestoreUndoesItWithRevisions(t *testing.T) {
	store, _ := openTestStore(t)
	alice := mustCreate(t, store, "Alice")
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	newName := "Alice S."
	if _, err := store.PatchPerson(alice.ID, PersonPatch{DisplayName: &newName}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeletePerson(alice.ID)
	if err != nil || deleted.DeletedAt == 0 {
		t.Fatalf("delete = %+v, %v", deleted, err)
	}
	// Still readable by id, gone from the list.
	if got, err := store.GetPerson(alice.ID); err != nil || got.DeletedAt == 0 || len(got.Identities) != 1 {
		t.Errorf("get after delete = %+v, %v", got, err)
	}
	if listed, _ := store.ListPersons(PersonFilter{}); len(listed) != 0 {
		t.Errorf("a deleted person is listed: %+v", listed)
	}
	if listed, _ := store.ListPersons(PersonFilter{IncludeDeleted: true}); len(listed) != 1 {
		t.Errorf("include_deleted listed %d", len(listed))
	}
	if _, err := store.PatchPerson(alice.ID, PersonPatch{DisplayName: &newName}); !errors.Is(err, ErrConflict) {
		t.Errorf("patching a deleted person = %v, want a 409", err)
	}
	if _, err := store.DeletePerson(alice.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("deleting twice = %v, want a 409", err)
	}
	restored, err := store.RestorePerson(alice.ID)
	if err != nil || restored.DeletedAt != 0 || restored.DisplayName != "Alice S." {
		t.Fatalf("restore = %+v, %v", restored, err)
	}

	revisions, err := store.Revisions(alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, revision := range revisions {
		actions = append(actions, revision.Action)
	}
	if strings.Join(actions, ",") != "identity_add,update,delete,restore" {
		t.Fatalf("actions = %v", actions)
	}
	// Each snapshot is the person as it was before that action.
	if revisions[0].Snapshot.DisplayName != "Alice" || len(revisions[0].Snapshot.Identities) != 0 {
		t.Errorf("before identity_add: %+v", revisions[0].Snapshot)
	}
	if revisions[1].Snapshot.DisplayName != "Alice" || len(revisions[1].Snapshot.Identities) != 1 {
		t.Errorf("before update: %+v", revisions[1].Snapshot)
	}
	if revisions[2].Snapshot.DisplayName != "Alice S." || revisions[2].Snapshot.DeletedAt != 0 {
		t.Errorf("before delete: %+v", revisions[2].Snapshot)
	}
	if revisions[3].Snapshot.DeletedAt == 0 {
		t.Errorf("before restore: %+v", revisions[3].Snapshot)
	}
}

func TestRelationshipsListFromBothSidesWithTheInverseKind(t *testing.T) {
	store, _ := openTestStore(t)
	mother := mustCreate(t, store, "Mother")
	son := mustCreate(t, store, "Son")
	// "Mother is Son's parent", stated on Son.
	relationship, err := store.AddRelationship(son.ID, RelationshipInput{PersonID: mother.ID, Kind: "parent", Note: " since birth "})
	if err != nil {
		t.Fatal(err)
	}
	if relationship.FromPersonID != mother.ID || relationship.ToPersonID != son.ID || relationship.Kind != "parent" || relationship.Note != "since birth" {
		t.Errorf("stored %+v", relationship)
	}
	sonView, _ := store.GetPerson(son.ID)
	motherView, _ := store.GetPerson(mother.ID)
	if len(sonView.Relationships) != 1 || sonView.Relationships[0].PersonID != mother.ID || sonView.Relationships[0].Kind != "parent" || sonView.Relationships[0].PersonDisplayName != "Mother" {
		t.Errorf("son sees %+v", sonView.Relationships)
	}
	if len(motherView.Relationships) != 1 || motherView.Relationships[0].PersonID != son.ID || motherView.Relationships[0].Kind != "child" {
		t.Errorf("mother sees %+v", motherView.Relationships)
	}
	// The same fact stated from the other side is not a second row.
	if _, err := store.AddRelationship(mother.ID, RelationshipInput{PersonID: son.ID, Kind: "child"}); !errors.Is(err, ErrConflict) {
		t.Errorf("the mirror = %v, want a 409", err)
	}
	if _, err := store.AddRelationship(son.ID, RelationshipInput{PersonID: son.ID, Kind: "friend"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a relationship with oneself = %v, want a 400", err)
	}
	if _, err := store.AddRelationship(son.ID, RelationshipInput{PersonID: mother.ID, Kind: "nemesis"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown kind = %v, want a 400", err)
	}
	if err := store.RemoveRelationship(relationship.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveRelationship(relationship.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("removing twice = %v", err)
	}
}

func TestEveryRelationshipKindHasAnInverseThatIsAKind(t *testing.T) {
	for _, kind := range RelationshipKinds {
		inverse, err := relationshipKindFor(kind.Inverse)
		if err != nil || inverse.Inverse != kind.Kind {
			t.Errorf("%s ↔ %s does not round-trip", kind.Kind, kind.Inverse)
		}
	}
}

func TestSuggestionsProposeOnlyNewHandlesAndAttachOnlyOnAccept(t *testing.T) {
	store, _ := openTestStore(t)
	telegram := IdentityInput{Kind: "matrix_user", Value: "@telegram_1:chat.example.com", Label: "telegram"}
	whatsapp := IdentityInput{Kind: "matrix_user", Value: "@whatsapp_2:chat.example.com", Label: "whatsapp"}

	proposal, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "Carol", Identities: []IdentityInput{telegram, whatsapp}})
	if err != nil || !proposal.Created || proposal.Suggestion.ID != "suggestion_000001" || proposal.Suggestion.Status != "proposed" {
		t.Fatalf("propose = %+v, %v", proposal, err)
	}
	// Nothing is attached until accepted.
	if _, err := store.ResolveIdentity("matrix_user", telegram.Value); !errors.Is(err, ErrNotFound) {
		t.Errorf("a proposed handle resolves before accept: %v", err)
	}
	// A second run proposes nothing.
	again, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "Carol", Identities: []IdentityInput{telegram, whatsapp}})
	if err != nil || again.Created || len(again.Skipped) != 2 {
		t.Errorf("second propose = %+v, %v", again, err)
	}
	if _, err := store.ProposeSuggestion(SuggestionInput{Source: "gmail", DisplayName: "Carol", Identities: []IdentityInput{telegram}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown source = %v", err)
	}

	accepted, err := store.AcceptSuggestion(proposal.Suggestion.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Suggestion.Status != "accepted" || accepted.Suggestion.PersonID != accepted.Person.ID || accepted.Person.DisplayName != "Carol" || len(accepted.Person.Identities) != 2 {
		t.Errorf("accepted = %+v", accepted)
	}
	if _, err := store.AcceptSuggestion(proposal.Suggestion.ID, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("accepting twice = %v, want a 409", err)
	}

	// A group holding one attached handle and one new one proposes the new one
	// and points at the person who owns the other.
	signal := IdentityInput{Kind: "matrix_user", Value: "@signal_3:chat.example.com", Label: "signal"}
	extension, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "carol", Identities: []IdentityInput{telegram, signal}})
	if err != nil || !extension.Created || len(extension.Suggestion.Payload.Identities) != 1 || len(extension.Suggestion.Payload.MatchedPersonIDs) != 1 || extension.Suggestion.Payload.MatchedPersonIDs[0] != accepted.Person.ID {
		t.Fatalf("extension = %+v, %v", extension, err)
	}
	extended, err := store.AcceptSuggestion(extension.Suggestion.ID, accepted.Person.ID)
	if err != nil || len(extended.Person.Identities) != 3 {
		t.Fatalf("accept onto an existing person = %+v, %v", extended, err)
	}

	// Rejected stays rejected, and its handles are not proposed again.
	discord := IdentityInput{Kind: "matrix_user", Value: "@discord_4:chat.example.com", Label: "discord"}
	guess, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "Dave", Identities: []IdentityInput{discord}})
	if err != nil {
		t.Fatal(err)
	}
	rejected, err := store.RejectSuggestion(guess.Suggestion.ID)
	if err != nil || rejected.Status != "rejected" || rejected.DecidedAt == 0 {
		t.Fatalf("reject = %+v, %v", rejected, err)
	}
	if _, err := store.AcceptSuggestion(guess.Suggestion.ID, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("accepting a rejected suggestion = %v", err)
	}
	if retry, _ := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "Dave", Identities: []IdentityInput{discord}}); retry.Created {
		t.Errorf("a rejected handle was proposed again")
	}
	if _, err := store.ResolveIdentity("matrix_user", discord.Value); !errors.Is(err, ErrNotFound) {
		t.Errorf("a rejected handle was attached: %v", err)
	}

	proposed, _ := store.ListSuggestions(SuggestionFilter{Status: "proposed"})
	all, _ := store.ListSuggestions(SuggestionFilter{})
	if len(proposed) != 0 || len(all) != 3 {
		t.Errorf("proposed=%d all=%d", len(proposed), len(all))
	}
}

func TestAHandleAttachedElsewhereLeavesAWaitingSuggestion(t *testing.T) {
	store, _ := openTestStore(t)
	first := IdentityInput{Kind: "email", Value: "a@example.com"}
	second := IdentityInput{Kind: "email", Value: "b@example.com"}
	proposal, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "AB", Identities: []IdentityInput{first, second}})
	if err != nil {
		t.Fatal(err)
	}
	other := mustCreate(t, store, "Other")
	if _, _, err := store.AddIdentity(other.ID, operatorLink(second)); err != nil {
		t.Fatal(err)
	}
	waiting, _ := store.GetSuggestion(proposal.Suggestion.ID)
	if waiting.Status != "proposed" || len(waiting.Payload.Identities) != 1 || waiting.Payload.Identities[0].Value != first.Value ||
		len(waiting.Payload.Covered) != 1 || waiting.Payload.Covered[0].PersonID != other.ID || waiting.Payload.MatchedPersonIDs[0] != other.ID {
		t.Fatalf("after b went to Other = %+v", waiting.Payload)
	}
	accepted, err := store.AcceptSuggestion(proposal.Suggestion.ID, "")
	if err != nil || len(accepted.Person.Identities) != 1 || accepted.Person.Identities[0].Value != first.Value {
		t.Fatalf("accept = %+v, %v", accepted, err)
	}
	if identity := accepted.Person.Identities[0]; identity.LinkedBy != "operator" || !strings.HasPrefix(identity.Reason, "accepted "+proposal.Suggestion.ID) {
		t.Errorf("accepted handle provenance = %+v", identity)
	}

	// A suggestion every handle of which went elsewhere is superseded, with
	// the person when they all went to one.
	third := IdentityInput{Kind: "email", Value: "c@example.com"}
	lone, _ := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "C", Identities: []IdentityInput{third}})
	if _, _, err := store.AddIdentity(other.ID, IdentityLinkInput{Kind: "email", Value: "c@example.com", LinkedBy: "phone_number", Reason: "same number"}); err != nil {
		t.Fatal(err)
	}
	superseded, _ := store.GetSuggestion(lone.Suggestion.ID)
	if superseded.Status != "superseded" || superseded.PersonID != other.ID || superseded.DecidedAt == 0 {
		t.Errorf("fully covered suggestion = %+v", superseded)
	}
	// And an importer still never proposes its handle again.
	if again, _ := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "C", Identities: []IdentityInput{third}}); again.Created {
		t.Errorf("a covered handle was proposed again")
	}
}

func TestEveryLinkRecordsWhoLinkedItAndWhy(t *testing.T) {
	store, _ := openTestStore(t)
	alice := mustCreate(t, store, "Alice")
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{Kind: "email", Value: "a@x.com"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no linked_by = %v, want a 400", err)
	}
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{Kind: "email", Value: "a@x.com", LinkedBy: "phone_number"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an automatic link with no reason = %v, want a 400", err)
	}
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{Kind: "email", Value: "a@x.com", LinkedBy: "operator", LinkedAt: now() + 3600}); !errors.Is(err, ErrInvalid) {
		t.Errorf("linked_at in the future = %v, want a 400", err)
	}
	identity, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{Kind: "matrix_user", Value: "@whatsapp_1:x.com", Label: "whatsapp", LinkedBy: "phone_number", Reason: "same phone number +1", LinkedAt: 1790000000})
	if err != nil {
		t.Fatal(err)
	}
	if identity.LinkedBy != "phone_number" || identity.Reason != "same phone number +1" || identity.LinkedAt != 1790000000 || identity.CreatedAt < identity.LinkedAt {
		t.Errorf("identity = %+v", identity)
	}
}

func TestRefusalsKeepAutomaticSourcesOffAndTheOperatorOverrides(t *testing.T) {
	store, _ := openTestStore(t)
	alice := mustCreate(t, store, "Alice")
	handle := IdentityLinkInput{Kind: "matrix_user", Value: "@discord_1:x.com", LinkedBy: "agent", Reason: "they say so"}
	identity, _, err := store.AddIdentity(alice.ID, handle)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveIdentity(identity.ID); err != nil {
		t.Fatal(err)
	}
	refusals, _ := store.ListRefusals(RefusalFilter{PersonID: alice.ID})
	if len(refusals) != 1 || refusals[0].RefusedBy != "operator" || refusals[0].Value != handle.Value || refusals[0].ID != "refusal_000001" {
		t.Fatalf("refusals after removal = %+v", refusals)
	}
	for _, source := range []string{"agent", "phone_number", "import"} {
		retry := handle
		retry.LinkedBy = source
		if _, _, err := store.AddIdentity(alice.ID, retry); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s relinking a refused pair = %v, want a 409", source, err)
		}
	}
	if _, err := store.CreatePerson(PersonInput{DisplayName: "Elsewhere", Identities: []IdentityLinkInput{handle}}); err != nil {
		t.Errorf("a refusal is for one person only, but a new person with the handle failed: %v", err)
	}
	moved := IdentityLinkInput{Kind: handle.Kind, Value: handle.Value, LinkedBy: "operator", Move: true}
	back, _, err := store.AddIdentity(alice.ID, moved)
	if err != nil || back.PersonID != alice.ID || back.LinkedBy != "operator" {
		t.Fatalf("operator move back = %+v, %v", back, err)
	}
	if refusals, _ := store.ListRefusals(RefusalFilter{PersonID: alice.ID}); len(refusals) != 0 {
		t.Errorf("the operator's link left its refusal: %+v", refusals)
	}
	// The move refused the handle for the person it left.
	if refusals, _ := store.ListRefusals(RefusalFilter{Kind: handle.Kind, Value: handle.Value}); len(refusals) != 1 || refusals[0].PersonID == alice.ID {
		t.Errorf("refusals after a move = %+v", refusals)
	}
	other := mustCreate(t, store, "Bob")
	automaticMove := IdentityLinkInput{Kind: handle.Kind, Value: handle.Value, LinkedBy: "agent", Reason: "r", Move: true}
	if _, _, err := store.AddIdentity(other.ID, automaticMove); !errors.Is(err, ErrInvalid) {
		t.Errorf("an automatic move = %v, want a 400", err)
	}
}

func TestANewPersonTakesItsHandlesAllOrNothing(t *testing.T) {
	store, _ := openTestStore(t)
	owner := mustCreate(t, store, "Owner")
	if _, _, err := store.AddIdentity(owner.ID, IdentityLinkInput{Kind: "email", Value: "taken@x.com", LinkedBy: "operator"}); err != nil {
		t.Fatal(err)
	}
	_, err := store.CreatePerson(PersonInput{DisplayName: "New", Identities: []IdentityLinkInput{
		{Kind: "email", Value: "free@x.com", LinkedBy: "phone_number", Reason: "r"},
		{Kind: "email", Value: "taken@x.com", LinkedBy: "phone_number", Reason: "r"},
	}})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("create with a taken handle = %v, want a 409", err)
	}
	if listed, _ := store.ListPersons(PersonFilter{}); len(listed) != 1 {
		t.Errorf("a failed create left a person: %+v", listed)
	}
	created, err := store.CreatePerson(PersonInput{DisplayName: "New", Identities: []IdentityLinkInput{{Kind: "email", Value: "free@x.com", LinkedBy: "phone_number", Reason: "r"}}})
	if err != nil || len(created.Identities) != 1 || created.Identities[0].LinkedBy != "phone_number" {
		t.Fatalf("create = %+v, %v", created, err)
	}
}

func TestAgentSuggestionsSitBesideImporterOnesAndHonorRefusals(t *testing.T) {
	store, _ := openTestStore(t)
	telegram := IdentityInput{Kind: "matrix_user", Value: "@telegram_1:x.com", Label: "telegram"}
	discord := IdentityInput{Kind: "matrix_user", Value: "@discord_2:x.com", Label: "discord"}
	if _, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "T", Identities: []IdentityInput{telegram}}); err != nil {
		t.Fatal(err)
	}
	erin, err := store.CreatePerson(PersonInput{DisplayName: "Erin", Identities: []IdentityLinkInput{operatorLink(discord)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ProposeSuggestion(SuggestionInput{Source: "agent", DisplayName: "T", Identities: []IdentityInput{telegram, discord}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("agent with no reason = %v, want a 400", err)
	}
	guess, err := store.ProposeSuggestion(SuggestionInput{Source: "agent", DisplayName: "Erin", Reason: "same friends", Identities: []IdentityInput{telegram, discord}})
	if err != nil || !guess.Created || guess.Suggestion.Payload.MatchedPersonIDs[0] != erin.ID || guess.Suggestion.Payload.Reason != "same friends" {
		t.Fatalf("agent suggestion over an importer's handle = %+v, %v", guess, err)
	}
	if again, _ := store.ProposeSuggestion(SuggestionInput{Source: "agent", DisplayName: "Erin", Reason: "again", Identities: []IdentityInput{telegram, discord}}); again.Created {
		t.Errorf("the agent proposed a waiting handle twice")
	}
	if _, err := store.RejectSuggestion(guess.Suggestion.ID); err != nil {
		t.Fatal(err)
	}
	if refusals, _ := store.ListRefusals(RefusalFilter{PersonID: erin.ID}); len(refusals) != 1 || refusals[0].Value != telegram.Value {
		t.Errorf("reject wrote refusals %+v", refusals)
	}
	if _, err := store.ProposeSuggestion(SuggestionInput{Source: "agent", DisplayName: "Erin", Reason: "new evidence", Identities: []IdentityInput{telegram, discord}}); !errors.Is(err, ErrConflict) {
		t.Errorf("agent suggesting a refused pair = %v, want a 409", err)
	}
}

func TestTheReviewQueueClaimsAndReviewsHandles(t *testing.T) {
	store, _ := openTestStore(t)
	if _, err := store.ReportHandleActivity(HandleActivityReport{Source: "multichat", Activities: []HandleActivity{
		{Kind: "matrix_user", Value: "@a_1:x.com", MessageCount: 3, LastActivityAt: 100},
		{Kind: "matrix_user", Value: "@b_2:x.com", DirectChat: true, LastActivityAt: 50},
		{Kind: "matrix_user", Value: "@c_3:x.com"},
	}}); err != nil {
		t.Fatal(err)
	}
	queue, err := store.PendingReviews(0)
	if err != nil || queue.Total != 2 || queue.Pending[0].Value != "@b_2:x.com" {
		t.Fatalf("queue = %+v, %v (a handle nobody talks with must not wait; a direct chat goes first)", queue, err)
	}
	claim, err := store.ClaimReviews(ReviewClaimInput{Limit: 1, ClaimedBy: "agent"})
	if err != nil || len(claim.Claimed) != 1 || claim.Remaining != 1 {
		t.Fatalf("claim = %+v, %v", claim, err)
	}
	if queue, _ := store.PendingReviews(0); queue.Total != 1 || queue.ActiveClaims != 1 {
		t.Errorf("queue after a claim = %+v", queue)
	}
	if _, err := store.ClaimReviews(ReviewClaimInput{Limit: 1, ClaimedBy: "robot"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown claimant = %v", err)
	}
	if _, err := store.MarkReviewed(ReviewInput{Handles: []Handle{{Kind: "matrix_user", Value: "@b_2:x.com"}, {Kind: "matrix_user", Value: "@a_1:x.com"}}, ReviewedBy: "agent"}); err != nil {
		t.Fatal(err)
	}
	if queue, _ := store.PendingReviews(0); queue.Total != 0 || queue.ActiveClaims != 0 {
		t.Errorf("queue after review = %+v", queue)
	}
}

func TestAnOldDatabaseGainsProvenanceAndKeepsItsSuggestedHandles(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir, &fakePrincipals{})
	if err != nil {
		t.Fatal(err)
	}
	alice := mustCreate(t, store, "Alice")
	if _, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "S", Identities: []IdentityInput{{Kind: "email", Value: "s@x.com"}}}); err != nil {
		t.Fatal(err)
	}
	// Make it look like a database from before 2026-10-01: an identity with no
	// provenance columns and suggested handles only in suggestion_identities.
	for _, statement := range []string{
		`INSERT INTO suggestion_identities (suggestion_id, kind, value) SELECT suggestion_id, kind, value FROM suggestion_handles`,
		`DELETE FROM suggestion_handles`,
		`ALTER TABLE identities DROP COLUMN linked_by`,
		`ALTER TABLE identities DROP COLUMN reason`,
		`ALTER TABLE identities DROP COLUMN linked_at`,
		`INSERT INTO identities (id, seq, person_id, kind, value, label, created_at, updated_at) VALUES ('identity_000001', 1, '` + alice.ID + `', 'email', 'alice@x.com', '', 5, 5)`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	store.Close()
	store, err = Open(dir, &fakePrincipals{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	resolved, err := store.ResolveIdentity("email", "alice@x.com")
	if err != nil || resolved.Identity.LinkedBy != "import" || resolved.Identity.LinkedAt != 5 || resolved.Identity.Reason == "" {
		t.Errorf("backfilled identity = %+v, %v", resolved, err)
	}
	if again, _ := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "S", Identities: []IdentityInput{{Kind: "email", Value: "s@x.com"}}}); again.Created {
		t.Errorf("a handle suggested before the migration was proposed again")
	}
}

func TestPrincipalIDIsCheckedAndAnOutageRefusesTheWrite(t *testing.T) {
	store, principals := openTestStore(t)
	if _, err := store.CreatePerson(PersonInput{DisplayName: "A", PrincipalID: "alice"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a name as principal_id = %v, want a 400", err)
	}
	if _, err := store.CreatePerson(PersonInput{DisplayName: "A", PrincipalID: "principal_000999"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("an unknown principal = %v, want a 400", err)
	}
	person, err := store.CreatePerson(PersonInput{DisplayName: "A", PrincipalID: "principal_000001"})
	if err != nil || person.PrincipalID != "principal_000001" {
		t.Fatalf("create = %+v, %v", person, err)
	}
	principals.down = true
	id := "principal_000001"
	if _, err := store.PatchPerson(person.ID, PersonPatch{PrincipalID: &id}); !errors.Is(err, ErrPrincipalStoreUnavailable) {
		t.Errorf("with principal-store down = %v, want a refusal", err)
	}
	empty := ""
	if patched, err := store.PatchPerson(person.ID, PersonPatch{PrincipalID: &empty}); err != nil || patched.PrincipalID != "" {
		t.Errorf("clearing needs no check: %+v, %v", patched, err)
	}
}

func TestBirthdaysTakeAYearOrNone(t *testing.T) {
	for raw, want := range map[string]string{"": "", "1990-05-14": "1990-05-14", "--05-14": "--05-14", "--02-29": "--02-29"} {
		if got, err := normalizeBirthday(raw); err != nil || got != want {
			t.Errorf("%q = %q, %v", raw, got, err)
		}
	}
	for _, raw := range []string{"1990-02-30", "--13-01", "05-14", "May 14", "1990-5-14"} {
		if _, err := normalizeBirthday(raw); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q was accepted", raw)
		}
	}
}

func TestSearchFindsNamesTagsHowMetAndHandles(t *testing.T) {
	store, _ := openTestStore(t)
	alice, err := store.CreatePerson(PersonInput{DisplayName: "Alice Smith", HowMet: "climbing gym in Springfield", Tags: []string{"family", "springfield"}})
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, store, "Bob")
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "matrix_user", Value: "@telegram_123456789:chat.example.com"}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"ali", "smith", "climbing", "family", "telegram_123456789"} {
		found, err := store.ListPersons(PersonFilter{Query: query})
		if err != nil || len(found) != 1 || found[0].ID != alice.ID {
			t.Errorf("q=%q found %+v, %v", query, found, err)
		}
	}
	tagged, err := store.ListPersons(PersonFilter{Tag: "springfield"})
	if err != nil || len(tagged) != 1 {
		t.Errorf("tag=springfield found %+v, %v", tagged, err)
	}
	if _, err := store.ListPersons(PersonFilter{Query: `"unclosed`}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a malformed query = %v, want a 400", err)
	}
}

func newTestServer(t *testing.T) (*httptest.Server, *Store) {
	t.Helper()
	store, _ := openTestStore(t)
	mux := http.NewServeMux()
	RegisterHandlers(mux, store)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server, store
}

func call(t *testing.T, server *httptest.Server, method, path, body string) (int, string) {
	t.Helper()
	request, err := http.NewRequest(method, server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(data)
}

func TestPatchRefusesAnUnknownFieldAndClearsWithNull(t *testing.T) {
	server, store := newTestServer(t)
	person := mustCreate(t, store, "Alice")

	status, body := call(t, server, http.MethodPatch, "/persons/"+person.ID, `{"dispaly_name":"Alicia"}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "dispaly_name") || !strings.Contains(body, "display_name") {
		t.Errorf("unknown field = %d %s", status, body)
	}
	status, body = call(t, server, http.MethodPatch, "/persons/"+person.ID, `{"deleted_at":0}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "restore") {
		t.Errorf("deleted_at = %d %s", status, body)
	}
	status, body = call(t, server, http.MethodPatch, "/persons/"+person.ID, `{"birthday":"--05-14","tags":["a","a","b"]}`)
	if status != http.StatusOK || !strings.Contains(body, `"birthday":"--05-14"`) || !strings.Contains(body, `"tags":["a","b"]`) {
		t.Errorf("patch = %d %s", status, body)
	}
	status, body = call(t, server, http.MethodPatch, "/persons/"+person.ID, `{"birthday":null}`)
	if status != http.StatusOK || !strings.Contains(body, `"birthday":""`) {
		t.Errorf("null birthday = %d %s", status, body)
	}
	status, _ = call(t, server, http.MethodPatch, "/persons/"+person.ID, `{"display_name":null}`)
	if status != http.StatusBadRequest {
		t.Errorf("null display_name = %d", status)
	}
	status, _ = call(t, server, http.MethodPost, "/persons", `{"display_name":"X","nickname":"x"}`)
	if status != http.StatusBadRequest {
		t.Errorf("POST with an unknown field = %d", status)
	}
}

func TestTheRoutesAnswerWithTheirStatusCodes(t *testing.T) {
	server, _ := newTestServer(t)
	status, body := call(t, server, http.MethodPost, "/persons", `{"display_name":"Alice"}`)
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	var person PersonDetail
	if err := json.Unmarshal([]byte(body), &person); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, `"identities":[]`) || !strings.Contains(body, `"relationships":[]`) {
		t.Errorf("empty lists are not [] on a person: %s", body)
	}
	if status, _ := call(t, server, http.MethodGet, "/persons/person_999999", ""); status != http.StatusNotFound {
		t.Errorf("unknown person = %d, want 404 (dash's resolver reads it as an honest miss)", status)
	}
	if status, _ := call(t, server, http.MethodPost, "/persons/"+person.ID+"/identities", `{"kind":"email","value":"a@b.c","linked_by":"operator"}`); status != http.StatusCreated {
		t.Errorf("add identity = %d", status)
	}
	if status, _ := call(t, server, http.MethodPost, "/persons/"+person.ID+"/identities", `{"kind":"email","value":"A@B.C","linked_by":"operator"}`); status != http.StatusOK {
		t.Errorf("re-add identity = %d, want 200", status)
	}
	if status, body := call(t, server, http.MethodGet, "/identities/resolve?kind=email&value=A%40b.c", ""); status != http.StatusOK || !strings.Contains(body, person.ID) {
		t.Errorf("resolve = %d %s", status, body)
	}
	if status, _ := call(t, server, http.MethodGet, "/identities/resolve?kind=email&value=z%40b.c", ""); status != http.StatusNotFound {
		t.Errorf("resolve unknown = %d", status)
	}
	status, body = call(t, server, http.MethodPost, "/persons", `{"display_name":"Bob"}`)
	var bob PersonDetail
	json.Unmarshal([]byte(body), &bob)
	if status, body := call(t, server, http.MethodPost, "/persons/"+bob.ID+"/identities", `{"kind":"email","value":"a@b.c","linked_by":"operator"}`); status != http.StatusConflict || !strings.Contains(body, person.ID) {
		t.Errorf("conflicting identity = %d %s", status, body)
	}
	if status, _ := call(t, server, http.MethodDelete, "/persons/"+person.ID, ""); status != http.StatusOK {
		t.Errorf("delete = %d", status)
	}
	if status, body := call(t, server, http.MethodGet, "/persons/"+person.ID, ""); status != http.StatusOK || strings.Contains(body, `"deleted_at":0`) {
		t.Errorf("get deleted = %d %s", status, body)
	}
	if status, _ := call(t, server, http.MethodPost, "/persons/"+person.ID+"/restore", ""); status != http.StatusOK {
		t.Errorf("restore = %d", status)
	}
	if status, body := call(t, server, http.MethodGet, "/persons/"+person.ID+"/revisions", ""); status != http.StatusOK || !strings.Contains(body, `"action":"restore"`) {
		t.Errorf("revisions = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodPost, "/suggestions", `{"source":"multichat","display_name":"C","identities":[{"kind":"discord","value":"cee"}]}`); status != http.StatusCreated || !strings.Contains(body, "suggestion_000001") {
		t.Errorf("propose = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodPost, "/suggestions/suggestion_000001/accept", ""); status != http.StatusOK || !strings.Contains(body, `"status":"accepted"`) {
		t.Errorf("accept with no body = %d %s", status, body)
	}
	if status, _ := call(t, server, http.MethodGet, "/suggestions?status=maybe", ""); status != http.StatusBadRequest {
		t.Errorf("unknown status = %d", status)
	}
	if status, body := call(t, server, http.MethodGet, "/identity-kinds", ""); status != http.StatusOK || !strings.Contains(body, "matrix_user") {
		t.Errorf("identity kinds = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodGet, "/relationship-kinds", ""); status != http.StatusOK || !strings.Contains(body, `"inverse":"child"`) {
		t.Errorf("relationship kinds = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodGet, "/health", ""); status != http.StatusOK || !strings.Contains(body, `"persons":3`) {
		t.Errorf("health = %d %s", status, body)
	}
	if status, _ := call(t, server, http.MethodGet, "/persons?limit=abc", ""); status != http.StatusBadRequest {
		t.Errorf("bad limit = %d", status)
	}
}

func TestTheHTTPPrincipalCheckerTellsAMissFromAnOutage(t *testing.T) {
	principalStore := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/principals/principal_000001":
			w.Write([]byte(`{"id":"principal_000001"}`))
		case "/principals/principal_000002":
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	defer principalStore.Close()
	checker := NewHTTPPrincipalChecker(principalStore.URL + "/")
	if err := checker.CheckPrincipal("principal_000001"); err != nil {
		t.Errorf("known = %v", err)
	}
	if err := checker.CheckPrincipal("principal_000002"); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown = %v", err)
	}
	if err := checker.CheckPrincipal("principal_000003"); !errors.Is(err, ErrPrincipalStoreUnavailable) {
		t.Errorf("a 500 = %v", err)
	}
	principalStore.Close()
	if err := checker.CheckPrincipal("principal_000001"); !errors.Is(err, ErrPrincipalStoreUnavailable) {
		t.Errorf("a closed port = %v", err)
	}
}

func TestHandleActivityIsSummedOverASuggestionsHandlesAndSortsIt(t *testing.T) {
	store, _ := openTestStore(t)
	propose := func(name string, values ...string) string {
		t.Helper()
		input := SuggestionInput{Source: "multichat", DisplayName: name}
		for _, value := range values {
			input.Identities = append(input.Identities, IdentityInput{Kind: "matrix_user", Value: value})
		}
		proposal, err := store.ProposeSuggestion(input)
		if err != nil || !proposal.Created {
			t.Fatalf("propose %s: %v %+v", name, err, proposal)
		}
		return proposal.Suggestion.ID
	}
	quiet := propose("Quiet", "@telegram_1:chat.example.com")
	busy := propose("Busy Bee", "@telegram_2:chat.example.com", "@whatsapp_2:chat.example.com")
	recent := propose("Recent", "@signal_3:chat.example.com")
	tied := propose("Tied", "@signal_4:chat.example.com")

	result, err := store.ReportHandleActivity(HandleActivityReport{Source: "multichat", Activities: []HandleActivity{
		{Kind: "matrix_user", Value: " @telegram_2:chat.example.com ", LastActivityAt: 100, MessageCount: 5},
		{Kind: "matrix_user", Value: "@whatsapp_2:chat.example.com", LastActivityAt: 300, MessageCount: 7, DirectChat: true},
		{Kind: "matrix_user", Value: "@signal_3:chat.example.com", LastActivityAt: 500, MessageCount: 1},
		{Kind: "matrix_user", Value: "@signal_4:chat.example.com", LastActivityAt: 300, MessageCount: 20},
		{Kind: "matrix_user", Value: "@nobody_9:chat.example.com", LastActivityAt: 900, MessageCount: 99},
	}})
	if err != nil || result.Updated != 5 {
		t.Fatalf("report = %+v, %v", result, err)
	}
	got, err := store.GetSuggestion(busy)
	if err != nil {
		t.Fatal(err)
	}
	if got.Activity != (SuggestionActivity{LastActivityAt: 300, MessageCount: 12, DirectChat: true}) {
		t.Errorf("busy activity = %+v, want max 300, sum 12, direct", got.Activity)
	}
	if got, _ := store.GetSuggestion(quiet); got.Activity != (SuggestionActivity{}) {
		t.Errorf("quiet activity = %+v, want zero", got.Activity)
	}

	// A second report replaces the row whole.
	if _, err := store.ReportHandleActivity(HandleActivityReport{Source: "multichat", Activities: []HandleActivity{
		{Kind: "matrix_user", Value: "@whatsapp_2:chat.example.com", LastActivityAt: 200, MessageCount: 1},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.GetSuggestion(busy); got.Activity != (SuggestionActivity{LastActivityAt: 200, MessageCount: 6}) {
		t.Errorf("busy after replace = %+v, want max 200, sum 6, not direct", got.Activity)
	}

	ids := func(sort string) []string {
		t.Helper()
		suggestions, err := store.ListSuggestions(SuggestionFilter{Status: "proposed", Sort: sort})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, suggestion := range suggestions {
			out = append(out, suggestion.ID)
		}
		return out
	}
	if got, want := strings.Join(ids(""), ","), strings.Join([]string{quiet, busy, recent, tied}, ","); got != want {
		t.Errorf("default order = %s, want %s", got, want)
	}
	if got, want := strings.Join(ids("created"), ","), strings.Join([]string{quiet, busy, recent, tied}, ","); got != want {
		t.Errorf("created order = %s, want %s", got, want)
	}
	// recent (500), tied (300), busy (200), quiet (none).
	if got, want := strings.Join(ids("activity"), ","), strings.Join([]string{recent, tied, busy, quiet}, ","); got != want {
		t.Errorf("activity order = %s, want %s", got, want)
	}
	// Equal last activity falls to the message count, then to age.
	if _, err := store.ReportHandleActivity(HandleActivityReport{Source: "multichat", Activities: []HandleActivity{
		{Kind: "matrix_user", Value: "@telegram_2:chat.example.com", LastActivityAt: 300, MessageCount: 5},
		{Kind: "matrix_user", Value: "@telegram_1:chat.example.com", LastActivityAt: 300, MessageCount: 20},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(ids("activity"), ","), strings.Join([]string{recent, quiet, tied, busy}, ","); got != want {
		t.Errorf("activity order with ties = %s, want %s", got, want)
	}
	// A one-to-one chat outranks any recency or message count.
	if _, err := store.ReportHandleActivity(HandleActivityReport{Source: "multichat", Activities: []HandleActivity{
		{Kind: "matrix_user", Value: "@signal_4:chat.example.com", LastActivityAt: 10, MessageCount: 1, DirectChat: true},
	}}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(ids("activity"), ","), strings.Join([]string{tied, recent, quiet, busy}, ","); got != want {
		t.Errorf("activity order with a direct chat = %s, want %s", got, want)
	}

	if named, err := store.ListSuggestions(SuggestionFilter{Query: "bee"}); err != nil || len(named) != 1 || named[0].ID != busy {
		t.Errorf("q=bee = %+v, %v", named, err)
	}
	if _, err := store.ListSuggestions(SuggestionFilter{Sort: "loudest"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown sort: %v", err)
	}
	for _, bad := range []HandleActivityReport{
		{Source: "gmail", Activities: []HandleActivity{}},
		{Source: "multichat"},
		{Source: "multichat", Activities: []HandleActivity{{Kind: "matrix_user", Value: "not-a-handle"}}},
		{Source: "multichat", Activities: []HandleActivity{{Kind: "matrix_user", Value: "@a_1:x", MessageCount: -1}}},
		{Source: "multichat", Activities: []HandleActivity{{Kind: "matrix_user", Value: "@a_1:x"}, {Kind: "matrix_user", Value: " @a_1:x"}}},
	} {
		if _, err := store.ReportHandleActivity(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("report %+v: %v, want ErrInvalid", bad, err)
		}
	}
}

func TestHandleActivityAndSuggestionSortRoutes(t *testing.T) {
	server, store := newTestServer(t)
	proposal, err := store.ProposeSuggestion(SuggestionInput{Source: "multichat", DisplayName: "Carol", Identities: []IdentityInput{{Kind: "matrix_user", Value: "@telegram_1:chat.example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	status, body := call(t, server, http.MethodPut, "/handle-activity", `{"source":"multichat","activities":[{"kind":"matrix_user","value":"@telegram_1:chat.example.com","last_activity_at":1790000000,"message_count":3,"direct_chat":true}]}`)
	if status != http.StatusOK || strings.TrimSpace(body) != `{"updated":1}` {
		t.Errorf("PUT /handle-activity = %d %s", status, body)
	}
	status, body = call(t, server, http.MethodGet, "/suggestions?status=proposed&sort=activity", "")
	var listed []Suggestion
	if status != http.StatusOK || json.Unmarshal([]byte(body), &listed) != nil || len(listed) != 1 || listed[0].ID != proposal.Suggestion.ID ||
		listed[0].Activity != (SuggestionActivity{LastActivityAt: 1790000000, MessageCount: 3, DirectChat: true}) {
		t.Errorf("GET /suggestions?sort=activity = %d %s", status, body)
	}
	status, body = call(t, server, http.MethodGet, "/suggestions/"+proposal.Suggestion.ID, "")
	if status != http.StatusOK || !strings.Contains(body, `"activity":{"last_activity_at":1790000000,"message_count":3,"direct_chat":true}`) {
		t.Errorf("GET /suggestions/{id} = %d %s", status, body)
	}
	status, body = call(t, server, http.MethodGet, "/suggestions?sort=loudest", "")
	if status != http.StatusBadRequest || !strings.Contains(body, "created, activity") {
		t.Errorf("unknown sort = %d %s, want 400 naming created, activity", status, body)
	}
	for _, bad := range []string{
		`{"source":"multichat","activities":[],"extra":1}`,
		`{"source":"multichat","activities":[{"kind":"matrix_user","value":"@a_1:x","seen":true}]}`,
		`{"source":"gmail","activities":[]}`,
	} {
		if status, body := call(t, server, http.MethodPut, "/handle-activity", bad); status != http.StatusBadRequest {
			t.Errorf("PUT %s = %d %s, want 400", bad, status, body)
		}
	}
}

// operatorLink is a suggestion's handle linked by the operator.
func operatorLink(handle IdentityInput) IdentityLinkInput {
	return IdentityLinkInput{Kind: handle.Kind, Value: handle.Value, Label: handle.Label, LinkedBy: LinkSourceOperator}
}

func TestTheLinkRefusalAndReviewRoutes(t *testing.T) {
	server, _ := newTestServer(t)
	if status, body := call(t, server, http.MethodGet, "/link-sources", ""); status != http.StatusOK || !strings.Contains(body, `"source":"phone_number"`) || !strings.Contains(body, `"overrides_refusals":true`) {
		t.Errorf("link sources = %d %s", status, body)
	}
	status, body := call(t, server, http.MethodPost, "/persons", `{"display_name":"Alice","identities":[{"kind":"matrix_user","value":"@telegram_1:x.com","label":"telegram","linked_by":"phone_number","reason":"same phone number +1"}]}`)
	if status != http.StatusCreated || !strings.Contains(body, `"linked_by":"phone_number"`) {
		t.Fatalf("create with identities = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodPost, "/persons/person_000001/identities", `{"kind":"email","value":"a@b.c"}`); status != http.StatusBadRequest || !strings.Contains(body, "linked_by") {
		t.Errorf("identity with no linked_by = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodGet, "/identities?kind=matrix_user", ""); status != http.StatusOK || !strings.Contains(body, "identity_000001") {
		t.Errorf("list identities = %d %s", status, body)
	}
	if status, _ := call(t, server, http.MethodGet, "/identities?kind=fax", ""); status != http.StatusBadRequest {
		t.Errorf("list identities of an unknown kind = %d", status)
	}
	if status, _ := call(t, server, http.MethodDelete, "/identities/identity_000001", ""); status != http.StatusNoContent {
		t.Errorf("remove = %d", status)
	}
	if status, body := call(t, server, http.MethodGet, "/refusals?person_id=person_000001", ""); status != http.StatusOK || !strings.Contains(body, "refusal_000001") {
		t.Errorf("refusals = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodPost, "/persons/person_000001/identities", `{"kind":"matrix_user","value":"@telegram_1:x.com","linked_by":"phone_number","reason":"r"}`); status != http.StatusConflict || !strings.Contains(body, "refused") {
		t.Errorf("refused relink = %d %s", status, body)
	}
	call(t, server, http.MethodPut, "/handle-activity", `{"source":"multichat","activities":[{"kind":"matrix_user","value":"@telegram_1:x.com","last_activity_at":1,"message_count":2,"direct_chat":false}]}`)
	if status, body := call(t, server, http.MethodGet, "/reviews/pending?limit=5", ""); status != http.StatusOK || !strings.Contains(body, `"total":1`) {
		t.Errorf("pending = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodPost, "/reviews/claim", `{"limit":5,"claimed_by":"agent"}`); status != http.StatusOK || !strings.Contains(body, `"remaining":0`) {
		t.Errorf("claim = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodPost, "/reviews", `{"handles":[{"kind":"matrix_user","value":"@telegram_1:x.com"}],"reviewed_by":"agent"}`); status != http.StatusOK || !strings.Contains(body, `"reviewed":1`) {
		t.Errorf("review = %d %s", status, body)
	}
}

func TestGoogleContactsIsAnImporterThatNeverProposesAHandleTwice(t *testing.T) {
	store, _ := openTestStore(t)
	alice := mustCreate(t, store, "Alice")
	if _, _, err := store.AddIdentity(alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	first, err := store.ProposeSuggestion(SuggestionInput{Source: SuggestionSourceGoogleContacts, DisplayName: "Alice A.",
		Identities: []IdentityInput{{Kind: "email", Value: "Alice@Example.com"}, {Kind: "phone", Value: "+1 202 555 0100", Label: "cell"}}})
	if err != nil || !first.Created {
		t.Fatalf("first proposal = %+v, %v", first, err)
	}
	payload := first.Suggestion.Payload
	if len(payload.Identities) != 1 || payload.Identities[0].Value != "+12025550100" || len(payload.MatchedPersonIDs) != 1 || payload.MatchedPersonIDs[0] != alice.ID {
		t.Errorf("payload = %+v, want only the phone, matched to %s", payload, alice.ID)
	}
	if _, err := store.RejectSuggestion(first.Suggestion.ID); err != nil {
		t.Fatal(err)
	}
	again, err := store.ProposeSuggestion(SuggestionInput{Source: SuggestionSourceGoogleContacts, DisplayName: "Alice A.",
		Identities: []IdentityInput{{Kind: "phone", Value: "+12025550100"}}})
	if err != nil || again.Created {
		t.Errorf("a rejected handle proposed again = %+v, %v", again, err)
	}
	// Another importer is held to the same rule.
	other, err := store.ProposeSuggestion(SuggestionInput{Source: SuggestionSourceMultichat, DisplayName: "Alice",
		Identities: []IdentityInput{{Kind: "phone", Value: "+12025550100"}}})
	if err != nil || other.Created {
		t.Errorf("multichat proposing a google_contacts handle = %+v, %v", other, err)
	}
}

func TestPhoneMessagesIsAnImporterThatNeverProposesAHandleTwice(t *testing.T) {
	store, _ := openTestStore(t)
	first, err := store.ProposeSuggestion(SuggestionInput{Source: SuggestionSourcePhoneMessages, DisplayName: "Carol",
		Identities: []IdentityInput{{Kind: "phone", Value: "+12025550111"}}})
	if err != nil || !first.Created {
		t.Fatalf("first proposal = %+v, %v", first, err)
	}
	again, err := store.ProposeSuggestion(SuggestionInput{Source: SuggestionSourcePhoneMessages, DisplayName: "Carol",
		Identities: []IdentityInput{{Kind: "phone", Value: "+12025550111"}}})
	if err != nil || again.Created {
		t.Errorf("the same handle proposed again = %+v, %v", again, err)
	}
}

func TestListPersonsByPrincipalFindsThePersonPointingAtIt(t *testing.T) {
	store, _ := openTestStore(t)
	member, err := store.CreatePerson(PersonInput{DisplayName: "Member", PrincipalID: "principal_000001"})
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, store, "Someone else")
	found, err := store.ListPersons(PersonFilter{PrincipalID: "principal_000001"})
	if err != nil || len(found) != 1 || found[0].ID != member.ID {
		t.Errorf("principal_id=principal_000001 found %+v, %v", found, err)
	}
	none, err := store.ListPersons(PersonFilter{PrincipalID: "principal_000002"})
	if err != nil || len(none) != 0 {
		t.Errorf("a principal nobody points at found %+v, %v", none, err)
	}
	if _, err := store.ListPersons(PersonFilter{PrincipalID: "alice"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a name as principal_id = %v, want a 400", err)
	}
}
