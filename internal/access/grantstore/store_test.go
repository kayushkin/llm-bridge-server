package grantstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/kayushkin/llm-bridge-server/postgresstorage/postgrestest"
)

// newTestStore gives the test a store on a Postgres database of its own, made
// from a template with the module migrated (postgresstorage/postgrestest).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	module, err := Module()
	if err != nil {
		t.Fatalf("module: %v", err)
	}
	return New(postgrestest.NewDatabase(t, module).Open(module))
}

// fakeDirectory stands in for principal-store. principals maps an id to the
// summary it answers; an id absent from it does not exist. failure, when set,
// is returned for every call as a directory that could not be asked.
type fakeDirectory struct {
	mu         sync.Mutex
	principals map[string]*PrincipalSummary
	failure    error
	calls      int
}

func newFakeDirectory() *fakeDirectory {
	return &fakeDirectory{principals: map[string]*PrincipalSummary{
		"principal_000001": {ID: "principal_000001", Kind: "human", ActsInThisDeployment: true, ActsAsCaller: true, GroupIDs: []string{"principal_000006"}},
		"principal_000002": {ID: "principal_000002", Kind: "human", ActsInThisDeployment: true, ActsAsCaller: true},
		"principal_000008": {ID: "principal_000008", Kind: "human", ActsInThisDeployment: true, ActsAsCaller: true, IsAdministrator: true},
		"principal_000010": {ID: "principal_000010", Kind: "contact"},
		"principal_000006": {ID: "principal_000006", Kind: "group", ActsInThisDeployment: true},
		"principal_000007": {ID: "principal_000007", Kind: "group", ActsInThisDeployment: true},
	}}
}

func (f *fakeDirectory) LookupPrincipal(_ context.Context, id string) (*PrincipalSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.failure != nil {
		return nil, f.failure
	}
	summary, ok := f.principals[id]
	if !ok {
		return nil, fmt.Errorf("%w: the fake directory has no %s", ErrPrincipalNotFound, id)
	}
	return summary, nil
}

// fakeResourceChecker stands in for the owners. The zero value says every
// resource exists; missing names the "type/id" refs it says do not; failure,
// when set, is returned for every call as an owner that could not be asked.
type fakeResourceChecker struct {
	mu      sync.Mutex
	missing map[string]bool
	failure error
	calls   []string
}

func (f *fakeResourceChecker) CheckResourceExists(_ context.Context, resourceType, resourceID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ref := resourceType + "/" + resourceID
	f.calls = append(f.calls, ref)
	if f.failure != nil {
		return f.failure
	}
	if f.missing[ref] {
		return fmt.Errorf("%w: the fake owner has no %s", ErrResourceNotFound, ref)
	}
	return nil
}

func (f *fakeResourceChecker) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func mustGrant(t *testing.T, s *Store, principalID, relation, resourceType, resourceID string) *Grant {
	t.Helper()
	g, created, err := s.Create(context.Background(), newFakeDirectory(), &fakeResourceChecker{},
		GrantRequest{PrincipalID: principalID, Relation: relation, ResourceType: resourceType, ResourceID: resourceID})
	if err != nil {
		t.Fatalf("grant %s %s %s/%s: %v", principalID, relation, resourceType, resourceID, err)
	}
	if !created {
		t.Fatalf("grant %s %s %s/%s: not created", principalID, relation, resourceType, resourceID)
	}
	return g
}

func TestCreateAssignsAPrefixedIDAndIsIdempotentOnTheActiveTuple(t *testing.T) {
	s := newTestStore(t)
	directory, checker := newFakeDirectory(), &fakeResourceChecker{}
	request := GrantRequest{PrincipalID: "principal_000001", Relation: RelationCanUse, ResourceType: ResourceTypeTool, ResourceID: "13", Note: "  playwright for e2e  "}

	first, created, err := s.Create(context.Background(), directory, checker, request)
	if err != nil || !created {
		t.Fatalf("first create: created=%v err=%v", created, err)
	}
	if first.ID != "grant_000001" || strings.Contains(first.ID, "/") {
		t.Fatalf("id = %q, want grant_000001", first.ID)
	}
	if first.Note != "playwright for e2e" {
		t.Fatalf("note = %q, want trimmed", first.Note)
	}

	request.Note = "a different note"
	again, created, err := s.Create(context.Background(), directory, checker, request)
	if err != nil || created {
		t.Fatalf("repeat create: created=%v err=%v", created, err)
	}
	if again.ID != first.ID || again.Note != first.Note || again.GrantedAt != first.GrantedAt {
		t.Fatalf("repeat returned %+v, want the stored row %+v", again, first)
	}
	if checker.callCount() != 2 {
		t.Fatalf("owner asked %d times, want 2 (once per call, before the tuple check)", checker.callCount())
	}

	second := mustGrant(t, s, "principal_000001", RelationCanUse, ResourceTypeSkill, "7")
	if second.ID != "grant_000002" {
		t.Fatalf("second id = %q", second.ID)
	}
}

func TestCreateRefusesWhatItCannotStoreBeforeAskingAnyone(t *testing.T) {
	s := newTestStore(t)
	cases := []struct {
		name    string
		request GrantRequest
		want    string
	}{
		{"unknown relation", GrantRequest{PrincipalID: "principal_000001", Relation: "may_use", ResourceType: "tool", ResourceID: "1"}, "can_use, can_run_as, can_dispatch_on, can_view, can_edit, can_administer, can_run_operation, can_observe_sessions_on, can_start_sessions_as, can_call_through, can_see_references, works_with"},
		{"case is not normalized", GrantRequest{PrincipalID: "principal_000001", Relation: "Can_Use", ResourceType: "tool", ResourceID: "1"}, "unknown relation"},
		{"unknown resource type", GrantRequest{PrincipalID: "principal_000001", Relation: "can_use", ResourceType: "robot", ResourceID: "1"}, "agent, instance, machine, skill, tool"},
		{"relation does not apply to type", GrantRequest{PrincipalID: "principal_000001", Relation: "can_run_as", ResourceType: "tool", ResourceID: "1"}, "can_run_as does not apply to a tool: it applies to agent"},
		{"dispatch on a skill", GrantRequest{PrincipalID: "principal_000001", Relation: "can_dispatch_on", ResourceType: "skill", ResourceID: "1"}, "applies to instance, machine"},
		{"slug for an agent", GrantRequest{PrincipalID: "principal_000001", Relation: "can_run_as", ResourceType: "agent", ResourceID: "claxon"}, "slug is not accepted"},
		{"leading zero", GrantRequest{PrincipalID: "principal_000001", Relation: "can_use", ResourceType: "tool", ResourceID: "013"}, "plain decimal integer"},
		{"empty principal", GrantRequest{Relation: "can_use", ResourceType: "tool", ResourceID: "1"}, "principal_id is required"},
		{"principal by name", GrantRequest{PrincipalID: "Slava Kayushkin", Relation: "can_use", ResourceType: "tool", ResourceID: "1"}, "not a principal-store id"},
		{"principal with whitespace", GrantRequest{PrincipalID: " principal_000001", Relation: "can_use", ResourceType: "tool", ResourceID: "1"}, "surrounding whitespace"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			directory, checker := newFakeDirectory(), &fakeResourceChecker{}
			_, _, err := s.Create(context.Background(), directory, checker, c.request)
			if !errors.Is(err, ErrInvalidGrant) {
				t.Fatalf("err = %v, want ErrInvalidGrant", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to say %q", err, c.want)
			}
			if directory.calls != 0 || checker.callCount() != 0 {
				t.Fatalf("a malformed grant reached an owner: directory=%d checker=%d", directory.calls, checker.callCount())
			}
		})
	}
	if counts, _ := s.Counts(context.Background()); counts.Grants != 0 {
		t.Fatalf("%d grants written by refused requests", counts.Grants)
	}
}

func TestCreateAsksBothOwnersAndWritesNothingWhenEitherSaysNoOrCannotAnswer(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	request := GrantRequest{PrincipalID: "principal_000099", Relation: RelationCanUse, ResourceType: ResourceTypeTool, ResourceID: "13"}

	// Unknown principal: 400 naming principal-store, and the resource owner is
	// never asked — the principal is checked first because it is the cheaper
	// thing for the caller to fix.
	checker := &fakeResourceChecker{}
	_, _, err := s.Create(ctx, newFakeDirectory(), checker, request)
	if !errors.Is(err, ErrInvalidGrant) || !strings.Contains(err.Error(), "principal_000099 does not exist in principal-store") {
		t.Fatalf("unknown principal: err = %v", err)
	}
	if checker.callCount() != 0 {
		t.Fatalf("resource owner asked about a grant whose principal does not exist")
	}

	// Unknown resource: 400 naming the owner.
	request.PrincipalID = "principal_000001"
	checker = &fakeResourceChecker{missing: map[string]bool{"tool/13": true}}
	_, _, err = s.Create(ctx, newFakeDirectory(), checker, request)
	if !errors.Is(err, ErrInvalidGrant) || !strings.Contains(err.Error(), "tool 13 does not exist in tool-store") {
		t.Fatalf("unknown resource: err = %v", err)
	}

	// Directory down: 502, not 400 — the caller did nothing wrong.
	directory := newFakeDirectory()
	directory.failure = errors.New("dial tcp: connection refused")
	_, _, err = s.Create(ctx, directory, &fakeResourceChecker{}, request)
	if !errors.Is(err, ErrOwnerUnavailable) || errors.Is(err, ErrInvalidGrant) || !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("directory down: err = %v", err)
	}

	// Owner down: the same.
	checker = &fakeResourceChecker{failure: errors.New("tool-store answered 500")}
	_, _, err = s.Create(ctx, newFakeDirectory(), checker, request)
	if !errors.Is(err, ErrOwnerUnavailable) || !strings.Contains(err.Error(), "nothing was written") {
		t.Fatalf("owner down: err = %v", err)
	}

	if counts, _ := s.Counts(context.Background()); counts.Grants != 0 {
		t.Fatalf("%d grants written despite refusals", counts.Grants)
	}
}

func TestRevokeIsIdempotentAndKeepsTheRow(t *testing.T) {
	s := newTestStore(t)
	g := mustGrant(t, s, "principal_000001", RelationCanUse, ResourceTypeTool, "13")

	revoked, err := s.Revoke(context.Background(), g.ID)
	if err != nil || revoked.RevokedAt == 0 {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	again, err := s.Revoke(context.Background(), g.ID)
	if err != nil || again.RevokedAt != revoked.RevokedAt {
		t.Fatalf("second revoke moved revoked_at: %d -> %d (%v)", revoked.RevokedAt, again.RevokedAt, err)
	}
	if _, err := s.Get(context.Background(), g.ID); err != nil {
		t.Fatalf("revoked grant gone: %v", err)
	}
	if _, err := s.Revoke(context.Background(), "grant_000404"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoke missing: err = %v", err)
	}

	// Hidden from the default list, visible with include_revoked, and the
	// tuple can be granted again as a fresh row.
	if active, _ := s.List(context.Background(), Filter{}); len(active) != 0 {
		t.Fatalf("revoked grant still listed: %+v", active[0])
	}
	if all, _ := s.List(context.Background(), Filter{IncludeRevoked: true}); len(all) != 1 {
		t.Fatalf("include_revoked listed %d", len(all))
	}
	fresh := mustGrant(t, s, "principal_000001", RelationCanUse, ResourceTypeTool, "13")
	if fresh.ID == g.ID {
		t.Fatalf("re-grant reused the revoked row")
	}
	counts, _ := s.Counts(context.Background())
	if counts.Grants != 2 || counts.Active != 1 || counts.Revoked != 1 || counts.Principals != 1 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestListFiltersAndRefusesAnUnknownVocabularyWord(t *testing.T) {
	s := newTestStore(t)
	mustGrant(t, s, "principal_000001", RelationCanUse, ResourceTypeTool, "13")
	mustGrant(t, s, "principal_000001", RelationWorksWith, ResourceTypeTool, "13")
	mustGrant(t, s, "principal_000006", RelationCanUse, ResourceTypeTool, "14")
	mustGrant(t, s, "principal_000002", RelationCanRunAs, ResourceTypeAgent, "65")

	byPrincipal, _ := s.List(context.Background(), Filter{PrincipalID: "principal_000001"})
	if len(byPrincipal) != 2 {
		t.Fatalf("by principal: %d", len(byPrincipal))
	}
	byResource, _ := s.List(context.Background(), Filter{ResourceType: ResourceTypeTool, ResourceID: "13"})
	if len(byResource) != 2 {
		t.Fatalf("by resource: %d", len(byResource))
	}
	byRelation, _ := s.List(context.Background(), Filter{Relation: RelationCanUse})
	if len(byRelation) != 2 {
		t.Fatalf("by relation: %d", len(byRelation))
	}
	page, _ := s.List(context.Background(), Filter{Limit: 2, Offset: 2})
	if len(page) != 2 {
		t.Fatalf("page: %d", len(page))
	}
	for _, f := range []Filter{{Relation: "may_use"}, {ResourceType: "robot"}} {
		if _, err := s.List(context.Background(), f); !errors.Is(err, ErrInvalidGrant) {
			t.Fatalf("filter %+v: err = %v, want ErrInvalidGrant, not an empty answer", f, err)
		}
	}
}

func TestEffectiveInheritsActiveGroupsFromTheDirectoryOnEveryCall(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	own := mustGrant(t, s, "principal_000001", RelationCanUse, ResourceTypeTool, "13")
	viaGroup := mustGrant(t, s, "principal_000006", RelationCanUse, ResourceTypeTool, "14")
	bothWays := mustGrant(t, s, "principal_000006", RelationCanUse, ResourceTypeTool, "13")
	mustGrant(t, s, "principal_000007", RelationCanUse, ResourceTypeTool, "15") // a group Slava is not in
	mustGrant(t, s, "principal_000002", RelationCanRunAs, ResourceTypeAgent, "65")
	revoked := mustGrant(t, s, "principal_000006", RelationCanRunAs, ResourceTypeAgent, "65")
	if _, err := s.Revoke(context.Background(), revoked.ID); err != nil {
		t.Fatal(err)
	}

	directory := newFakeDirectory()
	effective, err := s.Effective(ctx, directory, "principal_000001", "", "")
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(effective))
	for _, g := range effective {
		ids = append(ids, g.ID+"@"+g.PrincipalID)
	}
	// tool 13 twice (own row first), then tool 14 via the group; nothing from
	// the other group, the other human, or the revoked row.
	want := []string{own.ID + "@principal_000001", bothWays.ID + "@principal_000006", viaGroup.ID + "@principal_000006"}
	if strings.Join(ids, " ") != strings.Join(want, " ") {
		t.Fatalf("effective = %v, want %v", ids, want)
	}

	// Filters narrow it; the group's own effective set is just its own rows.
	tools, _ := s.Effective(ctx, directory, "principal_000001", RelationCanUse, ResourceTypeTool)
	if len(tools) != 3 {
		t.Fatalf("filtered: %d", len(tools))
	}
	agents, _ := s.Effective(ctx, directory, "principal_000001", "", ResourceTypeAgent)
	if len(agents) != 0 {
		t.Fatalf("agents for principal_000001: %d, want 0 (the group's agent grant is revoked)", len(agents))
	}
	group, _ := s.Effective(ctx, directory, "principal_000006", "", "")
	if len(group) != 2 {
		t.Fatalf("group's own set: %d", len(group))
	}

	// The membership is read live: drop Slava from the group and the
	// inherited rows vanish without this store being told.
	directory.principals["principal_000001"] = &PrincipalSummary{ID: "principal_000001", Kind: "human", ActsInThisDeployment: true, ActsAsCaller: true}
	after, _ := s.Effective(ctx, directory, "principal_000001", "", "")
	if len(after) != 1 || after[0].ID != own.ID {
		t.Fatalf("after leaving the group: %+v", after)
	}

	// An unknown principal is a 404 and a directory that is down is a 502 —
	// never an empty answer.
	if _, err := s.Effective(ctx, directory, "principal_000099", "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown principal: err = %v", err)
	}
	directory.failure = errors.New("connection refused")
	if _, err := s.Effective(ctx, directory, "principal_000001", "", ""); !errors.Is(err, ErrOwnerUnavailable) {
		t.Fatalf("directory down: err = %v", err)
	}
	if _, err := s.Effective(ctx, newFakeDirectory(), "principal_000001", "may_use", ""); !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("unknown relation filter: err = %v", err)
	}
}

func TestConcurrentCreatesNeverCollideOnSeq(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := s.Create(context.Background(), newFakeDirectory(), &fakeResourceChecker{},
				GrantRequest{PrincipalID: "principal_000001", Relation: RelationCanUse, ResourceType: ResourceTypeTool, ResourceID: fmt.Sprint(i)})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent create: %v", err)
		}
	}
	counts, _ := s.Counts(context.Background())
	if counts.Grants != 20 {
		t.Fatalf("grants = %d, want 20", counts.Grants)
	}
}

// TestConcurrentIdenticalCreatesWriteOneRow: twenty creates of one tuple at
// once leave one active row, and exactly one of them says it wrote it.
func TestConcurrentIdenticalCreatesWriteOneRow(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	type outcome struct {
		grant   *Grant
		created bool
		err     error
	}
	outcomes := make(chan outcome, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			grant, created, err := s.Create(context.Background(), newFakeDirectory(), &fakeResourceChecker{},
				GrantRequest{PrincipalID: "principal_000001", Relation: RelationCanUse, ResourceType: ResourceTypeTool, ResourceID: "13"})
			outcomes <- outcome{grant, created, err}
		}()
	}
	wg.Wait()
	close(outcomes)
	createdCount := 0
	ids := map[string]bool{}
	for o := range outcomes {
		if o.err != nil {
			t.Fatalf("concurrent identical create: %v", o.err)
		}
		if o.created {
			createdCount++
		}
		ids[o.grant.ID] = true
	}
	if createdCount != 1 || len(ids) != 1 {
		t.Fatalf("created %d times with ids %v, want once and one id", createdCount, ids)
	}
	if all, _ := s.List(context.Background(), Filter{IncludeRevoked: true}); len(all) != 1 {
		t.Fatalf("rows = %d, want 1", len(all))
	}
}

// TestOnlyTheExactFormOfAnIDNamesAGrant: grant_1 and a bare number are not
// grant_000001.
func TestOnlyTheExactFormOfAnIDNamesAGrant(t *testing.T) {
	s := newTestStore(t)
	g := mustGrant(t, s, "principal_000001", RelationCanUse, ResourceTypeTool, "13")
	if g.ID != "grant_000001" {
		t.Fatalf("id = %s", g.ID)
	}
	for _, id := range []string{"grant_1", "1", "grant_0000001", "prediction_000001", ""} {
		if _, err := s.Get(context.Background(), id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("Get(%q) = %v, want ErrNotFound", id, err)
		}
	}
}

func TestEveryRelationNamesOnlyKnownResourceTypes(t *testing.T) {
	for _, relation := range Relations {
		if len(relation.ResourceTypes) == 0 {
			t.Fatalf("%s applies to nothing", relation.Name)
		}
		for _, resourceType := range relation.ResourceTypes {
			if _, err := lookupResourceType(resourceType); err != nil {
				t.Fatalf("%s names %q, which resource_type.go does not define: %v", relation.Name, resourceType, err)
			}
		}
	}
}

// TestAContactHoldsNoGrants pins the boundary the contact kind exists for: a
// requester is the outside party on a ticket, so granting one a tool, an agent
// or a board is refused at the write rather than discovered later.
func TestAContactHoldsNoGrants(t *testing.T) {
	s := newTestStore(t)
	_, _, err := s.Create(context.Background(), newFakeDirectory(), &fakeResourceChecker{}, GrantRequest{
		PrincipalID: "principal_000010", Relation: RelationCanUse, ResourceType: ResourceTypeTool, ResourceID: "13",
	})
	if !errors.Is(err, ErrInvalidGrant) {
		t.Fatalf("a contact must be refused a grant, got %v", err)
	}
	if !strings.Contains(err.Error(), "contact") {
		t.Fatalf("the refusal should name the kind: %v", err)
	}
	// A human is still granted, so the guard refuses contacts and nothing else.
	if _, _, err := s.Create(context.Background(), newFakeDirectory(), &fakeResourceChecker{}, GrantRequest{
		PrincipalID: "principal_000002", Relation: RelationCanUse, ResourceType: ResourceTypeTool, ResourceID: "13",
	}); err != nil {
		t.Fatalf("a human must still be granted: %v", err)
	}
}
