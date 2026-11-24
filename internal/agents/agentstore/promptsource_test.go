package agentstore

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const testHostDirectives = "# Directives\n\nBe plain.\n\n## Services\n\nUse the stores.\n"
const testHostOverview = "# Overview\n\nRepos live here.\n\n## Scheduler\n\n```bash\n# List jobs\ncurl :8092/api/jobs\n```\n"

type promptFixture struct {
	t     *testing.T
	home  string
	store *Store
}

func newPromptFixture(t *testing.T) *promptFixture {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	f := &promptFixture{t: t, home: home}
	f.write("AGENTS.md", testHostDirectives)
	f.write("CLAUDE.md", testHostOverview)
	store, err := Open(filepath.Join(home, "agents.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store = store
	if _, err := store.Scan(); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *promptFixture) write(relativePath, content string) {
	f.t.Helper()
	path := filepath.Join(f.home, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *promptFixture) read(relativePath string) string {
	f.t.Helper()
	data, err := os.ReadFile(filepath.Join(f.home, relativePath))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(data)
}

// importHost imports the two host files and returns the global collection id.
func (f *promptFixture) importHost() int64 {
	f.t.Helper()
	reports, err := f.store.ImportUntrackedPromptFiles([]string{"AGENTS.md", "CLAUDE.md"})
	if err != nil {
		f.t.Fatal(err)
	}
	if len(reports) != 1 {
		f.t.Fatalf("imported %d collections, want 1", len(reports))
	}
	return reports[0].CollectionID
}

func (f *promptFixture) render(collectionID int64) *PromptRenderResult {
	f.t.Helper()
	result, err := f.store.RenderPromptCollection(collectionID)
	if err != nil {
		f.t.Fatal(err)
	}
	return result
}

func TestImportStoresEverySectionAndWritesNothing(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()

	view, err := f.store.GetPromptCollectionView(id)
	if err != nil {
		t.Fatal(err)
	}
	var headings []string
	for _, section := range view.Sections {
		headings = append(headings, section.Heading)
	}
	if got, want := strings.Join(headings, "|"), "# Directives|## Services|# Overview|## Scheduler"; got != want {
		t.Fatalf("headings = %s, want %s", got, want)
	}
	if !strings.Contains(view.Sections[3].Body, "# List jobs") {
		t.Fatal("the shell comment inside the code fence was cut out of its section")
	}
	if f.read("AGENTS.md") != testHostDirectives || f.read("CLAUDE.md") != testHostOverview {
		t.Fatal("import changed a file on disk")
	}
	for _, output := range view.Outputs {
		if output.Drifted {
			t.Fatalf("%s reads as drifted straight after import", output.RelativePath)
		}
	}
	revisions, err := f.store.ListPromptSectionRevisions(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(revisions) != 4 || revisions[0].Source != PromptRevisionSourceImport {
		t.Fatalf("revisions = %d (source %q), want 4 import revisions", len(revisions), revisions[0].Source)
	}

	// A second import finds nothing left to claim.
	again, err := f.store.ImportUntrackedPromptFiles(nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("second import = %v, %v; want nothing", again, err)
	}
}

func TestRenderGivesEveryOutputTheSameFullPrompt(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	result := f.render(id)
	if result.RefusedReason != "" {
		t.Fatal(result.RefusedReason)
	}
	if len(result.WrittenFiles) != 2 {
		t.Fatalf("wrote %d files, want 2", len(result.WrittenFiles))
	}
	if f.read("AGENTS.md") != f.read("CLAUDE.md") {
		t.Fatal("the two outputs differ after a render")
	}
	for _, want := range []string{"Be plain.", "Repos live here.", "# List jobs"} {
		if !strings.Contains(f.read("CLAUDE.md"), want) {
			t.Fatalf("render lost %q", want)
		}
	}
	if again := f.render(id); len(again.WrittenFiles) != 0 {
		t.Fatalf("an unchanged collection rewrote %d files", len(again.WrittenFiles))
	}
}

func TestBodyEditInAFileIsCarriedBackAndReachesTheOtherFile(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	f.write("CLAUDE.md", strings.Replace(f.read("CLAUDE.md"), "Use the stores.", "Use the stores, always.", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 1 || reconciliation.Detected[0].Status != PromptDriftStatusApplied {
		t.Fatalf("reconcile = %+v, want one applied drift", reconciliation.Detected)
	}
	if !strings.Contains(f.read("AGENTS.md"), "Use the stores, always.") {
		t.Fatal("the edit did not reach the other output")
	}
	revisions, _ := f.store.ListPromptSectionRevisions(id, 0, 1)
	if revisions[0].Source != PromptRevisionSourceDrift || revisions[0].DriftID != reconciliation.Detected[0].ID {
		t.Fatalf("newest revision = %+v, want one from the drift", revisions[0])
	}
}

func TestEditBeforeTheFirstRenderTouchesOnlyItsOwnSections(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	// CLAUDE.md holds only half the collection here. Diffing it against all
	// the sections would read the other half as deleted.
	f.write("CLAUDE.md", strings.Replace(testHostOverview, "Repos live here.", "Repos live in ~/repos.", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 1 || reconciliation.Detected[0].Status != PromptDriftStatusApplied {
		t.Fatalf("reconcile = %+v", reconciliation.Detected)
	}
	view, _ := f.store.GetPromptCollectionView(id)
	if len(view.Sections) != 4 {
		t.Fatalf("sections = %d, want all 4 kept", len(view.Sections))
	}
	if !strings.Contains(f.read("AGENTS.md"), "Repos live in ~/repos.") || !strings.Contains(f.read("AGENTS.md"), "Be plain.") {
		t.Fatal("render after the drift is missing text")
	}
}

func TestAddedSectionIsHeldBlocksRenderAndAppliesWithItsLabels(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	f.write("AGENTS.md", strings.Replace(f.read("AGENTS.md"), "# Overview", "## Reminders\n\nOne coordinator.\n\n# Overview", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 1 {
		t.Fatalf("detected %d drifts, want 1", len(reconciliation.Detected))
	}
	drift := reconciliation.Detected[0]
	if drift.Status != PromptDriftStatusHeld || !drift.NeedsAnnotation() {
		t.Fatalf("drift = %+v, want held and waiting for labels", drift)
	}
	if strings.Contains(f.read("CLAUDE.md"), "Reminders") {
		t.Fatal("a held drift was rendered")
	}
	if result := f.render(id); result.RefusedReason == "" || len(result.WrittenFiles) != 0 {
		t.Fatal("render wrote over a file with unsettled drift")
	}
	if !strings.Contains(f.read("AGENTS.md"), "One coordinator.") {
		t.Fatal("the drifted file was overwritten")
	}

	if _, err := f.store.SetPromptDriftAnnotation(drift.ID, PromptDriftAnnotation{InsertedSections: []PromptDriftInsertedSectionLabel{{OperationIndex: 5}}}); err == nil {
		t.Fatal("a label for an operation that does not exist was accepted")
	}
	annotation := PromptDriftAnnotation{Note: "added reminders", AnnotatedBy: "test", InsertedSections: []PromptDriftInsertedSectionLabel{{OperationIndex: 0, Tags: []string{"Reminders", "scheduler"}}}}
	if err := f.store.ApplyPromptDrift(drift.ID, &annotation); err != nil {
		t.Fatal(err)
	}
	f.render(id)
	view, _ := f.store.GetPromptCollectionView(id)
	var headings []string
	for _, section := range view.Sections {
		headings = append(headings, section.Heading)
		if section.Heading == "## Reminders" && strings.Join(section.Tags, ",") != "reminders,scheduler" {
			t.Fatalf("tags = %v", section.Tags)
		}
	}
	if got, want := strings.Join(headings, "|"), "# Directives|## Services|## Reminders|# Overview|## Scheduler"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	if f.read("CLAUDE.md") != f.read("AGENTS.md") || !strings.Contains(f.read("CLAUDE.md"), "One coordinator.") {
		t.Fatal("outputs disagree after the drift was applied")
	}
}

// A run of new sections is anchored, every one of them, to the existing
// section before the run. Applying must keep the run in the file's order.
func TestSeveralSectionsAddedInARowKeepTheirOrder(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	f.write("AGENTS.md", strings.Replace(f.read("AGENTS.md"), "# Overview", "## Reminders\n\nOne coordinator.\n\n## Calendar\n\nThrough dash.\n\n## Mail\n\nThrough mailstack.\n\n# Overview", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 1 || reconciliation.Detected[0].Status != PromptDriftStatusHeld {
		t.Fatalf("reconcile = %+v, want one held drift", reconciliation.Detected)
	}
	drift := reconciliation.Detected[0]
	annotation := PromptDriftAnnotation{Note: "three sections", AnnotatedBy: "test", InsertedSections: []PromptDriftInsertedSectionLabel{
		{OperationIndex: 0, Tags: []string{"scheduler"}}, {OperationIndex: 1, Tags: []string{"calendar"}}, {OperationIndex: 2, Tags: []string{"mail"}}}}
	if err := f.store.ApplyPromptDrift(drift.ID, &annotation); err != nil {
		t.Fatal(err)
	}
	view, _ := f.store.GetPromptCollectionView(id)
	var headings []string
	for _, section := range view.Sections {
		headings = append(headings, section.Heading)
	}
	if got, want := strings.Join(headings, "|"), "# Directives|## Services|## Reminders|## Calendar|## Mail|# Overview|## Scheduler"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
}

func TestDismissedDriftIsOverwrittenButKept(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)
	f.write("CLAUDE.md", f.read("CLAUDE.md")+"\n## Scratch\n\nnot wanted\n")
	reconciliation, _ := f.store.ReconcilePromptDrifts()
	drift := reconciliation.Detected[0]
	if _, err := f.store.DismissPromptDrift(drift.ID); err != nil {
		t.Fatal(err)
	}
	if result := f.render(id); result.RefusedReason != "" {
		t.Fatal(result.RefusedReason)
	}
	if strings.Contains(f.read("CLAUDE.md"), "not wanted") {
		t.Fatal("dismissed edit survived the render")
	}
	kept, err := f.store.GetPromptDriftDiskContent(drift.ID)
	if err != nil || !strings.Contains(string(kept), "not wanted") {
		t.Fatal("the dismissed content was not kept on the drift")
	}
}

func TestSectionBodyThatWouldReadBackAsTwoSectionsIsRefused(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	_, err := f.store.CreatePromptSection(id, &PromptSection{Heading: "## A", Body: "x\n\n## B\n\ny", Enabled: true}, 0, "")
	if err == nil {
		t.Fatal("accepted a body holding a section-level heading")
	}
	if _, err := f.store.CreatePromptSection(id, &PromptSection{Heading: "## A", Body: "```\n## B\n```", Enabled: true}, 0, ""); err != nil {
		t.Fatalf("refused a heading-like line inside a code fence: %v", err)
	}
}

func TestResolveContextDeliversOncePerHarness(t *testing.T) {
	f := newPromptFixture(t)
	f.write("repos/logstack/AGENTS.md", "# Logstack\n\nGo service.\n")
	if _, err := f.store.Scan(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ImportUntrackedPromptFiles([]string{"AGENTS.md", "CLAUDE.md"}); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(f.home, "repos", "logstack", "cmd")

	if _, err := f.store.ResolveContext("claude_code", workDir, nil); !errors.Is(err, ErrPromptHarnessDeliveryUnknown) {
		t.Fatalf("unknown harness: err = %v, want ErrPromptHarnessDeliveryUnknown", err)
	}
	if _, err := f.store.SetPromptHarnessDelivery(PromptHarnessDelivery{Harness: "codex", Delivery: PromptDeliveryInject}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetPromptHarnessDelivery(PromptHarnessDelivery{Harness: "claude_code", Delivery: PromptDeliveryNativeFile, NativeRelativePath: "CLAUDE.md"}); err != nil {
		t.Fatal(err)
	}

	injected, err := f.store.ResolveContext("codex", workDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Be plain.", "Repos live here.", "Go service."} {
		if !strings.Contains(injected.Content, want) {
			t.Fatalf("inject harness is missing %q", want)
		}
	}
	if strings.Index(injected.Content, "Be plain.") > strings.Index(injected.Content, "Go service.") {
		t.Fatal("project sections came before host sections")
	}

	native, err := f.store.ResolveContext("claude_code", workDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(native.Content, "Be plain.") {
		t.Fatal("the host prompt was injected into a harness that reads it from its own file")
	}
	if !strings.Contains(native.Content, "Go service.") {
		t.Fatal("logstack renders to no CLAUDE.md, so its sections had to be injected and were not")
	}

	elsewhere, _ := f.store.ResolveContext("codex", filepath.Join(f.home, "repos", "other"), nil)
	if strings.Contains(elsewhere.Content, "Go service.") {
		t.Fatal("a project's sections reached a session outside its root")
	}
}

func TestWorktreeCopiesAreIgnoredNotDeleted(t *testing.T) {
	f := newPromptFixture(t)
	f.write("repos/logstack/AGENTS.md", "# Logstack\n")
	f.write("repos/logstack/.git/HEAD", "ref: refs/heads/main\n")
	f.write("repos/logstack-copy/AGENTS.md", "# Logstack\n")
	f.write("repos/logstack-copy/.git", "gitdir: ../logstack/.git/worktrees/copy\n")
	if _, err := f.store.CreateTrackedFileIgnoreRule(TrackedFileIgnoreKindGitWorktree, "", "a worktree's files are copies"); err != nil {
		t.Fatal(err)
	}
	result, err := f.store.Scan()
	if err != nil {
		t.Fatal(err)
	}
	if result.Ignored != 1 {
		t.Fatalf("ignored = %d, want 1", result.Ignored)
	}
	visible, _ := f.store.ListTrackedFiles("project", "")
	all, _ := f.store.ListTrackedFilesIncludingIgnored("project", "")
	if len(visible) != 1 || len(all) != 2 {
		t.Fatalf("visible = %d, all = %d; want 1 and 2", len(visible), len(all))
	}
	reports, err := f.store.ImportUntrackedPromptFiles(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, report := range reports {
		for _, file := range report.Files {
			if strings.Contains(file.Path, "logstack-copy") {
				t.Fatal("a worktree copy became a prompt collection")
			}
		}
	}
}

func TestPerFileSectionsTableIsSetAsideNotDropped(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "agents.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Put the database back into the shape the first prompt-collections commit left.
	for _, statement := range []string{
		`DROP TABLE prompt_sections`,
		`CREATE TABLE prompt_sections (id INTEGER PRIMARY KEY, collection_id INTEGER NOT NULL, title TEXT NOT NULL, heading TEXT, body TEXT NOT NULL, applies_to TEXT NOT NULL, priority INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1, source_path TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`,
		`CREATE INDEX idx_prompt_sections_collection ON prompt_sections(collection_id, priority, id)`,
		`INSERT INTO prompt_sections (collection_id, title, body, applies_to, created_at, updated_at) VALUES (1, 'old', 'old body', 'claude', 1, 1)`,
	} {
		if _, err := store.db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	store.Close()

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var kept, fresh int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM prompt_sections_legacy_per_file`).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM prompt_sections`).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if kept != 1 || fresh != 0 {
		t.Fatalf("legacy rows = %d, new rows = %d; want 1 and 0", kept, fresh)
	}
}

func TestLevelAndTitleDescribeASectionAndTheHeadingFollows(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()

	group := &PromptSection{Level: 1, Title: "House rules", Enabled: true}
	if _, err := f.store.CreatePromptSection(id, group, 0, ""); err != nil {
		t.Fatalf("a group with no text of its own was refused: %v", err)
	}
	if group.Heading != "# House rules" {
		t.Fatalf("heading = %q, want it composed from level and title", group.Heading)
	}
	child := &PromptSection{Level: 2, Title: "Shoes off", Body: "At the door.", Enabled: true}
	if _, err := f.store.CreatePromptSection(id, child, 0, ""); err != nil {
		t.Fatal(err)
	}

	// Placed after the group by id, it must land before the imported sections that follow.
	first, _ := f.store.ListPromptSections(id)
	placed := &PromptSection{Level: 2, Title: "Placed", Body: "here", Enabled: true}
	if _, err := f.store.CreatePromptSection(id, placed, first[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	ordered, _ := f.store.ListPromptSections(id)
	if ordered[1].ID != placed.ID {
		t.Fatalf("section placed after %d landed at index != 1: %v", first[0].ID, ordered[1].Title)
	}
	if _, err := f.store.CreatePromptSection(id, &PromptSection{Level: 2, Title: "Lost", Body: "x", Enabled: true}, 999999, ""); err == nil {
		t.Fatal("accepted an after_section_id from nowhere")
	}

	child.Title, child.Heading = "Shoes off indoors", ""
	if _, err := f.store.UpdatePromptSection(child.ID, child, "clearer"); err != nil {
		t.Fatal(err)
	}
	stored, _ := f.store.GetPromptSection(child.ID)
	if stored.Heading != "## Shoes off indoors" || stored.Level != 2 || stored.Title != "Shoes off indoors" {
		t.Fatalf("stored = level %d title %q heading %q", stored.Level, stored.Title, stored.Heading)
	}
	if !strings.Contains(f.read("CLAUDE.md"), "# House rules\n\n## Shoes off indoors\n\nAt the door.") {
		t.Fatal("render does not carry the group and its section")
	}

	for name, bad := range map[string]*PromptSection{
		"a level in the title":        {Level: 2, Title: "## Sneaky", Body: "x"},
		"a heading with no title":     {Level: 1, Title: " "},
		"no heading and no body":      {Level: 0, Title: "Preamble"},
		"a level deeper than a split": {Level: 3, Title: "Deep", Body: "x"},
		"a two-line title":            {Level: 2, Title: "a\nb", Body: "x"},
	} {
		if _, err := f.store.CreatePromptSection(id, bad, 0, ""); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestContextSectionsReachASessionOnlyWhenTheCardCarriesEveryTag(t *testing.T) {
	f := newPromptFixture(t)
	f.importHost()
	if _, err := f.store.SetPromptHarnessDelivery(PromptHarnessDelivery{Harness: "claude_code", Delivery: PromptDeliveryNativeFile, NativeRelativePath: "CLAUDE.md"}); err != nil {
		t.Fatal(err)
	}
	everywhere, err := f.store.EnsurePromptCollection("context", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	northwind := filepath.Join(f.home, "repos", "northwind-api")
	onlyNorthwind, err := f.store.EnsurePromptCollection("context", northwind, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if everywhere.Slug != "context" || onlyNorthwind.Slug != "context-northwind-api" {
		t.Fatalf("slugs = %q, %q", everywhere.Slug, onlyNorthwind.Slug)
	}
	create := func(collectionID int64, heading, body string, tags ...string) {
		t.Helper()
		if _, err := f.store.CreatePromptSection(collectionID, &PromptSection{Heading: heading, Body: body, Tags: tags, Enabled: true}, 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	create(everywhere.ID, "# Payments", "")
	create(everywhere.ID, "## Refunds", "Refunds go through the ledger.", "billing")
	create(everywhere.ID, "## Chargebacks", "Chargebacks need a human.", "billing", "disputes")
	create(everywhere.ID, "## Untagged", "Nobody should see this.")
	create(onlyNorthwind.ID, "## Northwind billing", "Northwind bills monthly.", "billing")

	resolve := func(workDir string, tags ...string) *ResolvedContext {
		t.Helper()
		resolved, err := f.store.ResolveContext("claude_code", workDir, tags)
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}

	billing := resolve(filepath.Join(northwind, "cmd"), "billing")
	for _, want := range []string{"# Payments", "Refunds go through the ledger.", "Northwind bills monthly."} {
		if !strings.Contains(billing.Content, want) {
			t.Fatalf("a billing card in northwind-api is missing %q:\n%s", want, billing.Content)
		}
	}
	for _, unwanted := range []string{"Chargebacks need a human.", "Nobody should see this.", "Be plain."} {
		if strings.Contains(billing.Content, unwanted) {
			t.Fatalf("a billing card in northwind-api got %q:\n%s", unwanted, billing.Content)
		}
	}
	if strings.Index(billing.Content, "# Payments") > strings.Index(billing.Content, "Refunds go through") {
		t.Fatal("a selected section came before the heading of its group")
	}

	disputes := resolve(filepath.Join(f.home, "repos", "other"), "billing", "disputes")
	if !strings.Contains(disputes.Content, "Chargebacks need a human.") {
		t.Fatal("a card carrying both tags did not get the section that needs both")
	}
	if strings.Contains(disputes.Content, "Northwind bills monthly.") {
		t.Fatal("a context collection rooted at northwind-api reached a session outside it")
	}

	none := resolve(northwind)
	if none.Content != "" {
		t.Fatalf("a session with no card tags got context:\n%s", none.Content)
	}
	var reasons []string
	for _, entry := range none.Manifest {
		for _, section := range entry.UnmatchedSections {
			reasons = append(reasons, section.Title+": "+section.Reason)
		}
	}
	if !slices.Contains(reasons, "Untagged: has no tags, so no card selects it") || !slices.Contains(reasons, "Chargebacks: the context tags lack billing, disputes") {
		t.Fatalf("the manifest does not say why sections were left out: %q", reasons)
	}
}

func TestAContextCollectionRendersToNoFile(t *testing.T) {
	f := newPromptFixture(t)
	collection, err := f.store.EnsurePromptCollection("context", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AddPromptCollectionOutput(collection.ID, "CLAUDE.md"); !errors.Is(err, ErrContextCollectionHasNoOutputs) {
		t.Fatalf("err = %v, want ErrContextCollectionHasNoOutputs", err)
	}
	if _, err := f.store.EnsurePromptCollection("context", "repos/relative", "", ""); err == nil {
		t.Fatal("accepted a relative root")
	}
	if _, err := f.store.EnsurePromptCollection("project", "", "", ""); err == nil {
		t.Fatal("accepted a project collection with no root")
	}
}

// A harness that edits a repo's prompt usually writes the same text to both
// AGENTS.md and CLAUDE.md. The first file's drift applies; by then the
// sections render exactly what the second file holds, so the second is
// accounted for and nothing is left for a person to settle.
func TestTheSameEditInBothFilesIsAppliedOnceAndLeavesNothingHeld(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	edited := strings.Replace(f.read("AGENTS.md"), "Use the stores.", "Use the stores, always.", 1)
	f.write("AGENTS.md", edited)
	f.write("CLAUDE.md", edited)
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 1 || reconciliation.Detected[0].Status != PromptDriftStatusApplied {
		t.Fatalf("detected = %+v, want the one edit applied once", reconciliation.Detected)
	}
	if len(reconciliation.Refused) != 0 {
		t.Fatalf("the render after applying was refused: %v", reconciliation.Refused)
	}

	view, err := f.store.GetPromptCollectionView(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.OpenDrifts) != 0 {
		t.Fatalf("open or held drifts = %+v, want none", view.OpenDrifts)
	}
	for _, output := range view.Outputs {
		if output.Drifted || output.AccountedSHA256 != output.DiskSHA256 {
			t.Fatalf("%s: drifted=%v accounted=%s disk=%s, want accounted for", output.RelativePath, output.Drifted, output.AccountedSHA256, output.DiskSHA256)
		}
	}
	if result := f.render(id); result.RefusedReason != "" || len(result.WrittenFiles) != 0 {
		t.Fatalf("render = refused %q, wrote %d; want nothing to do", result.RefusedReason, len(result.WrittenFiles))
	}
	revisions, _ := f.store.ListPromptSectionRevisions(id, 0, 0)
	fromDrift := 0
	for _, revision := range revisions {
		if revision.Source == PromptRevisionSourceDrift {
			fromDrift++
		}
	}
	if fromDrift != 1 {
		t.Fatalf("revisions from drift = %d, want 1", fromDrift)
	}
}

// Before the fix above, the second file's drift was held with no operations,
// and the render that would have caught the file up was refused because of
// that very drift, so it stayed held forever. The next scan settles a drift
// like that on its own.
func TestAHeldDriftForContentTheSectionsAlreadyRenderIsSettledByTheNextScan(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)
	staleSHA := sha256Hex([]byte(f.read("CLAUDE.md")))

	f.write("AGENTS.md", strings.Replace(f.read("AGENTS.md"), "Use the stores.", "Use the stores, always.", 1))
	if _, err := f.store.ReconcilePromptDrifts(); err != nil {
		t.Fatal(err)
	}
	// Put CLAUDE.md back in the state the old scan left it in: the edit on
	// disk, the baseline from before it, and a held drift with no operations.
	view, _ := f.store.GetPromptCollectionView(id)
	var claude PromptCollectionOutput
	for _, output := range view.Outputs {
		if output.RelativePath == "CLAUDE.md" {
			claude = output
		}
	}
	if !claude.MatchesRender {
		t.Fatal("setup: CLAUDE.md should already hold the render")
	}
	if _, err := f.store.db.Exec(`UPDATE prompt_collection_outputs SET accounted_sha256=? WHERE id=?`, staleSHA, claude.ID); err != nil {
		t.Fatal(err)
	}
	res, err := f.store.db.Exec(`INSERT INTO prompt_drifts (collection_id, output_id, accounted_sha256, disk_sha256, disk_content, status, held_reason, operations, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, '[]', ?)`, id, claude.ID, staleSHA, claude.DiskSHA256, []byte(f.read("CLAUDE.md")), PromptDriftStatusHeld,
		`the section "## Services" this edit touches has itself changed in the sections since the file was last accounted for`, now())
	if err != nil {
		t.Fatal(err)
	}
	stuckID, _ := res.LastInsertId()

	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 0 {
		t.Fatalf("detected = %+v, want nothing new", reconciliation.Detected)
	}
	stuck, err := f.store.GetPromptDrift(stuckID)
	if err != nil {
		t.Fatal(err)
	}
	if stuck.Status != PromptDriftStatusAlreadyAccounted || stuck.HeldReason != "" {
		t.Fatalf("stuck drift = status %q reason %q, want %q and no reason", stuck.Status, stuck.HeldReason, PromptDriftStatusAlreadyAccounted)
	}
	view, _ = f.store.GetPromptCollectionView(id)
	if len(view.OpenDrifts) != 0 {
		t.Fatalf("open or held drifts = %+v, want none", view.OpenDrifts)
	}
	for _, output := range view.Outputs {
		if output.AccountedSHA256 != output.DiskSHA256 {
			t.Fatalf("%s is still not accounted for", output.RelativePath)
		}
	}
}

// Two files carrying different edits to one section are a real conflict:
// the second must still wait for a person, and its text must survive.
func TestDifferentEditsToOneSectionInTwoFilesStillHoldTheSecond(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	rendered := f.read("AGENTS.md")
	f.write("AGENTS.md", strings.Replace(rendered, "Use the stores.", "Use the stores, always.", 1))
	f.write("CLAUDE.md", strings.Replace(rendered, "Use the stores.", "Use the stores, never.", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, drift := range reconciliation.Detected {
		statuses[filepath.Base(drift.Path)] = drift.Status
	}
	if statuses["AGENTS.md"] != PromptDriftStatusApplied || statuses["CLAUDE.md"] != PromptDriftStatusHeld {
		t.Fatalf("statuses = %v, want AGENTS.md applied and CLAUDE.md held", statuses)
	}
	if len(reconciliation.Refused) == 0 {
		t.Fatal("the render wrote over a file holding a conflicting edit")
	}
	if !strings.Contains(f.read("CLAUDE.md"), "Use the stores, never.") {
		t.Fatal("the conflicting edit was overwritten")
	}
}

// Agents keep AGENTS.md and CLAUDE.md identical, so an added section usually
// lands in both. That is one edit: the Files page shows it once, the tagger is
// asked once, and approving it settles both files.
func TestTheSameAddedSectionInBothFilesIsOneEditToApprove(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	edited := strings.Replace(f.read("AGENTS.md"), "# Overview", "## Reminders\n\nOne coordinator.\n\n# Overview", 1)
	f.write("AGENTS.md", edited)
	f.write("CLAUDE.md", edited)
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciliation.Detected) != 2 {
		t.Fatalf("detected %d drifts, want one per file", len(reconciliation.Detected))
	}
	needingLabels := 0
	for _, drift := range reconciliation.Detected {
		if drift.NeedsAnnotation() {
			needingLabels++
		}
	}
	if needingLabels != 1 {
		t.Fatalf("%d drifts ask the tagger for labels, want 1", needingLabels)
	}

	view, _ := f.store.GetPromptCollectionView(id)
	if len(view.OpenDrifts) != 1 {
		t.Fatalf("the view shows %d drifts, want the one edit", len(view.OpenDrifts))
	}
	shown := view.OpenDrifts[0]
	if len(shown.Twins) != 1 || shown.Twins[0].Path == shown.Path {
		t.Fatalf("twins = %+v, want the other file", shown.Twins)
	}

	annotation := PromptDriftAnnotation{InsertedSections: []PromptDriftInsertedSectionLabel{{OperationIndex: 0, Tags: []string{"scheduler"}}}}
	if err := f.store.ApplyPromptDrift(shown.ID, &annotation); err != nil {
		t.Fatal(err)
	}
	twin, err := f.store.GetPromptDrift(shown.Twins[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if twin.Status != PromptDriftStatusApplied {
		t.Fatalf("twin status = %q, want %q", twin.Status, PromptDriftStatusApplied)
	}
	if result := f.render(id); result.RefusedReason != "" {
		t.Fatalf("render refused after approving: %s", result.RefusedReason)
	}
	view, _ = f.store.GetPromptCollectionView(id)
	if len(view.OpenDrifts) != 0 {
		t.Fatalf("open drifts = %+v, want none", view.OpenDrifts)
	}
	for _, output := range view.Outputs {
		if output.AccountedSHA256 != output.DiskSHA256 || !output.MatchesRender {
			t.Fatalf("%s is not accounted for after approving", output.RelativePath)
		}
	}
}

func TestDismissingOneFileOfAnEditDismissesTheOther(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)
	rendered := f.read("AGENTS.md")
	edited := rendered + "\n## Scratch\n\nnot wanted\n"
	f.write("AGENTS.md", edited)
	f.write("CLAUDE.md", edited)
	if _, err := f.store.ReconcilePromptDrifts(); err != nil {
		t.Fatal(err)
	}
	view, _ := f.store.GetPromptCollectionView(id)
	if len(view.OpenDrifts) != 1 {
		t.Fatalf("the view shows %d drifts, want 1", len(view.OpenDrifts))
	}
	if _, err := f.store.DismissPromptDrift(view.OpenDrifts[0].ID); err != nil {
		t.Fatal(err)
	}
	if result := f.render(id); result.RefusedReason != "" {
		t.Fatalf("render refused after dismissing: %s", result.RefusedReason)
	}
	if f.read("AGENTS.md") != rendered || f.read("CLAUDE.md") != rendered {
		t.Fatal("the dismissed edit survived the render")
	}
}

// An edit that only adds sections waits for their tags and then applies by
// itself: no person approves it, and no section lands untagged.
func TestAnAddedSectionAppliesItselfOnceTagged(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	f.write("AGENTS.md", strings.Replace(f.read("AGENTS.md"), "# Overview", "## Reminders\n\nOne coordinator.\n\n# Overview", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	drift := reconciliation.Detected[0]
	if drift.Status != PromptDriftStatusHeld || drift.HeldReason != PromptDriftHeldReasonAddsSections {
		t.Fatalf("drift = status %q reason %q, want held for its tags", drift.Status, drift.HeldReason)
	}

	untagged, err := f.store.SetPromptDriftAnnotation(drift.ID, PromptDriftAnnotation{Note: "no tags yet", InsertedSections: []PromptDriftInsertedSectionLabel{{OperationIndex: 0}}})
	if err != nil {
		t.Fatal(err)
	}
	if untagged.Status != PromptDriftStatusHeld {
		t.Fatalf("a label with no tags applied the drift: status %q", untagged.Status)
	}

	tagged, err := f.store.SetPromptDriftAnnotation(drift.ID, PromptDriftAnnotation{Note: "added reminders", AnnotatedBy: "test",
		InsertedSections: []PromptDriftInsertedSectionLabel{{OperationIndex: 0, Tags: []string{"scheduler"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if tagged.Status != PromptDriftStatusApplied {
		t.Fatalf("tagged drift = status %q reason %q, want applied", tagged.Status, tagged.HeldReason)
	}
	f.render(id)
	if !strings.Contains(f.read("CLAUDE.md"), "One coordinator.") {
		t.Fatal("the added section did not reach the other file")
	}
}

// Removing a section can drop a rule without anyone noticing, so it waits for
// a person even after it is annotated.
func TestARemovedSectionWaitsForAPerson(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	view, _ := f.store.GetPromptCollectionView(id)
	removed := ""
	for _, section := range view.Sections {
		if section.Heading == "## Scheduler" {
			removed = renderPromptSections([]PromptSection{section})
		}
	}
	if removed == "" || !strings.Contains(f.read("AGENTS.md"), removed) {
		t.Fatalf("fixture has no ## Scheduler section to remove")
	}
	f.write("AGENTS.md", strings.Replace(f.read("AGENTS.md"), removed, "", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	drift := reconciliation.Detected[0]
	if drift.Status != PromptDriftStatusHeld || drift.HeldReason != PromptDriftHeldReasonRemovesOrRenames {
		t.Fatalf("drift = status %q reason %q, want held for a person", drift.Status, drift.HeldReason)
	}
	annotated, err := f.store.SetPromptDriftAnnotation(drift.ID, PromptDriftAnnotation{Note: "removes scheduler"})
	if err != nil {
		t.Fatal(err)
	}
	if annotated.Status != PromptDriftStatusHeld {
		t.Fatalf("annotating a removal applied it: status %q", annotated.Status)
	}
}

// A changed heading is a rename: held for a person, with the old heading on
// the operation so the page can show what it was.
func TestARenamedHeadingWaitsForAPerson(t *testing.T) {
	f := newPromptFixture(t)
	id := f.importHost()
	f.render(id)

	f.write("AGENTS.md", strings.Replace(f.read("AGENTS.md"), "## Scheduler", "## Cron", 1))
	reconciliation, err := f.store.ReconcilePromptDrifts()
	if err != nil {
		t.Fatal(err)
	}
	drift := reconciliation.Detected[0]
	if drift.Status != PromptDriftStatusHeld || drift.HeldReason != PromptDriftHeldReasonRemovesOrRenames {
		t.Fatalf("drift = status %q reason %q, want held for a person", drift.Status, drift.HeldReason)
	}
	if len(drift.Operations) != 1 || drift.Operations[0].PreviousHeading != "## Scheduler" || drift.Operations[0].Heading != "## Cron" {
		t.Fatalf("operations = %+v, want one rename from ## Scheduler to ## Cron", drift.Operations)
	}
}
