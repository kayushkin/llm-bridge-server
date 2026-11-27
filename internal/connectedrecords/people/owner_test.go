package people

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/kayushkin/llm-bridge-server/internal/connectedrecords/ownership"
)

var otherScope = ownership.OwnedBy(otherOwner)

func TestANonOwnerReachesNoneOfAnotherOwnersRecords(t *testing.T) {
	store, _ := openTestStore(t)
	alice := mustCreate(t, store, "Alice")
	bob := mustCreate(t, store, "Bob")
	identity, _, err := store.AddIdentity(testContext, testScope, alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "alice@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	relationship, err := store.AddRelationship(testContext, testScope, alice.ID, RelationshipInput{PersonID: bob.ID, Kind: "friend"})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := store.ProposeSuggestion(testContext, testOwner, SuggestionInput{Source: "multichat", DisplayName: "Carol", Identities: []IdentityInput{{Kind: "email", Value: "carol@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if alice.OwnerPrincipalID != testOwner || identity.OwnerPrincipalID != testOwner || relationship.OwnerPrincipalID != testOwner || proposal.Suggestion.OwnerPrincipalID != testOwner {
		t.Fatalf("owners: person=%s identity=%s relationship=%s suggestion=%s, want %s on each", alice.OwnerPrincipalID, identity.OwnerPrincipalID, relationship.OwnerPrincipalID, proposal.Suggestion.OwnerPrincipalID, testOwner)
	}

	name := "Mallory"
	for label, err := range map[string]error{
		"get person":          second(store.GetPerson(testContext, otherScope, alice.ID)),
		"patch person":        second(store.PatchPerson(testContext, otherScope, alice.ID, PersonPatch{DisplayName: &name})),
		"delete person":       second(store.DeletePerson(testContext, otherScope, alice.ID)),
		"revisions":           second(store.Revisions(testContext, otherScope, alice.ID)),
		"add identity":        third(store.AddIdentity(testContext, otherScope, alice.ID, IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "m@example.com"})),
		"add relationship":    second(store.AddRelationship(testContext, otherScope, alice.ID, RelationshipInput{PersonID: bob.ID, Kind: "sibling"})),
		"remove identity":     store.RemoveIdentity(testContext, otherScope, identity.ID),
		"remove relationship": store.RemoveRelationship(testContext, otherScope, relationship.ID),
		"resolve":             second(store.ResolveIdentity(testContext, otherScope, "email", "alice@example.com")),
		"get suggestion":      second(store.GetSuggestion(testContext, otherScope, proposal.Suggestion.ID)),
		"accept suggestion":   second(store.AcceptSuggestion(testContext, otherScope, proposal.Suggestion.ID, "")),
		"reject suggestion":   second(store.RejectSuggestion(testContext, otherScope, proposal.Suggestion.ID)),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s as another owner = %v, want ErrNotFound", label, err)
		}
	}
	if persons, err := store.ListPersons(testContext, otherScope, PersonFilter{IncludeDeleted: true}); err != nil || len(persons) != 0 {
		t.Errorf("another owner lists %d persons, %v", len(persons), err)
	}
	if identities, err := store.ListIdentities(testContext, otherScope, IdentityFilter{}); err != nil || len(identities) != 0 {
		t.Errorf("another owner lists %d identities, %v", len(identities), err)
	}
	if suggestions, err := store.ListSuggestions(testContext, otherScope, SuggestionFilter{}); err != nil || len(suggestions) != 0 {
		t.Errorf("another owner lists %d suggestions, %v", len(suggestions), err)
	}
	if counts, err := store.Counts(testContext, otherScope); err != nil || counts != (Counts{}) {
		t.Errorf("another owner's counts = %+v, %v", counts, err)
	}
	if counts, err := store.Counts(testContext, testScope); err != nil || counts.Persons != 2 || counts.Identities != 1 || counts.Relationships != 1 || counts.ProposedSuggestions != 1 {
		t.Errorf("the owner's counts = %+v, %v", counts, err)
	}
	// The refused writes changed nothing.
	if person, err := store.GetPerson(testContext, testScope, alice.ID); err != nil || person.DisplayName != "Alice" || person.DeletedAt != 0 || len(person.Identities) != 1 || len(person.Relationships) != 1 {
		t.Errorf("after another owner's writes = %+v, %v", person, err)
	}
	if _, err := store.GetPerson(testContext, ownership.Scope{}, alice.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("the zero scope = %v, want a refusal that is not a 404", err)
	}
}

func second[T any](_ T, err error) error        { return err }
func third[T, U any](_ T, _ U, err error) error { return err }

func TestPerOwnerUniquenessLetsTwoOwnersHoldOneHandle(t *testing.T) {
	store, _ := openTestStore(t)
	mine := mustCreate(t, store, "Alice")
	theirs, err := store.CreatePerson(testContext, otherOwner, PersonInput{DisplayName: "Alice B."})
	if err != nil {
		t.Fatal(err)
	}
	handle := IdentityLinkInput{LinkedBy: "operator", Kind: "email", Value: "alice@example.com"}
	if _, _, err := store.AddIdentity(testContext, testScope, mine.ID, handle); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AddIdentity(testContext, otherScope, theirs.ID, handle); err != nil {
		t.Fatalf("a second owner could not hold the same handle: %v", err)
	}
	for scope, want := range map[ownership.Scope]string{testScope: mine.ID, otherScope: theirs.ID} {
		resolved, err := store.ResolveIdentity(testContext, scope, "email", "Alice@example.com")
		if err != nil || resolved.Person.ID != want {
			t.Errorf("resolve in %s = %+v, %v, want %s", scope, resolved, err, want)
		}
	}
	if _, err := store.ResolveIdentity(testContext, ownership.EveryOwner(), "email", "alice@example.com"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "owner_principal_id") {
		t.Errorf("resolve across every owner = %v, want a conflict saying to name owner_principal_id", err)
	}
	everywhere, err := store.ResolveIdentityInEveryOwner(testContext, "email", " ALICE@example.com")
	if err != nil || len(everywhere) != 2 || everywhere[0].Person.ID != mine.ID || everywhere[1].Person.ID != theirs.ID ||
		everywhere[0].Identity.OwnerPrincipalID != testOwner || everywhere[1].Person.OwnerPrincipalID != otherOwner {
		t.Errorf("resolve in every owner = %+v, %v", everywhere, err)
	}
	if none, err := store.ResolveIdentityInEveryOwner(testContext, "email", "nobody@example.com"); err != nil || len(none) != 0 {
		t.Errorf("an unknown handle in every owner = %+v, %v", none, err)
	}

	// An operator move takes a handle only from the mover's own persons.
	another := mustCreate(t, store, "Alice C.")
	move := handle
	move.Move = true
	moved, _, err := store.AddIdentity(testContext, testScope, another.ID, move)
	if err != nil || moved.PersonID != another.ID {
		t.Fatalf("move = %+v, %v", moved, err)
	}
	if kept, err := store.GetPerson(testContext, otherScope, theirs.ID); err != nil || len(kept.Identities) != 1 {
		t.Errorf("another owner's handle after a move = %+v, %v", kept, err)
	}
	if refusals, err := store.ListRefusals(testContext, otherScope, RefusalFilter{}); err != nil || len(refusals) != 0 {
		t.Errorf("a move wrote refusals for another owner: %+v, %v", refusals, err)
	}

	// Suggestions, activity and reviews are each owner's own.
	carol := IdentityInput{Kind: "matrix_user", Value: "@telegram_7:chat.example.com"}
	for _, owner := range []string{testOwner, otherOwner} {
		proposal, err := store.ProposeSuggestion(testContext, owner, SuggestionInput{Source: "multichat", DisplayName: "Carol", Identities: []IdentityInput{carol}})
		if err != nil || !proposal.Created {
			t.Errorf("%s proposing a handle = %+v, %v; an importer's rule is per owner", owner, proposal, err)
		}
	}
	if _, err := store.ReportHandleActivity(testContext, testOwner, HandleActivityReport{Source: "multichat", Activities: []HandleActivity{
		{Kind: carol.Kind, Value: carol.Value, MessageCount: 4, LastActivityAt: 100},
	}}); err != nil {
		t.Fatal(err)
	}
	if queue, err := store.PendingReviews(testContext, otherScope, 0); err != nil || queue.Total != 0 {
		t.Errorf("another owner's queue = %+v, %v", queue, err)
	}
	if claim, err := store.ClaimReviews(testContext, otherOwner, ReviewClaimInput{Limit: 5, ClaimedBy: "agent"}); err != nil || len(claim.Claimed) != 0 {
		t.Errorf("another owner claimed %+v, %v", claim, err)
	}
	theirSuggestions, err := store.ListSuggestions(testContext, otherScope, SuggestionFilter{})
	if err != nil || len(theirSuggestions) != 1 || theirSuggestions[0].Activity != (SuggestionActivity{}) {
		t.Errorf("another owner's suggestion took this owner's activity: %+v, %v", theirSuggestions, err)
	}
	queue, err := store.PendingReviews(testContext, testScope, 0)
	if err != nil || queue.Total != 1 || queue.Pending[0].OwnerPrincipalID != testOwner {
		t.Fatalf("the owner's queue = %+v, %v", queue, err)
	}
	if _, err := store.MarkReviewed(testContext, otherOwner, ReviewInput{Handles: []Handle{{Kind: carol.Kind, Value: carol.Value}}, ReviewedBy: "agent"}); err != nil {
		t.Fatal(err)
	}
	if queue, _ := store.PendingReviews(testContext, testScope, 0); queue.Total != 1 {
		t.Errorf("another owner's review emptied this owner's queue: %+v", queue)
	}
}

func TestAJoinOfTwoOwnersRecordsIsRefused(t *testing.T) {
	store, _ := openTestStore(t)
	mine := mustCreate(t, store, "Alice")
	theirs, err := store.CreatePerson(testContext, otherOwner, PersonInput{DisplayName: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	// To the owner, another owner's person is as missing as one that does not exist.
	_, reachable := store.AddRelationship(testContext, testScope, mine.ID, RelationshipInput{PersonID: theirs.ID, Kind: "friend"})
	_, missing := store.AddRelationship(testContext, testScope, mine.ID, RelationshipInput{PersonID: "person_999999", Kind: "friend"})
	if !errors.Is(reachable, ErrInvalid) || !errors.Is(missing, ErrInvalid) || strings.Contains(reachable.Error(), otherOwner) {
		t.Errorf("relationship to another owner's person = %v, to a missing one = %v; want the same 400 naming no owner", reachable, missing)
	}
	// An administrator is told the two have different owners.
	_, err = store.AddRelationship(testContext, ownership.EveryOwner(), mine.ID, RelationshipInput{PersonID: theirs.ID, Kind: "friend"})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), otherOwner) {
		t.Errorf("an administrator's cross-owner relationship = %v, want a 400 naming both owners", err)
	}
	proposal, err := store.ProposeSuggestion(testContext, testOwner, SuggestionInput{Source: "multichat", DisplayName: "Bob", Identities: []IdentityInput{{Kind: "email", Value: "bob@example.com"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptSuggestion(testContext, ownership.EveryOwner(), proposal.Suggestion.ID, theirs.ID); !errors.Is(err, ErrInvalid) {
		t.Errorf("accepting into another owner's person = %v, want a 400", err)
	}
	if got, _ := store.GetSuggestion(testContext, testScope, proposal.Suggestion.ID); got.Status != SuggestionStatusProposed {
		t.Errorf("a refused accept changed the suggestion: %+v", got)
	}
	// Accepted by an administrator with no person, the new person is the suggestion's owner's.
	accepted, err := store.AcceptSuggestion(testContext, ownership.EveryOwner(), proposal.Suggestion.ID, "")
	if err != nil || accepted.Person.OwnerPrincipalID != testOwner || accepted.Person.Identities[0].OwnerPrincipalID != testOwner {
		t.Fatalf("accept as an administrator = %+v, %v", accepted, err)
	}
	revisions, err := store.Revisions(testContext, ownership.EveryOwner(), accepted.Person.ID)
	if err != nil || len(revisions) != 1 || revisions[0].OwnerPrincipalID != testOwner || revisions[0].Snapshot.OwnerPrincipalID != testOwner {
		t.Errorf("revisions = %+v, %v; a revision takes the person's owner", revisions, err)
	}
}

func TestTheRoutesNarrowToTheCallersRecords(t *testing.T) {
	server, store := newTestServer(t)
	mine := mustCreate(t, store, "Alice")
	theirs, err := store.CreatePerson(testContext, otherOwner, PersonInput{DisplayName: "Bob"})
	if err != nil {
		t.Fatal(err)
	}
	other := ownership.Caller{PrincipalID: otherOwner}
	administrator := ownership.Caller{SeesEveryOwner: true}

	if status, body := call(t, server, http.MethodGet, "/persons/"+mine.ID, ""); status != http.StatusOK || !strings.Contains(body, `"owner_principal_id":"`+testOwner+`"`) {
		t.Errorf("the owner's read = %d %s", status, body)
	}
	if status, _ := callAs(t, server, other, http.MethodGet, "/persons/"+mine.ID, ""); status != http.StatusNotFound {
		t.Errorf("another owner's read = %d, want 404", status)
	}
	if status, _ := callAs(t, server, other, http.MethodPatch, "/persons/"+mine.ID, `{"display_name":"Mallory"}`); status != http.StatusNotFound {
		t.Errorf("another owner's patch = %d, want 404", status)
	}
	if status, _ := callAs(t, server, other, http.MethodPost, "/persons/"+mine.ID+"/identities", `{"kind":"email","value":"m@example.com","linked_by":"operator"}`); status != http.StatusNotFound {
		t.Errorf("another owner's identity = %d, want 404", status)
	}
	if status, _ := call(t, server, http.MethodGet, "/persons/not-an-id", ""); status != http.StatusNotFound {
		t.Errorf("a malformed id = %d, want 404", status)
	}

	listed := func(caller ownership.Caller, path string) []string {
		t.Helper()
		status, body := callAs(t, server, caller, http.MethodGet, path, "")
		var persons []Person
		if status != http.StatusOK || json.Unmarshal([]byte(body), &persons) != nil {
			t.Fatalf("GET %s = %d %s", path, status, body)
		}
		var ids []string
		for _, person := range persons {
			ids = append(ids, person.ID)
		}
		return ids
	}
	if got := listed(ownership.Caller{PrincipalID: testOwner}, "/persons"); strings.Join(got, ",") != mine.ID {
		t.Errorf("the owner lists %v", got)
	}
	if got := listed(administrator, "/persons"); strings.Join(got, ",") != mine.ID+","+theirs.ID {
		t.Errorf("an administrator lists %v, want both owners'", got)
	}
	if got := listed(administrator, "/persons?owner_principal_id="+otherOwner); strings.Join(got, ",") != theirs.ID {
		t.Errorf("an administrator narrowed to %s lists %v", otherOwner, got)
	}
	if status, _ := call(t, server, http.MethodGet, "/persons?owner_principal_id="+otherOwner, ""); status != http.StatusForbidden {
		t.Errorf("a caller naming another owner = %d, want 403", status)
	}
	if status, body := callAs(t, server, administrator, http.MethodGet, "/health", ""); status != http.StatusOK || !strings.Contains(body, `"persons":2`) {
		t.Errorf("an administrator's health = %d %s", status, body)
	}
	if status, body := call(t, server, http.MethodGet, "/health", ""); status != http.StatusOK || !strings.Contains(body, `"persons":1`) {
		t.Errorf("the owner's health = %d %s", status, body)
	}

	// Writes: what a request creates is its caller's; one acting as nobody creates nothing.
	if status, body := callAs(t, server, other, http.MethodPost, "/persons", `{"display_name":"Carol"}`); status != http.StatusCreated || !strings.Contains(body, `"owner_principal_id":"`+otherOwner+`"`) {
		t.Errorf("another owner's create = %d %s", status, body)
	}
	if status, _ := callAs(t, server, administrator, http.MethodPost, "/persons", `{"display_name":"Dave"}`); status != http.StatusForbidden {
		t.Errorf("a create acting as no principal = %d, want 403", status)
	}
	if status, _ := callAs(t, server, administrator, http.MethodPatch, "/persons/"+theirs.ID, `{"how_met":"school"}`); status != http.StatusOK {
		t.Errorf("an administrator's patch of another owner's person = %d", status)
	}

	// Resolving a handle two owners hold.
	for _, person := range []struct {
		caller ownership.Caller
		id     string
	}{{ownership.Caller{PrincipalID: testOwner}, mine.ID}, {other, theirs.ID}} {
		if status, body := callAs(t, server, person.caller, http.MethodPost, "/persons/"+person.id+"/identities", `{"kind":"email","value":"shared@example.com","linked_by":"operator"}`); status != http.StatusCreated {
			t.Fatalf("add shared handle = %d %s", status, body)
		}
	}
	if status, body := callAs(t, server, administrator, http.MethodGet, "/identities/resolve?kind=email&value=shared%40example.com", ""); status != http.StatusConflict || !strings.Contains(body, "owner_principal_id") {
		t.Errorf("an administrator's ambiguous resolve = %d %s", status, body)
	}
	if status, body := callAs(t, server, administrator, http.MethodGet, "/identities/resolve?kind=email&value=shared%40example.com&owner_principal_id="+otherOwner, ""); status != http.StatusOK || !strings.Contains(body, theirs.ID) {
		t.Errorf("an administrator's narrowed resolve = %d %s", status, body)
	}

	// No caller at all is 401 everywhere, vocabulary included.
	for _, path := range []string{"/persons", "/health", "/identity-kinds", "/suggestions", "/reviews/pending"} {
		if status, _ := callAs(t, server, ownership.Caller{}, http.MethodGet, path, ""); status != http.StatusUnauthorized {
			t.Errorf("GET %s with no caller = %d, want 401", path, status)
		}
	}
	if status, _ := callAs(t, server, ownership.Caller{}, http.MethodPost, "/persons", `{"display_name":"Eve"}`); status != http.StatusUnauthorized {
		t.Errorf("POST /persons with no caller = %d, want 401", status)
	}
	if status, _ := callAs(t, server, other, http.MethodGet, "/link-sources", ""); status != http.StatusOK {
		t.Errorf("vocabulary for another caller = %d", status)
	}
}

func TestSearchIsAPrefixOfEveryWord(t *testing.T) {
	store, _ := openTestStore(t)
	alice, err := store.CreatePerson(testContext, testOwner, PersonInput{DisplayName: "Alice Smith", HowMet: "book club", Tags: []string{"neighbors"},
		Identities: []IdentityLinkInput{{Kind: "email", Value: "alice.smith@example.com", Label: "work", LinkedBy: "operator"}}})
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, store, "Alice Jones")
	if _, err := store.CreatePerson(testContext, otherOwner, PersonInput{DisplayName: "Alice Smith"}); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		"ali smi": 1, "ALI": 2, "smith ali": 1, "neighb": 1, "work": 1, "alice.smith@example": 1, `"alice smi"`: 1,
		`"smith alice"`: 0, "ali AND smi": 1, "book club smith": 1, "zed": 0,
	} {
		found, err := store.ListPersons(testContext, testScope, PersonFilter{Query: query})
		if err != nil || len(found) != want {
			t.Errorf("q=%q found %d, %v; want %d", query, len(found), err, want)
		}
		if want == 1 && err == nil && len(found) == 1 && found[0].ID != alice.ID {
			t.Errorf("q=%q found %s, want %s", query, found[0].ID, alice.ID)
		}
	}
	for _, query := range []string{"ali OR bob", "NOT ali", "!!!", `"unclosed`} {
		if _, err := store.ListPersons(testContext, testScope, PersonFilter{Query: query}); !errors.Is(err, ErrInvalid) {
			t.Errorf("q=%q = %v, want a 400", query, err)
		}
	}
	// Removing a handle takes it out of the search.
	if err := store.RemoveIdentity(testContext, testScope, alice.Identities[0].ID); err != nil {
		t.Fatal(err)
	}
	if found, _ := store.ListPersons(testContext, testScope, PersonFilter{Query: "work"}); len(found) != 0 {
		t.Errorf("a removed handle's label still finds %d", len(found))
	}
}
