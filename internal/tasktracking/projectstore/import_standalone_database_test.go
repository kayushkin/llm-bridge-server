package projectstore

import (
	"context"
	"database/sql"
	_ "embed"
	"path/filepath"
	"testing"
)

//go:embed testdata/standalone_schema.sql
var standaloneSchema string

func standaloneDatabase(t *testing.T, statements ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "project-store.db")
	source, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	for _, statement := range append([]string{standaloneSchema}, statements...) {
		if _, err := source.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return path
}

var standaloneRows = []string{
	`INSERT INTO id_sequences VALUES ('project', 6), ('suggestion', 3), ('session_filing', 9), ('goal', 4)`,
	// project_000004 names a parent made after it.
	`INSERT INTO projects VALUES ('project_000004', 'Inbox', 'project', 'project_000005', 'One inbox.', 'Mail reads in one place.', 'building', 0.25, 'agent', 100, 200, 0)`,
	`INSERT INTO projects VALUES ('project_000005', 'Communication', 'stream', NULL, '', '', 'keeping_up', NULL, '', 50, 50, 0)`,
	`INSERT INTO projects VALUES ('project_000002', 'Old', 'project', '', '', '', 'done', NULL, '', 10, 10, 300)`,
	`INSERT INTO project_links VALUES (7, 'project_000004', 'repo', '15', 'dash', 'its front end', 'agent', 110)`,
	`INSERT INTO project_links VALUES (9, 'project_000004', 'session', 'br_1', 'a chat', 'filed (90% sure, session_filing_000008)', 'project-store session filer', 120)`,
	`INSERT INTO project_changes VALUES (3, 'project_000004', 'stage', 'idea', 'building', 'agent', 150)`,
	`INSERT INTO suggestions VALUES ('suggestion_000002', 'project_000004', 'Merge the branch', 'It waits.', 'proposed', 'model-a', 'agent', 160, '', 0, '')`,
	`INSERT INTO session_filings VALUES ('session_filing_000008', 'br_1', 'a chat', 'brief', 'Read mail', '[15]', 'project_000004', 0.9, 'It is about mail.', 'model-a', 'filed', 9, 'project_000004', 'project-store session filer', 120, '', 0)`,
	// goal_000002 names a parent made after it.
	`INSERT INTO goals VALUES ('goal_000003', 'project_000004', NULL, 'One inbox', 'Every chat in one list', 'open', 2, 1800000000, 'agent', 130, 140, '', 0, '')`,
	`INSERT INTO goals VALUES ('goal_000002', 'project_000004', 'goal_000003', 'Read Signal', '', 'reached', 0, 0, 'agent', 125, 150, 'agent', 150, 'it reads')`,
	`INSERT INTO goal_changes VALUES (5, 'goal_000002', 'state', 'open', 'reached', 'agent', 150)`,
	`INSERT INTO goal_links VALUES (6, 'goal_000003', 'session', 'br_1', 'a chat', '', 'agent', 135)`,
	`INSERT INTO session_filings VALUES ('session_filing_000003', 'br_2', '', 'none', '', '[]', '', 0, '', '', 'nothing_to_file', 0, '', 'project-store session filer', 90, '', 0)`,
}

func TestImportCopiesEveryRecordWithItsId(t *testing.T) {
	store := newTestStore(t)
	counts, err := ImportStandaloneDatabase(context.Background(), store.database, standaloneDatabase(t, standaloneRows...))
	if err != nil {
		t.Fatalf("ImportStandaloneDatabase: %v", err)
	}
	want := ImportCounts{Projects: 3, Links: 2, Changes: 1, Suggestions: 1, SessionFilings: 2, LastProjectNumber: 5,
		LastSuggestionNumber: 2, LastSessionFilingNumber: 8, LastLinkID: 9, LastChangeID: 3,
		Goals: 2, GoalChanges: 1, GoalLinks: 1, LastGoalNumber: 3, LastGoalChangeID: 5, LastGoalLinkID: 6}
	if counts != want {
		t.Errorf("counts = %+v, want %+v", counts, want)
	}

	inbox, err := store.Project("project_000004")
	if err != nil {
		t.Fatal(err)
	}
	if inbox.ParentID != "project_000005" || inbox.Name != "Inbox" || inbox.SpendTargetShare == nil || *inbox.SpendTargetShare != 0.25 || inbox.CreatedAt != 100 {
		t.Errorf("project_000004 came across as %+v", inbox)
	}
	if old, err := store.Project("project_000002"); err != nil || old.ParentID != "" || old.ArchivedAt != 300 {
		t.Errorf("project_000002 came across as %+v (%v)", old, err)
	}
	links, err := store.Links(LinkFilter{ProjectID: "project_000004"})
	if err != nil || len(links) != 2 || links[0].ID != 7 || links[1].EntityRef != "br_1" {
		t.Errorf("links = %+v (%v)", links, err)
	}
	filing, err := store.SessionFiling("session_filing_000008")
	if err != nil || filing.LinkID != 9 || filing.FiledProjectID != "project_000004" || len(filing.RepoStoreIDs) != 1 || filing.RepoStoreIDs[0] != 15 {
		t.Errorf("session_filing_000008 came across as %+v (%v)", filing, err)
	}
	if none, err := store.SessionFiling("session_filing_000003"); err != nil || none.ProjectID != "" || none.FiledProjectID != "" {
		t.Errorf("session_filing_000003 came across as %+v (%v)", none, err)
	}

	// Each table numbers on from the last number the file handed out.
	name := "Next"
	created, err := store.CreateProject(ProjectFields{Name: &name}, "")
	if err != nil || created.ID != "project_000006" {
		t.Errorf("the first project after the import is %q (%v), want project_000006: id_sequences had handed out 5", created.ID, err)
	}
	suggestions, err := store.ReplaceProposals("project_000004", []ProposedStep{{Title: "Write the test"}}, "model-b", "agent")
	if err != nil || len(suggestions) != 1 || suggestions[0].ID != "suggestion_000003" {
		t.Errorf("the first suggestion after the import is %+v (%v), want suggestion_000003", suggestions, err)
	}
	recorded, err := store.RecordSessionFiling(SessionFiling{SessionID: "br_3", Evidence: SessionFilingEvidenceNone, Status: SessionFilingNothingToFile})
	if err != nil || recorded.ID != "session_filing_000009" {
		t.Errorf("the first filing after the import is %q (%v), want session_filing_000009", recorded.ID, err)
	}

	signal, err := store.Goal("goal_000002")
	if err != nil || signal.ParentGoalID != "goal_000003" || signal.State != GoalReached || signal.ClosedBecause != "it reads" || signal.ProjectID != "project_000004" {
		t.Errorf("goal_000002 came across as %+v (%v)", signal, err)
	}
	if changes, err := store.GoalChanges("goal_000002"); err != nil || len(changes) != 1 || changes[0].ID != 5 {
		t.Errorf("goal changes %+v (%v)", changes, err)
	}
	if links, err := store.GoalLinks(GoalLinkFilter{ProjectID: "project_000004"}); err != nil || len(links) != 1 || links[0].GoalID != "goal_000003" {
		t.Errorf("goal links %+v (%v)", links, err)
	}
	statement := "Read Slack"
	if goal, err := store.CreateGoal("project_000004", GoalFields{Statement: &statement}, ""); err != nil || goal.ID != "goal_000004" {
		t.Errorf("the first goal after the import is %q (%v), want goal_000004: id_sequences had handed out 3", goal.ID, err)
	}

	if _, err := ImportStandaloneDatabase(context.Background(), store.database, standaloneDatabase(t, standaloneRows...)); err == nil {
		t.Error("a second import into a module that holds rows was taken")
	}
}

func TestImportRefusesWhatItCannotCopyWhole(t *testing.T) {
	cases := map[string][]string{
		"an unknown column":       {`ALTER TABLE projects ADD COLUMN colour TEXT`},
		"an id of the wrong form": {`INSERT INTO projects VALUES ('project_0000001', 'x', 'project', NULL, '', '', 'idea', NULL, '', 1, 1, 0)`},
		"a link to no project":    {`INSERT INTO project_links VALUES (1, 'project_000009', 'repo', '15', '', '', '', 1)`},
	}
	for name, statements := range cases {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			if _, err := ImportStandaloneDatabase(context.Background(), store.database, standaloneDatabase(t, statements...)); err == nil {
				t.Fatal("the import took a file it cannot copy whole")
			}
			if projects, err := store.Projects(ProjectFilter{IncludeArchived: true}); err != nil || len(projects) != 0 {
				t.Errorf("a refused import left %d projects (%v)", len(projects), err)
			}
		})
	}
}
